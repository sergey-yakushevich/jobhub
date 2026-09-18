// Package e2e drives jobhub the way its users do: over HTTP, through the real
// mux, against a real SQLite file, with the shipped profiles seeded in.
//
// The unit tests know the internals; this one knows only what a sweep, a
// browser and an agent know — a token, a link, and the HTML that comes back.
// Its job is to prove the board still behaves the way it did once leads and
// profiles became a many-to-many.
package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/httpapi"
	"github.com/sergey-yakushevich/jobhub/internal/seed"
	"github.com/sergey-yakushevich/jobhub/internal/store"
)

const (
	apiToken = "e2e-api-token"
	viewKey  = "e2e-view-key"
)

// board is one running jobhub: a database, a server and a client that does not
// follow redirects, so a 303 can be asserted on rather than swallowed.
type board struct {
	t      *testing.T
	srv    *httptest.Server
	client *http.Client
}

func newBoard(t *testing.T) *board {
	t.Helper()
	// Production runs with DEFAULT_PROFILE=sergey; the default owner matters
	// for every un-profiled push, so the test runs under the same setting.
	prev := store.DefaultJobProfile
	store.DefaultJobProfile = "sergey"
	t.Cleanup(func() { store.DefaultJobProfile = prev })

	st, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := seed.Apply(st, time.Now()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := httptest.NewServer(httpapi.New(st, apiToken, viewKey))
	t.Cleanup(srv.Close)
	return &board{
		t:   t,
		srv: srv,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// do runs one request and returns the status and the body. Paths are relative
// to the server; a token is sent only when asked for, since the view pages are
// guarded by their link key instead.
func (b *board) do(method, path, token, body string) (int, string) {
	b.t.Helper()
	req, err := http.NewRequest(method, b.srv.URL+path, strings.NewReader(body))
	if err != nil {
		b.t.Fatalf("request %s %s: %v", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if method == "POST" && strings.HasPrefix(body, "notes=") {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		b.t.Fatalf("read %s %s: %v", method, path, err)
	}
	return res.StatusCode, string(raw)
}

func (b *board) get(path string) string {
	b.t.Helper()
	code, body := b.do("GET", path, "", "")
	if code != http.StatusOK {
		b.t.Fatalf("GET %s: %d", path, code)
	}
	return body
}

// api posts to the token-gated side and fails on anything but 200.
func (b *board) api(method, path, body string) string {
	b.t.Helper()
	code, out := b.do(method, path, apiToken, body)
	if code != http.StatusOK {
		b.t.Fatalf("%s %s: %d %s", method, path, code, out)
	}
	return out
}

var (
	leadLinkRe   = regexp.MustCompile(`href="(/jobs/\d+\?[^"]+)"`)
	approveRe    = regexp.MustCompile(`id="gate"[^>]*action="([^"]+)"`)
	markFormRe   = regexp.MustCompile(`<form class="mark"[^>]*action="(/jobs/\d+/applied\?[^"]+)"`)
	profileTagRe = regexp.MustCompile(`class="tag who-tag[^"]*" href="([^"]+)"`)
)

// first pulls one capture out of a page, failing with the pattern when the
// page does not carry it — a missing link is the failure, not a nil deref
// three lines later.
func first(t *testing.T, re *regexp.Regexp, body, what string) string {
	t.Helper()
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("page has no %s (pattern %s)", what, re)
	}
	return html(m[1])
}

// html undoes the entity escaping the templates apply to link query strings.
func html(s string) string { return strings.NewReplacer("&amp;", "&", "&#34;", `"`).Replace(s) }

const sweep = `{"jobs":[
  {"dedupe_key":"t3_go","network":"reddit","job_type":"contract","score":9,"score_reason":"go, remote, invoice ok",
   "author":"founder","title":"[Hiring] Senior Go engineer","body":"Go, Postgres, remote",
   "url":"https://reddit.com/r/forhire/go","subreddit":"forhire"},
  {"dedupe_key":"t3_ugc","profile":"polina","network":"reddit","job_type":"contract","score":8,"score_reason":"ugc, remote",
   "author":"brandowner","title":"[Hiring] UGC video editor","body":"AI ads for a DTC brand",
   "url":"https://reddit.com/r/forhire/ugc","subreddit":"forhire"},
  {"dedupe_key":"tw_ads","profile":"polina","network":"x","job_type":"job","score":7,"score_reason":"paid ads",
   "author":"adsagency","title":"Meta ads creative","url":"https://x.com/i/status/9"}
]}`

// TestBoardEndToEnd walks the whole board the way it is actually used: a sweep
// pushes leads, a reader filters them, opens one, approves it, marks it
// applied — and then shares it with the second seeker, which is the behaviour
// the profiles table adds. Everything before the share has to work exactly as
// it did before there was one.
func TestBoardEndToEnd(t *testing.T) {
	b := newBoard(t)

	// 1. The sweep pushes a batch and gets the board link back.
	var ingest struct {
		Added   int64  `json:"added"`
		Updated int64  `json:"updated"`
		Link    string `json:"link"`
	}
	if err := json.Unmarshal([]byte(b.api("POST", "/api/jobs", sweep)), &ingest); err != nil {
		t.Fatalf("ingest json: %v", err)
	}
	if ingest.Added != 3 || ingest.Updated != 0 {
		t.Fatalf("ingest = %+v, want 3 added", ingest)
	}

	// 2. The board opens on that link and holds all three leads, tagged with
	//    whose hunt each belongs to.
	page := b.get(ingest.Link)
	for _, want := range []string{"Senior Go engineer", "UGC video editor", "Meta ads creative",
		`class="tag who-tag">polina<`, `class="tag who-tag">sergey<`} {
		if !strings.Contains(page, want) {
			t.Fatalf("board missing %q", want)
		}
	}
	key := url.Values{}
	if u, err := url.Parse(ingest.Link); err == nil {
		key.Set("k", u.Query().Get("k"))
	}
	boardKey := key.Get("k")

	// 3. Filtering to one seeker narrows the list, and the per-row tag goes
	//    away because every row would carry the same one.
	polina := b.get("/jobs?k=" + boardKey + "&profile=polina")
	if strings.Contains(polina, "Senior Go engineer") {
		t.Fatal("polina's board shows sergey's lead")
	}
	if !strings.Contains(polina, "UGC video editor") || strings.Contains(polina, `class="tag who-tag"`) {
		t.Fatal("polina's board is wrong")
	}

	// 4. Open the Go lead from the board. The link carries the lead's own key
	//    and the board URL to come back to.
	sergeyBoard := b.get("/jobs?k=" + boardKey + "&profile=sergey")
	leadLink := first(t, leadLinkRe, sergeyBoard, "lead link")
	lead := b.get(leadLink)
	for _, want := range []string{"Senior Go engineer", "go, remote, invoice ok", "approve for applying"} {
		if !strings.Contains(lead, want) {
			t.Fatalf("lead page missing %q", want)
		}
	}
	// The who-tag is a link to the profile behind the lead — the description
	// the board now carries.
	profileLink := first(t, profileTagRe, lead, "profile tag link")
	profile := b.get(profileLink)
	for _, want := range []string{"Sergey Yakushevich", "Senior Backend Engineer", "working conditions", "Batumi, Georgia"} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile page missing %q", want)
		}
	}

	// 5. Approve it through the gate, with a note, exactly as the form posts.
	approveLink := first(t, approveRe, lead, "approve form")
	code, _ := b.do("POST", approveLink, "", "notes=ask+about+the+on-call+rota")
	if code != http.StatusSeeOther {
		t.Fatalf("approve: %d, want 303", code)
	}
	lead = b.get(leadLink)
	if !strings.Contains(lead, "ask about the on-call rota") || !strings.Contains(lead, "approved") {
		t.Fatal("approval did not stick")
	}

	// 6. Mark it applied from the board, which is a form post like any other.
	sergeyBoard = b.get("/jobs?k=" + boardKey + "&profile=sergey")
	markLink := first(t, markFormRe, sergeyBoard, "applied form")
	if code, _ := b.do("POST", markLink, "", ""); code != http.StatusSeeOther {
		t.Fatalf("mark applied: %d, want 303", code)
	}

	// 7. The API and the board agree about all of it.
	var jobs []store.Job
	if err := json.Unmarshal([]byte(b.api("GET", "/api/jobs?profile=sergey", "")), &jobs); err != nil {
		t.Fatalf("api list: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("sergey holds %d leads, want 1", len(jobs))
	}
	goLead := jobs[0]
	if !goLead.Approved() || !goLead.Applied() || goLead.ReviewNotes != "ask about the on-call rota" {
		t.Fatalf("lead state after the walk: %+v", goLead)
	}

	// 8. The share: the same posting, on Polina's board too. One row, one set
	//    of state, two readers.
	b.api("POST", "/api/jobs/"+itoa(goLead.ID)+"/profiles", `{"profile":"polina"}`)
	polina = b.get("/jobs?k=" + boardKey + "&profile=polina")
	if !strings.Contains(polina, "Senior Go engineer") {
		t.Fatal("the shared lead is missing from polina's board")
	}
	if err := json.Unmarshal([]byte(b.api("GET", "/api/jobs", "")), &jobs); err != nil {
		t.Fatalf("api list: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("sharing changed the row count to %d, want 3", len(jobs))
	}
	// Sharing does not hand the lead over, and it does not carry the owner's
	// work across either: the state belongs to the one row.
	lead = b.get(leadLink)
	if !strings.Contains(lead, `class="tag who-tag" href="/profiles/sergey`) ||
		!strings.Contains(lead, `class="tag who-tag shared" href="/profiles/polina`) {
		t.Fatal("the lead page does not show both boards")
	}

	// 9. The profiles index is reachable from the board and describes all three
	//    seekers the binary ships with.
	index := b.get("/profiles?k=" + boardKey)
	for _, want := range []string{"Sergey Yakushevich", "Polina Avdevich", "Siarhei Lyagushevich"} {
		if !strings.Contains(index, want) {
			t.Fatalf("profiles index missing %q", want)
		}
	}
	// The seeded descriptions are on the pages, not just in the binary.
	polinaPage := b.get("/profiles/polina?k=" + boardKey)
	if !strings.Contains(polinaPage, "AI Creative Producer") || !strings.Contains(polinaPage, "Midjourney") {
		t.Fatal("polina's seeded description did not reach her page")
	}
	siarhei := b.get("/profiles/siarhei?k=" + boardKey)
	if !strings.Contains(siarhei, "Minsk, Belarus") || !strings.Contains(siarhei, "PCI-DSS") {
		t.Fatal("siarhei's seeded description did not reach his page")
	}
}

// A re-sweep is the normal case — the same posts come back every day — and it
// must not duplicate rows, reset state or lose the sharing.
func TestResweepKeepsStateAndShares(t *testing.T) {
	b := newBoard(t)
	b.api("POST", "/api/jobs", sweep)

	var jobs []store.Job
	if err := json.Unmarshal([]byte(b.api("GET", "/api/jobs?profile=sergey", "")), &jobs); err != nil {
		t.Fatalf("list: %v", err)
	}
	id := jobs[0].ID
	b.api("POST", "/api/jobs/"+itoa(id)+"/approved", `{"notes":"worth it"}`)
	b.api("POST", "/api/jobs/"+itoa(id)+"/applied", ``)
	b.api("POST", "/api/jobs/"+itoa(id)+"/profiles", `{"profile":"polina"}`)

	var again struct {
		Added   int64 `json:"added"`
		Updated int64 `json:"updated"`
	}
	if err := json.Unmarshal([]byte(b.api("POST", "/api/jobs", sweep)), &again); err != nil {
		t.Fatalf("re-sweep: %v", err)
	}
	if again.Added != 0 || again.Updated != 3 {
		t.Fatalf("re-sweep = %+v, want 3 updated and nothing added", again)
	}
	if err := json.Unmarshal([]byte(b.api("GET", "/api/jobs", "")), &jobs); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("re-sweep left %d rows, want 3", len(jobs))
	}
	for _, j := range jobs {
		if j.ID != id {
			continue
		}
		if !j.Approved() || !j.Applied() || j.ReviewNotes != "worth it" {
			t.Fatalf("re-sweep undid the decisions: %+v", j)
		}
	}
	// The share survived the push that rewrote the lead.
	if err := json.Unmarshal([]byte(b.api("GET", "/api/jobs?profile=polina", "")), &jobs); err != nil {
		t.Fatalf("list polina: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("polina holds %d leads after the re-sweep, want 3 (2 hers + 1 shared)", len(jobs))
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
