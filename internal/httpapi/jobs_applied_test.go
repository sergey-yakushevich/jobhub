package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

func firstJobID(t *testing.T, s *Server) int64 {
	t.Helper()
	jobs, err := s.Store.ListJobs(store.JobFilter{}, time.Now())
	if err != nil || len(jobs) == 0 {
		t.Fatalf("no seeded jobs: %v", err)
	}
	return jobs[0].ID
}

func jobState(t *testing.T, s *Server, id int64) *store.Job {
	t.Helper()
	j, err := s.Store.JobByID(id)
	if err != nil {
		t.Fatalf("load job %d: %v", id, err)
	}
	return j
}

// The endpoint an application agent calls. It has to work addressed by the
// numeric id and by the sweep's own dedupe_key, since the agent knows the key.
func TestAPIMarkAppliedByIDAndKey(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)

	rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/applied", apiToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("mark applied: %d %s", rec.Code, rec.Body.String())
	}
	if !jobState(t, s, id).Applied() {
		t.Fatal("empty body should default to applied")
	}

	rec = request(t, s, "POST", "/api/jobs/"+itoa(id)+"/applied", apiToken, `{"applied":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body.String())
	}
	if jobState(t, s, id).Applied() {
		t.Fatal("applied:false should clear the flag")
	}

	// Same endpoint, addressed by dedupe_key.
	rec = request(t, s, "POST", "/api/jobs/t3_go1/applied", apiToken, `{"applied":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("by key: %d %s", rec.Code, rec.Body.String())
	}
	jobs, _ := s.Store.ListJobs(store.JobFilter{Applied: "1"}, time.Now())
	if len(jobs) != 1 || jobs[0].DedupeKey != "t3_go1" {
		t.Fatalf("expected t3_go1 applied, got %+v", jobs)
	}

	rec = request(t, s, "POST", "/api/jobs/nosuchthing/applied", apiToken, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown target: %d", rec.Code)
	}
}

func TestAPIMarkAppliedRequiresToken(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)
	rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/applied", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if jobState(t, s, id).Applied() {
		t.Fatal("an unauthorized call changed the flag")
	}
}

// A push may carry the flag inline, so one call can both refresh a lead and
// record that it was applied to.
func TestIngestCarriesAppliedFlag(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	rec := request(t, s, "POST", "/api/jobs", apiToken,
		`{"jobs":[{"dedupe_key":"li_1","network":"linkedin","applied":true}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest: %d %s", rec.Code, rec.Body.String())
	}
	jobs, _ := s.Store.ListJobs(store.JobFilter{Applied: "1"}, time.Now())
	if len(jobs) != 1 || jobs[0].DedupeKey != "li_1" {
		t.Fatalf("expected li_1 applied, got %+v", jobs)
	}
	// The partial push must not have blanked the draft it did not mention.
	if jobs[0].Draft == "" {
		t.Fatal("a partial applied push wiped the draft")
	}
}

// The board's own button is guarded by the lead's link key, not the API token,
// so it works from the phone that is reading the board.
func TestBoardAppliedToggle(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)
	key := s.LinkKey(jobScope(id))
	back := s.BasePath + "/jobs?k=" + s.LinkKey(jobsScope()) + "&applied=0"

	post := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", target, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}

	rec := post("/jobs/" + itoa(id) + "/applied?k=" + key + "&back=" + url.QueryEscape(back))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("toggle: %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != back {
		t.Fatalf("redirect = %q, want the filtered board %q", got, back)
	}
	if !jobState(t, s, id).Applied() {
		t.Fatal("toggle did not apply")
	}
	// Toggling again is the undo.
	if post("/jobs/"+itoa(id)+"/applied?k="+key).Code != http.StatusSeeOther {
		t.Fatal("second toggle failed")
	}
	if jobState(t, s, id).Applied() {
		t.Fatal("second toggle did not undo")
	}

	// A wrong key must not flip anything.
	if rec := post("/jobs/" + itoa(id) + "/applied?k=deadbeef"); rec.Code != http.StatusNotFound {
		t.Fatalf("bad key: %d", rec.Code)
	}
}

// The board's fetch() sends Accept: application/json and gets the new state
// back instead of a redirect, so the page can settle without reloading.
func TestBoardAppliedToggleJSON(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)

	req := httptest.NewRequest("POST", "/jobs/"+itoa(id)+"/applied?k="+s.LinkKey(jobScope(id)), nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("json toggle: %d", rec.Code)
	}
	var resp struct {
		On bool `json:"on"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.On {
		t.Fatalf("json toggle body = %q, err %v", rec.Body.String(), err)
	}
	if !jobState(t, s, id).Applied() {
		t.Fatal("json toggle did not apply")
	}
}

// An attacker who could choose `back` would own a redirector on a trusted
// host, so anything that is not one of our own board paths is discarded.
func TestSafeBackRejectsOffsiteTargets(t *testing.T) {
	s, _ := testServer(t)
	board := s.BasePath + "/jobs?k=" + s.LinkKey(jobsScope())
	for _, bad := range []string{
		"https://evil.example/phish", "//evil.example", "/etc/passwd", "", s.BasePath + "/v/1",
	} {
		if got := s.safeBack(bad); got != board {
			t.Fatalf("safeBack(%q) = %q, want the board", bad, got)
		}
	}
	good := s.BasePath + "/jobs?k=abc&net=reddit"
	if got := s.safeBack(good); got != good {
		t.Fatalf("safeBack(%q) = %q, want it kept", good, got)
	}
}

// The board has to actually show the state, otherwise the flag is invisible.
func TestBoardRendersAppliedTagAndFilter(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)
	if err := s.Store.SetJobApplied(id, true, time.Now()); err != nil {
		t.Fatalf("set applied: %v", err)
	}

	req := httptest.NewRequest("GET", "/jobs?k="+s.LinkKey(jobsScope()), nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("board: %d", rec.Code)
	}
	// Not viewed, so the card class is "row applied" with no "seen" between.
	for _, want := range []string{`class="tag applied"`, `class="row applied"`, `class="mark"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("board is missing %q", want)
		}
	}

	// The applied filter has to narrow the list to that one lead.
	req = httptest.NewRequest("GET", "/jobs?k="+s.LinkKey(jobsScope())+"&applied=1", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if n := strings.Count(rec.Body.String(), `class="row`); n != 1 {
		t.Fatalf("applied=1 rendered %d rows, want 1", n)
	}
}
