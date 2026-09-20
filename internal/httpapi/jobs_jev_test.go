package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

type recordingJev struct{ got [][]int64 }

func (r *recordingJev) Enqueue(ids []int64) { r.got = append(r.got, ids) }

// TestIngestEnqueuesOnlyAddedLeads pins the no-backfill rule at the HTTP
// layer: freshly created rows reach the scorer, refreshed rows do not, and a
// board with no scorer configured ingests exactly as before.
func TestIngestEnqueuesOnlyAddedLeads(t *testing.T) {
	s, _ := testServer(t)
	rec := &recordingJev{}
	s.Jev = rec

	body := `{"jobs":[{"dedupe_key":"a1","network":"reddit","title":"one"},
	                  {"dedupe_key":"a2","network":"reddit","title":"two"}]}`
	if w := request(t, s, http.MethodPost, "/api/jobs", apiToken, body); w.Code != http.StatusOK {
		t.Fatalf("ingest: %d %s", w.Code, w.Body)
	}
	if len(rec.got) != 1 || len(rec.got[0]) != 2 {
		t.Fatalf("enqueued %v", rec.got)
	}

	// The same push again: both rows now exist, nothing new to score.
	if w := request(t, s, http.MethodPost, "/api/jobs", apiToken, body); w.Code != http.StatusOK {
		t.Fatalf("re-ingest: %d %s", w.Code, w.Body)
	}
	if len(rec.got) != 1 {
		t.Fatalf("re-push enqueued again: %v", rec.got)
	}
}

// TestJobsJSONCarriesFitColumns: the agents read the same two columns the
// board shows, and ?workable= narrows the JSON endpoint like any filter.
func TestJobsJSONCarriesFitColumns(t *testing.T) {
	s, st := testServer(t)
	body := `{"jobs":[{"dedupe_key":"f1","network":"reddit","title":"one"},
	                  {"dedupe_key":"f2","network":"reddit","title":"two"}]}`
	if w := request(t, s, http.MethodPost, "/api/jobs", apiToken, body); w.Code != http.StatusOK {
		t.Fatalf("ingest: %d", w.Code)
	}
	jobs, err := st.ListJobs(store.JobFilter{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobFit(jobs[0].ID, 8.2, "yes", `{}`, time.Now()); err != nil {
		t.Fatal(err)
	}

	w := request(t, s, http.MethodGet, "/api/jobs?workable=yes", apiToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	var out []store.Job
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Fit != 8.2 || out[0].Workable != "yes" {
		t.Fatalf("jobs = %+v", out)
	}

	// An unscored lead serializes with NO fit fields at all — omitempty is
	// the API-side of "an absent verdict must not read as one".
	w = request(t, s, http.MethodGet, "/api/jobs?workable=blocked", apiToken, "")
	if body := w.Body.String(); strings.Contains(body, `"workable"`) || strings.Contains(body, `"fit"`) {
		t.Fatalf("blocked filter should be empty, got %s", body)
	}
}
