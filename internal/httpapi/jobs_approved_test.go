package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

// The gate an application agent reads before sending anything, addressable by
// the numeric id or by the sweep's own dedupe_key.
func TestAPIApproveByIDAndKey(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)

	rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/approved", apiToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if !jobState(t, s, id).Approved() {
		t.Fatal("approve did not stamp the row")
	}

	rec = request(t, s, "POST", "/api/jobs/li_1/approved", apiToken,
		`{"notes":"confirm they take a foreign contractor"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve by key: %d %s", rec.Code, rec.Body.String())
	}
	jobs, err := s.Store.ListJobs(store.JobFilter{Approved: "1"}, timeNow())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("approved = %d, want 2", len(jobs))
	}

	// Un-approving needs no notes and does not require them to be re-sent.
	rec = request(t, s, "POST", "/api/jobs/"+itoa(id)+"/approved", apiToken, `{"approved":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("un-approve: %d %s", rec.Code, rec.Body.String())
	}
	if jobState(t, s, id).Approved() {
		t.Fatal("un-approve left the flag set")
	}

	if rec := request(t, s, "POST", "/api/jobs/nope/approved", apiToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown ref: %d", rec.Code)
	}
	if rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/approved", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
}

// prep is stored verbatim and only checked for being JSON: the shape belongs to
// the prep stage and the page that renders it, not to the store.
func TestAPIPrepStoresJSONAndRejectsGarbage(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)

	blob := `{"summary":"Go backend, payments","form_questions":["salary?"],"cv_url":"https://buildcv.cc/u/s"}`
	rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/prep", apiToken, blob)
	if rec.Code != http.StatusOK {
		t.Fatalf("prep: %d %s", rec.Code, rec.Body.String())
	}
	if got := jobState(t, s, id).Prep; got != blob {
		t.Fatalf("prep stored as %q", got)
	}

	if rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/prep", apiToken, "not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage prep: %d %s", rec.Code, rec.Body.String())
	}
	if got := jobState(t, s, id).Prep; got != blob {
		t.Fatalf("a refused prep overwrote the good one: %q", got)
	}

	// Empty clears it, which is how a bad CV gets sent back for a redo.
	if rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/prep", apiToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("clear prep: %d %s", rec.Code, rec.Body.String())
	}
	if jobState(t, s, id).Prepped() {
		t.Fatal("prep not cleared")
	}
}

