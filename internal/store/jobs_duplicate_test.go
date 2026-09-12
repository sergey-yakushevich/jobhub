package store

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func idOf(t *testing.T, s *Store, key string) int64 {
	t.Helper()
	return jobByKey(t, s, key).ID
}

func TestLinkDuplicateBasics(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	canon, dup := idOf(t, s, "t3_abc"), idOf(t, s, "tw_1")

	if err := s.LinkDuplicate(dup, canon); err != nil {
		t.Fatalf("link: %v", err)
	}
	if j := jobByKey(t, s, "tw_1"); j.DuplicateOf != canon {
		t.Fatalf("duplicate_of = %d, want %d", j.DuplicateOf, canon)
	}
	if j := jobByKey(t, s, "t3_abc"); j.IsDuplicate() {
		t.Fatal("the canonical row must stay canonical")
	}

	reps, err := s.DuplicatesOf(canon)
	if err != nil || len(reps) != 1 || reps[0].ID != dup {
		t.Fatalf("DuplicatesOf: %v / %+v", err, reps)
	}

	if err := s.UnlinkDuplicate(dup); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if j := jobByKey(t, s, "tw_1"); j.IsDuplicate() {
		t.Fatal("unlink left the row a duplicate")
	}
}

// Pointing at a row that is already a mirror must collapse to ITS canonical,
// so a read is always one hop.
func TestLinkDuplicateCollapsesChains(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a, b, c := idOf(t, s, "t3_abc"), idOf(t, s, "tw_1"), idOf(t, s, "li_1")

	if err := s.LinkDuplicate(b, a); err != nil { // b -> a
		t.Fatalf("link b: %v", err)
	}
	if err := s.LinkDuplicate(c, b); err != nil { // c -> b, must collapse to a
		t.Fatalf("link c: %v", err)
	}
	if j := jobByKey(t, s, "li_1"); j.DuplicateOf != a {
		t.Fatalf("chain not collapsed: c points at %d, want %d", j.DuplicateOf, a)
	}
}

// Making a row that others already point at into a duplicate must drag its
// followers along, otherwise the chain grows a second hop.
func TestLinkDuplicateRepointsFollowers(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_d", Network: "reddit", Score: 5, ScoreReason: "seed"},
	}, t0); err != nil {
		t.Fatalf("seed d: %v", err)
	}
	a, b, c := idOf(t, s, "t3_abc"), idOf(t, s, "tw_1"), idOf(t, s, "li_1")

	if err := s.LinkDuplicate(c, b); err != nil { // c -> b
		t.Fatalf("link c: %v", err)
	}
	if err := s.LinkDuplicate(b, a); err != nil { // b -> a; c must follow
		t.Fatalf("link b: %v", err)
	}
	if j := jobByKey(t, s, "li_1"); j.DuplicateOf != a {
		t.Fatalf("follower not repointed: c -> %d, want %d", j.DuplicateOf, a)
	}
	if j := jobByKey(t, s, "tw_1"); j.DuplicateOf != a {
		t.Fatalf("b -> %d, want %d", j.DuplicateOf, a)
	}
}

func TestLinkDuplicateRejectsCyclesAndUnknowns(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a, b := idOf(t, s, "t3_abc"), idOf(t, s, "tw_1")

	if err := s.LinkDuplicate(a, a); !errors.Is(err, ErrDuplicateCycle) {
		t.Fatalf("self-link: got %v, want ErrDuplicateCycle", err)
	}
	if err := s.LinkDuplicate(b, a); err != nil {
		t.Fatalf("link: %v", err)
	}
	// a -> b would collapse to a -> a, which is the cycle.
	if err := s.LinkDuplicate(a, b); !errors.Is(err, ErrDuplicateCycle) {
		t.Fatalf("cycle: got %v, want ErrDuplicateCycle", err)
	}
	if err := s.LinkDuplicate(b, 99999); err == nil {
		t.Fatal("expected an error for an unknown canonical id")
	}
}

func TestDuplicateFilterAndStats(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.LinkDuplicate(idOf(t, s, "tw_1"), idOf(t, s, "t3_abc")); err != nil {
		t.Fatalf("link: %v", err)
	}
	for _, tc := range []struct {
		filter string
		want   int
	}{{"", 3}, {"1", 1}, {"0", 2}} {
		jobs, err := s.ListJobs(JobFilter{Duplicates: tc.filter}, t0)
		if err != nil {
			t.Fatalf("list %q: %v", tc.filter, err)
		}
		if len(jobs) != tc.want {
			t.Fatalf("dups=%q: got %d, want %d", tc.filter, len(jobs), tc.want)
		}
	}
	st, err := s.JobStats(JobFilter{}, t0)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Duplicates != 1 || st.Total != 3 {
		t.Fatalf("stats: total %d, duplicates %d", st.Total, st.Duplicates)
	}
}

// The ref resolver must prefer a real row id but fall back to dedupe_key, which
// is what makes numeric tweet-id keys addressable.
func TestResolveJobRef(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "2095405525227704692", Network: "x", Score: 8, ScoreReason: "seed"},
	}, t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	j := jobByKey(t, s, "2095405525227704692")

	if got, err := s.ResolveJobRef("2095405525227704692"); err != nil || got != j.ID {
		t.Fatalf("by numeric key: %d, %v (want %d)", got, err, j.ID)
	}
	if got, err := s.ResolveJobRef(itoa64(j.ID)); err != nil || got != j.ID {
		t.Fatalf("by row id: %d, %v", got, err)
	}
	if _, err := s.ResolveJobRef("nope"); err == nil {
		t.Fatal("expected an error for an unknown ref")
	}
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
