package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func trackerStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	if err := s.MigrateTracker(); err != nil {
		t.Fatalf("migrate tracker: %v", err)
	}
	return s
}

func trackerFloat(v float64) *float64 { return &v }

func mustCreateTracker(t *testing.T, s *Store, j TrackedJob, now time.Time) *TrackedJob {
	t.Helper()
	got, replay, err := s.CreateTrackedJob(j, now)
	if err != nil {
		t.Fatalf("create tracker %q: %v", j.SourceKey, err)
	}
	if replay {
		t.Fatalf("create %q was unexpectedly a replay", j.SourceKey)
	}
	return got
}

func TestTrackerCreateDefaultsReplayAndConflict(t *testing.T) {
	s := trackerStore(t)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	in := TrackedJob{
		SourceKey: "  src-1  ", Company: "Acme", Title: "Go dev",
		URL: "https://example.com/jobs/1", Description: "Build things",
		Strengths: []string{" Go ", ""}, Gaps: []string{"K8s"}, Skills: []string{"Go"},
	}
	got, replay, err := s.CreateTrackedJob(in, t0)
	if err != nil || replay {
		t.Fatalf("create: %v replay=%v", err, replay)
	}
	if got.ID == 0 || got.Profile != "sergey" || got.Stage != "saved" || got.Remote != "unknown" {
		t.Fatalf("defaults: %+v", got)
	}
	if got.Version != 1 || got.CreatedAt != "2026-11-06T09:00:00Z" || got.UpdatedAt != got.CreatedAt {
		t.Fatalf("server fields: %+v", got)
	}
	if got.LastCheckedAt != "" || got.SalaryMin != nil || got.SalaryMax != nil || got.FitScore != nil {
		t.Fatalf("nullable fields should start unset: %+v", got)
	}
	if len(got.Strengths) != 1 || got.Strengths[0] != "Go" {
		t.Fatalf("strengths not normalized: %+v", got.Strengths)
	}

	// An identical retry is a replay: same row, same version, no new write.
	again, replay, err := s.CreateTrackedJob(in, t0.Add(time.Hour))
	if err != nil || !replay {
		t.Fatalf("replay: %v replay=%v", err, replay)
	}
	if again.ID != got.ID || again.Version != 1 || again.CreatedAt != got.CreatedAt {
		t.Fatalf("replay changed the row: %+v", again)
	}

	changed := in
	changed.Title = "Senior Go dev"
	if _, _, err := s.CreateTrackedJob(changed, t0.Add(2*time.Hour)); !errors.Is(err, ErrTrackerConflict) {
		t.Fatalf("changed payload: %v", err)
	}

	// Nullable salary/score round trip, including a zero fit score.
	withNums := mustCreateTracker(t, s, TrackedJob{
		SourceKey: "src-nums", Company: "Acme", Title: "Nums", SalaryMin: trackerFloat(1000),
		SalaryMax: trackerFloat(2000), Currency: "usd", SalaryPeriod: "month",
		FitScore: trackerFloat(0), FitSummary: "entry level",
	}, t0)
	if withNums.SalaryMin == nil || *withNums.SalaryMin != 1000 || withNums.Currency != "USD" {
		t.Fatalf("salary: %+v", withNums)
	}
	if withNums.FitScore == nil || *withNums.FitScore != 0 || withNums.FitSummary != "entry level" {
		t.Fatalf("fit: %+v", withNums)
	}
}

func TestTrackerPerProfileSourceIsolation(t *testing.T) {
	s := trackerStore(t)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	a := mustCreateTracker(t, s, TrackedJob{Profile: "sergey", SourceKey: "same", Company: "A", Title: "Role"}, t0)
	b := mustCreateTracker(t, s, TrackedJob{Profile: "polina", SourceKey: "same", Company: "A", Title: "Role"}, t0)
	if a.ID == b.ID {
		t.Fatal("the same source_key on two profiles must be two rows")
	}
	if a.Profile != "sergey" || b.Profile != "polina" {
		t.Fatalf("profiles: %q %q", a.Profile, b.Profile)
	}
}

