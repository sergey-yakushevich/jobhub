package store

import (
	"errors"
	"testing"
	"time"
)

// A rejection with no stated cause is useless a week later, so the store
// refuses it on every path in rather than trusting the handler to check.
func TestRejectRequiresReason(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := s.SetJobAppliedByKey("t3_abc", true, t0); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if err := s.SetJobRejectedByKey("t3_abc", true, "", t0); !errors.Is(err, ErrRejectReasonRequired) {
		t.Fatalf("empty reason: got %v, want ErrRejectReasonRequired", err)
	}
	if err := s.SetJobRejectedByKey("t3_abc", true, "   ", t0); !errors.Is(err, ErrRejectReasonRequired) {
		t.Fatalf("whitespace reason: got %v, want ErrRejectReasonRequired", err)
	}
	if j := jobByKey(t, s, "t3_abc"); j.Rejected() {
		t.Fatal("a refused rejection still marked the row")
	}

	// Same rule through the ingest path.
	_, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_abc", Network: "reddit", Rejected: boolPtr(true)},
	}, t0)
	if !errors.Is(err, ErrRejectReasonRequired) {
		t.Fatalf("ingest without reason: got %v", err)
	}
}

func TestRejectStoresReasonAndIsReversible(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const why = "US-only employment, no US work authorization"
	if err := s.SetJobRejectedByKey("t3_abc", true, why, t0); err != nil {
		t.Fatalf("reject: %v", err)
	}
	j := jobByKey(t, s, "t3_abc")
	if !j.Rejected() || j.RejectReason != why || !j.RejectedAt.Equal(t0) {
		t.Fatalf("after reject: %v / %q / %v", j.Rejected(), j.RejectReason, j.RejectedAt)
	}

	// Sticky date, refreshed wording — a second look often sharpens the reason.
	if err := s.SetJobRejectedByKey("t3_abc", true, "sharper wording", t0.Add(time.Hour)); err != nil {
		t.Fatalf("re-reject: %v", err)
	}
	j = jobByKey(t, s, "t3_abc")
	if !j.RejectedAt.Equal(t0) {
		t.Fatalf("re-rejecting moved the date to %v", j.RejectedAt)
	}
	if j.RejectReason != "sharper wording" {
		t.Fatalf("reason not refreshed: %q", j.RejectReason)
	}

	// Un-rejecting must clear the reason too, or the row shows a stale "why"
	// for a rejection that no longer stands.
	if err := s.SetJobRejectedByKey("t3_abc", false, "", t0); err != nil {
		t.Fatalf("un-reject: %v", err)
	}
	j = jobByKey(t, s, "t3_abc")
	if j.Rejected() || j.RejectReason != "" {
		t.Fatalf("un-reject left %v / %q", j.Rejected(), j.RejectReason)
	}
}

// A re-sweep pushes leads with no opinion on rejection; a lead already ruled
// out must stay ruled out, reason intact.
func TestRejectSurvivesResweep(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.SetJobRejectedByKey("li_1", true, "UK-only", t0); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "li_1", Network: "linkedin", Score: 12, ScoreReason: "seed", Body: "reposted"},
	}, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("resweep: %v", err)
	}
	j := jobByKey(t, s, "li_1")
	if !j.Rejected() || j.RejectReason != "UK-only" {
		t.Fatalf("re-sweep cleared the rejection: %v / %q", j.Rejected(), j.RejectReason)
	}
}

func TestJobFilterAndStatsRejected(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.SetJobRejectedByKey("tw_1", true, "onsite only", t0); err != nil {
		t.Fatalf("reject: %v", err)
	}
	for _, tc := range []struct {
		filter string
		want   int
	}{{"", 3}, {"1", 1}, {"0", 2}} {
		jobs, err := s.ListJobs(JobFilter{Rejected: tc.filter}, t0)
		if err != nil {
			t.Fatalf("list %q: %v", tc.filter, err)
		}
		if len(jobs) != tc.want {
			t.Fatalf("rejected=%q: got %d, want %d", tc.filter, len(jobs), tc.want)
		}
	}
	st, err := s.JobStats(JobFilter{}, t0)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Rejected != 1 {
		t.Fatalf("stats rejected = %d, want 1", st.Rejected)
	}
}

// A brand new lead can arrive already ruled out, and it still needs a reason.
func TestRejectOnInsert(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_new", Network: "reddit", Rejected: boolPtr(true), RejectReason: "onsite"},
	}, t0); err != nil {
		t.Fatalf("insert rejected: %v", err)
	}
	j := jobByKey(t, s, "t3_new")
	if !j.Rejected() || j.RejectReason != "onsite" {
		t.Fatalf("insert: %v / %q", j.Rejected(), j.RejectReason)
	}
}
