package store

import (
	"errors"
	"testing"
	"time"
)

func seedOne(t *testing.T, s *Store, now time.Time) int64 {
	t.Helper()
	if _, err := s.UpsertJobs(jobBatch(), now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return jobByKey(t, s, "t3_abc").ID
}

// The derived status walks the whole ladder: to-prep, to-review once prep
// lands, approved after the click, applied, and finally where the application
// landed. Agents read this field, so the words are part of the API.
func TestDerivedStatusLadder(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	id := seedOne(t, s, t0)

	assert := func(want string) {
		t.Helper()
		j, err := s.JobByID(id)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if j.Status != want {
			t.Fatalf("status = %q, want %q", j.Status, want)
		}
	}
	assert("to-prep")
	if err := s.SetJobPrep(id, `{"summary":"go role"}`); err != nil {
		t.Fatal(err)
	}
	assert("to-review")
	if err := s.SetJobApproved(id, true, "", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assert("approved")
	if err := s.SetJobApplied(id, true, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assert("applied")
	if err := s.SetJobAppStatus(id, "hired", "offer signed", t0.Add(72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assert("hired")
	if err := s.SetJobAppStatus(id, "rejected", "", t0.Add(96*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assert("rejected")
}

// A status on a lead never marked applied stamps applied_at itself: a status
// implies an application exists. And every change writes its own timeline
// event, while a re-statement of the same status only moves checked_at.
func TestAppStatusStampsAppliedCheckedAndTimeline(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	id := seedOne(t, s, t0)

	if err := s.SetJobAppStatus(id, "in process", "", t0.Add(time.Hour)); err != nil {
		t.Fatalf("set: %v", err)
	}
	j, _ := s.JobByID(id)
	if !j.Applied() || j.AppState() != AppInProcess || !j.Checked() {
		t.Fatalf("after set: applied=%v state=%q checked=%v", j.Applied(), j.AppState(), j.Checked())
	}
	events, err := s.JobEvents(id)
	if err != nil || len(events) != 1 || events[0].Kind != "status" {
		t.Fatalf("events after first status: %v %+v", err, events)
	}

	// Same status again: checked_at moves, the timeline does not grow.
	if err := s.SetJobAppStatus(id, AppInProcess, "", t0.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	j, _ = s.JobByID(id)
	if !j.CheckedAt.Equal(t0.Add(5 * time.Hour)) {
		t.Fatalf("checked_at did not move: %v", j.CheckedAt)
	}
	if events, _ = s.JobEvents(id); len(events) != 1 {
		t.Fatalf("re-statement grew the timeline: %+v", events)
	}

	if err := s.SetJobAppStatus(id, "ghosted", "", t0); !errors.Is(err, ErrBadAppStatus) {
		t.Fatalf("bad status: %v", err)
	}
}

// The timeline is newest first, an event stamps checked_at, and a zero
// happened-at means "now".
func TestJobEventsNewestFirst(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	id := seedOne(t, s, t0)

	if _, err := s.AddJobEvent(id, "Email", "HR wants a call", t0.Add(time.Hour), t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("event: %v", err)
	}
	ev, err := s.AddJobEvent(id, "dm", "pinged the recruiter", time.Time{}, t0.Add(50*time.Hour))
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if !ev.HappenedAt.Equal(t0.Add(50 * time.Hour)) {
		t.Fatalf("zero happened_at should default to now, got %v", ev.HappenedAt)
	}
	events, err := s.JobEvents(id)
	if err != nil || len(events) != 2 {
		t.Fatalf("list: %v %+v", err, events)
	}
	if events[0].Kind != "dm" || events[1].Kind != "email" {
		t.Fatalf("order/kind: %+v", events)
	}
	j, _ := s.JobByID(id)
	if !j.CheckedAt.Equal(t0.Add(50 * time.Hour)) {
		t.Fatalf("event did not stamp checked_at: %v", j.CheckedAt)
	}

	if _, err := s.AddJobEvent(9999, "email", "x", time.Time{}, t0); err == nil {
		t.Fatal("event on a missing job must fail")
	}
}

// ?app= narrows the board by where the application stands, and in_process
// includes applied leads that never got an explicit status.
func TestFilterByAppStatus(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetJobAppliedByKey("t3_abc", true, t0); err != nil {
		t.Fatal(err)
	}
	hired := jobByKey(t, s, "li_1").ID
	if err := s.SetJobAppStatus(hired, AppHired, "", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	inProc, err := s.ListJobs(JobFilter{AppStatus: AppInProcess}, t0.Add(2*time.Hour))
	if err != nil || len(inProc) != 1 || inProc[0].DedupeKey != "t3_abc" {
		t.Fatalf("in_process: %v %+v", err, inProc)
	}
	got, err := s.ListJobs(JobFilter{AppStatus: AppHired}, t0.Add(2*time.Hour))
	if err != nil || len(got) != 1 || got[0].ID != hired {
		t.Fatalf("hired: %v %+v", err, got)
	}

	st, err := s.JobStats(JobFilter{}, t0.Add(2*time.Hour))
	if err != nil || st.InProcess != 1 || st.Hired != 1 {
		t.Fatalf("stats: %v in=%d hired=%d", err, st.InProcess, st.Hired)
	}
}

// A sweep may push app_status like any other field: non-empty-only, validated
// on the way in.
func TestUpsertCarriesAppStatus(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_abc", Network: "reddit", AppStatus: "hired"},
	}, t0.Add(time.Hour)); err != nil {
		t.Fatalf("push status: %v", err)
	}
	if j := jobByKey(t, s, "t3_abc"); j.AppStatus != AppHired {
		t.Fatalf("app_status = %q", j.AppStatus)
	}
	// A later partial push with no opinion leaves it alone.
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_abc", Network: "reddit", Body: "reposted"},
	}, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if j := jobByKey(t, s, "t3_abc"); j.AppStatus != AppHired {
		t.Fatalf("partial push blanked app_status: %q", j.AppStatus)
	}
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_abc", Network: "reddit", AppStatus: "maybe"},
	}, t0); !errors.Is(err, ErrBadAppStatus) {
		t.Fatalf("bad status in a push: %v", err)
	}
}

// TouchJobChecked always moves forward — "last checked" means the LAST check.
func TestTouchJobChecked(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	id := seedOne(t, s, t0)
	if err := s.TouchJobChecked(id, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchJobChecked(id, t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	j, _ := s.JobByID(id)
	if !j.CheckedAt.Equal(t0.Add(3 * time.Hour)) {
		t.Fatalf("checked_at = %v", j.CheckedAt)
	}
	if err := s.TouchJobChecked(9999, t0); err == nil {
		t.Fatal("missing job must 404")
	}
}