func TestTrackerValidation(t *testing.T) {
	s := trackerStore(t)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	base := func() TrackedJob {
		return TrackedJob{SourceKey: "v", Company: "Acme", Title: "Role", URL: "https://example.com/r"}
	}
	cases := []struct {
		name string
		mut  func(*TrackedJob)
	}{
		{"missing source", func(j *TrackedJob) { j.SourceKey = " " }},
		{"missing company", func(j *TrackedJob) { j.Company = "" }},
		{"missing title", func(j *TrackedJob) { j.Title = "" }},
		{"bad stage", func(j *TrackedJob) { j.Stage = "ghosted" }},
		{"bad remote", func(j *TrackedJob) { j.Remote = "mars" }},
		{"bad waiting", func(j *TrackedJob) { j.WaitingOn = "nobody" }},
		{"bad employment", func(j *TrackedJob) { j.Employment = "internship" }},
		{"bad period", func(j *TrackedJob) { j.SalaryPeriod = "week" }},
		{"bad currency", func(j *TrackedJob) { j.Currency = "US" }},
		{"score without summary", func(j *TrackedJob) { j.FitScore = trackerFloat(7) }},
		{"score out of range", func(j *TrackedJob) {
			j.FitScore = trackerFloat(11)
			j.FitSummary = "great"
		}},
		{"salary inverted", func(j *TrackedJob) {
			j.SalaryMin = trackerFloat(10)
			j.SalaryMax = trackerFloat(5)
		}},
		{"overlong title", func(j *TrackedJob) { j.Title = strings.Repeat("x", 301) }},
		{"javascript url", func(j *TrackedJob) { j.URL = "javascript://alert(1)" }},
		{"relative url", func(j *TrackedJob) { j.URL = "/jobs/1" }},
		{"bad next action time", func(j *TrackedJob) { j.NextActionAt = "tomorrow" }},
		{"too many strengths", func(j *TrackedJob) {
			for i := 0; i < 101; i++ {
				j.Strengths = append(j.Strengths, "x")
			}
		}},
	}
	for _, c := range cases {
		j := base()
		c.mut(&j)
		if err := ValidateTrackedJob(j); !errors.Is(err, ErrTrackerValidation) {
			t.Fatalf("%s: got %v, want ErrTrackerValidation", c.name, err)
		}
		if _, _, err := s.CreateTrackedJob(j, t0); !errors.Is(err, ErrTrackerValidation) {
			t.Fatalf("%s through create: %v", c.name, err)
		}
	}
}

