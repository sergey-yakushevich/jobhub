package jev

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

// fakeCloudflare answers ai/run with a canned Jev result wrapped in the real
// double envelope, and lets a test inspect what state and questions arrived.
func fakeCloudflare(t *testing.T, answers map[string]Answer, sawState *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/ai/run") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("bad auth header %q", got)
		}
		var req struct {
			Model string `json:"model"`
			Input struct {
				State     string              `json:"state"`
				Questions map[string]Question `json:"questions"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != "typesafe/jev" {
			t.Errorf("model = %q", req.Model)
		}
		if sawState != nil {
			*sawState = req.Input.State
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"state": "Completed",
				"result": Result{Model: "jev-1.13.0", Answers: answers,
					Usage: Usage{InputTokens: 400, OutputTokens: 60}},
			},
		})
	}))
}

func client(url string) *Client {
	return &Client{AccountID: "acc", Token: "test-token", BaseURL: url}
}

// passAnswers is a lead every gate passes and every axis likes: a resolved
// senior Go payments role.
func passAnswers() map[string]Answer {
	choice := func(c string, conf float64) Answer {
		return Answer{Type: "choice", Choice: c, Confidence: conf,
			Probabilities: map[string]float64{c: conf}}
	}
	score := func(probs map[string]float64) Answer {
		return Answer{Type: "score", Probabilities: probs, Confidence: 0.9,
			Legend: map[string]string{"0": "l0", "1": "l1", "2": "l2", "3": "l3", "4": "l4"}}
	}
	return map[string]Answer{
		"is_it_a_job":   choice("live_paid_role", 0.95),
		"pay_model":     choice("salary", 0.9),
		"location_rule": choice("worldwide", 0.9),
		"apply_route":   choice("ats_form", 0.9),
		"go_depth":      score(map[string]float64{"2": 1}), // primary → 0.95
		"stack_overlap": score(map[string]float64{"3": 1}), // 5-6 → 1.0
		"domain":        choice("payments_fintech", 1),     // 1.0
		"seniority":     score(map[string]float64{"2": 1}), // senior → 1.0
		"frontend_load": score(map[string]float64{"0": 1}), // none → 1.0
	}
}

func TestEvaluateUnwrapsEnvelope(t *testing.T) {
	srv := fakeCloudflare(t, passAnswers(), nil)
	defer srv.Close()
	res, err := client(srv.URL).Evaluate(context.Background(), "state", map[string]Question{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "jev-1.13.0" || len(res.Answers) != 9 {
		t.Fatalf("model=%q answers=%d", res.Model, len(res.Answers))
	}
	if res.Usage.InputTokens != 400 {
		t.Fatalf("usage: %+v", res.Usage)
	}
}

func TestEvaluateCloudflareError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"errors":  []map[string]any{{"message": "no such model"}},
		})
	}))
	defer srv.Close()
	_, err := client(srv.URL).Evaluate(context.Background(), "s", nil)
	if err == nil || !strings.Contains(err.Error(), "no such model") {
		t.Fatalf("err = %v", err)
	}
}

func resolvedJob() *store.Job {
	return &store.Job{Profile: "sergey", Network: "linkedin", Title: "Senior Go Engineer",
		Body: "blurb", PostingText: "Senior Go Engineer, payments. 5+ years experience. Remote worldwide.",
		PostedAt: time.Now().Add(-72 * time.Hour), CreatedAt: time.Now()}
}

func TestComputeAllPass(t *testing.T) {
	sc := scorecards["sergey"]
	v := compute(sc, resolvedJob(), &Result{Model: "jev-1.13.0", Answers: passAnswers()})
	if v.Workable != WorkableYes {
		t.Fatalf("workable = %s, detail %s", v.Workable, v.Detail)
	}
	// 2.5*0.95 + 2.5*1.0 + 2.0*1.0 + 1.5*1.0 + 1.5*1.0 = 9.9
	if v.Fit != 9.9 {
		t.Fatalf("fit = %v", v.Fit)
	}
	var d verdictDetail
	if err := json.Unmarshal([]byte(v.Detail), &d); err != nil {
		t.Fatalf("detail not json: %v", err)
	}
	if d.Gates["pay_model"].Verdict != "pass" || d.Axes["go_depth"].Points != 2.38 {
		t.Fatalf("detail: %+v", d)
	}
}

func TestComputeKnockouts(t *testing.T) {
	sc := scorecards["sergey"]
	cases := []struct {
		gate, choice string
		conf         float64
		want         string
	}{
		{"pay_model", "equity_only", 0.9, WorkableBlocked},
		{"is_it_a_job", "advice_or_discussion", 0.95, WorkableBlocked},
		{"location_rule", "excludes_georgia", 0.8, WorkableBlocked},
		{"location_rule", "hybrid_or_onsite", 0.9, WorkableBlocked},
		// A blocking option under the confidence floor downgrades to unknown
		// instead of killing the lead.
		{"pay_model", "equity_only", 0.5, WorkableUnknown},
		// The routes that die on the Sales Navigator paywall are unknown.
		{"apply_route", "linkedin_dm", 0.9, WorkableUnknown},
		{"pay_model", "not_stated", 0.9, WorkableUnknown},
	}
	for _, c := range cases {
		answers := passAnswers()
		answers[c.gate] = Answer{Type: "choice", Choice: c.choice, Confidence: c.conf}
		v := compute(sc, resolvedJob(), &Result{Answers: answers})
		if v.Workable != c.want {
			t.Errorf("%s=%s conf %.2f: workable = %s, want %s", c.gate, c.choice, c.conf, v.Workable, c.want)
		}
	}
}

func TestComputeUnresolvedNeverYes(t *testing.T) {
	sc := scorecards["sergey"]
	j := resolvedJob()
	j.PostingText = "" // the resolver never verified this one
	v := compute(sc, j, &Result{Answers: passAnswers()})
	if v.Workable != WorkableUnknown {
		t.Fatalf("workable = %s, want unknown for an unresolved lead", v.Workable)
	}
}

func TestComputeFitIsExpectation(t *testing.T) {
	sc := scorecards["sergey"]
	answers := passAnswers()
	// A 60/40 split between "primary language" (0.95) and "nice to have"
	// (0.5): the fit must use the expectation, not the argmax.
	answers["go_depth"] = Answer{Type: "score", Confidence: 0.6,
		Probabilities: map[string]float64{"1": 0.4, "2": 0.6},
		Legend:        map[string]string{"1": "nice to have", "2": "primary language"}}
	v := compute(sc, resolvedJob(), &Result{Answers: answers})
	// go_depth = 2.5*(0.4*0.5 + 0.6*0.95) = 1.925; other axes sum to 7.5,
	// so the total is 9.425 → 9.4. The argmax level (0.95) would have given
	// 9.875 → 9.9, so this asserts the expectation is really in use.
	if v.Fit != 9.4 {
		t.Fatalf("fit = %v", v.Fit)
	}
	var d verdictDetail
	_ = json.Unmarshal([]byte(v.Detail), &d)
	if d.Axes["go_depth"].Level != "primary language" {
		t.Fatalf("level = %q", d.Axes["go_depth"].Level)
	}
}

func TestDerivedFacts(t *testing.T) {
	j := resolvedJob()
	facts := derivedFacts(j, time.Now())
	for _, want := range []string{"posted 3 day(s) ago", "5+ years", "employer's own posting"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts missing %q: %s", want, facts)
		}
	}
	j.PostingText = ""
	if facts := derivedFacts(j, time.Now()); !strings.Contains(facts, "NOT verified") {
		t.Errorf("unresolved facts wrong: %s", facts)
	}
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func ingest(t *testing.T, st *store.Store, key, profile, postingText string) int64 {
	t.Helper()
	res, err := st.UpsertJobs([]store.JobParams{{
		DedupeKey: key, Profile: profile, Network: "reddit",
		Title: "Senior Go Engineer", Body: "blurb", PostingText: postingText,
	}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AddedIDs) != 1 {
		t.Fatalf("AddedIDs = %v", res.AddedIDs)
	}
	return res.AddedIDs[0]
}

func TestScorerEndToEnd(t *testing.T) {
	var sawState string
	srv := fakeCloudflare(t, passAnswers(), &sawState)
	defer srv.Close()
	st := testStore(t)
	sc := NewScorer(client(srv.URL), st)

	id := ingest(t, st, "lead-1", "sergey", "Senior Go Engineer, payments. Remote worldwide.")
	if err := sc.scoreOne(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	j, err := st.JobByID(id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Workable != WorkableYes || j.Fit != 9.9 || j.FitAt.IsZero() {
		t.Fatalf("job after scoring: workable=%q fit=%v fit_at=%v", j.Workable, j.Fit, j.FitAt)
	}
	for _, want := range []string{"CANDIDATE:", "FACTS", "LEAD:", "TEXT:"} {
		if !strings.Contains(sawState, want) {
			t.Errorf("state missing %q", want)
		}
	}
	if len(sawState) > 4000 {
		t.Errorf("state too large: %d chars — the tight-state rule is broken", len(sawState))
	}
}

func TestScorerSkipsScoredAndForeignProfiles(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{"state": "Completed",
				"result": Result{Model: "m", Answers: passAnswers()}},
		})
	}))
	defer srv.Close()
	st := testStore(t)
	sc := NewScorer(client(srv.URL), st)

	// A profile with no scorecard is left alone entirely.
	polina := ingest(t, st, "lead-p", "polina", "UGC editor gig")
	if err := sc.scoreOne(context.Background(), polina); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("scored a profile that has no scorecard")
	}
	if j, _ := st.JobByID(polina); j.Workable != "" {
		t.Fatalf("polina lead got workable=%q", j.Workable)
	}

	// A lead that already has a verdict is never re-scored.
	id := ingest(t, st, "lead-s", "sergey", "posting")
	if err := sc.scoreOne(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
	if err := sc.scoreOne(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("re-scored an already scored lead")
	}
}
