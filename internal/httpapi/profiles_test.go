package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

// jobIDByKey finds a lead's numeric id through the JSON endpoint, which is how
// an agent that just pushed a batch learns the ids it has to work with.
func jobIDByKey(t *testing.T, s *Server, key string) int64 {
	t.Helper()
	w := request(t, s, "GET", "/api/jobs", apiToken, "")
	var jobs []store.Job
	if err := json.Unmarshal(w.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, j := range jobs {
		if j.DedupeKey == key {
			return j.ID
		}
	}
	t.Fatalf("no lead with dedupe_key %q", key)
	return 0
}

// The profile API is token-gated like the rest of /api, and it reports the
// profiles the leads already created.
func TestProfilesJSONRequiresTokenAndListsProfiles(t *testing.T) {
	s, _ := testServer(t)
	ingestTwoProfiles(t, s)

	if w := request(t, s, "GET", "/api/profiles", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	w := request(t, s, "GET", "/api/profiles", apiToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var profiles []store.Profile
	if err := json.Unmarshal(w.Body.Bytes(), &profiles); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, p := range profiles {
		counts[p.Slug] = p.Leads
	}
	if counts["polina"] != 2 || counts[store.DefaultJobProfile] != 1 {
		t.Fatalf("profile counts = %+v", counts)
	}
}

// Writing a description is one POST, and reading it back is one GET. The
// fields are the question a lead is judged against: skills, experience and
// what the person will accept.
func TestProfileUpsertAndRead(t *testing.T) {
	s, _ := testServer(t)

	body := `{"slug":"Polina ","name":"Polina Avdevich","headline":"AI Creative Producer",
	  "summary":"AI-generated UGC and ad creatives.","skills":"Generative AI, video editing",
	  "experience":"SickStuff — Founder","conditions":"Batumi, Georgia. Remote. EU and US brands.",
	  "links":"https://sy4iz.com"}`
	w := request(t, s, "POST", "/api/profiles", apiToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("upsert: %d %s", w.Code, w.Body.String())
	}
	var res struct {
		Profile store.Profile `json:"profile"`
		Link    string        `json:"link"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	// The slug is normalised on the way in, the way a lead's profile is.
	if res.Profile.Slug != "polina" {
		t.Fatalf("slug = %q", res.Profile.Slug)
	}
	// And the returned link opens the page.
	page := request(t, s, "GET", res.Link, "", "")
	if page.Code != http.StatusOK {
		t.Fatalf("link %q: %d", res.Link, page.Code)
	}
	for _, want := range []string{"Polina Avdevich", "working conditions", "Batumi, Georgia", "Generative AI"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("profile page missing %q", want)
		}
	}

	// A partial push updates what it names and nothing else.
	w = request(t, s, "POST", "/api/profiles", apiToken, `{"slug":"polina","conditions":"Remote only."}`)
	if w.Code != http.StatusOK {
		t.Fatalf("partial: %d %s", w.Code, w.Body.String())
	}
	w = request(t, s, "GET", "/api/profiles/polina", apiToken, "")
	var p store.Profile
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Conditions != "Remote only." || p.Skills != "Generative AI, video editing" {
		t.Fatalf("partial push overwrote a field: %+v", p)
	}

	if w := request(t, s, "POST", "/api/profiles", apiToken, `{"name":"nobody"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("slugless profile: %d", w.Code)
	}
	if w := request(t, s, "GET", "/api/profiles/nobody", apiToken, ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown profile: %d", w.Code)
	}
}

// Sharing one lead with a second profile puts it on that board without
// copying it: one row, one set of viewed/approved/applied state, two readers.
func TestShareJobWithSecondProfile(t *testing.T) {
	s, _ := testServer(t)
	ingestTwoProfiles(t, s)
	id := jobIDByKey(t, s, "t3_go1") // the default owner's Go lead
	key := s.LinkKey(jobsScope())

	w := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/profiles", apiToken, `{"profile":"polina"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("share: %d %s", w.Code, w.Body.String())
	}
	var res struct {
		Profiles []string `json:"profiles"`
		Linked   bool     `json:"linked"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Linked || len(res.Profiles) != 2 {
		t.Fatalf("share result = %+v", res)
	}

	// Polina's board now shows three leads: her two, plus the shared one.
	list := request(t, s, "GET", "/api/jobs?profile=polina", apiToken, "")
	var jobs []store.Job
	if err := json.Unmarshal(list.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("polina's board holds %d leads, want 3", len(jobs))
	}
	// The shared lead is the same row, still owned by the profile that swept it.
	for _, j := range jobs {
		if j.ID == id && j.Profile != store.DefaultJobProfile {
			t.Fatalf("sharing moved the owner to %q", j.Profile)
		}
	}
	// The whole board still holds three rows: sharing copies nothing.
	all := request(t, s, "GET", "/api/jobs", apiToken, "")
	if err := json.Unmarshal(all.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("board holds %d rows after sharing, want 3", len(jobs))
	}

	// The lead page shows both boards, the owner first and the share muted.
	page := request(t, s, "GET", "/jobs/"+itoa(id)+"?k="+s.LinkKey(jobScope(id)), "", "")
	if page.Code != http.StatusOK {
		t.Fatalf("lead page: %d", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, `class="tag who-tag" href="/profiles/`+store.DefaultJobProfile) {
		t.Fatal("lead page lost the owner tag")
	}
	if !strings.Contains(body, `class="tag who-tag shared" href="/profiles/polina`) {
		t.Fatal("lead page does not show the shared board")
	}

	// Polina's filtered HTML board shows it too.
	board := request(t, s, "GET", "/jobs?k="+key+"&profile=polina", "", "")
	if !strings.Contains(board.Body.String(), "Senior Go dev") {
		t.Fatal("shared lead missing from polina's board")
	}

	// Unsharing takes it back off, and the owner link cannot be removed.
	w = request(t, s, "POST", "/api/jobs/"+itoa(id)+"/profiles", apiToken,
		`{"profile":"`+store.DefaultJobProfile+`","linked":false}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unlinking the owner = %d, want 400", w.Code)
	}
	w = request(t, s, "POST", "/api/jobs/"+itoa(id)+"/profiles", apiToken, `{"profile":"polina","linked":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("unshare: %d %s", w.Code, w.Body.String())
	}
	list = request(t, s, "GET", "/api/jobs?profile=polina", apiToken, "")
	if err := json.Unmarshal(list.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("after unsharing polina holds %d leads, want 2", len(jobs))
	}

	if w := request(t, s, "POST", "/api/jobs/"+itoa(id)+"/profiles", apiToken, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("share with no profile: %d", w.Code)
	}
	if w := request(t, s, "POST", "/api/jobs/999999/profiles", apiToken, `{"profile":"polina"}`); w.Code != http.StatusNotFound {
		t.Fatalf("share on an unknown lead: %d", w.Code)
	}
}

// The profile pages open with the board's key and with nothing else.
func TestProfilePagesAreKeyGuarded(t *testing.T) {
	s, _ := testServer(t)
	ingestTwoProfiles(t, s)
	key := s.LinkKey(jobsScope())

	for _, path := range []string{"/profiles", "/profiles/polina"} {
		if w := request(t, s, "GET", path, "", ""); w.Code != http.StatusNotFound {
			t.Fatalf("%s with no key: %d, want 404", path, w.Code)
		}
		if w := request(t, s, "GET", path+"?k=deadbeef", "", ""); w.Code != http.StatusNotFound {
			t.Fatalf("%s with a wrong key: %d, want 404", path, w.Code)
		}
		if w := request(t, s, "GET", path+"?k="+key, "", ""); w.Code != http.StatusOK {
			t.Fatalf("%s with the board key: %d", path, w.Code)
		}
	}
	if w := request(t, s, "GET", "/profiles/nobody?k="+key, "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown profile page: %d", w.Code)
	}

	// The index lists both seekers and links to each page; a profile with no
	// description says so rather than showing empty boxes.
	w := request(t, s, "GET", "/profiles?k="+key, "", "")
	body := w.Body.String()
	for _, want := range []string{">polina<", "/profiles/polina?k=" + key, "no description yet"} {
		if !strings.Contains(body, want) {
			t.Fatalf("profiles index missing %q", want)
		}
	}
	// And the board leads there: the profile chip row's label is the way in.
	board := request(t, s, "GET", "/jobs?k="+key, "", "")
	if !strings.Contains(board.Body.String(), `href="/profiles?k=`+key+`">profile<`) {
		t.Fatal("board does not link to the profiles")
	}
}

// A profile described before its first sweep still gets a chip, so the leads
// have somewhere to land and the reader can see who is being hunted for.
func TestDescribedProfileWithoutLeadsGetsAChip(t *testing.T) {
	s, _ := testServer(t)
	ingestTwoProfiles(t, s)
	if w := request(t, s, "POST", "/api/profiles", apiToken,
		`{"slug":"siarhei","name":"Siarhei Lyagushevich","summary":"Backend, Go and Ruby."}`); w.Code != http.StatusOK {
		t.Fatalf("upsert: %d %s", w.Code, w.Body.String())
	}
	w := request(t, s, "GET", "/jobs?k="+s.LinkKey(jobsScope()), "", "")
	if !strings.Contains(w.Body.String(), "profile=siarhei") {
		t.Fatal("a profile with no leads yet has no chip on the board")
	}
}
