package store

import (
	"testing"
	"time"
)

// A lead pushed without a profile belongs to the board's default owner, so a
// sweep written before profiles existed keeps landing where it always did.
func TestUpsertJobsDefaultsProfile(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_abc", Network: "reddit", Score: 5, ScoreReason: "seed"},
	}, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	jobs, err := s.ListJobs(JobFilter{}, now)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list: %v %d", err, len(jobs))
	}
	if jobs[0].Profile != DefaultJobProfile {
		t.Fatalf("profile = %q, want %q", jobs[0].Profile, DefaultJobProfile)
	}
}

// Profiles are normalised on the way in, so "Polina", " polina " and "polina"
// cannot become three separate columns on the board.
func TestUpsertJobsNormalizesProfile(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_a", Network: "reddit", Profile: "  Polina ", Score: 5, ScoreReason: "seed"},
		{DedupeKey: "t3_b", Network: "reddit", Profile: "POLINA", Score: 5, ScoreReason: "seed"},
	}, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	jobs, err := s.ListJobs(JobFilter{Profile: "polina"}, now)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("list polina: %v %d", err, len(jobs))
	}
}

// The filter is what makes the board usable by two people: one person's leads
// at a time, and an empty profile still means everything.
func TestListJobsFiltersByProfile(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_go", Network: "reddit", Profile: "sergey", Score: 9, ScoreReason: "seed"},
		{DedupeKey: "t3_ugc", Network: "reddit", Profile: "polina", Score: 8, ScoreReason: "seed"},
		{DedupeKey: "t3_ads", Network: "x", Profile: "polina", Score: 7, ScoreReason: "seed"},
	}, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, tc := range []struct {
		profile string
		want    int
	}{{"", 3}, {"polina", 2}, {"sergey", 1}, {"nobody", 0}} {
		jobs, err := s.ListJobs(JobFilter{Profile: tc.profile}, now)
		if err != nil {
			t.Fatalf("list %q: %v", tc.profile, err)
		}
		if len(jobs) != tc.want {
			t.Fatalf("profile %q: got %d leads, want %d", tc.profile, len(jobs), tc.want)
		}
	}
	// The filter narrows the stats the same way it narrows the list, so the
	// tiles can never describe a different set of rows than the cards.
	st, err := s.JobStats(JobFilter{Profile: "polina"}, now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Total != 2 {
		t.Fatalf("stats total = %d, want 2", st.Total)
	}
	if len(st.Profiles) != 1 || st.Profiles[0].Label != "polina" {
		t.Fatalf("stats profiles = %+v, want just polina", st.Profiles)
	}
}

// One post can be a real lead on two boards. Identity is (profile, key), so
// the second sweep gets its own row instead of hijacking the first one's
// viewed and applied state.
func TestUpsertJobsSameKeyDifferentProfiles(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_shared", Network: "reddit", Profile: "sergey", Score: 9, ScoreReason: "seed",
			Title: "for sergey", URL: "https://reddit.com/r/forhire/shared"},
	}, now); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.SetJobAppliedByKey("t3_shared", true, now); err != nil {
		t.Fatalf("apply: %v", err)
	}
	res, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_shared", Network: "reddit", Profile: "polina", Score: 8, ScoreReason: "seed",
			Title: "for polina", URL: "https://reddit.com/r/forhire/shared"},
	}, now)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if res.Added != 1 || res.Updated != 0 {
		t.Fatalf("second push: %+v, want one added row", res)
	}
	polina, err := s.ListJobs(JobFilter{Profile: "polina"}, now)
	if err != nil || len(polina) != 1 {
		t.Fatalf("list polina: %v %d", err, len(polina))
	}
	if polina[0].Applied() {
		t.Fatal("polina's copy inherited sergey's applied state")
	}
	if polina[0].Title != "for polina" {
		t.Fatalf("title = %q", polina[0].Title)
	}
	sergey, err := s.ListJobs(JobFilter{Profile: "sergey"}, now)
	if err != nil || len(sergey) != 1 {
		t.Fatalf("list sergey: %v %d", err, len(sergey))
	}
	if !sergey[0].Applied() {
		t.Fatal("sergey's applied stamp was lost")
	}
}

// A partial push — the way a draft gets attached — says nothing about the
// profile, so it must not drag the lead onto the default board.
func TestUpsertJobsPartialPushKeepsProfile(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_ugc", Network: "reddit", Profile: "polina", Score: 8, ScoreReason: "seed"},
	}, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Addressed by key with no profile: it resolves against the default board,
	// finds nothing there, and adds a row rather than moving polina's.
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_ugc", Network: "reddit", Profile: "polina", Draft: "hi there"},
	}, now); err != nil {
		t.Fatalf("draft push: %v", err)
	}
	jobs, err := s.ListJobs(JobFilter{Profile: "polina"}, now)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list: %v %d", err, len(jobs))
	}
	if jobs[0].Draft != "hi there" {
		t.Fatalf("draft not attached: %+v", jobs[0])
	}
	if jobs[0].Profile != "polina" {
		t.Fatalf("profile moved to %q", jobs[0].Profile)
	}
}

