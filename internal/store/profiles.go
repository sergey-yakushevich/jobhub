package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Profile is one job seeker the board hunts for: the person behind a lead's
// `profile` tag, described well enough that the prep and apply stages can read
// the description instead of a file kept beside the app.
//
// Slug is the identity. It is the same lowercase name the board has always
// filtered by ("sergey", "polina"), so a profile row and the leads tagged with
// it find each other without anything having to be re-keyed.
//
// The description is four plain-text fields rather than a CV schema. What a
// reader — human or agent — needs is the prose: who this is, what they can do,
// what they have done, and what they will accept. A structured CV belongs in
// the tailored-CV pipeline; this is the standing brief that pipeline works from.
type Profile struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name,omitempty"`
	Headline string `json:"headline,omitempty"`
	// Summary is the "about" paragraph: who this person is in one read.
	Summary string `json:"summary,omitempty"`
	// Skills, Experience and Conditions are the three questions anyone judging
	// a lead against a person actually asks. Conditions is the one a job board
	// usually loses: rate floor, notice period, location, work authorization.
	Skills     string `json:"skills,omitempty"`
	Experience string `json:"experience,omitempty"`
	Conditions string `json:"conditions,omitempty"`
	// Links are the public faces — site, GitHub, LinkedIn — one per line.
	Links string `json:"links,omitempty"`
	// Leads is how many leads this profile owns. It is filled by ListProfiles
	// and left at zero elsewhere, since a single profile read has no list to
	// count against.
	Leads     int64     `json:"leads"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Described reports whether a profile carries any description at all. A row
// backfilled from the leads that mention it has a slug and nothing else, and
// the page says so rather than rendering four empty boxes.
func (p *Profile) Described() bool {
	return p.Summary != "" || p.Skills != "" || p.Experience != "" || p.Conditions != ""
}

// ProfileParams is one profile as a caller sends it. Every field except Slug
// follows the same non-empty-only rule the job fields do, so a partial push
// updates what it names and leaves the rest alone.
type ProfileParams struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Headline   string `json:"headline"`
	Summary    string `json:"summary"`
	Skills     string `json:"skills"`
	Experience string `json:"experience"`
	Conditions string `json:"conditions"`
	Links      string `json:"links"`
}

// ErrProfileSlugRequired is returned when a profile is written without a name
// to file it under. The slug is the whole identity here — it is what ties the
// row to the leads already tagged with it — so an empty one is not a profile.
var ErrProfileSlugRequired = errors.New("profile slug is required")

// ErrOwnerLinkRequired is returned when the link being removed is the one that
// says whose sweep found the lead. A lead always belongs to exactly one board;
// extra profiles are shares on top of that, and unsharing may not orphan it.
var ErrOwnerLinkRequired = errors.New("a lead cannot be unlinked from the profile that owns it")

// migrateProfiles adds the profiles table and the job_profiles join, then
// backfills both from the profile names already written on the leads.
//
// The backfill is the whole safety story of this migration: no column on jobs
// is touched, nothing is rewritten, and every existing lead gains one link row
// pointing at a profile created from its own `profile` value. A board that has
// never heard of the profiles table therefore comes up with exactly the leads,
// the counts and the filters it had before — the many-to-many starts as the
// one-to-many it replaces, and only diverges when somebody shares a lead.
//
// Both steps are INSERT ... WHERE NOT EXISTS, so running them on every boot is
// free and also self-heals a row written by a path that forgot to link.
func (s *Store) migrateProfiles() error {
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS profiles (
  id         INTEGER PRIMARY KEY,
  -- The lowercase name the leads are already tagged with. It is the join key,
  -- so renaming a profile means moving its leads, not editing this in place.
  slug       TEXT NOT NULL UNIQUE,
  name       TEXT NOT NULL DEFAULT '',
  headline   TEXT NOT NULL DEFAULT '',
  summary    TEXT NOT NULL DEFAULT '',
  skills     TEXT NOT NULL DEFAULT '',
  experience TEXT NOT NULL DEFAULT '',
  conditions TEXT NOT NULL DEFAULT '',
  links      TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS job_profiles (
  job_id     INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
  -- owner marks the one link that is not a share: the profile whose sweep
  -- found the lead, and the profile jobs.profile still names. It is what makes
  -- deleting a share safe — the owner link cannot be the one removed.
  owner      INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  PRIMARY KEY (job_id, profile_id)
);
CREATE INDEX IF NOT EXISTS idx_job_profiles_profile ON job_profiles(profile_id, job_id);
`); err != nil {
		return err
	}
	return s.backfillJobProfiles()
}

