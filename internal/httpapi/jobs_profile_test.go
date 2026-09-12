package httpapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

const twoProfilePayload = `{"jobs":[
  {"dedupe_key":"t3_go1","network":"reddit","job_type":"contract","score":12.5,"score_reason":"seed",
   "author":"founder1","title":"[Hiring] Senior Go dev","url":"https://reddit.com/r/forhire/x"},
  {"dedupe_key":"t3_ugc1","profile":"polina","network":"reddit","job_type":"contract","score":11,"score_reason":"seed",
   "author":"brandowner","title":"[Hiring] UGC video editor","url":"https://reddit.com/r/forhire/y"},
  {"dedupe_key":"tw_ads","profile":"Polina","network":"x","job_type":"job","score":8,"score_reason":"seed",
   "author":"adsagency","title":"Meta ads creative","url":"https://x.com/i/status/9"}
]}`

func ingestTwoProfiles(t *testing.T, s *Server) {
	t.Helper()
	if w := request(t, s, "POST", "/api/jobs", apiToken, twoProfilePayload); w.Code != http.StatusOK {
		t.Fatalf("ingest: %d %s", w.Code, w.Body.String())
	}
}

// ?profile= narrows the JSON endpoint, and a lead pushed with no profile still
// lands on the default owner's board.
func TestJobsJSONFiltersByProfile(t *testing.T) {
	s, _ := testServer(t)
	ingestTwoProfiles(t, s)

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 3},
		{"?profile=polina", 2},
		{"?profile=" + store.DefaultJobProfile, 1},
		{"?profile=nobody", 0},
	} {
		w := request(t, s, "GET", "/api/jobs"+tc.query, apiToken, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%q: %d", tc.query, w.Code)
		}
		var jobs []store.Job
		if err := json.Unmarshal(w.Body.Bytes(), &jobs); err != nil {
			t.Fatalf("%q: %v", tc.query, err)
		}
		if len(jobs) != tc.want {
			t.Fatalf("%q: %d leads, want %d", tc.query, len(jobs), tc.want)
		}
	}

	// The profile is normalised on ingest, so "Polina" and "polina" are one board.
	w := request(t, s, "GET", "/api/jobs?profile=POLINA", apiToken, "")
	var jobs []store.Job
	if err := json.Unmarshal(w.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("uppercase profile query: %d leads, want 2", len(jobs))
	}
}

// The board renders profile chips, and picking one narrows the list while
// keeping every other filter in the link.
func TestJobsBoardProfileChips(t *testing.T) {
	s, _ := testServer(t)
	ingestTwoProfiles(t, s)
	key := s.LinkKey(jobsScope())

	w := request(t, s, "GET", "/jobs?k="+key, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("board: %d", w.Code)
	}
	body := w.Body.String()
	// "everyone" is the active chip on an unfiltered board, so it renders with
	// the ✓ the template puts in front of every active chip.
	for _, want := range []string{">profile<", "profile=polina", "profile=" + store.DefaultJobProfile, "✓ everyone<"} {
		if !strings.Contains(body, want) {
			t.Fatalf("board missing %q", want)
		}
	}
	// A mixed board tags each row with whose lead it is.
	if !strings.Contains(body, `class="tag who-tag">polina<`) {
		t.Fatal("mixed board did not tag rows with the profile")
	}

	// Narrowed to one profile: only that person's leads, and no per-row tag,
	// because every row would carry the same one.
	w = request(t, s, "GET", "/jobs?k="+key+"&profile=polina", "", "")
	body = w.Body.String()
	if strings.Contains(body, "Senior Go dev") {
		t.Fatal("polina's board showed the default owner's lead")
	}
	if !strings.Contains(body, "UGC video editor") {
		t.Fatal("polina's board is missing her lead")
	}
	if strings.Contains(body, `class="tag who-tag"`) {
		t.Fatal("single-profile board still repeats the profile tag on every row")
	}

	// Chips compose: switching network from within Polina's board keeps her
	// profile in the link rather than dropping back to everyone.
	if !strings.Contains(body, "net=x") || !strings.Contains(body, "profile=polina") {
		t.Fatal("network chips dropped the active profile")
	}
}

// A profile with no leads yet still gets a chip, so the first sweep for a new
// seeker has somewhere to land and be found.
func TestJobProfileChoicesIncludeKnownNames(t *testing.T) {
	s, _ := testServer(t)
	got := s.jobProfileChoices()
	for _, want := range knownJobProfiles {
		if !slices.Contains(got, want) {
			t.Fatalf("choices %v missing %q on an empty board", got, want)
		}
	}
	// Once a profile has leads it is listed from the data, not duplicated.
	ingestTwoProfiles(t, s)
	got = s.jobProfileChoices()
	seen := map[string]int{}
	for _, p := range got {
		seen[p]++
	}
	for p, n := range seen {
		if n != 1 {
			t.Fatalf("profile %q listed %d times in %v", p, n, got)
		}
	}
}
