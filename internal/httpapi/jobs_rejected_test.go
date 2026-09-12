package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

func TestAPIRejectRequiresReason(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)

	rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/rejected", apiToken, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no reason: %d %s", rec.Code, rec.Body.String())
	}
	if jobState(t, s, id).Rejected() {
		t.Fatal("a refused rejection still marked the row")
	}

	rec = request(t, s, "POST", "/api/jobs/"+itoa(id)+"/rejected", apiToken,
		`{"reason":"US-only, no work authorization"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("with reason: %d %s", rec.Code, rec.Body.String())
	}
	j := jobState(t, s, id)
	if !j.Rejected() || j.RejectReason != "US-only, no work authorization" {
		t.Fatalf("after reject: %v / %q", j.Rejected(), j.RejectReason)
	}

	// Un-rejecting needs no reason and clears the stored one.
	rec = request(t, s, "POST", "/api/jobs/"+itoa(id)+"/rejected", apiToken, `{"rejected":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("un-reject: %d %s", rec.Code, rec.Body.String())
	}
	if j := jobState(t, s, id); j.Rejected() || j.RejectReason != "" {
		t.Fatalf("un-reject left %v / %q", j.Rejected(), j.RejectReason)
	}
}

// The ingest path enforces the same rule, and a bad batch is the caller's
// mistake — a 400, not a 500.
func TestIngestRejectWithoutReasonIsBadRequest(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	rec := request(t, s, "POST", "/api/jobs", apiToken,
		`{"jobs":[{"dedupe_key":"li_1","network":"linkedin","rejected":true}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %s", rec.Code, rec.Body.String())
	}

	rec = request(t, s, "POST", "/api/jobs", apiToken,
		`{"jobs":[{"dedupe_key":"li_1","network":"linkedin","rejected":true,"reject_reason":"UK-only"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("with reason: %d %s", rec.Code, rec.Body.String())
	}
	jobs, _ := s.Store.ListJobs(store.JobFilter{Rejected: "1"}, time.Now())
	if len(jobs) != 1 || jobs[0].RejectReason != "UK-only" {
		t.Fatalf("expected li_1 rejected with reason, got %+v", jobs)
	}
}

// X leads use the tweet id as their dedupe_key, so the reference parses as a
// valid int64 and used to be looked up as a row id that does not exist. The
// resolver must fall back to the key.
func TestNumericDedupeKeyResolvesAfterIDMiss(t *testing.T) {
	s, _ := testServer(t)
	const tweetID = "2095405525227704692"
	rec := request(t, s, "POST", "/api/jobs", apiToken,
		`{"jobs":[{"dedupe_key":"`+tweetID+`","network":"x","score":8,"score_reason":"seed","author":"recruiter","body":"Go role"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}

	if rec := request(t, s, "POST", "/api/jobs/"+tweetID+"/applied", apiToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("applied by numeric key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(t, s, "POST", "/api/jobs/"+tweetID+"/rejected", apiToken,
		`{"reason":"anonymous contact"}`); rec.Code != http.StatusOK {
		t.Fatalf("rejected by numeric key: %d %s", rec.Code, rec.Body.String())
	}

	jobs, _ := s.Store.ListJobs(store.JobFilter{}, time.Now())
	var found *store.Job
	for i := range jobs {
		if jobs[i].DedupeKey == tweetID {
			found = &jobs[i]
		}
	}
	if found == nil {
		t.Fatal("seeded job vanished")
	}
	if !found.Applied() || !found.Rejected() || found.RejectReason != "anonymous contact" {
		t.Fatalf("numeric key did not resolve: %+v", found)
	}

	// A genuinely unknown reference still 404s rather than silently passing.
	if rec := request(t, s, "POST", "/api/jobs/99999999/applied", apiToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown numeric ref: %d", rec.Code)
	}
}

// The board must show the reason in place of the description, and go red.
func TestBoardShowsRejectReasonInsteadOfDescription(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)
	const why = "Real posting is US-only W2"
	if err := s.Store.SetJobRejected(id, true, why, time.Now()); err != nil {
		t.Fatalf("reject: %v", err)
	}

	req := httptest.NewRequest("GET", "/jobs?k="+s.LinkKey(jobsScope()), nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("board: %d", rec.Code)
	}
	for _, want := range []string{`class="tag rejected"`, "rejected", why} {
		if !strings.Contains(body, want) {
			t.Fatalf("board missing %q", want)
		}
	}
	// The original pitch for that row must be gone from the snippet.
	if strings.Contains(body, "[Hiring] Senior Go dev") {
		t.Fatal("rejected row still shows its description instead of the reason")
	}

	req = httptest.NewRequest("GET", "/jobs?k="+s.LinkKey(jobsScope())+"&rejected=1", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if n := strings.Count(rec.Body.String(), `class="row`); n != 1 {
		t.Fatalf("rejected=1 rendered %d rows, want 1", n)
	}
}
