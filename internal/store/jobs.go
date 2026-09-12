package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Job is one lead found by a job-search sweep: a post on Reddit, X or
// LinkedIn that looks like paid work. Rows are written in batches by the
// sweep and read back by the /jobs pages; viewed_at is the only field the
// pages themselves ever change.
type Job struct {
	ID        int64  `json:"id"`
	DedupeKey string `json:"dedupe_key"`
	// Profile is whose hunt this lead belongs to. The board is shared by more
	// than one job seeker, and a Go backend role and an AI-video contract have
	// nothing to say to each other — so every query carries the profile and the
	// board opens on one person's leads at a time.
	Profile   string  `json:"profile"`
	Network   string  `json:"network"`
	JobType   string  `json:"job_type"`
	Score     float64 `json:"score"`
	Author    string  `json:"author"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	URL       string  `json:"url"`
	Subreddit string  `json:"subreddit,omitempty"`
	Emails    string  `json:"emails,omitempty"`
	Signals   string  `json:"signals,omitempty"`
	Draft     string  `json:"draft,omitempty"`
	// PostingURL and PostingText are the real listing behind the lead: the
	// company's own board, reached by following the aggregator through. They are
	// deliberately not URL/Body — about half of the highest-scored leads used to
	// die on contact because the blurb and the posting disagreed about location,
	// and keeping both means the disagreement stays visible instead of being
	// overwritten by whichever pass ran last.
	PostingURL  string `json:"posting_url,omitempty"`
	PostingText string `json:"posting_text,omitempty"`
	// ScoreReason is why the score is what it is. Mandatory alongside a non-zero
	// score for the same reason RejectReason is mandatory: a bare 8 tells you
	// nothing a week later.
	ScoreReason string `json:"score_reason,omitempty"`
	// Prep is the JSON artifact the prep stage writes — job summary, what the
	// application form asks, questions that need a decision, links to the
	// tailored CV. Opaque to the store: it is read on one page and never
	// queried, so one blob beats six columns that would each need a migration.
	Prep      string    `json:"prep,omitempty"`
	PostedAt  time.Time `json:"posted_at,omitzero"`
	ViewedAt  time.Time `json:"viewed_at,omitzero"`
	AppliedAt time.Time `json:"applied_at,omitzero"`
	// ApprovedAt is the human gate between prep and applying, and ReviewNotes is
	// what was said while passing it. Unlike a rejection, notes are optional —
	// most approvals have nothing to add, and demanding a sentence for those
	// would just get an empty one typed to clear the field.
	ApprovedAt  time.Time `json:"approved_at,omitzero"`
	ReviewNotes string    `json:"review_notes,omitempty"`
	// RejectedAt marks a lead ruled out, and RejectReason says why. The reason
	// is mandatory: a rejection with no stated cause is exactly the kind of
	// note that is useless a week later, when you cannot remember whether the
	// role was US-only or just a bad stack fit.
	RejectedAt   time.Time `json:"rejected_at,omitzero"`
	RejectReason string    `json:"reject_reason,omitempty"`
	// DuplicateOf points at the canonical row when this lead is the same
	// underlying role reached by another route: an aggregator mirror, a repost,
	// or a second recruiter at one agency. 0 means this row is canonical.
	//
	// Chains are collapsed on write, so this is always the canonical id and
	// never another duplicate. That keeps the read side a single hop.
	DuplicateOf int64     `json:"duplicate_of,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// IsDuplicate reports whether this lead is a mirror of another one.
func (j *Job) IsDuplicate() bool { return j.DuplicateOf != 0 }

func (j *Job) Viewed() bool { return !j.ViewedAt.IsZero() }

// Applied reports whether an application has gone out for this lead. The
// timestamp is the flag: empty means not applied, so there is no separate
// boolean that could disagree with the date.
func (j *Job) Applied() bool { return !j.AppliedAt.IsZero() }

// Rejected reports whether this lead has been ruled out.
func (j *Job) Rejected() bool { return !j.RejectedAt.IsZero() }

// Approved reports whether this lead has cleared the review gate. Applying
// reads it, so it is the one flag that stands between a generated application
// and a real one going out.
func (j *Job) Approved() bool { return !j.ApprovedAt.IsZero() }

// Prepped reports whether the prep stage has run: a CV, a summary and the
// form's questions are waiting to be reviewed.
func (j *Job) Prepped() bool { return j.Prep != "" }

// JobParams is one job as the sweep sends it. DedupeKey is the network's own
// stable id (reddit fullname, tweet id, a LinkedIn author+text hash), so the
// same post pushed by two sweeps lands on one row.
type JobParams struct {
	DedupeKey string `json:"dedupe_key"`
	// Profile whose hunt this lead belongs to. Empty means DefaultJobProfile,
	// so a sweep written before profiles existed keeps landing on the owner's
	// board instead of in an unfilterable nowhere.
	Profile   string  `json:"profile"`
	Network   string  `json:"network"`
	JobType   string  `json:"job_type"`
	Score     float64 `json:"score"`
	Author    string  `json:"author"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	URL       string  `json:"url"`
	Subreddit string  `json:"subreddit"`
	Emails    string  `json:"emails"`
	Signals   string  `json:"signals"`
	Draft     string  `json:"draft"`
	// PostingURL / PostingText carry the real listing found by the resolve pass;
	// ScoreReason is why the judge landed on Score. All three follow the normal
	// non-empty-only rule, so a later partial push cannot blank them.
	PostingURL  string    `json:"posting_url"`
	PostingText string    `json:"posting_text"`
	ScoreReason string    `json:"score_reason"`
	Prep        string    `json:"prep"`
	PostedAt    time.Time `json:"posted_at"`
	// Applied is tri-state on purpose. Every other field updates only when it
	// is non-empty, which cannot express "clear this"; a pointer can. nil means
	// the push says nothing about applied state, true stamps it, false clears
	// it. That keeps `{dedupe_key, network, applied}` a legal partial push.
	Applied *bool `json:"applied"`
	// Rejected is tri-state for the same reason. Setting it true requires a
	// non-empty RejectReason; ErrRejectReasonRequired otherwise.
	Rejected     *bool  `json:"rejected"`
	RejectReason string `json:"reject_reason"`
	// Approved is tri-state like the other two, which is what makes a re-sweep
	// safe: a push that says nothing about approval leaves a decision already
	// made alone. ReviewNotes is an ordinary non-empty-only field rather than
	// part of the flag — a note describes the review, not the approval, and
	// stays worth reading after an approval is withdrawn.
	Approved    *bool  `json:"approved"`
	ReviewNotes string `json:"review_notes"`
}

// ErrRejectReasonRequired is returned when a lead is rejected with no reason.
// The rule lives in the store rather than only in the handler so that every
// path into the data — ingest, the per-row endpoint, a future importer — is
// held to it.
var ErrRejectReasonRequired = errors.New("reject_reason is required when rejecting a job")

// ErrScoreReasonRequired is returned when a job is scored with no stated cause.
// The same argument as the reject reason: a bare 8 is not a judgement anyone
// can re-read a week later, and the scoring pass is the one place where the
// reasoning is still in hand.
//
// It is gated on a NON-ZERO score, which is what keeps a partial push legal:
// score is subject to the non-empty-only update rule, so `score: 0` already
// means "say nothing about the score" and `{dedupe_key, network, draft}` still
// attaches a draft without anyone having to justify a number it never sent.
var ErrScoreReasonRequired = errors.New("score_reason is required when scoring a job")

// DefaultJobProfile owns every lead that arrives without a profile: an
// un-profiled row belongs to the board's owner, not to nobody. Overridable
// with the DEFAULT_PROFILE env var (set before the store opens, since the
// profile backfill migration writes it into rows).
var DefaultJobProfile = "me"

// NormalizeProfile lowercases and trims a profile name, defaulting the empty
// one. Every write and every filter goes through it, so "Polina", "polina "
// and "polina" cannot become three columns on the board.
func NormalizeProfile(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return DefaultJobProfile
	}
	return p
}

func (s *Store) migrateJobs() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS jobs (
  id         INTEGER PRIMARY KEY,
  dedupe_key TEXT NOT NULL,
  -- A lead is identified by (profile, dedupe_key), not by the key alone: one
  -- post can legitimately be a lead on two people's boards. Databases created
  -- before profiles existed carry the old single-column UNIQUE and are
  -- rebuilt by scopeDedupeKeyToProfile below.
  profile    TEXT NOT NULL DEFAULT '',
  network    TEXT NOT NULL,
  job_type   TEXT NOT NULL DEFAULT '',
  score      REAL NOT NULL DEFAULT 0,
  author     TEXT NOT NULL DEFAULT '',
  title      TEXT NOT NULL DEFAULT '',
  body       TEXT NOT NULL DEFAULT '',
  url        TEXT NOT NULL DEFAULT '',
  subreddit  TEXT NOT NULL DEFAULT '',
  emails     TEXT NOT NULL DEFAULT '',
  signals    TEXT NOT NULL DEFAULT '',
  draft      TEXT NOT NULL DEFAULT '',
  posted_at  TEXT NOT NULL DEFAULT '',
  viewed_at  TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE (profile, dedupe_key)
);
CREATE INDEX IF NOT EXISTS idx_jobs_network ON jobs(network, created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at);
`)
	if err != nil {
		return err
	}
	// url_key is the normalized URL, the second identity a job has: the same
	// listing reposted under a different dedupe_key (an aggregator fanning one
	// role across subreddits) still lands on one row. Added after the first
	// release, so existing rows are backfilled from their stored url once.
	if err := s.addColumn("jobs", "url_key TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_jobs_url_key ON jobs(url_key)`); err != nil {
		return err
	}
	// applied_at tracks the outreach half of the hunt: empty until an
	// application goes out. Added after the first release, so every existing
	// row starts correctly as not applied.
	if err := s.addColumn("jobs", "applied_at TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// rejected_at / reject_reason: the other end of the funnel. Existing rows
	// start un-rejected, which is right — nothing has been ruled out yet.
	if err := s.addColumn("jobs", "rejected_at TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumn("jobs", "reject_reason TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// duplicate_of links mirrors of one role to a canonical row, so the board
	// can count roles rather than postings.
	if err := s.addColumn("jobs", "duplicate_of INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_jobs_duplicate_of ON jobs(duplicate_of)`); err != nil {
		return err
	}
	// profile splits the board by whose hunt a lead belongs to. Every row that
	// predates the column is the owner's — the board was single-tenant until
	// now — so they are backfilled rather than left blank and unfilterable.
	if err := s.addColumn("jobs", "profile TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_jobs_profile ON jobs(profile, created_at)`); err != nil {
		return err
	}
	// Idempotent, and it also catches any row a caller managed to write blank:
	// the invariant this holds up is that profile is never empty, which is what
	// lets the filter be a plain equality test.
	if _, err := s.db.Exec(`UPDATE jobs SET profile = ? WHERE profile = ''`, DefaultJobProfile); err != nil {
		return err
	}
	if err := s.scopeDedupeKeyToProfile(); err != nil {
		return err
	}
	// The pipeline columns, added when the sweep grew a resolve pass and a human
	// review gate between finding a lead and applying to it.
	//
	// posting_url / posting_text hold the real listing behind an aggregator
	// blurb, kept separate from url / body so the two can be compared rather
	// than one silently replacing the other. score_reason is the judge's
	// reasoning. prep is the review artifact, opaque JSON. approved_at /
	// review_notes are the gate itself.
	//
	// Every existing row starts blank on all six, which reads correctly: leads
	// found before the pipeline existed were never resolved, never prepped and
	// never approved.
	//
	// These run AFTER the rebuild above, deliberately. scopeDedupeKeyToProfile
	// copies a fixed column list into a fresh table, so a column added before it
	// would exist for the length of one migration and then be dropped on the
	// boot that rebuilds. Adding them afterwards means the rebuilt table gets
	// them like any other, and the ordering stops mattering once the rebuild has
	// run its single time.
	for _, col := range []string{
		"posting_url TEXT NOT NULL DEFAULT ''",
		"posting_text TEXT NOT NULL DEFAULT ''",
		"score_reason TEXT NOT NULL DEFAULT ''",
		"prep TEXT NOT NULL DEFAULT ''",
		"approved_at TEXT NOT NULL DEFAULT ''",
		"review_notes TEXT NOT NULL DEFAULT ''",
	} {
		if err := s.addColumn("jobs", col); err != nil {
			return err
		}
	}
	// The apply stage's query is "approved, not yet applied, on this board", so
	// the index leads with profile the way idx_jobs_profile does.
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_jobs_approved ON jobs(profile, approved_at)`); err != nil {
		return err
	}
	return s.backfillJobURLKeys()
}

// scopeDedupeKeyToProfile widens the lead identity from `dedupe_key` to
// `(profile, dedupe_key)`.
//
// The original table declared `dedupe_key TEXT NOT NULL UNIQUE`, which was
// right while the board held one person's hunt. It stops being right the moment
// two people share it: one Reddit post can legitimately be a lead on two
// boards, and a globally unique key would make the second sweep either fail or
// hijack the first person's row — including its viewed and applied state.
//
// SQLite builds an inline UNIQUE as an auto-index that no ALTER can drop, so
// the table is rebuilt once. The migration is guarded on that auto-index still
// existing, which makes it a no-op on every later boot.
func (s *Store) scopeDedupeKeyToProfile() error {
	legacy, err := s.hasLegacyDedupeUnique()
	if err != nil || !legacy {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
CREATE TABLE jobs_new (
  id         INTEGER PRIMARY KEY,
  dedupe_key TEXT NOT NULL,
  profile    TEXT NOT NULL DEFAULT '',
  network    TEXT NOT NULL,
  job_type   TEXT NOT NULL DEFAULT '',
  score      REAL NOT NULL DEFAULT 0,
  author     TEXT NOT NULL DEFAULT '',
  title      TEXT NOT NULL DEFAULT '',
  body       TEXT NOT NULL DEFAULT '',
  url        TEXT NOT NULL DEFAULT '',
  url_key    TEXT NOT NULL DEFAULT '',
  subreddit  TEXT NOT NULL DEFAULT '',
  emails     TEXT NOT NULL DEFAULT '',
  signals    TEXT NOT NULL DEFAULT '',
  draft      TEXT NOT NULL DEFAULT '',
  posted_at  TEXT NOT NULL DEFAULT '',
  viewed_at  TEXT NOT NULL DEFAULT '',
  applied_at TEXT NOT NULL DEFAULT '',
  rejected_at TEXT NOT NULL DEFAULT '',
  reject_reason TEXT NOT NULL DEFAULT '',
  duplicate_of INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  UNIQUE (profile, dedupe_key)
)`); err != nil {
		return err
	}
	// Row ids are carried over, because every link key on the board and in the
	// Telegram history is derived from the id.
	if _, err := tx.Exec(`
INSERT INTO jobs_new (id, dedupe_key, profile, network, job_type, score, author, title,
                      body, url, url_key, subreddit, emails, signals, draft, posted_at,
                      viewed_at, applied_at, rejected_at, reject_reason, duplicate_of, created_at)
SELECT id, dedupe_key, profile, network, job_type, score, author, title,
       body, url, url_key, subreddit, emails, signals, draft, posted_at,
       viewed_at, applied_at, rejected_at, reject_reason, duplicate_of, created_at
FROM jobs`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE jobs`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE jobs_new RENAME TO jobs`); err != nil {
		return err
	}
	// The old indexes died with the old table.
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_jobs_network ON jobs(network, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_url_key ON jobs(url_key)`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_duplicate_of ON jobs(duplicate_of)`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_profile ON jobs(profile, created_at)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// hasLegacyDedupeUnique reports whether the table still carries the old
// single-column UNIQUE on dedupe_key.
func (s *Store) hasLegacyDedupeUnique() (bool, error) {
	rows, err := s.db.Query(`PRAGMA index_list(jobs)`)
	if err != nil {
		return false, err
	}
	var names []string
	for rows.Next() {
		// seq, name, unique, origin, partial
		var seq int
		var name, origin string
		var uniq, partial int
		if err := rows.Scan(&seq, &name, &uniq, &origin, &partial); err != nil {
			rows.Close()
			return false, err
		}
		if uniq == 1 && origin == "u" {
			names = append(names, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, name := range names {
		cols, err := s.indexColumns(name)
		if err != nil {
			return false, err
		}
		if len(cols) == 1 && cols[0] == "dedupe_key" {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) indexColumns(index string) ([]string, error) {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA index_info(%q)`, index))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var seqno, cid int
		var name sql.NullString
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			return nil, err
		}
		cols = append(cols, name.String)
	}
	return cols, rows.Err()
}

// backfillJobURLKeys fills url_key for rows written before the column existed.
// Idempotent: only rows with an empty url_key and a non-empty url are touched.
func (s *Store) backfillJobURLKeys() error {
	rows, err := s.db.Query(`SELECT id, url FROM jobs WHERE url_key = '' AND url != ''`)
	if err != nil {
		return err
	}
	type row struct {
		id  int64
		key string
	}
	var pending []row
	for rows.Next() {
		var id int64
		var url string
		if err := rows.Scan(&id, &url); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, row{id, normalizeURL(url)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range pending {
		if _, err := s.db.Exec(`UPDATE jobs SET url_key = ? WHERE id = ?`, r.key, r.id); err != nil {
			return err
		}
	}
	return nil
}

// normalizeURL is the dedup key derived from a job's URL: lowercased, with the
// fragment and any trailing slash removed. The query string is kept, because
// some job boards carry the listing id there and dropping it would collapse
// distinct roles. Two posts with the same normalized URL are the same job.
func normalizeURL(u string) string {
	u = strings.TrimSpace(u)
	if i := strings.IndexByte(u, '#'); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimRight(u, "/")
	return strings.ToLower(u)
}

// UpsertResult says what one ingest batch actually did, so the sweep's log
// can distinguish "42 new leads" from "the same 42 again".
type UpsertResult struct {
	Added   int64 `json:"added"`
	Updated int64 `json:"updated"`
}

// UpsertJobs writes a sweep batch. A re-pushed job refreshes its score, type,
// text and draft (a later sweep knows more), but never touches viewed_at or
// created_at: what you have already looked at stays looked at, and the chart
// buckets by the day a lead first appeared.
//
// Every field updates only when the incoming value is non-empty. That makes a
// partial push — dedupe_key plus a draft, nothing else — a legal way to attach
// a draft to a stored lead, rather than a way to silently blank it. Applied is
// the one exception, a tri-state pointer, because "not applied" is a real value
// that a non-empty rule could never send.
//
// A job has two identities: its dedupe_key and its normalized URL. An incoming
// job that matches EITHER an existing row updates that row instead of adding a
// second — so the same listing reposted under a different key, or the same URL
// pushed twice, can never create a duplicate. Writes are serialized (one
// connection), so the lookup-then-write below cannot race another ingest.
func (s *Store) UpsertJobs(batch []JobParams, now time.Time) (UpsertResult, error) {
	var res UpsertResult
	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	for _, p := range batch {
		if p.DedupeKey == "" || p.Network == "" {
			continue
		}
		posted := ""
		if !p.PostedAt.IsZero() {
			posted = fmtTime(p.PostedAt)
		}
		urlKey := normalizeURL(p.URL)
		// applied is a timestamp in the row, so the tri-state pointer becomes
		// "leave it alone" (0), "stamp now" (1) or "clear" (2).
		applied, appliedMode := "", 0
		if p.Applied != nil {
			if appliedMode = 2; *p.Applied {
				applied, appliedMode = fmtTime(now), 1
			}
		}
		// Rejecting follows the same tri-state, but true carries a reason and
		// false clears both fields together, so a cleared rejection never
		// leaves an orphaned reason behind.
		rejected, rejectedMode := "", 0
		reason := strings.TrimSpace(p.RejectReason)
		if p.Rejected != nil {
			if rejectedMode = 2; *p.Rejected {
				if reason == "" {
					return res, ErrRejectReasonRequired
				}
				rejected, rejectedMode = fmtTime(now), 1
			} else {
				reason = ""
			}
		}
		// Approval is the same tri-state again. Its notes are NOT part of the
		// flag, though: they update non-empty-only like draft or title, so a
		// withdrawn approval keeps the note explaining why.
		approved, approvedMode := "", 0
		notes := strings.TrimSpace(p.ReviewNotes)
		if p.Approved != nil {
			if approvedMode = 2; *p.Approved {
				approved, approvedMode = fmtTime(now), 1
			}
		}
		// A score arrives with its reasoning or not at all. Zero is exempt
		// because zero already means "this push says nothing about the score".
		scoreReason := strings.TrimSpace(p.ScoreReason)
		if p.Score != 0 && scoreReason == "" {
			return res, ErrScoreReasonRequired
		}

		// Both identities are scoped to the profile: the same Reddit post can
		// be a real lead on two people's boards, and matching across profiles
		// would let one sweep hijack the other's row along with its viewed and
		// applied state.
		//
		// The profile a partial push resolves against is the raw one, so a push
		// that omits it searches the default board rather than silently landing
		// on whichever row happened to share the key.
		insertProfile := NormalizeProfile(p.Profile)
		rawProfile := strings.ToLower(strings.TrimSpace(p.Profile))

		// Existing row by either identity. dedupe_key wins when both match
		// different rows, since it is the more specific per-post id.
		var id int64
		err := tx.QueryRow(
			`SELECT id FROM jobs
			 WHERE profile = ? AND (dedupe_key = ? OR (url_key != '' AND url_key = ?))
			 ORDER BY (dedupe_key = ?) DESC LIMIT 1`,
			insertProfile, p.DedupeKey, urlKey, p.DedupeKey).Scan(&id)
		if err == sql.ErrNoRows {
			if _, err := tx.Exec(`
INSERT INTO jobs (dedupe_key, profile, network, job_type, score, author, title, body, url, url_key,
                  subreddit, emails, signals, draft, posting_url, posting_text, score_reason,
                  prep, posted_at, applied_at,
                  rejected_at, reject_reason, approved_at, review_notes, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				p.DedupeKey, insertProfile, p.Network, p.JobType, p.Score, p.Author, p.Title, p.Body, p.URL, urlKey,
				p.Subreddit, p.Emails, p.Signals, p.Draft, p.PostingURL, p.PostingText, scoreReason,
				p.Prep, posted, applied,
				rejected, reason, approved, notes, fmtTime(now)); err != nil {
				return res, err
			}
			res.Added++
			continue
		}
		if err != nil {
			return res, err
		}
		// Update in place. Non-empty-only, so a partial push never blanks a
		// field; viewed_at and created_at are deliberately left untouched.
		if _, err := tx.Exec(`
UPDATE jobs SET
  profile   = CASE WHEN ? != '' THEN ? ELSE profile END,
  job_type  = CASE WHEN ? != '' THEN ? ELSE job_type END,
  score     = CASE WHEN ? != 0  THEN ? ELSE score END,
  author    = CASE WHEN ? != '' THEN ? ELSE author END,
  title     = CASE WHEN ? != '' THEN ? ELSE title END,
  body      = CASE WHEN ? != '' THEN ? ELSE body END,
  url       = CASE WHEN ? != '' THEN ? ELSE url END,
  url_key   = CASE WHEN ? != '' THEN ? ELSE url_key END,
  subreddit = CASE WHEN ? != '' THEN ? ELSE subreddit END,
  emails    = CASE WHEN ? != '' THEN ? ELSE emails END,
  signals   = CASE WHEN ? != '' THEN ? ELSE signals END,
  draft     = CASE WHEN ? != '' THEN ? ELSE draft END,
  posting_url  = CASE WHEN ? != '' THEN ? ELSE posting_url END,
  posting_text = CASE WHEN ? != '' THEN ? ELSE posting_text END,
  score_reason = CASE WHEN ? != '' THEN ? ELSE score_reason END,
  prep      = CASE WHEN ? != '' THEN ? ELSE prep END,
  posted_at = CASE WHEN ? != '' THEN ? ELSE posted_at END,
  applied_at = CASE ?
                 WHEN 1 THEN CASE WHEN applied_at != '' THEN applied_at ELSE ? END
                 WHEN 2 THEN ''
                 ELSE applied_at END,
  rejected_at = CASE ?
                  WHEN 1 THEN CASE WHEN rejected_at != '' THEN rejected_at ELSE ? END
                  WHEN 2 THEN ''
                  ELSE rejected_at END,
  reject_reason = CASE ?
                    WHEN 1 THEN ?
                    WHEN 2 THEN ''
                    ELSE reject_reason END,
  approved_at = CASE ?
                  WHEN 1 THEN CASE WHEN approved_at != '' THEN approved_at ELSE ? END
                  WHEN 2 THEN ''
                  ELSE approved_at END,
  review_notes = CASE WHEN ? != '' THEN ? ELSE review_notes END
WHERE id = ?`,
			rawProfile, rawProfile,
			p.JobType, p.JobType, p.Score, p.Score, p.Author, p.Author, p.Title, p.Title,
			p.Body, p.Body, p.URL, p.URL, urlKey, urlKey, p.Subreddit, p.Subreddit,
			p.Emails, p.Emails, p.Signals, p.Signals, p.Draft, p.Draft,
			p.PostingURL, p.PostingURL, p.PostingText, p.PostingText,
			scoreReason, scoreReason, p.Prep, p.Prep, posted, posted,
			appliedMode, applied, rejectedMode, rejected, rejectedMode, reason,
			approvedMode, approved, notes, notes, id); err != nil {
			return res, err
		}
		res.Updated++
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}

// JobFilter narrows every jobs query the same way, so the tiles, the chart,
// the breakdown cards and the list can never describe different sets of rows.
type JobFilter struct {
	// Profile narrows to one person's hunt; "" = every profile. It is the
	// board's outermost lens, so it composes with all the others rather than
	// replacing them.
	Profile      string
	Network      string // "" = all
	JobType      string // "" = all
	UnviewedOnly bool
	// Applied narrows by outreach state: "" = all, "1" = applied only,
	// "0" = still to apply. A string rather than a *bool so it maps straight
	// from a query parameter.
	Applied string
	// Rejected narrows the same way: "" = all, "1" = ruled out, "0" = still live.
	Rejected string
	// Approved narrows by the review gate: "" = all, "1" = cleared for applying,
	// "0" = not yet. `?approved=1&applied=0&rejected=0` is the apply stage's
	// whole input — a board URL that is also its work queue.
	Approved string
	// Prepped narrows by whether the prep stage has produced a review artifact:
	// "" = all, "1" = prepped, "0" = still to prep. That makes the prep stage's
	// input a board URL too.
	Prepped string
	// Duplicates narrows by mirror state: "" = all, "1" = mirrors only,
	// "0" = canonical rows only (one row per real role).
	Duplicates string
	Since      time.Duration // 0 = all time, else created_at within the window
	Limit      int
	SortNewest bool // false = score first
}

func (f JobFilter) where(now time.Time) (string, []any) {
	conds := []string{"1=1"}
	var args []any
	if f.Profile != "" {
		conds = append(conds, "profile = ?")
		args = append(args, f.Profile)
	}
	if f.Network != "" {
		conds = append(conds, "network = ?")
		args = append(args, f.Network)
	}
	if f.JobType != "" {
		conds = append(conds, "job_type = ?")
		args = append(args, f.JobType)
	}
	if f.UnviewedOnly {
		conds = append(conds, "viewed_at = ''")
	}
	switch f.Applied {
	case "1":
		conds = append(conds, "applied_at != ''")
	case "0":
		conds = append(conds, "applied_at = ''")
	}
	switch f.Rejected {
	case "1":
		conds = append(conds, "rejected_at != ''")
	case "0":
		conds = append(conds, "rejected_at = ''")
	}
	switch f.Approved {
	case "1":
		conds = append(conds, "approved_at != ''")
	case "0":
		conds = append(conds, "approved_at = ''")
	}
	switch f.Prepped {
	case "1":
		conds = append(conds, "prep != ''")
	case "0":
		conds = append(conds, "prep = ''")
	}
	switch f.Duplicates {
	case "1":
		conds = append(conds, "duplicate_of != 0")
	case "0":
		conds = append(conds, "duplicate_of = 0")
	}
	if f.Since > 0 {
		conds = append(conds, "created_at >= ?")
		args = append(args, fmtTime(now.Add(-f.Since)))
	}
	return strings.Join(conds, " AND "), args
}

// jobCols and scanJob are one unit: the scan is positional, so a column added
// to either has to be added to the other in the same edit.
const jobCols = `id, dedupe_key, profile, network, job_type, score, author, title, body, url,
  subreddit, emails, signals, draft, posting_url, posting_text, score_reason, prep,
  posted_at, viewed_at, applied_at,
  rejected_at, reject_reason, approved_at, review_notes, duplicate_of, created_at`

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var posted, viewed, applied, rejected, approved, created string
	if err := row.Scan(&j.ID, &j.DedupeKey, &j.Profile, &j.Network, &j.JobType, &j.Score, &j.Author,
		&j.Title, &j.Body, &j.URL, &j.Subreddit, &j.Emails, &j.Signals, &j.Draft,
		&j.PostingURL, &j.PostingText, &j.ScoreReason, &j.Prep,
		&posted, &viewed, &applied, &rejected, &j.RejectReason,
		&approved, &j.ReviewNotes, &j.DuplicateOf, &created); err != nil {
		return nil, err
	}
	if rejected != "" {
		j.RejectedAt = parseTime(rejected)
	}
	if approved != "" {
		j.ApprovedAt = parseTime(approved)
	}
	if posted != "" {
		j.PostedAt = parseTime(posted)
	}
	if viewed != "" {
		j.ViewedAt = parseTime(viewed)
	}
	if applied != "" {
		j.AppliedAt = parseTime(applied)
	}
	j.CreatedAt = parseTime(created)
	return &j, nil
}

func (s *Store) ListJobs(f JobFilter, now time.Time) ([]Job, error) {
	where, args := f.where(now)
	order := "score DESC, created_at DESC"
	if f.SortNewest {
		order = "created_at DESC, score DESC"
	}
	// Repeats sink to the bottom whatever the sort. A mirror is bookkeeping,
	// not a lead: it says nothing the canonical row does not already say, so a
	// high-scoring repost sitting above unread roles wastes the top of the
	// list. The clause is a no-op on a repeats-only board, where every row
	// scores 1 on it. `duplicate_of != 0` yields 0/1 in SQLite, so ASC puts
	// canonical rows first.
	order = "(duplicate_of != 0) ASC, " + order
	// 200 by default — a board page — but an explicit limit may go far higher:
	// the dedupe pass reads the whole history, and capping it at a page would
	// silently hide the very rows it exists to match against.
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 5000 {
		limit = 5000
	}
	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT %s FROM jobs WHERE %s ORDER BY %s LIMIT %d`, jobCols, where, order, limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func (s *Store) JobByID(id int64) (*Job, error) {
	return scanJob(s.db.QueryRow(fmt.Sprintf(`SELECT %s FROM jobs WHERE id = ?`, jobCols), id))
}

// JobProfiles lists every profile that actually has leads, most first. The
// board unions it with the profiles it knows by name, so a seeker whose first
// sweep has not run yet still gets a chip and a brand-new profile appears
// without a code change.
func (s *Store) JobProfiles() ([]LabelStat, error) {
	rows, err := s.db.Query(
		`SELECT profile, COUNT(*) FROM jobs WHERE profile != ''
		 GROUP BY profile ORDER BY COUNT(*) DESC, profile`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LabelStat
	for rows.Next() {
		var r LabelStat
		if err := rows.Scan(&r.Label, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PurgeJobs deletes every lead. A sweep whose scoring turned out wrong is
// cheaper to redo than to patch row by row; viewed state dies with the rows,
// which is correct — it described leads that no longer exist.
func (s *Store) PurgeJobs() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM jobs`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteJob removes one row — the per-row cousin of PurgeJobs, for taking a
// mistaken or migrated lead off the board without nuking the table.
func (s *Store) DeleteJob(id int64) error {
	return s.deleteJob(`id = ?`, id)
}

// DeleteJobByKey deletes by dedupe_key. The key is scoped per profile, so one
// key can hold several rows; they all describe the same post and all go.
func (s *Store) DeleteJobByKey(key string) error {
	return s.deleteJob(`dedupe_key = ?`, key)
}

func (s *Store) deleteJob(where string, arg any) error {
	res, err := s.db.Exec(`DELETE FROM jobs WHERE `+where, arg)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkJobViewed stamps the first look and keeps it: opening a lead twice does
// not move the timestamp, so "viewed 3 days ago" stays honest.
func (s *Store) MarkJobViewed(id int64, now time.Time) error {
	res, err := s.db.Exec(`UPDATE jobs SET viewed_at = ? WHERE id = ? AND viewed_at = ''`, fmtTime(now), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either already viewed (fine) or unknown id (the caller's 404).
		var exists int
		if err := s.db.QueryRow(`SELECT 1 FROM jobs WHERE id = ?`, id).Scan(&exists); err != nil {
			return sql.ErrNoRows
		}
	}
	return nil
}

// SetJobApplied flips one lead's outreach flag. Applying is sticky the way
// viewing is: marking an already-applied lead again keeps the original date,
// so "applied 6 days ago, still no reply" stays true. Clearing is allowed,
// because a mis-click should be undoable.
//
// Returns sql.ErrNoRows when the id does not exist, so a caller can 404.
func (s *Store) SetJobApplied(id int64, applied bool, now time.Time) error {
	return s.setApplied(`id = ?`, id, applied, now)
}

// SetJobAppliedByKey is the same flip addressed by the sweep's own dedupe_key,
// so an agent that just pushed a lead can mark it applied without first
// looking up the numeric id trackhub happened to give it.
func (s *Store) SetJobAppliedByKey(key string, applied bool, now time.Time) error {
	id, err := s.firstIDByKey(key)
	if err != nil {
		return err
	}
	return s.setApplied(`id = ?`, id, applied, now)
}

// firstIDByKey resolves a dedupe_key to one row. Keys are unique per profile
// rather than globally, so a post that is a lead on two boards has two rows;
// the oldest wins, which keeps repeated calls landing on the same row instead
// of alternating. Callers that must be exact address the numeric id.
func (s *Store) firstIDByKey(key string) (int64, error) {
	var id int64
	if err := s.db.QueryRow(
		`SELECT id FROM jobs WHERE dedupe_key = ? ORDER BY id LIMIT 1`, key).Scan(&id); err != nil {
		return 0, sql.ErrNoRows
	}
	return id, nil
}

func (s *Store) setApplied(where string, arg any, applied bool, now time.Time) error {
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM jobs WHERE `+where, arg).Scan(&exists); err != nil {
		return sql.ErrNoRows
	}
	stamp := ""
	if applied {
		stamp = fmtTime(now)
	}
	_, err := s.db.Exec(
		`UPDATE jobs SET applied_at = CASE WHEN ? != '' AND applied_at != '' THEN applied_at ELSE ? END
		 WHERE `+where, stamp, stamp, arg)
	return err
}

// SetJobRejected rules a lead out, or puts it back in play. A reason is
// mandatory when rejecting and is cleared along with the flag when un-rejecting,
// so a row can never show a stale "why" for a rejection that no longer stands.
//
// Like applying, the timestamp is sticky: re-rejecting keeps the original date
// but does refresh the reason, since a second look often sharpens the wording.
func (s *Store) SetJobRejected(id int64, rejected bool, reason string, now time.Time) error {
	return s.setRejected(`id = ?`, id, rejected, reason, now)
}

// SetJobRejectedByKey is the same, addressed by the sweep's own dedupe_key.
func (s *Store) SetJobRejectedByKey(key string, rejected bool, reason string, now time.Time) error {
	// The reason is checked before the lookup so an unknown key and a missing
	// reason cannot report each other's error.
	if rejected && strings.TrimSpace(reason) == "" {
		return ErrRejectReasonRequired
	}
	id, err := s.firstIDByKey(key)
	if err != nil {
		return err
	}
	return s.setRejected(`id = ?`, id, rejected, reason, now)
}

func (s *Store) setRejected(where string, arg any, rejected bool, reason string, now time.Time) error {
	reason = strings.TrimSpace(reason)
	if rejected && reason == "" {
		return ErrRejectReasonRequired
	}
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM jobs WHERE `+where, arg).Scan(&exists); err != nil {
		return sql.ErrNoRows
	}
	stamp := ""
	if rejected {
		stamp = fmtTime(now)
	} else {
		reason = ""
	}
	_, err := s.db.Exec(
		`UPDATE jobs SET
		   rejected_at = CASE WHEN ? != '' AND rejected_at != '' THEN rejected_at ELSE ? END,
		   reject_reason = ?
		 WHERE `+where, stamp, stamp, reason, arg)
	return err
}

// SetJobApproved passes a lead through the review gate, or takes it back. It is
// the flag the apply stage reads, so it is the last thing standing between a
// prepared application and a real one going out.
//
// Notes are optional, unlike a reject reason: most approvals have nothing to
// add, and demanding a sentence for those only produces empty ones typed to
// clear the field. Approving again keeps the original date, so re-approving to
// attach a note does not rewrite when the decision was made.
//
// Notes are NOT cleared by un-approving, which is where they differ from a
// reject reason. A reject reason belongs to the rejection and is meaningless
// without it; a review note belongs to the review, and "actually this is US
// only, do not apply" is worth more after the approval is withdrawn than it was
// before. Clearing notes is its own edit — see SetJobReviewNotes.
func (s *Store) SetJobApproved(id int64, approved bool, notes string, now time.Time) error {
	return s.setApproved(`id = ?`, id, approved, notes, now)
}

// SetJobReviewNotes writes the review notes on their own, leaving the approval
// flag alone. Empty is a legal value: clearing a note is a real edit.
//
// It exists separately because the usual shape of a review is to write the note
// while reading and decide afterwards — so the notes field has to be writable
// on a lead that has not been approved, and must survive the decision either
// way.
func (s *Store) SetJobReviewNotes(id int64, notes string) error {
	res, err := s.db.Exec(`UPDATE jobs SET review_notes = ? WHERE id = ?`, strings.TrimSpace(notes), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetJobApprovedByKey is the same, addressed by the sweep's own dedupe_key.
func (s *Store) SetJobApprovedByKey(key string, approved bool, notes string, now time.Time) error {
	id, err := s.firstIDByKey(key)
	if err != nil {
		return err
	}
	return s.setApproved(`id = ?`, id, approved, notes, now)
}

func (s *Store) setApproved(where string, arg any, approved bool, notes string, now time.Time) error {
	notes = strings.TrimSpace(notes)
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM jobs WHERE `+where, arg).Scan(&exists); err != nil {
		return sql.ErrNoRows
	}
	stamp := ""
	if approved {
		stamp = fmtTime(now)
	}
	// Notes are non-empty-only here: this call is about the flag, and passing no
	// notes means "say nothing about them" rather than "erase them".
	_, err := s.db.Exec(
		`UPDATE jobs SET
		   approved_at = CASE WHEN ? != '' AND approved_at != '' THEN approved_at ELSE ? END,
		   review_notes = CASE WHEN ? != '' THEN ? ELSE review_notes END
		 WHERE `+where, stamp, stamp, notes, notes, arg)
	return err
}

// SetJobPrep replaces the review artifact the prep stage produces. Empty is a
// legal value — clearing it puts the lead back in the prep queue, which is how
// a bad CV or a stale summary gets redone.
//
// The blob is opaque here on purpose: it is rendered on one page and never
// queried, so the store has no reason to know its shape and no reason to break
// when that shape changes.
func (s *Store) SetJobPrep(id int64, prep string) error {
	res, err := s.db.Exec(`UPDATE jobs SET prep = ? WHERE id = ?`, prep, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ErrDuplicateCycle is returned when linking two leads would make a loop, so
// no row can end up pointing at itself through a chain.
var ErrDuplicateCycle = errors.New("that link would make a duplicate cycle")

// LinkDuplicate marks dupID as a mirror of canonicalID: the same underlying
// role reached by another route. Both ids may be row ids or dedupe_keys.
//
// Two rules keep the graph a shallow forest rather than a linked list:
// pointing at a row that is itself a duplicate collapses to ITS canonical, and
// any row already pointing at dupID is re-pointed at the canonical too. So
// reads are always one hop and the canonical is always a real, non-duplicate
// row.
func (s *Store) LinkDuplicate(dupID, canonicalID int64) error {
	if dupID == canonicalID {
		return ErrDuplicateCycle
	}
	// Collapse: if the target is a mirror, adopt its canonical instead.
	var targetParent int64
	if err := s.db.QueryRow(`SELECT duplicate_of FROM jobs WHERE id = ?`, canonicalID).Scan(&targetParent); err != nil {
		return sql.ErrNoRows
	}
	if targetParent != 0 {
		canonicalID = targetParent
	}
	if dupID == canonicalID {
		return ErrDuplicateCycle
	}
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM jobs WHERE id = ?`, dupID).Scan(&exists); err != nil {
		return sql.ErrNoRows
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Anything that pointed at the row now becoming a duplicate has to follow
	// it, or the chain would grow a second hop.
	if _, err := tx.Exec(`UPDATE jobs SET duplicate_of = ? WHERE duplicate_of = ?`, canonicalID, dupID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE jobs SET duplicate_of = ? WHERE id = ?`, canonicalID, dupID); err != nil {
		return err
	}
	return tx.Commit()
}

// UnlinkDuplicate makes a lead canonical again.
func (s *Store) UnlinkDuplicate(id int64) error {
	res, err := s.db.Exec(`UPDATE jobs SET duplicate_of = 0 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DuplicatesOf lists the mirrors pointing at one canonical lead, so its detail
// page can show what else is the same role.
func (s *Store) DuplicatesOf(id int64) ([]Job, error) {
	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT %s FROM jobs WHERE duplicate_of = ? ORDER BY score DESC, id`, jobCols), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// ResolveJobRef turns a row id or a dedupe_key into a row id. The numeric form
// is tried first and falls back to the key, which is what makes X leads
// (numeric tweet-id keys) addressable.
func (s *Store) ResolveJobRef(ref string) (int64, error) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		var found int64
		if err := s.db.QueryRow(`SELECT id FROM jobs WHERE id = ?`, id).Scan(&found); err == nil {
			return found, nil
		}
	}
	return s.firstIDByKey(ref)
}

// JobStats is the dashboard above the list, computed under the same filter as
// the list itself.
type JobStats struct {
	Total    int64
	Unviewed int64
	Applied  int64
	Rejected int64
	// ToReview and ToSend are queue lengths, not totals: leads prepped but not
	// yet decided, and leads approved but not yet applied. Counting every
	// prepped or approved row instead would make both numbers grow as the work
	// got done, which is the opposite of what a queue should do.
	ToReview int64
	ToSend   int64
	// Duplicates counts mirrors; Total minus Duplicates is how many distinct
	// roles the board actually holds, which is the number worth reporting.
	Duplicates int64
	Last24h    int64
	Days       []DayBar
	BucketDays int
	// Profiles counts leads per job seeker. It is computed under the same
	// filter as everything else, so on a profile-filtered board it holds one
	// row — which is exactly the confirmation the reader wants.
	Profiles   []LabelStat
	Networks   []LabelStat
	Types      []LabelStat
	Subreddits []LabelStat
}

func (s *Store) JobStats(f JobFilter, now time.Time) (*JobStats, error) {
	where, args := f.where(now)
	st := &JobStats{}
	if err := s.db.QueryRow(fmt.Sprintf(
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN viewed_at = '' THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN applied_at != '' THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN rejected_at != '' THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN prep != '' AND approved_at = '' AND rejected_at = '' THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN approved_at != '' AND applied_at = '' AND rejected_at = '' THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN duplicate_of != 0 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END), 0)
		 FROM jobs WHERE %s`, where),
		append([]any{fmtTime(now.Add(-24 * time.Hour))}, args...)...,
	).Scan(&st.Total, &st.Unviewed, &st.Applied, &st.Rejected,
		&st.ToReview, &st.ToSend, &st.Duplicates, &st.Last24h); err != nil {
		return nil, err
	}

	for _, b := range []struct {
		col  string
		dest *[]LabelStat
	}{
		{"profile", &st.Profiles},
		{"network", &st.Networks},
		{"job_type", &st.Types},
		{"subreddit", &st.Subreddits},
	} {
		rows, err := s.db.Query(fmt.Sprintf(
			`SELECT %s, COUNT(*) FROM jobs WHERE %s AND %s != ''
			 GROUP BY %s ORDER BY COUNT(*) DESC LIMIT 8`, b.col, where, b.col, b.col), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r LabelStat
			if err := rows.Scan(&r.Label, &r.Count); err != nil {
				rows.Close()
				return nil, err
			}
			*b.dest = append(*b.dest, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// Chart: leads per day since the first one under this filter, same
	// bucketing rule as the site dashboard (days, then weeks past 92 columns).
	var firstStr string
	err := s.db.QueryRow(fmt.Sprintf(
		`SELECT MIN(created_at) FROM jobs WHERE %s`, where), args...).Scan(&firstStr)
	if err == sql.ErrNoRows || firstStr == "" {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	from := parseTime(firstStr).UTC().Truncate(24 * time.Hour)
	to := now.UTC().Truncate(24 * time.Hour)
	if to.Sub(from) < 6*24*time.Hour {
		from = to.Add(-6 * 24 * time.Hour)
	}
	totalDays := int(to.Sub(from).Hours()/24) + 1
	st.BucketDays = 1
	if totalDays > 92 {
		st.BucketDays = 7
	}
	counts := map[string]int64{}
	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT substr(created_at, 1, 10), COUNT(*) FROM jobs WHERE %s GROUP BY 1`, where), args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var day string
		var n int64
		if err := rows.Scan(&day, &n); err != nil {
			rows.Close()
			return nil, err
		}
		counts[day] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	step := time.Duration(st.BucketDays) * 24 * time.Hour
	for d := from; !d.After(to); d = d.Add(step) {
		var n int64
		for i := 0; i < st.BucketDays; i++ {
			n += counts[d.Add(time.Duration(i)*24*time.Hour).Format("2006-01-02")]
		}
		st.Days = append(st.Days, DayBar{Day: d.Format("2006-01-02"), Visits: n})
	}
	return st, nil
}
