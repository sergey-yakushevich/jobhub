package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Application statuses — where a sent application stands. A lead that has been
// applied to with no explicit status yet is in process by definition: the
// application exists and nobody has answered.
const (
	AppInProcess = "in_process"
	AppRejected  = "rejected"
	AppHired     = "hired"
)

// ErrBadAppStatus is returned for a status outside the three the funnel knows.
// The set is closed on purpose: "ghosted", "second round" and friends belong in
// the timeline as events, not in a status column that filters would then have
// to enumerate.
var ErrBadAppStatus = errors.New(`app_status must be "in_process", "rejected" or "hired"`)

// NormalizeAppStatus maps the spellings people actually type onto the three
// stored values; empty stays empty (it means "no explicit status").
func NormalizeAppStatus(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "", AppInProcess, AppRejected, AppHired:
		return v, nil
	case "in process", "in-process", "inprocess", "in_progress", "in progress", "in-progress":
		return AppInProcess, nil
	}
	return "", ErrBadAppStatus
}

// JobEvent is one moment on a lead's application timeline: an email that
// arrived, a DM reply, a status change, a note from a monitoring pass. The
// timeline is append-only — progress is a history, not a field that gets
// overwritten, which is what lets "recruiter replied 3 days ago, then silence"
// stay visible.
type JobEvent struct {
	ID    int64  `json:"id"`
	JobID int64  `json:"job_id"`
	Kind  string `json:"kind"`
	Note  string `json:"note"`
	// HappenedAt is when the thing occurred (the email's date), CreatedAt when
	// it was recorded. They differ whenever a monitoring pass catches up on a
	// few days at once, and the timeline sorts by the first.
	HappenedAt time.Time `json:"happened_at"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Store) migrateEvents() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS job_events (
  id          INTEGER PRIMARY KEY,
  job_id      INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL DEFAULT '',
  note        TEXT NOT NULL DEFAULT '',
  happened_at TEXT NOT NULL,
  created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_job_events_job ON job_events(job_id, happened_at);
`)
	return err
}

// AddJobEvent appends one moment to a lead's timeline and stamps the lead as
// checked — an event arriving means somebody just looked at the application's
// progress. A zero happenedAt means "now".
func (s *Store) AddJobEvent(jobID int64, kind, note string, happenedAt, now time.Time) (*JobEvent, error) {
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM jobs WHERE id = ?`, jobID).Scan(&exists); err != nil {
		return nil, sql.ErrNoRows
	}
	if happenedAt.IsZero() {
		happenedAt = now
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`INSERT INTO job_events (job_id, kind, note, happened_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		jobID, strings.ToLower(strings.TrimSpace(kind)), strings.TrimSpace(note),
		fmtTime(happenedAt), fmtTime(now))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE jobs SET checked_at = ? WHERE id = ?`, fmtTime(now), jobID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &JobEvent{ID: id, JobID: jobID, Kind: strings.ToLower(strings.TrimSpace(kind)),
		Note: strings.TrimSpace(note), HappenedAt: happenedAt, CreatedAt: now}, nil
}

// JobEvents lists a lead's timeline newest first — the order the job page
// reads it in.
func (s *Store) JobEvents(jobID int64) ([]JobEvent, error) {
	rows, err := s.db.Query(
		`SELECT id, job_id, kind, note, happened_at, created_at
		 FROM job_events WHERE job_id = ? ORDER BY happened_at DESC, id DESC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobEvent
	for rows.Next() {
		var e JobEvent
		var happened, created string
		if err := rows.Scan(&e.ID, &e.JobID, &e.Kind, &e.Note, &happened, &created); err != nil {
			return nil, err
		}
		e.HappenedAt = parseTime(happened)
		e.CreatedAt = parseTime(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TouchJobChecked stamps when the application's progress was last looked at —
// inbox read, DMs read, nothing new. Unlike viewed_at it always moves forward:
// "last checked" is only useful if it means the LAST check.
func (s *Store) TouchJobChecked(id int64, now time.Time) error {
	res, err := s.db.Exec(`UPDATE jobs SET checked_at = ? WHERE id = ?`, fmtTime(now), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetJobAppStatus records where the sent application stands. Setting a status
// on a lead that was never marked applied stamps applied_at too — a status
// implies an application exists — and every non-empty change appends a
// "status" event so the timeline tells the story without anyone remembering to
// write it. Empty clears the explicit status (back to the in-process default).
func (s *Store) SetJobAppStatus(id int64, status, note string, now time.Time) error {
	status, err := NormalizeAppStatus(status)
	if err != nil {
		return err
	}
	job, err := s.JobByID(id)
	if err != nil {
		return sql.ErrNoRows
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE jobs SET app_status = ?, checked_at = ?,
		   applied_at = CASE WHEN ? != '' AND applied_at = '' THEN ? ELSE applied_at END
		 WHERE id = ?`,
		status, fmtTime(now), status, fmtTime(now), id); err != nil {
		return err
	}
	// The change is only history when it IS a change; re-stating the same
	// status (a monitoring pass confirming "still in process") just moves
	// checked_at.
	if status != "" && status != job.AppStatus {
		text := strings.TrimSpace(note)
		label := strings.ReplaceAll(status, "_", " ")
		if text != "" {
			label += " — " + text
		}
		if _, err := tx.Exec(
			`INSERT INTO job_events (job_id, kind, note, happened_at, created_at) VALUES (?, 'status', ?, ?, ?)`,
			id, label, fmtTime(now), fmtTime(now)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