// The ingest path enforces the score rule, and a bad batch is the caller's
// mistake — a 400, not a 500. The whole batch is one transaction, so nothing
// lands.
func TestIngestScoreWithoutReasonIsBadRequest(t *testing.T) {
	s, _ := testServer(t)
	rec := request(t, s, "POST", "/api/jobs", apiToken,
		`{"jobs":[{"dedupe_key":"t3_bare","network":"reddit","score":8}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "score_reason") {
		t.Fatalf("the error does not name the rule: %s", rec.Body.String())
	}
	jobs, _ := s.Store.ListJobs(store.JobFilter{}, timeNow())
	if len(jobs) != 0 {
		t.Fatalf("a refused batch still wrote %d row(s)", len(jobs))
	}

	rec = request(t, s, "POST", "/api/jobs", apiToken,
		`{"jobs":[{"dedupe_key":"t3_bare","network":"reddit","score":8,"score_reason":"Go, remote"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("with reason: %d %s", rec.Code, rec.Body.String())
	}
}

// The board's own approve button is guarded by the lead's link key rather than
// the API token, so the gate can be cleared from the phone reading the review.
func TestBoardApproveToggleAndNotes(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)
	key := s.LinkKey(jobScope(id))
	back := s.BasePath + "/jobs?k=" + s.LinkKey(jobsScope()) + "&approved=1"

	post := func(target string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", target, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}

	// A note written while reading, before any decision is made.
	rec := post("/jobs/"+itoa(id)+"/notes?k="+key, "notes=ask+about+the+on-call+rota")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("notes: %d", rec.Code)
	}
	j := jobState(t, s, id)
	if j.Approved() {
		t.Fatal("saving a note approved the lead")
	}
	if j.ReviewNotes != "ask about the on-call rota" {
		t.Fatalf("notes = %q", j.ReviewNotes)
	}

	rec = post("/jobs/"+itoa(id)+"/approved?k="+key+"&back="+url.QueryEscape(back), "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve toggle: %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != back {
		t.Fatalf("redirect = %q, want %q", got, back)
	}
	j = jobState(t, s, id)
	if !j.Approved() {
		t.Fatal("toggle did not approve")
	}
	if j.ReviewNotes != "ask about the on-call rota" {
		t.Fatalf("approving dropped the note: %q", j.ReviewNotes)
	}

	// Toggling again is the undo, and the note survives it.
	if post("/jobs/"+itoa(id)+"/approved?k="+key, "").Code != http.StatusSeeOther {
		t.Fatal("second toggle failed")
	}
	j = jobState(t, s, id)
	if j.Approved() {
		t.Fatal("second toggle did not undo")
	}
	if j.ReviewNotes != "ask about the on-call rota" {
		t.Fatalf("un-approving dropped the note: %q", j.ReviewNotes)
	}

	// Wrong key flips nothing, on either endpoint.
	if rec := post("/jobs/"+itoa(id)+"/approved?k=deadbeef", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bad key on approve: %d", rec.Code)
	}
	if rec := post("/jobs/"+itoa(id)+"/notes?k=deadbeef", "notes=x"); rec.Code != http.StatusNotFound {
		t.Fatalf("bad key on notes: %d", rec.Code)
	}
	if jobState(t, s, id).ReviewNotes != "ask about the on-call rota" {
		t.Fatal("a bad-key post changed the notes")
	}
}

// The review artifact is the reason the detail page exists once prep has run,
// so everything a decision needs has to be on it.
func TestJobShowRendersTheReview(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)

	prep := `{"summary":"Senior Go role at a payments company",
	          "fit":"Go since 2024, payments domain matches",
	          "form_questions":["Expected salary?","Work authorization?"],
	          "open_questions":["They ask for 5 years of Go — state the gap?"],
	          "blockers":["Workable puts a Turnstile on submit"],
	          "cv_url":"https://buildcv.cc/u/s/acme-go","cv_label":"CV for Acme"}`
	if rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/prep", apiToken, prep); rec.Code != http.StatusOK {
		t.Fatalf("prep: %d %s", rec.Code, rec.Body.String())
	}
	if rec := request(t, s, "POST", "/api/jobs", apiToken,
		`{"jobs":[{"dedupe_key":"t3_go1","network":"reddit","posting_url":"https://boards.example/acme/1",
		           "posting_text":"Location: Remote EMEA","score":9,"score_reason":"Go, remote EMEA, invoice ok"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("resolve push: %d %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest("GET", "/jobs/"+itoa(id)+"?k="+s.LinkKey(jobScope(id)), nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("show: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Senior Go role at a payments company",
		"Expected salary?",
		"They ask for 5 years of Go",
		"Workable puts a Turnstile on submit",
		"CV for Acme",
		"Go, remote EMEA, invoice ok",
		"Location: Remote EMEA",
		"the real posting",
		// The decision lives in the top button row; the box below it holds the
		// lead's one editable note for the apply stage.
		"note for the AI apply stage",
		"approve for applying",
		// The status chips: the CSS picks one via the classes on <main>.
		`<span class="st st-review">`,
		"mark gate-toggle",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the review page is missing %q", want)
		}
	}
}

// Prep artifacts written before drafts existed are a plain []string, and there
// are enough of them on the board that re-running prep just to read one would
// be silly. Both shapes have to render.
func TestFormQuestionsRenderStringAndObjectForms(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)

	prep := `{"summary":"Senior Go role",
	          "form_questions":[
	            "Full name",
	            {"question":"Phone","source":"persona: +1 555 010 4477"},
	            {"question":"What was the hardest feature you shipped?",
	             "answer":"PCI-DSS tokenisation across 350M+ payments."}]}`
	if err := s.Store.SetJobPrep(id, prep); err != nil {
		t.Fatalf("seed prep: %v", err)
	}
	// Note the escaping: html/template writes "+" as "&#43;", so these look
	// for the parts either side of it rather than the raw text.
	body := getJobPage(t, s, id)
	for _, want := range []string{
		"Full name",                    // bare string, no answer
		"persona: &#43;1 555 010 4477", // formal, shown as its source
		"What was the hardest feature", // drafted question
		"PCI-DSS tokenisation across",  // the draft being reviewed
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the review page is missing %q", want)
		}
	}
}

// The note rides along with the decision: approving with something typed in
// the box has to save both, or a caveat written seconds before approving is
// silently dropped.
func TestApprovingSavesTheNoteInTheSamePost(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)
	key := s.LinkKey(jobScope(id))

	post := func(path, notes string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/jobs/"+itoa(id)+path+"?k="+key,
			strings.NewReader("notes="+url.QueryEscape(notes)))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("POST %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}

	post("/approved", "cap the rate at $5k")
	job := jobState(t, s, id)
	if !job.Approved() {
		t.Fatal("lead should be approved")
	}
	if job.ReviewNotes != "cap the rate at $5k" {
		t.Fatalf("note not saved with the approval: %q", job.ReviewNotes)
	}

	// Editing the note must not disturb the approval.
	post("/notes", "cap at $5k, Warsaw CV")
	job = jobState(t, s, id)
	if !job.Approved() {
		t.Fatal("editing the note withdrew the approval")
	}
	if job.ReviewNotes != "cap at $5k, Warsaw CV" {
		t.Fatalf("note not updated: %q", job.ReviewNotes)
	}

	// Withdrawing keeps whatever the note says — the reason to withdraw is
	// usually written in it.
	post("/approved", "cap at $5k, Warsaw CV")
	job = jobState(t, s, id)
	if job.Approved() {
		t.Fatal("second post should have withdrawn the approval")
	}
	if job.ReviewNotes != "cap at $5k, Warsaw CV" {
		t.Fatalf("withdrawing lost the note: %q", job.ReviewNotes)
	}
}

func getJobPage(t *testing.T, s *Server, id int64) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/jobs/"+itoa(id)+"?k="+s.LinkKey(jobScope(id)), nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("show: %d", rec.Code)
	}
	return rec.Body.String()
}

// prep is written by an agent against a schema the store does not enforce, so
// the one thing that must not happen is a malformed blob taking the lead down.
func TestMalformedPrepDoesNotBreakThepage(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)

	// Valid JSON, wrong shape: summary came back as a number.
	if err := s.Store.SetJobPrep(id, `{"summary":42}`); err != nil {
		t.Fatalf("seed prep: %v", err)
	}
	req := httptest.NewRequest("GET", "/jobs/"+itoa(id)+"?k="+s.LinkKey(jobScope(id)), nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("show with bad prep: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "prep unreadable") {
		t.Error("the page does not say the prep could not be read")
	}
	// The lead itself is still readable, and still reviewable.
	if !strings.Contains(body, "[Hiring] Senior Go dev") || !strings.Contains(body, "approve for applying") {
		t.Error("a bad prep blob took the rest of the page with it")
	}
}

// Both queues are one board URL each, which is what keeps the prep and apply
// stages pointed at a filter rather than a list somebody assembled by hand.
func TestBoardFiltersByPipelineState(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)
	if rec := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/approved", apiToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d", rec.Code)
	}

	get := func(q string) string {
		req := httptest.NewRequest("GET", "/jobs?k="+s.LinkKey(jobsScope())+q, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("board%s: %d", q, rec.Code)
		}
		return rec.Body.String()
	}

	approved := get("&approved=1&applied=0")
	if !strings.Contains(approved, "[Hiring] Senior Go dev") {
		t.Error("the approved board is missing the approved lead")
	}
	if strings.Contains(approved, "Ruby on Rails, C2C") {
		t.Error("the approved board shows an unapproved lead")
	}
	if !strings.Contains(get(""), "to send") {
		t.Error("the board has no to-send tile")
	}

	// The JSON endpoint takes the same params, so any board URL is an API call.
	req := httptest.NewRequest("GET", "/api/jobs?approved=1&applied=0", nil)
	req.Header.Set("Authorization", "Bearer "+apiToken)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("json: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "t3_go1") || strings.Contains(rec.Body.String(), "li_1") {
		t.Fatalf("json queue = %s", rec.Body.String())
	}
}

// The composer pill is the lead's one editable note: a note saved through the
// API-side /notes endpoint prefills the pill (and the approved card), so the
// next send carries it forward instead of silently blanking it.
func TestNoteSaveConfirmsAndPrefillsForEditing(t *testing.T) {
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)
	key := s.LinkKey(jobScope(id))

	body := getJobPage(t, s, id)
	if !strings.Contains(body, "note for the AI apply stage (optional)") {
		t.Error("a lead without a note should offer the composer placeholder")
	}

	req := httptest.NewRequest("POST", "/jobs/"+itoa(id)+"/notes?k="+key,
		strings.NewReader("notes=ask+about+equity"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save note: %d", rec.Code)
	}

	req = httptest.NewRequest("GET", rec.Header().Get("Location"), nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `value="ask about equity"`) {
		t.Error("saved note does not prefill the composer pill")
	}
	// And it survives a plain reload.
	if !strings.Contains(getJobPage(t, s, id), `value="ask about equity"`) {
		t.Error("the note should still prefill the composer on reload")
	}
}
