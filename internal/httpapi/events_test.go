package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

// The monitoring loop's whole API surface: record what the inbox held, move
// the funnel, stamp a quiet check — and read it all back.
func TestAPIApplicationMonitoring(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)

	// An event lands on the timeline and stamps checked_at.
	rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/events", apiToken,
		`{"kind":"email","note":"HR asks for a call Tuesday"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("event: %d %s", rec.Code, rec.Body.String())
	}
	if j := jobState(t, s, id); !j.Checked() {
		t.Fatal("event did not stamp checked_at")
	}

	// The funnel moves; the change writes its own status event.
	rec = request(t, s, "POST", "/api/jobs/"+itoa(id)+"/appstatus", apiToken,
		`{"status":"hired","note":"offer signed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("appstatus: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		AppStatus string `json:"app_status"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.AppStatus != "hired" || out.Status != "hired" {
		t.Fatalf("response: %+v", out)
	}
	j := jobState(t, s, id)
	if !j.Applied() || !j.Hired() {
		t.Fatalf("a status implies an application: applied=%v hired=%v", j.Applied(), j.Hired())
	}

	// The timeline reads back newest first.
	rec = request(t, s, "GET", "/api/jobs/"+itoa(id)+"/events", apiToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var events []store.JobEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "status" || events[1].Kind != "email" {
		t.Fatalf("timeline: %+v", events)
	}

	// A quiet check moves checked_at and nothing else.
	before := len(events)
	rec = request(t, s, "POST", "/api/jobs/"+itoa(id)+"/checked", apiToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("checked: %d", rec.Code)
	}
	rec = request(t, s, "GET", "/api/jobs/"+itoa(id)+"/events", apiToken, "")
	events = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != before {
		t.Fatalf("a quiet check grew the timeline: %d -> %d", before, len(events))
	}

	// Guardrails: bad status is a caller mistake, missing job a 404, and
	// everything here is token-gated.
	if rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/appstatus", apiToken, `{"status":"ghosted"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad status: %d", rec.Code)
	}
	if rec := request(t, s, "POST", "/api/jobs/nope/events", apiToken, `{"kind":"email"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing job: %d", rec.Code)
	}
	if rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/checked", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
}

// The job page grows an application section once an application is out: the
// standing, when it was last checked, and the timeline newest first.
func TestShowPageRendersApplicationProgress(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)

	// Before applying there is no application section.
	page := getJobPage(t, s, id)
	if strings.Contains(page, `class="app-row"`) {
		t.Fatal("application section rendered before any application went out")
	}

	request(t, s, "POST", "/api/jobs/"+itoa(id)+"/applied", apiToken, "")
	request(t, s, "POST", "/api/jobs/"+itoa(id)+"/events", apiToken, `{"kind":"email","note":"they want references"}`)

	page = getJobPage(t, s, id)
	for _, want := range []string{
		`class="app-row"`, "in process", "last checked",
		`class="tl"`, "they want references", "applied", "found",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	// The timeline reads newest to oldest: the email sits above the found row.
	if strings.Index(page, "they want references") > strings.Index(page, `>found<`) {
		t.Fatal("timeline is not newest first")
	}

	request(t, s, "POST", "/api/jobs/"+itoa(id)+"/appstatus", apiToken, `{"status":"hired"}`)
	page = getJobPage(t, s, id)
	if !strings.Contains(page, "hired") {
		t.Fatal("hired page must say so")
	}
}

// The page's own reject button: a reason is mandatory on the way in, and
// clicking again puts the lead back in play without one.
func TestPageRejectToggle(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)
	key := s.LinkKey(jobScope(id))

	post := func(body url.Values, accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/jobs/"+itoa(id)+"/rejected?k="+key,
			strings.NewReader(body.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}

	if rec := post(url.Values{}, "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("reject with no reason: %d", rec.Code)
	}
	rec := post(url.Values{"reason": {"US-only, no visa"}}, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	j := jobState(t, s, id)
	if !j.Rejected() || j.RejectReason != "US-only, no visa" {
		t.Fatalf("after reject: %v %q", j.Rejected(), j.RejectReason)
	}
	// The rejected page offers the way back.
	if page := getJobPage(t, s, id); !strings.Contains(page, "put back in play") {
		t.Fatal("rejected page missing the un-reject")
	}
	// Toggling again un-rejects, no reason needed, and redirects the form post.
	if rec := post(url.Values{}, ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("un-reject: %d", rec.Code)
	}
	if jobState(t, s, id).Rejected() {
		t.Fatal("un-reject left the flag set")
	}
	// The wrong key gets nothing.
	req := httptest.NewRequest("POST", "/jobs/"+itoa(id)+"/rejected?k=wrong", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad key: %d", rec.Code)
	}
}

// The page's status buttons move the funnel with the lead's own link key.
func TestPageAppStatusButtons(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)
	request(t, s, "POST", "/api/jobs/"+itoa(id)+"/applied", apiToken, "")

	req := httptest.NewRequest("POST", "/jobs/"+itoa(id)+"/appstatus?k="+s.LinkKey(jobScope(id)),
		strings.NewReader(url.Values{"status": {"rejected"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status form: %d", rec.Code)
	}
	if j := jobState(t, s, id); !j.AppRejected() {
		t.Fatalf("app status not set: %q", j.AppStatus)
	}
}

// The board filters and counts the post-apply funnel.
func TestBoardAppStatusChipsAndTiles(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)
	request(t, s, "POST", "/api/jobs/"+itoa(id)+"/appstatus", apiToken, `{"status":"hired"}`)

	req := httptest.NewRequest("GET", "/jobs?k="+s.LinkKey(jobsScope()), nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	page := rec.Body.String()
	for _, want := range []string{">application<", ">hired<", ">in process<", `class="tag hired"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("board missing %q", want)
		}
	}

	req = httptest.NewRequest("GET", "/jobs?k="+s.LinkKey(jobsScope())+"&app=hired", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if body := rec.Body.String(); !strings.Contains(body, `class="tag hired"`) || strings.Contains(body, "recruiter") {
		t.Fatal("?app=hired should list exactly the hired lead")
	}
}
