package workspace

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

func renderedPage(t *testing.T, name string, d pageData) string {
	t.Helper()
	var out bytes.Buffer
	if err := pageTemplates.ExecuteTemplate(&out, name, d); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func closedDisclosure(t *testing.T, html, class string) string {
	t.Helper()
	start := strings.Index(html, `<details class="`+class+`">`)
	if start < 0 {
		t.Fatalf("missing closed %s disclosure", class)
	}
	end := strings.Index(html[start:], "</details>")
	if end < 0 {
		t.Fatalf("unclosed %s disclosure", class)
	}
	return html[start : start+end+len("</details>")]
}

func TestPagesKeepUtilitiesAndAPIExamplesCollapsed(t *testing.T) {
	d := pageData{Title: "Jobs", Demo: true, CSRF: "test-csrf"}
	board := renderedPage(t, "board", d)
	menu := closedDisclosure(t, board, "workspace-menu")
	for _, wanted := range []string{`href="/api-docs?profile=`, `action="/logout"`, `value="test-csrf"`, "demo data", `aria-label="Workspace options"`} {
		if !strings.Contains(menu, wanted) {
			t.Errorf("menu lost %s", wanted)
		}
	}
	for _, unwanted := range []string{"local preview", "fictional jobs", "records only", "no outbound actions", "Record what you know", `<footer`} {
		if strings.Contains(board, unwanted) {
			t.Errorf("board still contains %s", unwanted)
		}
	}
	d.Title = "Agent API"
	docs := renderedPage(t, "docs", d)
	for _, unwanted := range []string{"FOR YOUR AGENTS", "A small API", "1. Authenticate", "Contract & boundaries"} {
		if strings.Contains(docs, unwanted) {
			t.Errorf("docs still contains %s", unwanted)
		}
	}
	if strings.Count(docs, `<details class="api-detail">`) != 3 || strings.Count(docs, `href="/jobs?profile=sergey"`) != 1 {
		t.Fatal("API should have three collapsed sections and one back link")
	}
	if !strings.Contains(closedDisclosure(t, docs, "api-detail"), "Authorization: Bearer") {
		t.Fatal("authentication instructions must remain available on demand")
	}
}

func TestTimelinePreservesProvenanceWithoutDefaultClutter(t *testing.T) {
	score := 8.5
	d := pageData{Title: "Acme", Job: &store.TrackedJob{
		ID: 1, Company: "Acme", Title: "Engineer", Stage: "talking", Profile: "sergey",
		FitScore: &score, FitSummary: "Strong Go match", Version: 3,
	}, Events: []store.TimelineEvent{{
		Kind: "message", Summary: "Recruiter replied", Body: "Original reply",
		Actor: "Job agent", Channel: "system", Direction: "internal", Verification: "uncertain",
		SourceKey: "mail:1", ThreadID: "thread:1", ExternalID: "message:1",
	}}}
	html := renderedPage(t, "job", d)
	metadata := closedDisclosure(t, html, "event-details")
	for _, wanted := range []string{"Original reply", "Job agent", "system", "internal", "mail:1", "thread:1", "message:1"} {
		if !strings.Contains(metadata, wanted) {
			t.Errorf("lost event detail %s", wanted)
		}
	}
	outside := strings.Replace(html, metadata, "", 1)
	for _, unwanted := range []string{"Job agent", `class="event-attribution"`, "Does not send anything", "record details"} {
		if strings.Contains(outside, unwanted) {
			t.Errorf("default timeline still contains %s", unwanted)
		}
	}
	for _, wanted := range []string{"Recruiter replied", `<span class="needs-label">uncertain</span>`, `data-date="short"`} {
		if !strings.Contains(outside, wanted) {
			t.Errorf("essential timeline fact missing: %s", wanted)
		}
	}
	if !strings.Contains(closedDisclosure(t, html, "match-details"), "Strong Go match") {
		t.Fatal("match reasoning lost")
	}
	if strings.Contains(html, `class="record-meta"`) || strings.Contains(html, `<summary>record</summary>`) {
		t.Fatal("record section must not be rendered")
	}
	if strings.Count(html, `href="/jobs?profile=sergey"`) != 1 {
		t.Fatal("duplicate board navigation")
	}
}

func TestBoardKeepsOnlyActiveProfilesAndScopesNavigation(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	cookie, _ := login(t, s)
	ids := map[string]int64{}
	for _, profile := range []string{"sergey", "polina", "siarhei"} {
		j, _, err := s.Store.CreateTrackedJob(store.TrackedJob{SourceKey: "profile-test", Profile: profile, Company: profile + " company", Title: "Role"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		ids[profile] = j.ID
	}
	for _, profile := range []string{"sergey", "polina"} {
		r := getWithCookie(t, s, "/jobs?profile="+profile, cookie)
		if r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
		html := r.Body.String()
		if !strings.Contains(html, profile+" company") || strings.Contains(html, "siarhei") {
			t.Fatal("profile scope was lost")
		}
		for _, wanted := range []string{`name="profile" aria-label="Profile"`, `<option value="` + profile + `" selected>`, `href="/jobs?profile=` + profile + `&needs_you=true"`, `href="/jobs?profile=` + profile + `&stage=interview"`} {
			if !strings.Contains(html, wanted) {
				t.Errorf("missing profile selection/navigation %s", wanted)
			}
		}
		filters := strings.Split(strings.Split(html, `id="job-filters"`)[1], "</form>")[0]
		if strings.Contains(filters, "<button") || strings.Contains(filters, `type="submit"`) || strings.Contains(filters, `<select name="stage"`) || strings.Contains(filters, `<select name="remote"`) {
			t.Fatal("removed filter controls remain")
		}
		form := strings.Split(html, `id="job-form"`)[1]
		if !strings.Contains(form, `<select name="profile" required>`) || !strings.Contains(form, `<option value="`+profile+`" selected>`) {
			t.Fatal("new job does not inherit profile")
		}
		detail := getWithCookie(t, s, fmt.Sprintf("/jobs/%d", ids[profile]), cookie).Body.String()
		if !strings.Contains(detail, `href="/jobs?profile=`+profile+`"`) {
			t.Fatal("detail back link loses profile")
		}
	}
	all := getWithCookie(t, s, "/jobs?profile=all", cookie)
	html := all.Body.String()
	if all.Code != 200 || !strings.Contains(html, "sergey company") || !strings.Contains(html, "polina company") || strings.Contains(html, "siarhei company") {
		t.Fatal("All must include exactly the active profiles", all.Code)
	}
	for _, wanted := range []string{`<option value="all" selected>All</option>`, `<span class="n">2</span>`, `href="/jobs?profile=all&needs_you=true"`, fmt.Sprintf(`href="/jobs/%d?profile=all"`, ids["polina"])} {
		if !strings.Contains(html, wanted) {
			t.Errorf("All missing %s", wanted)
		}
	}
	if strings.Contains(html, `<span class="k">saved</span>`) {
		t.Fatal("Saved KPI remains")
	}
	form := strings.Split(html, `id="job-form"`)[1]
	if strings.Contains(form, `value="all"`) {
		t.Fatal("All is not a writable profile")
	}
	detailAll := getWithCookie(t, s, fmt.Sprintf("/jobs/%d?profile=all", ids["polina"]), cookie).Body.String()
	if !strings.Contains(detailAll, `href="/jobs?profile=all"`) || !strings.Contains(detailAll, `name="profile" value="polina" readonly`) {
		t.Fatal("All navigation must not alter job ownership")
	}
	filtered := getWithCookie(t, s, "/jobs?profile=all&stage=saved&q=polina", cookie).Body.String()
	if !strings.Contains(filtered, "polina company") || strings.Contains(filtered, "sergey company") || !strings.Contains(filtered, `type="hidden" name="stage" value="saved"`) {
		t.Fatal("All must preserve search and KPI filters")
	}
	r := getWithCookie(t, s, "/jobs?profile=siarhei&stage=interview&cursor=9", cookie)
	u, err := url.Parse(r.Header().Get("Location"))
	if err != nil || r.Code != 303 || u.Query().Get("profile") != "sergey" || u.Query().Get("stage") != "interview" || u.Query().Has("cursor") {
		t.Fatal("retired profile should redirect to active selection", r.Code, r.Header())
	}
	if _, err := s.Store.TrackedJob(ids["siarhei"]); err != nil {
		t.Fatal("historical job must be preserved", err)
	}
}

func TestCreateJobAcceptsOnlySergeyAndPolina(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	for _, profile := range []string{"sergey", "polina", "siarhei", "someone-else", "all"} {
		body := fmt.Sprintf(`{"source_key":"profile-test","profile":%q,"company":"Acme","title":"Role"}`, profile)
		r := apiRequest(t, s, "POST", "/api/v1/jobs", body)
		want := 400
		if isActiveProfile(profile) {
			want = 201
		}
		if r.Code != want {
			t.Errorf("profile %s: status %d want %d: %s", profile, r.Code, want, r.Body.String())
		}
	}
}
