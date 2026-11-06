package workspace

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

func patchRequest(t *testing.T, s *Server, path, body, tag string) *httptest.ResponseRecorder {
	t.Helper()
	r := newRequest("PATCH", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Content-Type", "application/json")
	if tag != "" {
		r.Header.Set("If-Match", tag)
	}
	return doReq(t, s, r)
}

func TestJobLifecycleVersionAndReplay(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	body := `{"source_key":"test-role","company":"Acme","title":"Backend","salary_min":0,"salary_max":8000,"fit_score":8.2,"fit_summary":"Go fit"}`
	r := apiRequest(t, s, "POST", "/api/v1/jobs", body)
	if r.Code != 201 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r.Header().Get("ETag") != `"1"` {
		t.Fatal("missing ETag")
	}
	var j store.TrackedJob
	if err := json.Unmarshal(r.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/jobs/%d", j.ID)
	if j.Profile != "sergey" || j.Stage != "saved" || j.Remote != "unknown" || j.SalaryMin == nil || *j.SalaryMin != 0 {
		t.Fatalf("defaults: %+v", j)
	}
	r = apiRequest(t, s, "POST", "/api/v1/jobs", body)
	if r.Code != 200 || r.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("retry", r.Code, r.Body.String())
	}
	r = patchRequest(t, s, path, `{"stage":"talking","needs_you":true,"salary_min":null}`, "")
	if r.Code != 428 {
		t.Fatal("missing version", r.Code)
	}
	r = patchRequest(t, s, path, `{"stage":"talking","needs_you":true,"salary_min":null}`, `"1"`)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r.Header().Get("ETag") != `"2"` {
		t.Fatal("version not incremented")
	}
	r = patchRequest(t, s, path, `{"title":"lost update"}`, `"1"`)
	if r.Code != 409 {
		t.Fatal("stale edit", r.Code)
	}
	r = apiRequest(t, s, "GET", path, "")
	if r.Code != 200 {
		t.Fatal(r.Code)
	}
	json.Unmarshal(r.Body.Bytes(), &j)
	if j.Title != "Backend" || j.SalaryMin != nil || !j.NeedsYou {
		t.Fatal("bad merge", r.Body.String())
	}
	// A delayed create retry must not conflict merely because the job changed.
	r = apiRequest(t, s, "POST", "/api/v1/jobs", body)
	if r.Code != 200 {
		t.Fatal("retry after edit", r.Code, r.Body.String())
	}
	r = apiRequest(t, s, "POST", "/api/v1/jobs", strings.Replace(body, "Backend", "Different", 1))
	if r.Code != 409 {
		t.Fatal("changed payload", r.Code)
	}
	r = apiRequest(t, s, "GET", path+"/events", "")
	var events store.TimelinePage
	json.Unmarshal(r.Body.Bytes(), &events)
	if len(events.Items) != 1 || events.Items[0].Kind != "status" {
		t.Fatal("missing atomic status record", r.Body.String())
	}
}

func TestAPIRejectsMalformedBodies(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	for _, body := range []string{`null`, `[]`, `{"company":"A"} {}`, `{"source_key":"bad","company":"A","title":"Dev","unknown":1}`, `{"source_key":"bad","company":null,"title":"Dev"}`, `{"source_key":"bad","company":"A","title":"Dev","salary_min":"100"}`, `{"source_key":"bad","company":"A","title":"Dev","stage":"magic"}`, `{"source_key":"bad","company":"A","title":"Dev","fit_score":9}`, `{"source_key":"bad","company":"A","title":"Dev","url":"javascript:alert(1)"}`, `{"source_key":"bad","company":"A","title":"Dev","url":"https://user:pass@example.com"}`, `{"source_key":"bad","company":"A","title":"Dev","version":2}`, `{"source_key":"bad","company":"A","title":"Dev","needs_you":null}`, `{"source_key":"bad","company":"A","title":"Dev","interview_at":"tomorrow"}`, `{"source_key":"bad","company":"A","title":"Dev","salary_min":300,"salary_max":200}`} {
		r := apiRequest(t, s, "POST", "/api/v1/jobs", body)
		if r.Code != 400 {
			t.Errorf("body %s status %d: %s", body, r.Code, r.Body.String())
		}
		if r.Header().Get("Content-Type") == "" {
			t.Fatal("missing JSON content type")
		}
		if errorCode(t, r) == "" {
			t.Fatal("no error code")
		}
	}
	r := newRequest("POST", "/api/v1/jobs", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+testKey)
	if got := doReq(t, s, r); got.Code != 415 {
		t.Fatal("content-type", got.Code)
	}
	big := `{"description":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	if got := apiRequest(t, s, "POST", "/api/v1/jobs", big); got.Code != 413 {
		t.Fatal("size limit", got.Code)
	}
}

func TestTimelineIdempotencyCheckTimeAndPaging(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	id, _ := createTestJob(t, s, `{"source_key":"events","company":"Acme","title":"Go"}`)
	path := fmt.Sprintf("/api/v1/jobs/%d/events", id)
	body := `{"source_key":"msg:1","kind":"message","summary":"HR replied","actor":"agent","channel":"email","direction":"inbound","stage":"talking"}`
	r := apiRequest(t, s, "POST", path, body)
	if r.Code != 201 {
		t.Fatal(r.Code, r.Body.String())
	}
	r = apiRequest(t, s, "POST", path, body)
	if r.Code != 200 || r.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("event retry", r.Code)
	}
	r = apiRequest(t, s, "POST", path, strings.Replace(body, "HR replied", "Different content", 1))
	if r.Code != 409 {
		t.Fatal("event conflict", r.Code)
	}
	j, _ := s.Store.TrackedJob(id)
	if j.LastCheckedAt != "" || j.Stage != "talking" || j.Version != 2 {
		t.Fatalf("event effects %+v", j)
	}
	for i, at := range []string{"2026-11-06T09:00:00Z", "2026-11-05T09:00:00Z"} {
		b := fmt.Sprintf(`{"source_key":"check:%d","kind":"check","summary":"Checked inbox","actor":"agent","happened_at":%q}`, i, at)
		if got := apiRequest(t, s, "POST", path, b); got.Code != 201 {
			t.Fatal(got.Code, got.Body.String())
		}
	}
	j, _ = s.Store.TrackedJob(id)
	if j.LastCheckedAt != "2026-11-06T09:00:00Z" {
		t.Fatal("last check must mean actual occurrence and never go back", j.LastCheckedAt)
	}
	r = apiRequest(t, s, "GET", path+"?limit=2", "")
	var page store.TimelinePage
	json.Unmarshal(r.Body.Bytes(), &page)
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatal("event page", r.Body.String())
	}
	r = apiRequest(t, s, "GET", path+"?limit=2&cursor="+page.NextCursor, "")
	json.Unmarshal(r.Body.Bytes(), &page)
	if len(page.Items) != 1 || page.NextCursor != "" {
		t.Fatal("event next page", r.Body.String())
	}
	r = apiRequest(t, s, "DELETE", path, "")
	if r.Code != 405 {
		t.Fatal("timeline deletion should not exist", r.Code)
	}
}

func TestAPIFiltersAndErrors(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	for i := 0; i < 3; i++ {
		createTestJob(t, s, fmt.Sprintf(`{"source_key":"job-%d","company":"Acme","title":"Backend","remote":"remote","needs_you":true}`, i))
	}
	r := apiRequest(t, s, "GET", "/api/v1/jobs?limit=2&remote=remote&needs_you=true", "")
	var p store.TrackerPage
	json.Unmarshal(r.Body.Bytes(), &p)
	if len(p.Items) != 2 || p.NextCursor == "" {
		t.Fatal(r.Body.String())
	}
	r = apiRequest(t, s, "GET", "/api/v1/jobs?limit=2&cursor="+p.NextCursor, "")
	json.Unmarshal(r.Body.Bytes(), &p)
	if len(p.Items) != 1 || p.NextCursor != "" {
		t.Fatal(r.Body.String())
	}
	for _, query := range []string{"limit=0", "limit=101", "cursor=-1", "cursor=abc", "stage=nonsense", "remote=nonsense", "needs_you=maybe", "typo=1"} {
		r := apiRequest(t, s, "GET", "/api/v1/jobs?"+query, "")
		if r.Code != 400 {
			t.Errorf("%s = %d", query, r.Code)
		}
	}
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"GET", "/api/v1/missing", 404}, {"DELETE", "/api/v1/jobs", 405}, {"GET", "/api/v1/jobs/9999", 404}, {"GET", "/api/v1/jobs/invalid", 400}, {"GET", "/api/jobs?k=old", 404}, {"GET", "/api/v1/jobs/1/events?cursor=bad", 400}} {
		r := apiRequest(t, s, tc.method, tc.path, "")
		if r.Code != tc.want {
			t.Errorf("%s %s=%d want%d", tc.method, tc.path, r.Code, tc.want)
		}
		if errorCode(t, r) == "" {
			t.Fatal("missing typed error")
		}
	}
}

func TestConcurrentEventRetryCreatesOneEvent(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	id, _ := createTestJob(t, s, `{"source_key":"race","company":"Acme","title":"Dev"}`)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := apiRequest(t, s, "POST", fmt.Sprintf("/api/v1/jobs/%d/events", id), `{"source_key":"same","kind":"note","summary":"one event","actor":"agent"}`)
			codes <- r.Code
		}()
	}
	wg.Wait()
	close(codes)
	created, replayed := 0, 0
	for code := range codes {
		switch code {
		case 201:
			created++
		case 200:
			replayed++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if created != 1 || replayed != 7 {
		t.Fatalf("created=%d replayed=%d", created, replayed)
	}
	j, _ := s.Store.TrackedJob(id)
	if j.Version != 2 {
		t.Fatal("duplicate version bump", j.Version)
	}
}

func TestPagesRenderAndEscapeWithoutSecrets(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	cookie, _ := login(t, s)
	j, _, err := s.Store.CreateTrackedJob(store.TrackedJob{SourceKey: "html", Company: `<script>alert(1)</script>`, Title: "Role", SalaryMin: func() *float64 { n := 0.0; return &n }()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/jobs", fmt.Sprintf("/jobs/%d", j.ID), "/api-docs", "/login"} {
		r := getWithCookie(t, s, path, cookie)
		if r.Code != 200 {
			t.Fatal(path, r.Code)
		}
		body := r.Body.String()
		if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
			t.Fatal("truncated template", path, body)
		}
		if strings.Contains(body, testKey) || strings.Contains(body, `<script>alert(1)</script>`) {
			t.Fatal("secret or HTML injection", path)
		}
		if r.Header().Get("Content-Security-Policy") == "" || r.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing security headers")
		}
	}
	for _, path := range []string{"/jobs?k=old", "/jobs/1?k=old", "/api-docs"} {
		r := doReq(t, s, newRequest("GET", path, nil))
		if r.Code != 303 {
			t.Fatal("weak key bypass", path, r.Code)
		}
	}
	for _, path := range []string{"/assets/app.css", "/assets/app.js"} {
		r := doReq(t, s, newRequest("GET", path, nil))
		if r.Code != 200 || r.Body.Len() < 100 {
			t.Fatal("missing asset", path, r.Code)
		}
	}
	r := apiRequest(t, s, "GET", "/api/v1/openapi.json", "")
	m := decodeMap(t, r)
	if m["openapi"] != "3.1.0" {
		t.Fatal("missing specification")
	}
	paths := m["paths"].(map[string]any)
	if len(paths) != 5 {
		t.Fatal("incomplete specification")
	}
}