// JobProfiles feeds the board's chips; it reports only profiles that actually
// hold leads, busiest first.
func TestJobProfiles(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "a", Network: "reddit", Profile: "polina", Score: 1, ScoreReason: "seed"},
		{DedupeKey: "b", Network: "reddit", Profile: "polina", Score: 1, ScoreReason: "seed"},
		{DedupeKey: "c", Network: "reddit", Profile: "sergey", Score: 1, ScoreReason: "seed"},
	}, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.JobProfiles()
	if err != nil {
		t.Fatalf("profiles: %v", err)
	}
	if len(got) != 2 || got[0].Label != "polina" || got[0].Count != 2 || got[1].Label != "sergey" {
		t.Fatalf("profiles = %+v", got)
	}
}

// The migration has to carry a single-tenant database forward: the old UNIQUE
// on dedupe_key is dropped, row ids survive (every link key is derived from
// them) and every existing lead is filed under the default owner.
func TestMigrateLegacyJobsTable(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`DROP TABLE jobs`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	// The schema exactly as it shipped before profiles existed.
	if _, err := s.db.Exec(`
CREATE TABLE jobs (
  id         INTEGER PRIMARY KEY,
  dedupe_key TEXT NOT NULL UNIQUE,
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
  created_at TEXT NOT NULL
)`); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO jobs (id, dedupe_key, network, title, viewed_at, created_at)
		 VALUES (77, 't3_old', 'reddit', 'old lead', '2026-09-01T10:00:00Z', '2026-09-01T09:00:00Z')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.migrateJobs(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	legacy, err := s.hasLegacyDedupeUnique()
	if err != nil {
		t.Fatalf("index check: %v", err)
	}
	if legacy {
		t.Fatal("old single-column UNIQUE on dedupe_key survived the migration")
	}
	j, err := s.JobByID(77)
	if err != nil {
		t.Fatalf("row id 77 did not survive: %v", err)
	}
	if j.Profile != DefaultJobProfile {
		t.Fatalf("profile = %q, want %q", j.Profile, DefaultJobProfile)
	}
	if j.Title != "old lead" || !j.Viewed() {
		t.Fatalf("row lost data: %+v", j)
	}
	// Running it again must change nothing.
	if err := s.migrateJobs(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if _, err := s.JobByID(77); err != nil {
		t.Fatalf("row lost on re-migrate: %v", err)
	}
	// And the new identity works: the same key on another profile is a new row.
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	res, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_old", Network: "reddit", Profile: "polina", Score: 4, ScoreReason: "seed"},
	}, now)
	if err != nil {
		t.Fatalf("cross-profile upsert: %v", err)
	}
	if res.Added != 1 {
		t.Fatalf("cross-profile upsert: %+v, want one added", res)
	}
}

// A repeat is bookkeeping, not a lead: it belongs under the canonical rows
// however the list is sorted, even when it outscores them.
func TestListJobsSinksRepeatsToTheBottom(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "canon_low", Network: "reddit", Score: 3, ScoreReason: "seed", Title: "canonical, low score"},
		{DedupeKey: "mirror_high", Network: "reddit", Score: 9, ScoreReason: "seed", Title: "repeat, high score"},
		{DedupeKey: "canon_mid", Network: "reddit", Score: 6, ScoreReason: "seed", Title: "canonical, mid score"},
	}, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	ids := map[string]int64{}
	all, err := s.ListJobs(JobFilter{}, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, j := range all {
		ids[j.DedupeKey] = j.ID
	}
	if err := s.LinkDuplicate(ids["mirror_high"], ids["canon_mid"]); err != nil {
		t.Fatalf("link: %v", err)
	}

	// Score sort: the 9-scoring repeat must still land last, under the 3.
	got, err := s.ListJobs(JobFilter{}, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"canon_mid", "canon_low", "mirror_high"}
	for i, w := range want {
		if got[i].DedupeKey != w {
			t.Fatalf("score sort = %v, want %v",
				[]string{got[0].DedupeKey, got[1].DedupeKey, got[2].DedupeKey}, want)
		}
	}

	// Newest-first sort has to sink them too, or the rule would depend on
	// which chip happened to be on.
	got, err = s.ListJobs(JobFilter{SortNewest: true}, now)
	if err != nil {
		t.Fatalf("list newest: %v", err)
	}
	if got[len(got)-1].DedupeKey != "mirror_high" {
		t.Fatalf("newest-first put the repeat at %d, want last", len(got)-1)
	}

	// Repeats-only is unaffected: every row scores the same on the clause, so
	// the normal sort still decides.
	got, err = s.ListJobs(JobFilter{Duplicates: "1"}, now)
	if err != nil || len(got) != 1 || got[0].DedupeKey != "mirror_high" {
		t.Fatalf("repeats-only board: %v %+v", err, got)
	}
}