// backfillJobProfiles makes the join table agree with jobs.profile: a row for
// every profile name in use, and an owner link from every lead to its own.
func (s *Store) backfillJobProfiles() error {
	now := fmtTime(time.Now())
	if _, err := s.db.Exec(`
INSERT INTO profiles (slug, created_at, updated_at)
SELECT DISTINCT j.profile, ?, ?
  FROM jobs j
 WHERE j.profile != ''
   AND NOT EXISTS (SELECT 1 FROM profiles p WHERE p.slug = j.profile)`, now, now); err != nil {
		return err
	}
	// created_at is the lead's own, not this migration's: the link has existed
	// in substance since the lead was written, and dating it today would make
	// every backfilled share look like it happened at deploy time.
	_, err := s.db.Exec(`
INSERT INTO job_profiles (job_id, profile_id, owner, created_at)
SELECT j.id, p.id, 1, j.created_at
  FROM jobs j
  JOIN profiles p ON p.slug = j.profile
 WHERE NOT EXISTS (
       SELECT 1 FROM job_profiles jp WHERE jp.job_id = j.id AND jp.profile_id = p.id)`)
	return err
}

// EnsureProfile creates a bare profile row for a slug that has none, so a lead
// swept for a brand-new seeker still has something to link to. It never
// touches an existing row.
func (s *Store) EnsureProfile(slug string) (int64, error) {
	return ensureProfile(s.db, slug, time.Now())
}

// execQuerier is the half of *sql.DB and *sql.Tx this file needs, so the same
// helpers serve an ingest transaction and a standalone call.
type execQuerier interface {
	Exec(string, ...any) (sql.Result, error)
	QueryRow(string, ...any) *sql.Row
}

func ensureProfile(q execQuerier, slug string, now time.Time) (int64, error) {
	slug = NormalizeProfile(slug)
	var id int64
	err := q.QueryRow(`SELECT id FROM profiles WHERE slug = ?`, slug).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	stamp := fmtTime(now)
	res, err := q.Exec(
		`INSERT INTO profiles (slug, created_at, updated_at) VALUES (?, ?, ?)`, slug, stamp, stamp)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// setOwnerLink points a lead's owner link at one profile, creating the profile
// if it is new. Any other owner link on the lead is demoted to a share rather
// than deleted: moving a lead to another board is not a reason to forget that
// the first board was once looking at it.
func setOwnerLink(q execQuerier, jobID int64, slug string, now time.Time) error {
	profileID, err := ensureProfile(q, slug, now)
	if err != nil {
		return err
	}
	if _, err := q.Exec(
		`UPDATE job_profiles SET owner = 0 WHERE job_id = ? AND profile_id != ?`, jobID, profileID); err != nil {
		return err
	}
	if _, err := q.Exec(`
INSERT INTO job_profiles (job_id, profile_id, owner, created_at) VALUES (?, ?, 1, ?)
ON CONFLICT (job_id, profile_id) DO UPDATE SET owner = 1`,
		jobID, profileID, fmtTime(now)); err != nil {
		return err
	}
	return nil
}

// UpsertProfile writes a profile's description. Non-empty-only, like the job
// fields: a push that carries just the conditions updates the conditions and
// leaves the summary standing.
func (s *Store) UpsertProfile(p ProfileParams, now time.Time) (*Profile, error) {
	slug := strings.ToLower(strings.TrimSpace(p.Slug))
	if slug == "" {
		return nil, ErrProfileSlugRequired
	}
	if _, err := ensureProfile(s.db, slug, now); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`
UPDATE profiles SET
  name       = CASE WHEN ? != '' THEN ? ELSE name END,
  headline   = CASE WHEN ? != '' THEN ? ELSE headline END,
  summary    = CASE WHEN ? != '' THEN ? ELSE summary END,
  skills     = CASE WHEN ? != '' THEN ? ELSE skills END,
  experience = CASE WHEN ? != '' THEN ? ELSE experience END,
  conditions = CASE WHEN ? != '' THEN ? ELSE conditions END,
  links      = CASE WHEN ? != '' THEN ? ELSE links END,
  updated_at = ?
WHERE slug = ?`,
		p.Name, p.Name, p.Headline, p.Headline, p.Summary, p.Summary,
		p.Skills, p.Skills, p.Experience, p.Experience,
		p.Conditions, p.Conditions, p.Links, p.Links,
		fmtTime(now), slug); err != nil {
		return nil, err
	}
	return s.ProfileBySlug(slug)
}

