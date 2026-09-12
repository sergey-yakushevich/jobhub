package store

import (
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

func jobByKey(t *testing.T, s *Store, key string) Job {
	t.Helper()
	jobs, err := s.ListJobs(JobFilter{}, time.Now())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, j := range jobs {
		if j.DedupeKey == key {
			return j
		}
	}
	t.Fatalf("job %q not found", key)
	return Job{}
}

// A re-sweep must never undo outreach. The sweep pushes the same lead with no
// applied field at all, which is the common case, and the flag has to survive.
func TestAppliedSurvivesResweepAndIsSticky(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if j := jobByKey(t, s, "t3_abc"); j.Applied() {
		t.Fatal("a fresh lead must start as not applied")
	}

	applied := t0.Add(time.Hour)
	if err := s.SetJobAppliedByKey("t3_abc", true, applied); err != nil {
		t.Fatalf("set applied: %v", err)
	}
	if j := jobByKey(t, s, "t3_abc"); !j.Applied() || !j.AppliedAt.Equal(applied) {
		t.Fatalf("after set: applied=%v at=%v", j.Applied(), j.AppliedAt)
	}

	// Re-sweep with no opinion on applied.
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_abc", Network: "reddit", Score: 9, ScoreReason: "seed", Body: "reposted"},
	}, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("resweep: %v", err)
	}
	j := jobByKey(t, s, "t3_abc")
	if !j.Applied() || !j.AppliedAt.Equal(applied) {
		t.Fatalf("re-sweep changed applied state: %v at=%v", j.Applied(), j.AppliedAt)
	}

	// Marking an already-applied lead again keeps the original date, so
	// "applied 3 days ago" does not silently reset to today.
	if err := s.SetJobAppliedByKey("t3_abc", true, t0.Add(72*time.Hour)); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if j := jobByKey(t, s, "t3_abc"); !j.AppliedAt.Equal(applied) {
		t.Fatalf("re-applying moved the date to %v", j.AppliedAt)
	}
}

// The tri-state pointer is the only field that can express "make this false",
// which is what an undo needs.
func TestAppliedTriStateOnPush(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "tw_1", Network: "x", Applied: boolPtr(true)},
	}, t0); err != nil {
		t.Fatalf("apply push: %v", err)
	}
	if j := jobByKey(t, s, "tw_1"); !j.Applied() {
		t.Fatal("applied:true push did not stamp the lead")
	}
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "tw_1", Network: "x", Applied: boolPtr(false)},
	}, t0); err != nil {
		t.Fatalf("clear push: %v", err)
	}
	if j := jobByKey(t, s, "tw_1"); j.Applied() {
		t.Fatal("applied:false push did not clear the lead")
	}

	// A brand new lead can arrive already applied — an agent that applies and
	// records in one step.
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_fresh", Network: "reddit", Applied: boolPtr(true)},
	}, t0); err != nil {
		t.Fatalf("insert applied: %v", err)
	}
	if j := jobByKey(t, s, "t3_fresh"); !j.Applied() {
		t.Fatal("new lead pushed with applied:true is not applied")
	}
}

func TestJobFilterApplied(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.SetJobAppliedByKey("li_1", true, t0); err != nil {
		t.Fatalf("set: %v", err)
	}
	for _, tc := range []struct {
		filter string
		want   int
	}{{"", 3}, {"1", 1}, {"0", 2}} {
		jobs, err := s.ListJobs(JobFilter{Applied: tc.filter}, t0)
		if err != nil {
			t.Fatalf("list %q: %v", tc.filter, err)
		}
		if len(jobs) != tc.want {
			t.Fatalf("applied=%q: got %d leads, want %d", tc.filter, len(jobs), tc.want)
		}
	}
	st, err := s.JobStats(JobFilter{}, t0)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Applied != 1 {
		t.Fatalf("stats applied = %d, want 1", st.Applied)
	}
}

func TestSetJobAppliedUnknownTarget(t *testing.T) {
	s := testStore(t)
	if err := s.SetJobApplied(4242, true, time.Now()); err == nil {
		t.Fatal("expected an error for an unknown id")
	}
	if err := s.SetJobAppliedByKey("nope", true, time.Now()); err == nil {
		t.Fatal("expected an error for an unknown dedupe_key")
	}
}
