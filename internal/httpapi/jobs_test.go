package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
func timeNow() time.Time   { return time.Now() }

const jobsPayload = `{"jobs":[
  {"dedupe_key":"t3_go1","network":"reddit","job_type":"contract","score":12.5,"score_reason":"seed",
   "author":"founder1","title":"[Hiring] Senior Go dev","body":"remote, invoice ok",
   "url":"https://reddit.com/r/forhire/x","subreddit":"forhire","signals":"go,remote"},
  {"dedupe_key":"li_1","network":"linkedin","job_type":"job","score":9,"score_reason":"seed",
   "author":"recruiter","body":"Ruby on Rails, C2C","url":"https://linkedin.com/in/r",
   "emails":"r@corp.com","draft":"hello, I have 10 years of Rails"}
]}`

func ingestJobs(t *testing.T, s *Server) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/jobs", strings.NewReader(jobsPayload))
	req.Header.Set("Authorization", "Bearer "+apiToken)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest: %d %s", rec.Code, rec.Body.String())
	}
}

func TestJobsIngestRequiresAuthAndReturnsLink(t *testing.T) {
	s, _ := testServer(t)

	req := httptest.NewRequest("POST", "/api/jobs", strings.NewReader(jobsPayload))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}

	req = httptest.NewRequest("POST", "/api/jobs", strings.NewReader(jobsPayload))
	req.Header.Set("Authorization", "Bearer "+apiToken)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Added   int64  `json:"added"`
		Updated int64  `json:"updated"`
		Link    string `json:"link"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Added != 2 || out.Updated != 0 {
		t.Fatalf("counts: %+v", out)
	}
	// The returned link must actually open the board.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", out.Link, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "founder1") {
		t.Fatalf("board via returned link: %d", rec.Code)
	}
}

func TestJobsBoardKeyAndFilters(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/jobs?k=wrong000", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrong key: %d", rec.Code)
	}

	key := s.LinkKey(jobsScope())
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/jobs?k="+key+"&net=reddit", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "founder1") {
		t.Fatalf("board: %d", rec.Code)
	}
	if strings.Contains(body, "recruiter") {
		t.Fatal("network filter leaked the linkedin row")
	}
	// The board key must not open a lead's detail page: scopes are separate.
	jobs, _ := s.Store.ListJobs(store.JobFilter{}, timeNow())
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/jobs/"+itoa(jobs[0].ID)+"?k="+key, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("board key on detail page: %d", rec.Code)
	}
}

func TestJobShowMarksViewedAndGoRedirects(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	jobs, _ := s.Store.ListJobs(store.JobFilter{Network: "reddit"}, timeNow())
	id := jobs[0].ID
	key := s.LinkKey(jobScope(id))

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/jobs/"+itoa(id)+"?k="+key, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "remote, invoice ok") {
		t.Fatalf("show: %d", rec.Code)
	}
	j, _ := s.Store.JobByID(id)
	if !j.Viewed() {
		t.Fatal("opening the detail page must mark the lead viewed")
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", "/jobs/"+itoa(id)+"/go?k="+key, nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://reddit.com/r/forhire/x" {
		t.Fatalf("go: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestJobsPurge(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	req := httptest.NewRequest("DELETE", "/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+apiToken)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deleted":2`) {
		t.Fatalf("purge: %d %s", rec.Code, rec.Body.String())
	}
	jobs, _ := s.Store.ListJobs(store.JobFilter{}, timeNow())
	if len(jobs) != 0 {
		t.Fatalf("rows survived the purge: %d", len(jobs))
	}
}

func TestJobsJSONFilter(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	req := httptest.NewRequest("GET", "/api/jobs?type=contract", nil)
	req.Header.Set("Authorization", "Bearer "+apiToken)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var jobs []store.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].DedupeKey != "t3_go1" {
		t.Fatalf("filtered json: %+v", jobs)
	}
}
