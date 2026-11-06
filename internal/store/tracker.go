package store

// Timeline-first tracker: a job is a row, its progress is an append-only list
// of events. This file owns the additive tracker_jobs / tracker_events tables,
// their validation and the one-time import of the legacy jobs / job_events
// data. Nothing here touches the legacy tables.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const (
	TrackerStageSaved     = "saved"
	TrackerStageApplied   = "applied"
	TrackerStageTalking   = "talking"
	TrackerStageInterview = "interview"
	TrackerStageOffer     = "offer"
	TrackerStageHired     = "hired"
	TrackerStageRejected  = "rejected"
	TrackerStageArchived  = "archived"

	TrackerRemoteUnknown = "unknown"
	TrackerRemoteRemote  = "remote"
	TrackerRemoteHybrid  = "hybrid"
	TrackerRemoteOnsite  = "onsite"

	TrackerWaitingAgent     = "agent"
	TrackerWaitingRecruiter = "recruiter"
	TrackerWaitingYou       = "you"
)

var (
	// ErrTrackerConflict means the caller replayed a source_key with a payload
	// that differs from the stored one. The timeline is append-only, so the
	// only honest answer is 409, never a silent rewrite.
	ErrTrackerConflict = errors.New("tracker: source_key already exists with a different payload")
	// ErrTrackerVersion is an optimistic-concurrency failure: the caller edited
	// a row that changed since it was read.
	ErrTrackerVersion = errors.New("tracker: version mismatch")
	// ErrTrackerValidation covers every invalid field. Callers should surface
	// the wrapped message to the person who sent the payload.
	ErrTrackerValidation = errors.New("tracker: invalid")
)

