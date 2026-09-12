package store

import (
	"testing"
	"time"
)

func jobBatch() []JobParams {
	return []JobParams{
		{DedupeKey: "t3_abc", Network: "reddit", JobType: "contract", Score: 12.5, ScoreReason: "seed",
			Author: "founder1", Title: "[Hiring] Senior Go dev", Body: "remote contract",
			URL: "https://reddit.com/r/forhire/abc", Subreddit: "forhire"},
		{DedupeKey: "tw_1", Network: "x", JobType: "job", Score: 9, ScoreReason: "seed",
			Author: "hotfixjobs", Body: "GitLab is hiring", URL: "https://x.com/i/status/1"},
		{DedupeKey: "li_1", Network: "linkedin", JobType: "job", Score: 13, ScoreReason: "seed",
			Author: "recruiter", Body: "Ruby on Rails, C2C", URL: "https://linkedin.com/in/r",
			Emails: "r@corp.com", Draft: "hi, 10y of Rails"},
	}
}

func TestUpsertJobsInsertThenUpdate(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	res, err := s.UpsertJobs(jobBatch(), t0)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if res.Added != 3 || res.Updated != 0 {
		t.Fatalf("first batch: %+v", res)
	}

	// Second sweep re-finds one job with a better score and no draft, and one
	// new job. The stored draft must survive the empty incoming one.
	t1 := t0.Add(24 * time.Hour)
	res, err = s.UpsertJobs([]JobParams{
		{DedupeKey: "li_1", Network: "linkedin", JobType: "contract", Score: 14, ScoreReason: "seed", Body: "updated"},
		{DedupeKey: "t3_new", Network: "reddit", Score: 5, ScoreReason: "seed"},
	}, t1)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if res.Added != 1 || res.Updated != 1 {
		t.Fatalf("second batch: %+v", res)
	}
	jobs, err := s.ListJobs(JobFilter{Network: "linkedin"}, t1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list linkedin: %v %d", err, len(jobs))
	}
	j := jobs[0]
	if j.Score != 14 || j.JobType != "contract" || j.Draft != "hi, 10y of Rails" {
		t.Fatalf("update lost fields: %+v", j)
	}
	if !j.CreatedAt.Equal(t0) {
		t.Fatalf("created_at moved on update: %v", j.CreatedAt)
	}

	// Attaching a draft alone must not blank the rest of the row.
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_abc", Network: "reddit", Draft: "hi, saw your post"},
	}, t1); err != nil {
		t.Fatalf("draft attach: %v", err)
	}
	jobs, _ = s.ListJobs(JobFilter{Network: "reddit"}, t1)
	j = jobs[0]
	if j.Draft != "hi, saw your post" || j.Score != 12.5 || j.Author != "founder1" || j.Body == "" {
		t.Fatalf("partial push clobbered the row: %+v", j)
	}
}

func TestUpsertJobsDedupesByURL(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Same listing, DIFFERENT dedupe_key, same URL but with a trailing slash and
	// a #fragment — must update the existing row, not add a second.
	res, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_repost", Network: "reddit", Score: 15, ScoreReason: "seed",
			URL: "https://reddit.com/r/forhire/abc/#comments"},
	}, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("repost: %v", err)
	}
	if res.Added != 0 || res.Updated != 1 {
		t.Fatalf("URL repost should update, not add: %+v", res)
	}
	all, _ := s.ListJobs(JobFilter{}, t0.Add(2*time.Hour))
	if len(all) != 3 {
		t.Fatalf("URL duplicate created a row: %d jobs", len(all))
	}
	// The matched row kept its original dedupe_key and took the new score.
	got, _ := s.ListJobs(JobFilter{Network: "reddit"}, t0.Add(2*time.Hour))
	if got[0].DedupeKey != "t3_abc" || got[0].Score != 15 {
		t.Fatalf("wrong row updated: %+v", got[0])
	}
	// Two brand-new jobs sharing one URL within a single batch collapse to one.
	res, err = s.UpsertJobs([]JobParams{
		{DedupeKey: "x_a", Network: "x", URL: "https://x.com/i/status/999"},
		{DedupeKey: "x_b", Network: "x", URL: "https://x.com/i/status/999"},
	}, t0.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("intra-batch: %v", err)
	}
	if res.Added != 1 || res.Updated != 1 {
		t.Fatalf("intra-batch URL dupe: %+v", res)
	}
}

func TestListJobsFiltersAndOrder(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := t0.Add(48 * time.Hour)

	all, err := s.ListJobs(JobFilter{}, now)
	if err != nil || len(all) != 3 {
		t.Fatalf("all: %v %d", err, len(all))
	}
	// Default order is score first.
	if all[0].DedupeKey != "li_1" || all[1].DedupeKey != "t3_abc" {
		t.Fatalf("order: %s %s", all[0].DedupeKey, all[1].DedupeKey)
	}
	if got, _ := s.ListJobs(JobFilter{JobType: "contract"}, now); len(got) != 1 {
		t.Fatalf("type filter: %d", len(got))
	}
	// Everything was ingested 48h ago; a 24h window must be empty.
	if got, _ := s.ListJobs(JobFilter{Since: 24 * time.Hour}, now); len(got) != 0 {
		t.Fatalf("since filter: %d", len(got))
	}
}

func TestMarkJobViewedIsSticky(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	jobs, _ := s.ListJobs(JobFilter{}, t0)
	id := jobs[0].ID

	if err := s.MarkJobViewed(id, t0.Add(time.Hour)); err != nil {
		t.Fatalf("mark: %v", err)
	}
	// A second look must not move the first-viewed timestamp.
	if err := s.MarkJobViewed(id, t0.Add(9*time.Hour)); err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	j, err := s.JobByID(id)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if !j.Viewed() || !j.ViewedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("viewed_at: %v", j.ViewedAt)
	}
	if unv, _ := s.ListJobs(JobFilter{UnviewedOnly: true}, t0); len(unv) != 2 {
		t.Fatalf("unviewed: %d", len(unv))
	}
	if err := s.MarkJobViewed(99999, t0); err == nil {
		t.Fatal("marking an unknown id must error")
	}
}

func TestJobStats(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := t0.Add(12 * time.Hour)
	jobs, _ := s.ListJobs(JobFilter{}, now)
	_ = s.MarkJobViewed(jobs[0].ID, now)

	st, err := s.JobStats(JobFilter{}, now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Total != 3 || st.Unviewed != 2 || st.Last24h != 3 {
		t.Fatalf("totals: %+v", st)
	}
	if len(st.Networks) != 3 {
		t.Fatalf("networks: %+v", st.Networks)
	}
	if len(st.Days) < 7 {
		t.Fatalf("chart should span at least a week, got %d", len(st.Days))
	}
	var charted int64
	for _, d := range st.Days {
		charted += d.Visits
	}
	if charted != 3 {
		t.Fatalf("chart total %d", charted)
	}
	// The filter narrows the stats the same way it narrows the list.
	st, err = s.JobStats(JobFilter{Network: "reddit"}, now)
	if err != nil || st.Total != 1 {
		t.Fatalf("filtered stats: %v %+v", err, st)
	}
}
