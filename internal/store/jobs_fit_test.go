package store

import (
	"testing"
	"time"
)

func fitLead(t *testing.T, s *Store, key string) int64 {
	t.Helper()
	res, err := s.UpsertJobs([]JobParams{{DedupeKey: key, Network: "reddit", Title: key}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AddedIDs) != 1 {
		t.Fatalf("AddedIDs = %v", res.AddedIDs)
	}
	return res.AddedIDs[0]
}

func TestSetJobFit(t *testing.T) {
	s := testStore(t)
	id := fitLead(t, s, "fit-1")

	if err := s.SetJobFit(id, 7.5, "yes", `{"why":"test"}`, time.Now()); err != nil {
		t.Fatal(err)
	}
	j, err := s.JobByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Fit != 7.5 || j.Workable != "yes" || j.FitDetail != `{"why":"test"}` || j.FitAt.IsZero() {
		t.Fatalf("fit=%v workable=%q detail=%q at=%v", j.Fit, j.Workable, j.FitDetail, j.FitAt)
	}

	if err := s.SetJobFit(id, 1, "maybe", "", time.Now()); err == nil {
		t.Fatal("accepted a bad workable value")
	}
	if err := s.SetJobFit(9999, 1, "yes", "", time.Now()); err == nil {
		t.Fatal("accepted a missing id")
	}
}

// TestUpsertAddedIDsOnlyNewRows is the no-backfill contract: a re-push of an
// existing lead must not put it back in the scoring queue.
func TestUpsertAddedIDsOnlyNewRows(t *testing.T) {
	s := testStore(t)
	id := fitLead(t, s, "fit-2")

	res, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "fit-2", Network: "reddit", Title: "refreshed"},
		{DedupeKey: "fit-3", Network: "reddit", Title: "brand new"},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || res.Updated != 1 {
		t.Fatalf("added=%d updated=%d", res.Added, res.Updated)
	}
	if len(res.AddedIDs) != 1 || res.AddedIDs[0] == id {
		t.Fatalf("AddedIDs = %v (existing id %d)", res.AddedIDs, id)
	}
}

func TestListJobsWorkableFilterAndFitSort(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	blocked := fitLead(t, s, "l-blocked")
	yesLow := fitLead(t, s, "l-yes-low")
	yesHigh := fitLead(t, s, "l-yes-high")
	unknown := fitLead(t, s, "l-unknown")
	unscored := fitLead(t, s, "l-unscored")

	for _, w := range []struct {
		id       int64
		fit      float64
		workable string
	}{
		{blocked, 9.9, "blocked"}, // great fit, but a knockout fired
		{yesLow, 5.1, "yes"},
		{yesHigh, 8.2, "yes"},
		{unknown, 7.0, "unknown"},
	} {
		if err := s.SetJobFit(w.id, w.fit, w.workable, "{}", now); err != nil {
			t.Fatal(err)
		}
	}

	jobs, err := s.ListJobs(JobFilter{Workable: "yes"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("workable=yes returned %d rows", len(jobs))
	}

	jobs, err = s.ListJobs(JobFilter{Workable: "scored"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 4 {
		t.Fatalf("workable=scored returned %d rows", len(jobs))
	}

	// The fit sort: yes first (best fit first), then unknown and unscored
	// together, blocked last even with the best fit on the board.
	jobs, err = s.ListJobs(JobFilter{SortFit: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]int64, len(jobs))
	for i, j := range jobs {
		got[i] = j.ID
	}
	want := []int64{yesHigh, yesLow, unknown, unscored, blocked}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fit sort order = %v, want %v", got, want)
		}
	}
}