// TrackedJob is one role on the timeline-first board. Times are RFC3339
// strings, empty when unknown, so the JSON shape survives a round trip through
// a browser without re-defining what a zero time means.
type TrackedJob struct {
	ID               int64    `json:"id"`
	SourceKey        string   `json:"source_key"`
	Profile          string   `json:"profile"`
	Company          string   `json:"company"`
	Title            string   `json:"title"`
	URL              string   `json:"url"`
	Description      string   `json:"description"`
	Stage            string   `json:"stage"`
	Remote           string   `json:"remote"`
	Location         string   `json:"location"`
	Employment       string   `json:"employment"`
	SalaryMin        *float64 `json:"salary_min"`
	SalaryMax        *float64 `json:"salary_max"`
	Currency         string   `json:"currency"`
	SalaryPeriod     string   `json:"salary_period"`
	FitScore         *float64 `json:"fit_score"`
	FitSummary       string   `json:"fit_summary"`
	Strengths        []string `json:"strengths"`
	Gaps             []string `json:"gaps"`
	Skills           []string `json:"skills"`
	RecruiterName    string   `json:"recruiter_name"`
	RecruiterContact string   `json:"recruiter_contact"`
	NextAction       string   `json:"next_action"`
	NextActionAt     string   `json:"next_action_at"`
	WaitingOn        string   `json:"waiting_on"`
	NeedsYou         bool     `json:"needs_you"`
	InterviewAt      string   `json:"interview_at"`
	InterviewURL     string   `json:"interview_url"`
	LastCheckedAt    string   `json:"last_checked_at"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
	Version          int64    `json:"version"`
}

// TimelineEvent is one moment in a job's history. It carries its own source_key
// so a retried agent write is idempotent, and never claims more verification
// than it has: "recorded" means the app stored what a caller said, not that any
// server proved an external message was sent.
type TimelineEvent struct {
	ID           int64  `json:"id"`
	JobID        int64  `json:"job_id"`
	SourceKey    string `json:"source_key"`
	Kind         string `json:"kind"`
	Summary      string `json:"summary"`
	Body         string `json:"body"`
	Actor        string `json:"actor"`
	Channel      string `json:"channel"`
	Direction    string `json:"direction"`
	Contact      string `json:"contact"`
	Account      string `json:"account"`
	ThreadID     string `json:"thread_id"`
	ExternalID   string `json:"external_id"`
	SourceURL    string `json:"source_url"`
	Verification string `json:"verification"`
	HappenedAt   string `json:"happened_at"`
	RecordedAt   string `json:"recorded_at"`
	Stage        string `json:"stage"`
}

type TrackerFilter struct {
	// Profile narrows to one person's board; empty means every profile.
	Profile string
	// ActiveProfilesOnly is used by the UI's All view, excluding retired history.
	ActiveProfilesOnly bool
	// ExcludeArchived keeps the normal board focused; explicit archive views
	// and agent API reads may still include archived records.
	ExcludeArchived bool
	Stage           string
	Remote          string
	// Query is a case-insensitive contains test over title, company and
	// description.
	Query    string
	NeedsYou bool
	Limit    int
	BeforeID int64
}

type TrackerPage struct {
	Items      []TrackedJob `json:"items"`
	NextCursor string       `json:"next_cursor"`
}

type TimelinePage struct {
	Items      []TimelineEvent `json:"items"`
	NextCursor string          `json:"next_cursor"`
}

const (
	trackerMaxSourceKey  = 300
	trackerMaxCompany    = 300
	trackerMaxTitle      = 300
	trackerMaxURL        = 2048
	trackerMaxDesc       = 20000
	trackerMaxText       = 4000
	trackerMaxListItems  = 100
	trackerMaxListItem   = 200
	trackerMaxEventSum   = 2000
	trackerMaxEventBody  = 20000
	trackerMaxEventActor = 200
)

var (
	trackerStages = map[string]bool{
		TrackerStageSaved: true, TrackerStageApplied: true, TrackerStageTalking: true,
		TrackerStageInterview: true, TrackerStageOffer: true, TrackerStageHired: true,
		TrackerStageRejected: true, TrackerStageArchived: true,
	}
	trackerRemotes = map[string]bool{
		TrackerRemoteUnknown: true, TrackerRemoteRemote: true,
		TrackerRemoteHybrid: true, TrackerRemoteOnsite: true,
	}
	trackerWaiting = map[string]bool{"": true, TrackerWaitingAgent: true,
		TrackerWaitingRecruiter: true, TrackerWaitingYou: true}
	trackerEmployment = map[string]bool{"": true, "full-time": true, "part-time": true,
		"contract": true, "freelance": true}
	trackerSalaryPeriods = map[string]bool{"": true, "year": true, "month": true, "hour": true}
	trackerEventKinds    = map[string]bool{"note": true, "discovered": true,
		"application": true, "message": true, "follow_up": true, "interview": true,
		"status": true, "check": true, "document": true}
	trackerEventChannels = map[string]bool{"": true, "email": true, "linkedin": true,
		"telegram": true, "ats": true, "calendar": true, "manual": true,
		"system": true, "other": true}
	trackerEventDirections = map[string]bool{"": true, "inbound": true, "outbound": true,
		"internal": true}
	trackerEventVerification = map[string]bool{"recorded": true, "confirmed": true,
		"uncertain": true}
)

// MigrateTracker creates the tracker schema and imports the legacy board once.
// The marker row is written in the same transaction as the backfill, so a crash
// cannot leave a half-imported board that is then imported again on boot. DDL
// is CREATE IF NOT EXISTS, so calling this on every open is harmless.
func (s *Store) MigrateTracker() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS tracker_jobs (
  id                INTEGER PRIMARY KEY,
  source_key        TEXT NOT NULL,
  profile           TEXT NOT NULL,
  company           TEXT NOT NULL DEFAULT '',
  title             TEXT NOT NULL DEFAULT '',
  url               TEXT NOT NULL DEFAULT '',
  description       TEXT NOT NULL DEFAULT '',
  stage             TEXT NOT NULL DEFAULT 'saved',
  remote            TEXT NOT NULL DEFAULT 'unknown',
  location          TEXT NOT NULL DEFAULT '',
  employment        TEXT NOT NULL DEFAULT '',
  salary_min        REAL,
  salary_max        REAL,
  currency          TEXT NOT NULL DEFAULT '',
  salary_period     TEXT NOT NULL DEFAULT '',
  fit_score         REAL,
  fit_summary       TEXT NOT NULL DEFAULT '',
  strengths         TEXT NOT NULL DEFAULT '[]',
  gaps              TEXT NOT NULL DEFAULT '[]',
  skills            TEXT NOT NULL DEFAULT '[]',
  recruiter_name    TEXT NOT NULL DEFAULT '',
  recruiter_contact TEXT NOT NULL DEFAULT '',
  next_action       TEXT NOT NULL DEFAULT '',
  next_action_at    TEXT NOT NULL DEFAULT '',
  waiting_on        TEXT NOT NULL DEFAULT '',
  needs_you         INTEGER NOT NULL DEFAULT 0,
  interview_at      TEXT NOT NULL DEFAULT '',
  interview_url     TEXT NOT NULL DEFAULT '',
  last_checked_at   TEXT NOT NULL DEFAULT '',
  created_at        TEXT NOT NULL,
  updated_at        TEXT NOT NULL,
  version           INTEGER NOT NULL DEFAULT 1,
  create_payload    TEXT NOT NULL DEFAULT '',
  UNIQUE (profile, source_key)
);
CREATE INDEX IF NOT EXISTS idx_tracker_jobs_stage ON tracker_jobs(profile, stage, id);
CREATE INDEX IF NOT EXISTS idx_tracker_jobs_id ON tracker_jobs(id);

CREATE TABLE IF NOT EXISTS tracker_events (
  id           INTEGER PRIMARY KEY,
  job_id       INTEGER NOT NULL REFERENCES tracker_jobs(id) ON DELETE CASCADE,
  source_key   TEXT NOT NULL,
  kind         TEXT NOT NULL,
  summary      TEXT NOT NULL DEFAULT '',
  body         TEXT NOT NULL DEFAULT '',
  actor        TEXT NOT NULL,
  channel      TEXT NOT NULL DEFAULT '',
  direction    TEXT NOT NULL DEFAULT '',
  contact      TEXT NOT NULL DEFAULT '',
  account      TEXT NOT NULL DEFAULT '',
  thread_id    TEXT NOT NULL DEFAULT '',
  external_id  TEXT NOT NULL DEFAULT '',
  source_url   TEXT NOT NULL DEFAULT '',
  verification TEXT NOT NULL DEFAULT 'recorded',
  happened_at  TEXT NOT NULL,
  recorded_at  TEXT NOT NULL,
  stage        TEXT NOT NULL DEFAULT '',
  UNIQUE (job_id, source_key)
);
CREATE INDEX IF NOT EXISTS idx_tracker_events_job ON tracker_events(job_id, id);
CREATE INDEX IF NOT EXISTS idx_tracker_events_happened ON tracker_events(job_id, happened_at);

CREATE TABLE IF NOT EXISTS tracker_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`); err != nil {
		return err
	}
	// Existing local preview databases may predate request snapshots.
	if _, err := tx.Exec(`ALTER TABLE tracker_jobs ADD COLUMN create_payload TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	var marker string
	err = tx.QueryRow(`SELECT value FROM tracker_meta WHERE key = 'legacy_backfill_v1'`).Scan(&marker)
	if err == nil {
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := backfillTracker(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO tracker_meta (key, value) VALUES ('legacy_backfill_v1', ?)`,
		fmtTime(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

type trackerExecer interface {
	Exec(string, ...any) (sql.Result, error)
	QueryRow(string, ...any) *sql.Row
}

type trackerQuerier interface {
	Query(string, ...any) (*sql.Rows, error)
}

// legacyStage maps the old funnel onto the new one. hired/rejected are carried
// over as outcomes, an applied lead stays applied, and anything earlier is
// honestly still saved — the old board did not distinguish talking from
// interviewing, so inventing those stages would rewrite history.
func legacyStage(applied, rejected, appStatus string) string {
	switch {
	case appStatus == AppHired:
		return TrackerStageHired
	case appStatus == AppRejected || rejected != "":
		return TrackerStageRejected
	case applied != "" || appStatus == AppInProcess:
		return TrackerStageApplied
	default:
		return TrackerStageSaved
	}
}

func legacyEventKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "status":
		return "status"
	case "check", "checked":
		return "check"
	case "application", "applied":
		return "application"
	case "message", "email", "dm", "reply":
		return "message"
	case "follow_up", "followup", "follow-up":
		return "follow_up"
	case "interview":
		return "interview"
	case "discovered", "discovery":
		return "discovered"
	case "document":
		return "document"
	default:
		return "note"
	}
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// backfillTracker imports legacy rows preserving ids, so old links and event
// histories still resolve. Validation is deliberately bypassed: this is a
// historical import, and rejecting an old row would lose a person's board.
func backfillTracker(tx *sql.Tx) error {
	rows, err := tx.Query(`
SELECT id, dedupe_key, profile, title, body, url, author, score, score_reason,
       applied_at, rejected_at, app_status, checked_at, created_at
  FROM jobs ORDER BY id`)
	if err != nil {
		return err
	}
	type legacyJob struct {
		id                                        int64
		dedupe, profile, title, body, url, author string
		score                                     float64
		reason, applied, rejected, appStatus      string
		checked, created                          string
	}
	var jobs []legacyJob
	for rows.Next() {
		var j legacyJob
		if err := rows.Scan(&j.id, &j.dedupe, &j.profile, &j.title, &j.body, &j.url,
			&j.author, &j.score, &j.reason, &j.applied, &j.rejected, &j.appStatus,
			&j.checked, &j.created); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, j := range jobs {
		profile := strings.ToLower(strings.TrimSpace(j.profile))
		if profile == "" {
			profile = "sergey"
		}
		sourceKey := strings.TrimSpace(j.dedupe)
		if sourceKey == "" {
			sourceKey = fmt.Sprintf("legacy-job-%d", j.id)
		}
		company := strings.TrimSpace(j.author)
		if company == "" {
			company = "Unknown company"
		}
		title := strings.TrimSpace(j.title)
		if title == "" {
			title = firstLine(j.body)
		}
		if title == "" {
			title = "Untitled role"
		}
		stage := legacyStage(j.applied, j.rejected, j.appStatus)

		// The legacy score is not necessarily a current fit verdict. It is kept
		// only when a reason was recorded and the number fits the new 0..10
		// scale; a bare number is left out rather than fabricated into a fit.
		var fit *float64
		if j.reason = strings.TrimSpace(j.reason); j.reason != "" && j.score > 0 && j.score <= 10 {
			v := j.score
			fit = &v
		}
		if _, err := tx.Exec(`
INSERT OR IGNORE INTO tracker_jobs
 (id, source_key, profile, company, title, url, description, stage, remote, location,
  employment, salary_min, salary_max, currency, salary_period, fit_score, fit_summary,
  strengths, gaps, skills, recruiter_name, recruiter_contact, next_action, next_action_at,
  waiting_on, needs_you, interview_at, interview_url, last_checked_at, created_at, updated_at, version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'unknown', '', '', NULL, NULL, '', '', ?, ?, '[]', '[]', '[]',
        '', '', '', '', '', 0, '', '', ?, ?, ?, 1)`,
			j.id, truncateRunes(sourceKey, trackerMaxSourceKey), profile,
			truncateRunes(company, trackerMaxCompany), truncateRunes(title, trackerMaxTitle),
			j.url, strings.TrimSpace(j.body), stage, nullableFloatPtr(fit),
			truncateRunes(j.reason, trackerMaxText), j.checked, j.created, j.created); err != nil {
			return err
		}
	}

	rows, err = tx.Query(`SELECT id, job_id, kind, note, happened_at, created_at
	                        FROM job_events ORDER BY id`)
	if err != nil {
		return err
	}
	type legacyEvent struct {
		id, jobID                int64
		kind, note, happened, at string
	}
	var events []legacyEvent
	for rows.Next() {
		var e legacyEvent
		if err := rows.Scan(&e.id, &e.jobID, &e.kind, &e.note, &e.happened, &e.at); err != nil {
			rows.Close()
			return err
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range events {
		note := strings.TrimSpace(e.note)
		summary := firstLine(note)
		if summary == "" {
			summary = "Imported legacy event"
		}
		if _, err := tx.Exec(`
INSERT OR IGNORE INTO tracker_events
 (id, job_id, source_key, kind, summary, body, actor, channel, direction, contact,
  account, thread_id, external_id, source_url, verification, happened_at, recorded_at, stage)
VALUES (?, ?, ?, ?, ?, ?, 'legacy', 'system', 'internal', '', '', '', '', '', 'recorded', ?, ?, '')`,
			e.id, e.jobID, fmt.Sprintf("legacy-event-%d", e.id), legacyEventKind(e.kind),
			truncateRunes(summary, trackerMaxEventSum), truncateRunes(note, trackerMaxEventBody),
			e.happened, e.at); err != nil {
			return err
		}
	}
	// The old board also stored milestones outside job_events. Carry these
	// dates into the visible timeline without inventing a send or changing the
	// current stage. Insert after original events so their IDs stay unchanged.
	for _, milestone := range []struct{ column, kind, summary, stage string }{
		{"applied_at", "application", "Application recorded", "applied"},
		{"rejected_at", "status", "Rejection recorded", "rejected"},
	} {
		_, err := tx.Exec(`INSERT OR IGNORE INTO tracker_events
		 (job_id, source_key, kind, summary, body, actor, channel, direction,
		  verification, happened_at, recorded_at, stage)
		 SELECT j.id, 'legacy-job-' || j.id || '-' || ?, ?, ?, ?, 'legacy', 'system', 'internal',
		        'recorded', j.`+milestone.column+`, ?, ?
		 FROM jobs j JOIN tracker_jobs t ON t.id = j.id
		 WHERE j.`+milestone.column+` <> ''`, milestone.column, milestone.kind,
			milestone.summary, "Imported from legacy "+milestone.column+".", fmtTime(time.Now()), milestone.stage)
		if err != nil {
			return err
		}
	}
	return nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func nullableFloatPtr(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func encodeStrings(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func decodeStrings(s string) []string {
	var v []string
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	if err := json.Unmarshal([]byte(s), &v); err != nil || v == nil {
		return []string{}
	}
	for i := range v {
		v[i] = strings.TrimSpace(v[i])
	}
	return v
}

func normalizeStringList(v []string) ([]string, error) {
	out := make([]string, 0, len(v))
	for _, item := range v {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if len([]rune(item)) > trackerMaxListItem {
			return nil, fmt.Errorf("%w: list item exceeds %d characters", ErrTrackerValidation, trackerMaxListItem)
		}
		out = append(out, item)
	}
	if len(out) > trackerMaxListItems {
		return nil, fmt.Errorf("%w: too many list items (max %d)", ErrTrackerValidation, trackerMaxListItems)
	}
	return out, nil
}

func normalizeTrackerTime(field, s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", fmt.Errorf("%w: %s must be RFC3339", ErrTrackerValidation, field)
	}
	return t.UTC().Format(time.RFC3339), nil
}

func validateTrackerURL(field, s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if len(s) > trackerMaxURL {
		return fmt.Errorf("%w: %s exceeds %d characters", ErrTrackerValidation, field, trackerMaxURL)
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("%w: %s must be an absolute http(s) URL", ErrTrackerValidation, field)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	}
	return fmt.Errorf("%w: %s must use http or https", ErrTrackerValidation, field)
}

func normalizeTrackedJob(j TrackedJob, createDefaults bool) (TrackedJob, error) {
	j.SourceKey = strings.TrimSpace(j.SourceKey)
	j.Profile = strings.ToLower(strings.TrimSpace(j.Profile))
	j.Company = strings.TrimSpace(j.Company)
	j.Title = strings.TrimSpace(j.Title)
	j.URL = strings.TrimSpace(j.URL)
	j.Description = strings.TrimSpace(j.Description)
	j.Stage = strings.ToLower(strings.TrimSpace(j.Stage))
	j.Remote = strings.ToLower(strings.TrimSpace(j.Remote))
	j.Location = strings.TrimSpace(j.Location)
	j.Employment = strings.ToLower(strings.TrimSpace(j.Employment))
	j.Currency = strings.ToUpper(strings.TrimSpace(j.Currency))
	j.SalaryPeriod = strings.ToLower(strings.TrimSpace(j.SalaryPeriod))
	j.FitSummary = strings.TrimSpace(j.FitSummary)
	j.RecruiterName = strings.TrimSpace(j.RecruiterName)
	j.RecruiterContact = strings.TrimSpace(j.RecruiterContact)
	j.NextAction = strings.TrimSpace(j.NextAction)
	j.WaitingOn = strings.ToLower(strings.TrimSpace(j.WaitingOn))
	j.InterviewURL = strings.TrimSpace(j.InterviewURL)

	if createDefaults {
		if j.Profile == "" {
			j.Profile = "sergey"
		}
		if j.Stage == "" {
			j.Stage = TrackerStageSaved
		}
	}
	if j.Remote == "" {
		j.Remote = TrackerRemoteUnknown
	}
	var err error
	if j.Strengths, err = normalizeStringList(j.Strengths); err != nil {
		return j, err
	}
	if j.Gaps, err = normalizeStringList(j.Gaps); err != nil {
		return j, err
	}
	if j.Skills, err = normalizeStringList(j.Skills); err != nil {
		return j, err
	}
	if j.NextActionAt, err = normalizeTrackerTime("next_action_at", j.NextActionAt); err != nil {
		return j, err
	}
	if j.InterviewAt, err = normalizeTrackerTime("interview_at", j.InterviewAt); err != nil {
		return j, err
	}
	return j, nil
}

func validateTrackedJobDomain(j TrackedJob) error {
	if j.SourceKey == "" {
		return fmt.Errorf("%w: source_key is required", ErrTrackerValidation)
	}
	if len([]rune(j.SourceKey)) > trackerMaxSourceKey {
		return fmt.Errorf("%w: source_key exceeds %d characters", ErrTrackerValidation, trackerMaxSourceKey)
	}
	if j.Profile == "" {
		return fmt.Errorf("%w: profile is required", ErrTrackerValidation)
	}
	if len(j.Profile) > 64 {
		return fmt.Errorf("%w: profile exceeds 64 characters", ErrTrackerValidation)
	}
	for _, r := range j.Profile {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return fmt.Errorf("%w: profile must use lowercase letters, digits, underscores or hyphens", ErrTrackerValidation)
		}
	}
	if len([]rune(j.Location)) > 1000 {
		return fmt.Errorf("%w: location exceeds 1000 characters", ErrTrackerValidation)
	}
	for _, v := range []*float64{j.SalaryMin, j.SalaryMax, j.FitScore} {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) {
			return fmt.Errorf("%w: numbers must be finite", ErrTrackerValidation)
		}
	}
	if j.Company == "" {
		return fmt.Errorf("%w: company is required", ErrTrackerValidation)
	}
	if j.Title == "" {
		return fmt.Errorf("%w: title is required", ErrTrackerValidation)
	}
	if len([]rune(j.Company)) > trackerMaxCompany {
		return fmt.Errorf("%w: company exceeds %d characters", ErrTrackerValidation, trackerMaxCompany)
	}
	if len([]rune(j.Title)) > trackerMaxTitle {
		return fmt.Errorf("%w: title exceeds %d characters", ErrTrackerValidation, trackerMaxTitle)
	}
	if len([]rune(j.Description)) > trackerMaxDesc {
		return fmt.Errorf("%w: description exceeds %d characters", ErrTrackerValidation, trackerMaxDesc)
	}
	if !trackerStages[j.Stage] {
		return fmt.Errorf("%w: stage %q is not one of saved/applied/talking/interview/offer/hired/rejected/archived", ErrTrackerValidation, j.Stage)
	}
	if !trackerRemotes[j.Remote] {
		return fmt.Errorf("%w: remote %q is not one of unknown/remote/hybrid/onsite", ErrTrackerValidation, j.Remote)
	}
	if !trackerWaiting[j.WaitingOn] {
		return fmt.Errorf("%w: waiting_on %q is not one of agent/recruiter/you", ErrTrackerValidation, j.WaitingOn)
	}
	if !trackerEmployment[j.Employment] {
		return fmt.Errorf("%w: employment %q is not one of full-time/part-time/contract/freelance", ErrTrackerValidation, j.Employment)
	}
	if !trackerSalaryPeriods[j.SalaryPeriod] {
		return fmt.Errorf("%w: salary_period %q is not one of year/month/hour", ErrTrackerValidation, j.SalaryPeriod)
	}
	if j.Currency != "" {
		if len(j.Currency) != 3 {
			return fmt.Errorf("%w: currency must be three letters", ErrTrackerValidation)
		}
		for _, r := range j.Currency {
			if r < 'A' || r > 'Z' {
				return fmt.Errorf("%w: currency must be three uppercase letters", ErrTrackerValidation)
			}
		}
	}
	if j.SalaryMin != nil && *j.SalaryMin < 0 {
		return fmt.Errorf("%w: salary_min cannot be negative", ErrTrackerValidation)
	}
	if j.SalaryMax != nil && *j.SalaryMax < 0 {
		return fmt.Errorf("%w: salary_max cannot be negative", ErrTrackerValidation)
	}
	if j.SalaryMin != nil && j.SalaryMax != nil && *j.SalaryMin > *j.SalaryMax {
		return fmt.Errorf("%w: salary_min cannot exceed salary_max", ErrTrackerValidation)
	}
	if j.FitScore != nil {
		if *j.FitScore < 0 || *j.FitScore > 10 {
			return fmt.Errorf("%w: fit_score must be between 0 and 10", ErrTrackerValidation)
		}
		if j.FitSummary == "" {
			return fmt.Errorf("%w: fit_summary is required when fit_score is set", ErrTrackerValidation)
		}
	}
	if len([]rune(j.FitSummary)) > trackerMaxText {
		return fmt.Errorf("%w: fit_summary exceeds %d characters", ErrTrackerValidation, trackerMaxText)
	}
	for field, value := range map[string]string{
		"url": j.URL, "interview_url": j.InterviewURL,
	} {
		if err := validateTrackerURL(field, value); err != nil {
			return err
		}
	}
	for field, value := range map[string]string{
		"recruiter_name": j.RecruiterName, "recruiter_contact": j.RecruiterContact,
		"next_action": j.NextAction,
	} {
		if len([]rune(value)) > trackerMaxText {
			return fmt.Errorf("%w: %s exceeds %d characters", ErrTrackerValidation, field, trackerMaxText)
		}
	}
	return nil
}

// ValidateTrackedJob checks a payload as CreateTrackedJob would normalise it.
// It is exported so an HTTP layer can reject a bad body before opening a
// transaction, with the same rules the store will apply.
func ValidateTrackedJob(j TrackedJob) error {
	n, err := normalizeTrackedJob(j, true)
	if err != nil {
		return err
	}
	return validateTrackedJobDomain(n)
}

func trackerJobsEquivalent(a, b TrackedJob) bool {
	strip := func(j TrackedJob) TrackedJob {
		j.ID = 0
		j.Version = 0
		j.CreatedAt = ""
		j.UpdatedAt = ""
		j.LastCheckedAt = ""
		return j
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

// Keep the original normalized create request separate from the mutable job.
// A delayed retry must still identify the same record after HR activity or edits.
func trackerCreatePayload(j TrackedJob) string {
	j.ID = 0
	j.Version = 0
	j.CreatedAt = ""
	j.UpdatedAt = ""
	j.LastCheckedAt = ""
	b, _ := json.Marshal(j)
	return string(b)
}

const trackerJobCols = `id, source_key, profile, company, title, url, description, stage, remote,
  location, employment, salary_min, salary_max, currency, salary_period, fit_score,
  fit_summary, strengths, gaps, skills, recruiter_name, recruiter_contact, next_action,
  next_action_at, waiting_on, needs_you, interview_at, interview_url, last_checked_at,
  created_at, updated_at, version`

func scanTrackedJob(row interface{ Scan(...any) error }) (*TrackedJob, error) {
	var j TrackedJob
	var min, max, fit sql.NullFloat64
	var strengths, gaps, skills string
	var needs int
	if err := row.Scan(&j.ID, &j.SourceKey, &j.Profile, &j.Company, &j.Title, &j.URL,
		&j.Description, &j.Stage, &j.Remote, &j.Location, &j.Employment,
		&min, &max, &j.Currency, &j.SalaryPeriod, &fit, &j.FitSummary,
		&strengths, &gaps, &skills, &j.RecruiterName, &j.RecruiterContact,
		&j.NextAction, &j.NextActionAt, &j.WaitingOn, &needs, &j.InterviewAt,
		&j.InterviewURL, &j.LastCheckedAt, &j.CreatedAt, &j.UpdatedAt, &j.Version); err != nil {
		return nil, err
	}
	if min.Valid {
		v := min.Float64
		j.SalaryMin = &v
	}
	if max.Valid {
		v := max.Float64
		j.SalaryMax = &v
	}
	if fit.Valid {
		v := fit.Float64
		j.FitScore = &v
	}
	j.Strengths = decodeStrings(strengths)
	j.Gaps = decodeStrings(gaps)
	j.Skills = decodeStrings(skills)
	j.NeedsYou = needs == 1
	return &j, nil
}

func getTrackedJobRow(q interface{ QueryRow(string, ...any) *sql.Row }, id int64) (*TrackedJob, error) {
	return scanTrackedJob(q.QueryRow(fmt.Sprintf(`SELECT %s FROM tracker_jobs WHERE id = ?`, trackerJobCols), id))
}

// CreateTrackedJob inserts one job. A retry of the same (profile, source_key)
// payload is a replay and returns the stored row; a different payload under the
// same key is a conflict, because the board must not silently lose the first
// write. Server-managed time/version fields on the input are ignored.
func (s *Store) CreateTrackedJob(j TrackedJob, now time.Time) (*TrackedJob, bool, error) {
	n, err := normalizeTrackedJob(j, true)
	if err != nil {
		return nil, false, err
	}
	if err := validateTrackedJobDomain(n); err != nil {
		return nil, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var existingID int64
	err = tx.QueryRow(`SELECT id FROM tracker_jobs WHERE profile = ? AND source_key = ?`,
		n.Profile, n.SourceKey).Scan(&existingID)
	if err == nil {
		existing, err := getTrackedJobRow(tx, existingID)
		if err != nil {
			return nil, false, err
		}
		var original string
		if err := tx.QueryRow(`SELECT create_payload FROM tracker_jobs WHERE id=?`, existingID).Scan(&original); err != nil {
			return nil, false, err
		}
		if (original != "" && original != trackerCreatePayload(n)) || (original == "" && !trackerJobsEquivalent(*existing, n)) {
			return nil, false, fmt.Errorf("%w: profile %q source_key %q", ErrTrackerConflict, n.Profile, n.SourceKey)
		}
		return existing, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	n.ID = 0
	n.Version = 1
	n.CreatedAt = fmtTime(now)
	n.UpdatedAt = n.CreatedAt
	n.LastCheckedAt = ""
	res, err := tx.Exec(`
INSERT INTO tracker_jobs
 (source_key, profile, company, title, url, description, stage, remote, location,
  employment, salary_min, salary_max, currency, salary_period, fit_score, fit_summary,
  strengths, gaps, skills, recruiter_name, recruiter_contact, next_action, next_action_at,
  waiting_on, needs_you, interview_at, interview_url, last_checked_at, created_at, updated_at, version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.SourceKey, n.Profile, n.Company, n.Title, n.URL, n.Description, n.Stage, n.Remote,
		n.Location, n.Employment, nullableFloatPtr(n.SalaryMin), nullableFloatPtr(n.SalaryMax),
		n.Currency, n.SalaryPeriod, nullableFloatPtr(n.FitScore), n.FitSummary,
		encodeStrings(n.Strengths), encodeStrings(n.Gaps), encodeStrings(n.Skills),
		n.RecruiterName, n.RecruiterContact, n.NextAction, n.NextActionAt, n.WaitingOn,
		boolInt(n.NeedsYou), n.InterviewAt, n.InterviewURL, n.LastCheckedAt,
		n.CreatedAt, n.UpdatedAt, n.Version)
	if err != nil {
		return nil, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, false, err
	}
	n.ID = id
	if _, err := tx.Exec(`UPDATE tracker_jobs SET create_payload=? WHERE id=?`, trackerCreatePayload(n), id); err != nil {
		return nil, false, err
	}
	created := n
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &created, false, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) TrackedJob(id int64) (*TrackedJob, error) {
	return getTrackedJobRow(s.db, id)
}

// UpdateTrackedJob replaces the editable fields after the HTTP layer has
// merged the payload. Identity (id/profile/source_key/created_at) is immutable;
// last_checked_at is server-owned and only check events move it. A stage change
// appends a system status event in the same transaction, so the timeline can
// never disagree with the row.
func (s *Store) UpdateTrackedJob(id int64, j TrackedJob, expectedVersion int64, now time.Time) (*TrackedJob, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	existing, err := getTrackedJobRow(tx, id)
	if err != nil {
		return nil, err
	}
	if existing.Version != expectedVersion {
		return nil, fmt.Errorf("%w: have %d, expected %d", ErrTrackerVersion, existing.Version, expectedVersion)
	}
	n, err := normalizeTrackedJob(j, false)
	if err != nil {
		return nil, err
	}
	if n.Profile == "" {
		n.Profile = existing.Profile
	} else if n.Profile != existing.Profile {
		return nil, fmt.Errorf("%w: profile is immutable", ErrTrackerValidation)
	}
	if n.SourceKey == "" {
		n.SourceKey = existing.SourceKey
	} else if n.SourceKey != existing.SourceKey {
		return nil, fmt.Errorf("%w: source_key is immutable", ErrTrackerValidation)
	}
	if n.Stage == "" {
		n.Stage = existing.Stage
	}
	if err := validateTrackedJobDomain(n); err != nil {
		return nil, err
	}

	n.ID = id
	n.CreatedAt = existing.CreatedAt
	n.LastCheckedAt = existing.LastCheckedAt
	n.Version = expectedVersion + 1
	n.UpdatedAt = fmtTime(now)
	res, err := tx.Exec(`
UPDATE tracker_jobs SET
  company = ?, title = ?, url = ?, description = ?, stage = ?, remote = ?, location = ?,
  employment = ?, salary_min = ?, salary_max = ?, currency = ?, salary_period = ?,
  fit_score = ?, fit_summary = ?, strengths = ?, gaps = ?, skills = ?,
  recruiter_name = ?, recruiter_contact = ?, next_action = ?, next_action_at = ?,
  waiting_on = ?, needs_you = ?, interview_at = ?, interview_url = ?,
  updated_at = ?, version = ?
WHERE id = ? AND version = ?`,
		n.Company, n.Title, n.URL, n.Description, n.Stage, n.Remote, n.Location,
		n.Employment, nullableFloatPtr(n.SalaryMin), nullableFloatPtr(n.SalaryMax),
		n.Currency, n.SalaryPeriod, nullableFloatPtr(n.FitScore), n.FitSummary,
		encodeStrings(n.Strengths), encodeStrings(n.Gaps), encodeStrings(n.Skills),
		n.RecruiterName, n.RecruiterContact, n.NextAction, n.NextActionAt, n.WaitingOn,
		boolInt(n.NeedsYou), n.InterviewAt, n.InterviewURL, n.UpdatedAt, n.Version,
		id, expectedVersion)
	if err != nil {
		return nil, err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return nil, fmt.Errorf("%w: row changed during update", ErrTrackerVersion)
	}
	if n.Stage != existing.Stage {
		statusEvent := TimelineEvent{
			JobID:        id,
			SourceKey:    fmt.Sprintf("__system:stage-change-v%d", n.Version),
			Kind:         "status",
			Summary:      fmt.Sprintf("Stage changed from %s to %s", existing.Stage, n.Stage),
			Actor:        "system",
			Channel:      "system",
			Direction:    "internal",
			Verification: "recorded",
			HappenedAt:   fmtTime(now),
			RecordedAt:   fmtTime(now),
			Stage:        n.Stage,
		}
		if _, err := insertTrackerEvent(tx, statusEvent); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *Store) ListTrackedJobs(f TrackerFilter) (*TrackerPage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	conds := []string{"1=1"}
	var args []any
	if f.ExcludeArchived {
		conds = append(conds, "stage <> 'archived'")
	}
	if f.ActiveProfilesOnly {
		conds = append(conds, "profile IN ('sergey','polina')")
	}
	if f.Profile != "" {
		conds = append(conds, "profile = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(f.Profile)))
	}
	if f.Stage == "active" {
		conds = append(conds, "stage IN ('applied','talking','interview')")
	} else if f.Stage != "" {
		conds = append(conds, "stage = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(f.Stage)))
	}
	if f.Remote != "" {
		conds = append(conds, "remote = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(f.Remote)))
	}
	if f.NeedsYou {
		conds = append(conds, "needs_you = 1 AND stage NOT IN ('hired','rejected','archived')")
	}
	if q := strings.ToLower(strings.TrimSpace(f.Query)); q != "" {
		like := "%" + q + "%"
		conds = append(conds, `(LOWER(title) LIKE ? OR LOWER(company) LIKE ? OR LOWER(description) LIKE ?)`)
		args = append(args, like, like, like)
	}
	if f.BeforeID > 0 {
		conds = append(conds, "id < ?")
		args = append(args, f.BeforeID)
	}
	args = append(args, limit+1)
	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT %s FROM tracker_jobs WHERE %s ORDER BY id DESC LIMIT ?`,
		trackerJobCols, strings.Join(conds, " AND ")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &TrackerPage{Items: make([]TrackedJob, 0, limit)}
	for rows.Next() {
		j, err := scanTrackedJob(rows)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, *j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = strconv.FormatInt(page.Items[len(page.Items)-1].ID, 10)
	}
	return page, nil
}

func normalizeTimelineEvent(e TimelineEvent) (TimelineEvent, error) {
	e.SourceKey = strings.TrimSpace(e.SourceKey)
	e.Kind = strings.ToLower(strings.TrimSpace(e.Kind))
	e.Summary = strings.TrimSpace(e.Summary)
	e.Body = strings.TrimSpace(e.Body)
	e.Actor = strings.TrimSpace(e.Actor)
	e.Channel = strings.ToLower(strings.TrimSpace(e.Channel))
	e.Direction = strings.ToLower(strings.TrimSpace(e.Direction))
	e.Contact = strings.TrimSpace(e.Contact)
	e.Account = strings.TrimSpace(e.Account)
	e.ThreadID = strings.TrimSpace(e.ThreadID)
	e.ExternalID = strings.TrimSpace(e.ExternalID)
	e.SourceURL = strings.TrimSpace(e.SourceURL)
	e.Verification = strings.ToLower(strings.TrimSpace(e.Verification))
	e.Stage = strings.ToLower(strings.TrimSpace(e.Stage))
	if e.Verification == "" {
		// The honest default: the app recorded what the caller reported. It is
		// never promoted to "confirmed" without evidence.
		e.Verification = "recorded"
	}
	return e, nil
}

func validateTimelineEventDomain(e TimelineEvent) error {
	if e.SourceKey == "" {
		return fmt.Errorf("%w: event source_key is required", ErrTrackerValidation)
	}
	if strings.HasPrefix(e.SourceKey, "__system:") {
		return fmt.Errorf("%w: __system: event keys are reserved", ErrTrackerValidation)
	}
	if len([]rune(e.SourceKey)) > trackerMaxSourceKey {
		return fmt.Errorf("%w: event source_key is too long", ErrTrackerValidation)
	}
	if e.Actor == "" {
		return fmt.Errorf("%w: actor is required", ErrTrackerValidation)
	}
	if len([]rune(e.Actor)) > trackerMaxEventActor {
		return fmt.Errorf("%w: actor is too long", ErrTrackerValidation)
	}
	if !trackerEventKinds[e.Kind] {
		return fmt.Errorf("%w: kind %q is not a timeline kind", ErrTrackerValidation, e.Kind)
	}
	if !trackerEventChannels[e.Channel] {
		return fmt.Errorf("%w: channel %q is not valid", ErrTrackerValidation, e.Channel)
	}
	if !trackerEventDirections[e.Direction] {
		return fmt.Errorf("%w: direction %q is not valid", ErrTrackerValidation, e.Direction)
	}
	if !trackerEventVerification[e.Verification] {
		return fmt.Errorf("%w: verification %q is not valid", ErrTrackerValidation, e.Verification)
	}
	if e.Stage != "" && !trackerStages[e.Stage] {
		return fmt.Errorf("%w: stage %q is not valid", ErrTrackerValidation, e.Stage)
	}
	if len([]rune(e.Summary)) > trackerMaxEventSum {
		return fmt.Errorf("%w: summary exceeds %d characters", ErrTrackerValidation, trackerMaxEventSum)
	}
	if e.Summary == "" {
		return fmt.Errorf("%w: summary is required", ErrTrackerValidation)
	}
	for name, value := range map[string]string{"contact": e.Contact, "account": e.Account, "thread_id": e.ThreadID, "external_id": e.ExternalID} {
		if len([]rune(value)) > 1000 {
			return fmt.Errorf("%w: %s exceeds 1000 characters", ErrTrackerValidation, name)
		}
	}
	if len([]rune(e.Body)) > trackerMaxEventBody {
		return fmt.Errorf("%w: body exceeds %d characters", ErrTrackerValidation, trackerMaxEventBody)
	}
	return validateTrackerURL("source_url", e.SourceURL)
}

func eventsEquivalent(a, b TimelineEvent) bool {
	strip := func(e TimelineEvent) TimelineEvent {
		e.ID = 0
		e.JobID = 0
		e.RecordedAt = ""
		return e
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

func insertTrackerEvent(q trackerExecer, e TimelineEvent) (int64, error) {
	res, err := q.Exec(`
INSERT INTO tracker_events
 (job_id, source_key, kind, summary, body, actor, channel, direction, contact, account,
  thread_id, external_id, source_url, verification, happened_at, recorded_at, stage)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.JobID, e.SourceKey, e.Kind, e.Summary, e.Body, e.Actor, e.Channel,
		e.Direction, e.Contact, e.Account, e.ThreadID, e.ExternalID, e.SourceURL,
		e.Verification, e.HappenedAt, e.RecordedAt, e.Stage)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddTimelineEvent appends one event idempotently. A blank happened_at becomes
// the server's now (a retry with blank happened_at still matches the first
// write), a new event bumps the job version and updated_at, and only a "check"
// event moves last_checked_at. An optional stage is applied atomically with the
// event; status is never inferred from kind.
func (s *Store) AddTimelineEvent(jobID int64, e TimelineEvent, now time.Time) (*TimelineEvent, bool, error) {
	e, err := normalizeTimelineEvent(e)
	if err != nil {
		return nil, false, err
	}
	happenedBlank := e.HappenedAt == ""
	if e.HappenedAt, err = normalizeTrackerTime("happened_at", e.HappenedAt); err != nil {
		return nil, false, err
	}
	if err := validateTimelineEventDomain(e); err != nil {
		return nil, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM tracker_jobs WHERE id = ?`, jobID).Scan(&exists); err != nil {
		return nil, false, sql.ErrNoRows
	}
	e.JobID = jobID
	e.RecordedAt = fmtTime(now)
	if happenedBlank {
		e.HappenedAt = e.RecordedAt
	}
	var existingID int64
	err = tx.QueryRow(`SELECT id FROM tracker_events WHERE job_id = ? AND source_key = ?`,
		jobID, e.SourceKey).Scan(&existingID)
	if err == nil {
		existing, err := getTimelineEvent(tx, existingID)
		if err != nil {
			return nil, false, err
		}
		cmp := e
		if happenedBlank {
			cmp.HappenedAt = existing.HappenedAt
		}
		if !eventsEquivalent(cmp, *existing) {
			return nil, false, fmt.Errorf("%w: job %d source_key %q", ErrTrackerConflict, jobID, e.SourceKey)
		}
		return existing, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	id, err := insertTrackerEvent(tx, e)
	if err != nil {
		return nil, false, err
	}
	e.ID = id
	e.RecordedAt = fmtTime(now)
	_, err = tx.Exec(`
UPDATE tracker_jobs SET
  version = version + 1,
  updated_at = ?,
  last_checked_at = CASE WHEN ? = 'check' AND (last_checked_at = '' OR last_checked_at < ?) THEN ? ELSE last_checked_at END,
  stage = CASE WHEN ? != '' THEN ? ELSE stage END
WHERE id = ?`,
		fmtTime(now), e.Kind, e.HappenedAt, e.HappenedAt, e.Stage, e.Stage, jobID)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &e, false, nil
}

const timelineEventCols = `id, job_id, source_key, kind, summary, body, actor, channel,
  direction, contact, account, thread_id, external_id, source_url, verification,
  happened_at, recorded_at, stage`

func scanTimelineEvent(row interface{ Scan(...any) error }) (*TimelineEvent, error) {
	var e TimelineEvent
	if err := row.Scan(&e.ID, &e.JobID, &e.SourceKey, &e.Kind, &e.Summary, &e.Body,
		&e.Actor, &e.Channel, &e.Direction, &e.Contact, &e.Account, &e.ThreadID,
		&e.ExternalID, &e.SourceURL, &e.Verification, &e.HappenedAt, &e.RecordedAt,
		&e.Stage); err != nil {
		return nil, err
	}
	return &e, nil
}

func getTimelineEvent(q interface{ QueryRow(string, ...any) *sql.Row }, id int64) (*TimelineEvent, error) {
	return scanTimelineEvent(q.QueryRow(
		fmt.Sprintf(`SELECT %s FROM tracker_events WHERE id = ?`, timelineEventCols), id))
}

// TimelineEvents returns events newest id first. That is a deterministic
// pagination order; the UI is free to re-sort the displayed page by
// happened_at, because a caught-up monitoring pass can record an older moment
// later.
func (s *Store) TimelineEvents(jobID int64, limit int, beforeID int64) (*TimelinePage, error) {
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM tracker_jobs WHERE id = ?`, jobID).Scan(&exists); err != nil {
		return nil, sql.ErrNoRows
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	conds := []string{"job_id = ?"}
	args := []any{jobID}
	if beforeID > 0 {
		conds = append(conds, "id < ?")
		args = append(args, beforeID)
	}
	args = append(args, limit+1)
	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT %s FROM tracker_events WHERE %s ORDER BY id DESC LIMIT ?`,
		timelineEventCols, strings.Join(conds, " AND ")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &TimelinePage{Items: make([]TimelineEvent, 0, limit)}
	for rows.Next() {
		e, err := scanTimelineEvent(rows)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = strconv.FormatInt(page.Items[len(page.Items)-1].ID, 10)
	}
	return page, nil
}
