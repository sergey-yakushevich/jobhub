package store

import (
	"errors"
	"testing"
	"time"
)

// Approving is the gate between a prepared application and a real one going
// out, so it is sticky the way applying is: re-approving records the same
// decision, not a new one.
func TestApproveIsStickyAndReversible(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(48 * time.Hour)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if j := jobByKey(t, s, "t3_abc"); j.Approved() {
		t.Fatal("a fresh lead is already approved")
	}
	if err := s.SetJobApprovedByKey("t3_abc", true, "ask about the on-call rota", t0); err != nil {
		t.Fatalf("approve: %v", err)
	}
	j := jobByKey(t, s, "t3_abc")
	if !j.Approved() || !j.ApprovedAt.Equal(t0) {
		t.Fatalf("after approve: %v / %v", j.Approved(), j.ApprovedAt)
	}
	if j.ReviewNotes != "ask about the on-call rota" {
		t.Fatalf("notes: %q", j.ReviewNotes)
	}

	// Approving again keeps the original date: "approved 2 days ago, still not
	// sent" has to stay true.
	if err := s.SetJobApprovedByKey("t3_abc", true, "", t1); err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	if j := jobByKey(t, s, "t3_abc"); !j.ApprovedAt.Equal(t0) {
		t.Fatalf("re-approve moved the date to %v", j.ApprovedAt)
	}
	// ...and says nothing about the notes, so they survive.
	if j := jobByKey(t, s, "t3_abc"); j.ReviewNotes != "ask about the on-call rota" {
		t.Fatalf("re-approve lost the notes: %q", j.ReviewNotes)
	}

	if err := s.SetJobApprovedByKey("t3_abc", false, "", t1); err != nil {
		t.Fatalf("un-approve: %v", err)
	}
	if j := jobByKey(t, s, "t3_abc"); j.Approved() {
		t.Fatal("un-approve left the flag set")
	}
}

// Where a reject reason dies with the rejection, a review note outlives the
// approval it was written under: "actually this is US-only, do not apply" is
// worth more after the approval is withdrawn than before.
func TestUnapproveKeepsTheNotes(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.SetJobApprovedByKey("t3_abc", true, "looks fine", t0); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := s.SetJobApprovedByKey("t3_abc", false, "actually US-only, do not apply", t0); err != nil {
		t.Fatalf("un-approve: %v", err)
	}
	j := jobByKey(t, s, "t3_abc")
	if j.Approved() {
		t.Fatal("still approved")
	}
	if j.ReviewNotes != "actually US-only, do not apply" {
		t.Fatalf("notes after un-approve: %q", j.ReviewNotes)
	}
}

// The usual shape of a review is to write the note while reading and decide
// afterwards, so notes have to be writable on an unapproved lead and must
// survive the decision either way.
func TestReviewNotesAreIndependentOfTheFlag(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := jobByKey(t, s, "t3_abc").ID

	if err := s.SetJobReviewNotes(id, "salary band is below the floor, ask"); err != nil {
		t.Fatalf("notes: %v", err)
	}
	j := jobByKey(t, s, "t3_abc")
	if j.Approved() {
		t.Fatal("writing a note approved the lead")
	}
	if j.ReviewNotes != "salary band is below the floor, ask" {
		t.Fatalf("notes: %q", j.ReviewNotes)
	}

	if err := s.SetJobApproved(id, true, "", t0); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if j := jobByKey(t, s, "t3_abc"); j.ReviewNotes != "salary band is below the floor, ask" {
		t.Fatalf("approving overwrote the note: %q", j.ReviewNotes)
	}

	// Clearing is a real edit, so empty is legal here even though it is not on
	// the approve path.
	if err := s.SetJobReviewNotes(id, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if j := jobByKey(t, s, "t3_abc"); j.ReviewNotes != "" {
		t.Fatalf("notes not cleared: %q", j.ReviewNotes)
	}
}

// A re-sweep must never undo a decision already taken: approval is tri-state
// for exactly the reason applied and rejected are.
func TestApproveSurvivesResweep(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.SetJobApprovedByKey("t3_abc", true, "go", t0); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// A sweep that says nothing about approval, only a fresh score.
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_abc", Network: "reddit", Score: 9, ScoreReason: "rescored"},
	}, t0.Add(time.Hour)); err != nil {
		t.Fatalf("resweep: %v", err)
	}
	j := jobByKey(t, s, "t3_abc")
	if !j.Approved() {
		t.Fatal("a re-sweep un-approved the lead")
	}
	if j.ReviewNotes != "go" {
		t.Fatalf("a re-sweep dropped the notes: %q", j.ReviewNotes)
	}
	// And the tri-state can still clear it deliberately.
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_abc", Network: "reddit", Approved: boolPtr(false)},
	}, t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("explicit un-approve: %v", err)
	}
	if j := jobByKey(t, s, "t3_abc"); j.Approved() {
		t.Fatal("explicit false did not clear the flag")
	}
}