// SeedProfile fills a profile in without ever overwriting a human. It writes
// only the fields that are currently empty, so shipping a seed is safe on a
// board where somebody has already edited the description, and a seed that
// later grows a field still fills that one in.
//
// It reports whether anything was written.
func (s *Store) SeedProfile(p ProfileParams, now time.Time) (bool, error) {
	slug := strings.ToLower(strings.TrimSpace(p.Slug))
	if slug == "" {
		return false, ErrProfileSlugRequired
	}
	if _, err := ensureProfile(s.db, slug, now); err != nil {
		return false, err
	}
	res, err := s.db.Exec(`
UPDATE profiles SET
  name       = CASE WHEN name       = '' THEN ? ELSE name END,
  headline   = CASE WHEN headline   = '' THEN ? ELSE headline END,
  summary    = CASE WHEN summary    = '' THEN ? ELSE summary END,
  skills     = CASE WHEN skills     = '' THEN ? ELSE skills END,
  experience = CASE WHEN experience = '' THEN ? ELSE experience END,
  conditions = CASE WHEN conditions = '' THEN ? ELSE conditions END,
  links      = CASE WHEN links      = '' THEN ? ELSE links END,
  updated_at = ?
WHERE slug = ?
  AND (name = '' OR headline = '' OR summary = '' OR skills = ''
       OR experience = '' OR conditions = '' OR links = '')`,
		p.Name, p.Headline, p.Summary, p.Skills, p.Experience, p.Conditions, p.Links,
		fmtTime(now), slug)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

const profileCols = `id, slug, name, headline, summary, skills, experience, conditions, links,
  created_at, updated_at`

func scanProfile(row interface{ Scan(...any) error }) (*Profile, error) {
	var p Profile
	var created, updated string
	if err := row.Scan(&p.ID, &p.Slug, &p.Name, &p.Headline, &p.Summary, &p.Skills,
		&p.Experience, &p.Conditions, &p.Links, &created, &updated); err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = parseTime(created), parseTime(updated)
	return &p, nil
}

// ProfileBySlug reads one profile. sql.ErrNoRows means the board has never
// heard of that seeker, which is a 404 and not an empty profile.
func (s *Store) ProfileBySlug(slug string) (*Profile, error) {
	return scanProfile(s.db.QueryRow(
		fmt.Sprintf(`SELECT %s FROM profiles WHERE slug = ?`, profileCols),
		strings.ToLower(strings.TrimSpace(slug))))
}

// ListProfiles lists every profile with the number of leads it owns, busiest
// first. Shared leads are counted on the board that owns them, so the counts
// still add up to the number of rows in jobs.
func (s *Store) ListProfiles() ([]Profile, error) {
	rows, err := s.db.Query(fmt.Sprintf(`
SELECT %s, (SELECT COUNT(*) FROM jobs j WHERE j.profile = p.slug)
  FROM profiles p ORDER BY 12 DESC, p.slug`,
		prefixed(profileCols, "p")))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		var p Profile
		var created, updated string
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Headline, &p.Summary, &p.Skills,
			&p.Experience, &p.Conditions, &p.Links, &created, &updated, &p.Leads); err != nil {
			return nil, err
		}
		p.CreatedAt, p.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, p)
	}
	return out, rows.Err()
}