func TestTrackerUpdateVersionStageEventAndImmutability(t *testing.T) {
	s := trackerStore(t)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	created := mustCreateTracker(t, s, TrackedJob{
		SourceKey: "upd", Company: "Acme", Title: "Go dev", Stage: "saved",
	}, t0)

	edit := *created
	edit.Title = "Senior Go dev"
	edit.Stage = "applied"
	edit.NeedsYou = true
	edit.SalaryMin = trackerFloat(100)
	edit.Description = "new description"
	updated, err := s.UpdateTrackedJob(created.ID, edit, created.Version, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Version != 2 || updated.UpdatedAt != "2026-11-06T10:00:00Z" || updated.CreatedAt != created.CreatedAt {
		t.Fatalf("version/times: %+v", updated)
	}
	if updated.Title != "Senior Go dev" || updated.Stage != "applied" || !updated.NeedsYou || updated.SalaryMin == nil {
		t.Fatalf("replacement lost fields: %+v", updated)
	}
	if updated.LastCheckedAt != "" {
		t.Fatalf("update must not move last_checked_at: %q", updated.LastCheckedAt)
	}

	page, err := s.TimelineEvents(created.ID, 10, 0)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("stage event: %v %+v", err, page)
	}
	ev := page.Items[0]
	if ev.Kind != "status" || ev.Actor != "system" || ev.Stage != "applied" || ev.Verification != "recorded" {
		t.Fatalf("stage event: %+v", ev)
	}
	if !strings.Contains(ev.Summary, "saved") || !strings.Contains(ev.Summary, "applied") {
		t.Fatalf("stage summary must be honest: %q", ev.Summary)
	}

	// A non-stage edit does not grow the timeline.
	edit2 := *updated
	edit2.Title = "Staff Go dev"
	updated, err = s.UpdateTrackedJob(created.ID, edit2, updated.Version, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if updated.Version != 3 {
		t.Fatalf("version = %d", updated.Version)
	}
	if page, _ = s.TimelineEvents(created.ID, 10, 0); len(page.Items) != 1 {
		t.Fatalf("non-stage update grew the timeline: %+v", page.Items)
	}

	// Stale write is refused, not merged.
	if _, err := s.UpdateTrackedJob(created.ID, edit2, 1, t0.Add(3*time.Hour)); !errors.Is(err, ErrTrackerVersion) {
		t.Fatalf("stale version: %v", err)
	}
	// Identity is immutable; an empty identity field means "keep it".
	badProfile := edit2
	badProfile.Profile = "polina"
	if _, err := s.UpdateTrackedJob(created.ID, badProfile, updated.Version, t0); !errors.Is(err, ErrTrackerValidation) {
		t.Fatalf("profile change: %v", err)
	}
	badKey := edit2
	badKey.SourceKey = "other"
	if _, err := s.UpdateTrackedJob(created.ID, badKey, updated.Version, t0); !errors.Is(err, ErrTrackerValidation) {
		t.Fatalf("source_key change: %v", err)
	}
	keep := edit2
	keep.Profile, keep.SourceKey = "", ""
	if _, err := s.UpdateTrackedJob(created.ID, keep, updated.Version, t0.Add(4*time.Hour)); err != nil {
		t.Fatalf("empty identity should keep existing: %v", err)
	}
}

func TestTrackerTimelineReplayCheckAndStageAtomic(t *testing.T) {
	s := trackerStore(t)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	job := mustCreateTracker(t, s, TrackedJob{SourceKey: "tl", Company: "Acme", Title: "Role"}, t0)

	e := TimelineEvent{
		SourceKey: "m1", Kind: "message", Summary: "Recruiter replied",
		Body: "Can you talk Tuesday?", Actor: "agent", Channel: "email",
		Direction: "inbound", Contact: "recruiter@acme.test", ThreadID: "t1",
	}
	got, replay, err := s.AddTimelineEvent(job.ID, e, t0.Add(time.Hour))
	if err != nil || replay {
		t.Fatalf("event: %v replay=%v", err, replay)
	}
	if got.HappenedAt != "2026-11-06T10:00:00Z" || got.RecordedAt != got.HappenedAt ||
		got.Verification != "recorded" {
		t.Fatalf("event defaults: %+v", got)
	}
	j, _ := s.TrackedJob(job.ID)
	if j.Version != 2 || j.UpdatedAt != "2026-11-06T10:00:00Z" {
		t.Fatalf("new event must bump version/updated: %+v", j)
	}
	if j.LastCheckedAt != "" {
		t.Fatalf("a message is not an inbox check: %q", j.LastCheckedAt)
	}

	// Blank happened_at means now, but an identical blank retry still matches.
	again, replay, err := s.AddTimelineEvent(job.ID, e, t0.Add(5*time.Hour))
	if err != nil || !replay || again.ID != got.ID {
		t.Fatalf("blank-time replay: %v replay=%v id=%d/%d", err, replay, again.ID, got.ID)
	}
	j, _ = s.TrackedJob(job.ID)
	if j.Version != 2 || j.UpdatedAt != "2026-11-06T10:00:00Z" {
		t.Fatalf("replay must not bump: %+v", j)
	}

	changed := e
	changed.Summary = "different"
	if _, _, err := s.AddTimelineEvent(job.ID, changed, t0.Add(6*time.Hour)); !errors.Is(err, ErrTrackerConflict) {
		t.Fatalf("changed event: %v", err)
	}

	// Only a check event moves last_checked_at.
	if _, _, err := s.AddTimelineEvent(job.ID, TimelineEvent{
		SourceKey: "c1", Kind: "check", Summary: "Inbox read, nothing new", Actor: "agent",
	}, t0.Add(7*time.Hour)); err != nil {
		t.Fatalf("check: %v", err)
	}
	j, _ = s.TrackedJob(job.ID)
	if j.LastCheckedAt != "2026-11-06T16:00:00Z" || j.Version != 3 {
		t.Fatalf("check: %+v", j)
	}
	if _, _, err := s.AddTimelineEvent(job.ID, TimelineEvent{
		SourceKey: "m2", Kind: "note", Summary: "Called them", Actor: "user",
	}, t0.Add(8*time.Hour)); err != nil {
		t.Fatalf("note: %v", err)
	}
	j, _ = s.TrackedJob(job.ID)
	if j.LastCheckedAt != "2026-11-06T16:00:00Z" || j.Version != 4 {
		t.Fatalf("note moved last check: %+v", j)
	}

	// An explicit stage is applied in the same transaction; kind is not
	// interpreted as a status.
	if _, _, err := s.AddTimelineEvent(job.ID, TimelineEvent{
		SourceKey: "iv1", Kind: "note", Summary: "Booked", Actor: "user", Stage: "interview",
	}, t0.Add(9*time.Hour)); err != nil {
		t.Fatalf("stage event: %v", err)
	}
	j, _ = s.TrackedJob(job.ID)
	if j.Stage != "interview" || j.Version != 5 {
		t.Fatalf("stage not applied atomically: %+v", j)
	}

	badEvents := []TimelineEvent{
		{SourceKey: "", Kind: "note", Summary: "x", Actor: "user"},
		{SourceKey: "b1", Kind: "note", Summary: "x", Actor: ""},
		{SourceKey: "b2", Kind: "telepathy", Summary: "x", Actor: "user"},
		{SourceKey: "b3", Kind: "note", Summary: "x", Actor: "user", Channel: "carrier-pigeon"},
		{SourceKey: "b4", Kind: "note", Summary: "x", Actor: "user", Direction: "sideways"},
		{SourceKey: "b5", Kind: "note", Summary: "x", Actor: "user", Verification: "proven"},
		{SourceKey: "b6", Kind: "note", Summary: "x", Actor: "user", SourceURL: "javascript://x"},
	}
	for i, bad := range badEvents {
		if _, _, err := s.AddTimelineEvent(job.ID, bad, t0); !errors.Is(err, ErrTrackerValidation) {
			t.Fatalf("bad event %d: %v", i, err)
		}
	}
	if _, _, err := s.AddTimelineEvent(99999, e, t0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing job: %v", err)
	}
}

func TestTrackerTimelinePagination(t *testing.T) {
	s := trackerStore(t)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	job := mustCreateTracker(t, s, TrackedJob{SourceKey: "page", Company: "Acme", Title: "Role"}, t0)
	for i := 1; i <= 5; i++ {
		if _, _, err := s.AddTimelineEvent(job.ID, TimelineEvent{
			SourceKey: "e" + string(rune('0'+i)), Kind: "note", Summary: "event", Actor: "user",
		}, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	p1, err := s.TimelineEvents(job.ID, 2, 0)
	if err != nil || len(p1.Items) != 2 || p1.NextCursor == "" {
		t.Fatalf("page1: %v %+v", err, p1)
	}
	if p1.Items[0].ID <= p1.Items[1].ID {
		t.Fatalf("not id desc: %+v", p1.Items)
	}
	before, _ := strconvParse(p1.NextCursor)
	p2, err := s.TimelineEvents(job.ID, 2, before)
	if err != nil || len(p2.Items) != 2 || p2.NextCursor == "" {
		t.Fatalf("page2: %v %+v", err, p2)
	}
	before, _ = strconvParse(p2.NextCursor)
	p3, err := s.TimelineEvents(job.ID, 2, before)
	if err != nil || len(p3.Items) != 1 || p3.NextCursor != "" {
		t.Fatalf("page3: %v %+v", err, p3)
	}
	if _, err := s.TimelineEvents(99999, 10, 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing job timeline: %v", err)
	}
}

func strconvParse(s string) (int64, error) {
	var n int64
	for _, r := range s {
		n = n*10 + int64(r-'0')
	}
	return n, nil
}

func TestTrackerListFiltersAndPaging(t *testing.T) {
	s := trackerStore(t)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	jobs := []TrackedJob{
		{Profile: "sergey", SourceKey: "l1", Company: "Acme", Title: "Go dev", Description: "Kubernetes", Stage: "saved", Remote: "remote"},
		{Profile: "sergey", SourceKey: "l2", Company: "Beta", Title: "Rails dev", Description: "Postgres", Stage: "applied", Remote: "hybrid", NeedsYou: true},
		{Profile: "polina", SourceKey: "l3", Company: "Gamma", Title: "Video editor", Description: "ADS and UGC", Stage: "saved", Remote: "onsite"},
		{Profile: "sergey", SourceKey: "l4", Company: "Delta", Title: "Platform", Description: "GO services", Stage: "interview", Remote: "remote", NeedsYou: true},
		{Profile: "sergey", SourceKey: "l5", Company: "Epsilon", Title: "Backend", Description: "SQL", Stage: "saved"},
	}
	for _, j := range jobs {
		mustCreateTracker(t, s, j, t0)
	}

	all, err := s.ListTrackedJobs(TrackerFilter{})
	if err != nil || len(all.Items) != 5 {
		t.Fatalf("all: %v %+v", err, all)
	}
	for i := 1; i < len(all.Items); i++ {
		if all.Items[i-1].ID <= all.Items[i].ID {
			t.Fatalf("list not id desc: %+v", all.Items)
		}
	}
	polina, _ := s.ListTrackedJobs(TrackerFilter{Profile: "Polina"})
	if len(polina.Items) != 1 || polina.Items[0].SourceKey != "l3" {
		t.Fatalf("profile filter: %+v", polina.Items)
	}
	applied, _ := s.ListTrackedJobs(TrackerFilter{Stage: "applied"})
	if len(applied.Items) != 1 || applied.Items[0].SourceKey != "l2" {
		t.Fatalf("stage filter: %+v", applied.Items)
	}
	remote, _ := s.ListTrackedJobs(TrackerFilter{Remote: "remote"})
	if len(remote.Items) != 2 {
		t.Fatalf("remote filter: %+v", remote.Items)
	}
	needs, _ := s.ListTrackedJobs(TrackerFilter{NeedsYou: true})
	if len(needs.Items) != 2 {
		t.Fatalf("needs-you filter: %+v", needs.Items)
	}
	search, _ := s.ListTrackedJobs(TrackerFilter{Query: "go DEV"})
	if len(search.Items) != 1 || search.Items[0].SourceKey != "l1" {
		t.Fatalf("case-insensitive title search: %+v", search.Items)
	}
	search, _ = s.ListTrackedJobs(TrackerFilter{Query: "postgres"})
	if len(search.Items) != 1 || search.Items[0].SourceKey != "l2" {
		t.Fatalf("description search: %+v", search.Items)
	}
	search, _ = s.ListTrackedJobs(TrackerFilter{Query: "gamma"})
	if len(search.Items) != 1 || search.Items[0].SourceKey != "l3" {
		t.Fatalf("company search: %+v", search.Items)
	}

	p1, err := s.ListTrackedJobs(TrackerFilter{Limit: 2})
	if err != nil || len(p1.Items) != 2 || p1.NextCursor == "" {
		t.Fatalf("page1: %v %+v", err, p1)
	}
	before, _ := strconvParse(p1.NextCursor)
	p2, err := s.ListTrackedJobs(TrackerFilter{Limit: 2, BeforeID: before})
	if err != nil || len(p2.Items) != 2 {
		t.Fatalf("page2: %v %+v", err, p2)
	}
	before, _ = strconvParse(p2.NextCursor)
	p3, err := s.ListTrackedJobs(TrackerFilter{Limit: 2, BeforeID: before})
	if err != nil || len(p3.Items) != 1 || p3.NextCursor != "" {
		t.Fatalf("page3: %v %+v", err, p3)
	}
	// Limit is capped at 100 rather than trusted.
	capped, err := s.ListTrackedJobs(TrackerFilter{Limit: 5000})
	if err != nil || len(capped.Items) != 5 {
		t.Fatalf("cap: %v %d", err, len(capped.Items))
	}
}

func TestTrackerMigrationBlankAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blank.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.MigrateTracker(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	assertCount(t, s, "tracker_jobs", 0)
	assertCount(t, s, "tracker_events", 0)
	assertCount(t, s, "tracker_meta", 1)
	if err := s.MigrateTracker(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	assertCount(t, s, "tracker_meta", 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if err := s.MigrateTracker(); err != nil {
		t.Fatalf("migrate after reopen: %v", err)
	}
	assertCount(t, s, "tracker_jobs", 0)
	assertCount(t, s, "tracker_meta", 1)
}

func assertCount(t *testing.T, s *Store, table string, want int) {
	t.Helper()
	var got int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}

func TestTrackerMigrationBackfillsLegacyBoardOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t0 := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	// A legacy lead with no author or title: the import must use honest
	// fallbacks rather than inventing a company.
	if _, err := s.UpsertJobs([]JobParams{{
		DedupeKey: "legacy_blank", Network: "reddit", Body: "First line role\nmore detail",
	}}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetJobAppliedByKey("tw_1", true, t0); err != nil {
		t.Fatal(err)
	}
	li := jobByKey(t, s, "li_1")
	if err := s.SetJobAppStatus(li.ID, AppHired, "", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	blank := jobByKey(t, s, "legacy_blank")
	if err := s.SetJobRejected(blank.ID, true, "not a fit", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Prep is an opaque artifact and must never leak into the new description.
	abc := jobByKey(t, s, "t3_abc")
	if err := s.SetJobPrep(abc.ID, `{"secret_prep":"do not expose"}`); err != nil {
		t.Fatal(err)
	}
	ev1, err := s.AddJobEvent(abc.ID, "email", "HR wants a call", t0.Add(2*time.Hour), t0.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := s.AddJobEvent(abc.ID, "status", "moved to screen", t0.Add(3*time.Hour), t0.Add(49*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	beforeJobs, beforeEvents := countRows(t, s, "jobs"), countRows(t, s, "job_events")
	var milestones int
	if err := s.db.QueryRow(`SELECT SUM((applied_at <> '') + (rejected_at <> '')) FROM jobs`).Scan(&milestones); err != nil {
		t.Fatal(err)
	}
	// Open now runs all migrations. Remove the empty-database import marker to
	// simulate opening a pre-tracker database containing these legacy fixtures.
	if _, err := s.db.Exec(`DELETE FROM tracker_meta WHERE key = 'legacy_backfill_v1'`); err != nil {
		t.Fatal(err)
	}

	if err := s.MigrateTracker(); err != nil {
		t.Fatalf("migrate tracker: %v", err)
	}
	if got := countRows(t, s, "tracker_jobs"); got != beforeJobs {
		t.Fatalf("tracker_jobs = %d, want %d", got, beforeJobs)
	}
	if got := countRows(t, s, "tracker_events"); got != beforeEvents+milestones {
		t.Fatalf("tracker_events = %d, want %d", got, beforeEvents+milestones)
	}
	// Legacy tables are untouched.
	if countRows(t, s, "jobs") != beforeJobs || countRows(t, s, "job_events") != beforeEvents {
		t.Fatal("migration modified the legacy tables")
	}

	abcT, err := s.TrackedJob(abc.ID)
	if err != nil {
		t.Fatalf("read migrated t3_abc: %v", err)
	}
	if abcT.SourceKey != "t3_abc" || abcT.Company != "founder1" || abcT.Stage != "saved" {
		t.Fatalf("migrated identity/stage: %+v", abcT)
	}
	if abcT.Description != "remote contract" || strings.Contains(abcT.Description, "secret_prep") {
		t.Fatalf("description should be legacy body only: %q", abcT.Description)
	}
	if abcT.FitScore != nil || abcT.FitSummary != "seed" {
		t.Fatalf("legacy score 12.5 must not be fabricated into a 0..10 fit: %+v", abcT)
	}

	tw, _ := s.TrackedJob(jobByKey(t, s, "tw_1").ID)
	if tw.Stage != "applied" {
		t.Fatalf("applied mapping: %q", tw.Stage)
	}
	applications, err := s.TimelineEvents(tw.ID, 10, 0)
	if err != nil || len(applications.Items) != 1 || applications.Items[0].Kind != "application" || applications.Items[0].HappenedAt != fmtTime(t0) {
		t.Fatal("legacy application date lost", err, applications)
	}
	if tw.FitScore == nil || *tw.FitScore != 9 || tw.FitSummary != "seed" {
		t.Fatalf("legacy score 9 with reason should survive: %+v", tw)
	}
	hired, _ := s.TrackedJob(li.ID)
	if hired.Stage != "hired" {
		t.Fatalf("hired mapping: %q", hired.Stage)
	}
	rejected, _ := s.TrackedJob(blank.ID)
	if rejected.Stage != "rejected" || rejected.Company != "Unknown company" || rejected.Title != "First line role" {
		t.Fatalf("rejected/fallback mapping: %+v", rejected)
	}
	rejections, err := s.TimelineEvents(rejected.ID, 10, 0)
	if err != nil || len(rejections.Items) != 1 || rejections.Items[0].Stage != "rejected" || rejections.Items[0].HappenedAt != fmtTime(t0.Add(time.Hour)) {
		t.Fatal("legacy rejection date lost", err, rejections)
	}

	// Histories keep their ids, order and meaning.
	page, err := s.TimelineEvents(abc.ID, 10, 0)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("migrated events: %v %+v", err, page)
	}
	if page.Items[0].ID != ev2.ID || page.Items[1].ID != ev1.ID {
		t.Fatalf("ids not preserved: %+v", page.Items)
	}
	if page.Items[1].Kind != "message" || page.Items[0].Kind != "status" {
		t.Fatalf("kind mapping: %+v", page.Items)
	}
	if page.Items[0].Actor != "legacy" || page.Items[0].Verification != "recorded" {
		t.Fatalf("event provenance: %+v", page.Items[0])
	}
	if abcT.LastCheckedAt == "" {
		t.Fatal("legacy checked_at should carry over")
	}

	// Running the migration again is a no-op.
	if err := s.MigrateTracker(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if countRows(t, s, "tracker_jobs") != beforeJobs || countRows(t, s, "tracker_events") != beforeEvents+milestones {
		t.Fatal("second migration duplicated rows")
	}

	// New tracker writes are not replayed by a later boot.
	created := mustCreateTracker(t, s, TrackedJob{
		SourceKey: "after-import", Company: "New", Title: "New role",
	}, t0.Add(72*time.Hour))
	if err := s.MigrateTracker(); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s, "tracker_jobs"); got != beforeJobs+1 {
		t.Fatalf("tracker_jobs after new write = %d, want %d", got, beforeJobs+1)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if err := s.MigrateTracker(); err != nil {
		t.Fatalf("migrate after reopen: %v", err)
	}
	if got := countRows(t, s, "tracker_jobs"); got != beforeJobs+1 {
		t.Fatalf("reopen replayed the import: %d rows", got)
	}
	if _, err := s.TrackedJob(created.ID); err != nil {
		t.Fatalf("new tracker row lost: %v", err)
	}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