// A bare 8 is not a judgement anyone can re-read a week later. The rule lives
// in the store so every path in is held to it — but only for a non-zero score,
// because zero already means "this push says nothing about the score".
func TestScoreRequiresReason(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)

	_, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_bare", Network: "reddit", Score: 8},
	}, t0)
	if !errors.Is(err, ErrScoreReasonRequired) {
		t.Fatalf("scored insert without a reason: got %v", err)
	}
	_, err = s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_bare", Network: "reddit", Score: 8, ScoreReason: "   "},
	}, t0)
	if !errors.Is(err, ErrScoreReasonRequired) {
		t.Fatalf("whitespace reason: got %v", err)
	}

	// A partial push carrying no score stays legal — that is what attaches a
	// draft or flips applied without anyone justifying a number never sent.
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_ok", Network: "reddit", Draft: "hi"},
	}, t0); err != nil {
		t.Fatalf("unscored partial push: %v", err)
	}
	if _, err := s.UpsertJobs([]JobParams{
		{DedupeKey: "t3_ok", Network: "reddit", Score: 7, ScoreReason: "Go, remote, invoice-friendly"},
	}, t0); err != nil {
		t.Fatalf("scored with reason: %v", err)
	}
	if j := jobByKey(t, s, "t3_ok"); j.ScoreReason != "Go, remote, invoice-friendly" {
		t.Fatalf("score_reason: %q", j.ScoreReason)
	}
}

// The resolve pass writes the real posting alongside the original, never over
// it: the two disagreeing about location is the single most useful signal the
// board carries.
func TestPostingIsStoredBesideTheOriginalPost(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs([]JobParams{{
		DedupeKey: "t3_agg", Network: "reddit", Body: "Remote - Anywhere, $200-220k",
		URL: "https://aggregator.example/x", Score: 9, ScoreReason: "looks remote",
	}}, t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The resolve pass comes back with what the company's own board says.
	if _, err := s.UpsertJobs([]JobParams{{
		DedupeKey: "t3_agg", Network: "reddit",
		PostingURL:  "https://boards.example/co/123",
		PostingText: "Location: Remote US. 401k, US-only medical.",
		Score:       0.5, ScoreReason: "posting says Remote US, W2 only",
	}}, t0.Add(time.Hour)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	j := jobByKey(t, s, "t3_agg")
	if j.Body != "Remote - Anywhere, $200-220k" {
		t.Fatalf("the original post was overwritten: %q", j.Body)
	}
	if j.PostingURL != "https://boards.example/co/123" {
		t.Fatalf("posting_url: %q", j.PostingURL)
	}
	if j.PostingText != "Location: Remote US. 401k, US-only medical." {
		t.Fatalf("posting_text: %q", j.PostingText)
	}
	if j.Score != 0.5 {
		t.Fatalf("score not demoted: %v", j.Score)
	}
}

// prep is opaque to the store: it is rendered on one page and never queried, so
// clearing it is how a bad CV gets sent back for a redo.
func TestPrepRoundTripsAndClears(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := jobByKey(t, s, "t3_abc").ID
	if j := jobByKey(t, s, "t3_abc"); j.Prepped() {
		t.Fatal("a fresh lead is already prepped")
	}

	blob := `{"summary":"Go backend at a payments company","cv_url":"https://buildcv.cc/u/s"}`
	if err := s.SetJobPrep(id, blob); err != nil {
		t.Fatalf("set prep: %v", err)
	}
	j := jobByKey(t, s, "t3_abc")
	if !j.Prepped() || j.Prep != blob {
		t.Fatalf("prep: %v / %q", j.Prepped(), j.Prep)
	}

	if err := s.SetJobPrep(id, ""); err != nil {
		t.Fatalf("clear prep: %v", err)
	}
	if j := jobByKey(t, s, "t3_abc"); j.Prepped() {
		t.Fatal("prep not cleared")
	}
}

// Both pipeline queues are gaps, not totals: leads waiting to be read, and
// leads waiting to be sent. Counting every prepped or approved row would make
// the numbers grow as the work got done.
func TestFilterAndStatsForThePipelineQueues(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	if _, err := s.UpsertJobs(jobBatch(), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	abc := jobByKey(t, s, "t3_abc").ID
	tw := jobByKey(t, s, "tw_1").ID
	li := jobByKey(t, s, "li_1").ID

	// abc: prepped and approved, not applied  -> to send
	// tw:  prepped, undecided                 -> to review
	// li:  prepped and approved and applied   -> neither
	for _, id := range []int64{abc, tw, li} {
		if err := s.SetJobPrep(id, `{"summary":"x"}`); err != nil {
			t.Fatalf("prep %d: %v", id, err)
		}
	}
	if err := s.SetJobApproved(abc, true, "", t0); err != nil {
		t.Fatalf("approve abc: %v", err)
	}
	if err := s.SetJobApproved(li, true, "", t0); err != nil {
		t.Fatalf("approve li: %v", err)
	}
	if err := s.SetJobApplied(li, true, t0); err != nil {
		t.Fatalf("apply li: %v", err)
	}

	st, err := s.JobStats(JobFilter{}, t0)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.ToReview != 1 {
		t.Fatalf("to review = %d, want 1", st.ToReview)
	}
	if st.ToSend != 1 {
		t.Fatalf("to send = %d, want 1", st.ToSend)
	}

	// The apply stage's whole input is a filter, not a list somebody assembled.
	queue, err := s.ListJobs(JobFilter{Approved: "1", Applied: "0"}, t0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(queue) != 1 || queue[0].ID != abc {
		t.Fatalf("apply queue = %v", queue)
	}

	// And so is the prep stage's.
	todo, err := s.ListJobs(JobFilter{Prepped: "0"}, t0)
	if err != nil {
		t.Fatalf("list unprepped: %v", err)
	}
	if len(todo) != 0 {
		t.Fatalf("unprepped = %d, want 0", len(todo))
	}
}