// prefixed qualifies a column list with a table alias, so one const can serve
// both the plain read and the joined one.
func prefixed(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// JobProfileLink is one lead-to-profile link as the lead page shows it: who,
// and whether this is the board that owns the lead or one it was shared with.
type JobProfileLink struct {
	Profile
	Owner bool `json:"owner"`
}

// ProfilesForJob lists every profile a lead is on, owner first. That is the
// many-to-many read: one lead, the board that swept it, and everyone it has
// been shared with since.
func (s *Store) ProfilesForJob(jobID int64) ([]JobProfileLink, error) {
	rows, err := s.db.Query(fmt.Sprintf(`
SELECT %s, jp.owner
  FROM job_profiles jp JOIN profiles p ON p.id = jp.profile_id
 WHERE jp.job_id = ?
 ORDER BY jp.owner DESC, p.slug`, prefixed(profileCols, "p")), jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobProfileLink
	for rows.Next() {
		var l JobProfileLink
		var created, updated string
		var owner int
		if err := rows.Scan(&l.ID, &l.Slug, &l.Name, &l.Headline, &l.Summary, &l.Skills,
			&l.Experience, &l.Conditions, &l.Links, &created, &updated, &owner); err != nil {
			return nil, err
		}
		l.CreatedAt, l.UpdatedAt = parseTime(created), parseTime(updated)
		l.Owner = owner == 1
		out = append(out, l)
	}
	return out, rows.Err()
}

// LinkJobProfile shares a lead with another profile: the same posting, on a
// second person's board, without the copy that (profile, dedupe_key) identity
// would otherwise force. It is idempotent, and it never changes who owns the
// lead — a share is an addition, not a handover.
func (s *Store) LinkJobProfile(jobID int64, slug string, now time.Time) error {
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM jobs WHERE id = ?`, jobID).Scan(&exists); err != nil {
		return sql.ErrNoRows
	}
	if strings.TrimSpace(slug) == "" {
		return ErrProfileSlugRequired
	}
	profileID, err := ensureProfile(s.db, slug, now)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
INSERT INTO job_profiles (job_id, profile_id, owner, created_at) VALUES (?, ?, 0, ?)
ON CONFLICT (job_id, profile_id) DO NOTHING`, jobID, profileID, fmtTime(now))
	return err
}

// UnlinkJobProfile takes a lead off a board it was shared with. The owner link
// is refused: a lead with no profile at all would be invisible on every board,
// which is a worse outcome than a share nobody wanted.
func (s *Store) UnlinkJobProfile(jobID int64, slug string) error {
	var owner int
	err := s.db.QueryRow(`
SELECT jp.owner FROM job_profiles jp JOIN profiles p ON p.id = jp.profile_id
 WHERE jp.job_id = ? AND p.slug = ?`,
		jobID, NormalizeProfile(slug)).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if owner == 1 {
		return ErrOwnerLinkRequired
	}
	_, err = s.db.Exec(`
DELETE FROM job_profiles
 WHERE job_id = ?
   AND profile_id = (SELECT id FROM profiles WHERE slug = ?)`, jobID, NormalizeProfile(slug))
	return err
}
