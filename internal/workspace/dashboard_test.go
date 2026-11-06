package workspace

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

func TestStatsAPIAuthScopeAndClickableCounts(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	for _, stage := range []string{"saved", "applied", "talking", "interview", "offer", "hired", "rejected", "archived"} {
		_, _, err := s.Store.CreateTrackedJob(store.TrackedJob{SourceKey: stage, Profile: "sergey", Company: "Acme", Title: stage, Stage: stage, NeedsYou: true}, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, profile := range []string{"polina", "siarhei"} {
		_, _, err := s.Store.CreateTrackedJob(store.TrackedJob{SourceKey: profile, Profile: profile, Company: "Acme", Title: "Designer", Stage: "saved"}, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if r := doReq(t, s, newRequest("GET", "/api/v1/stats", nil)); r.Code != 401 {
		t.Fatal("stats must require bearer", r.Code)
	}
	cookie, _ := login(t, s)
	if r := getWithCookie(t, s, "/api/v1/stats", cookie); r.Code != 401 {
		t.Fatal("cookie must not authorize agent stats", r.Code)
	}
	if r := getWithCookie(t, s, "/ui/stats?profile=polina", cookie); r.Code != 200 {
		t.Fatal("browser stats missing", r.Code)
	}
	if r := doReq(t, s, newRequest("GET", "/ui/stats", nil)); r.Code != 401 {
		t.Fatal("browser stats needs a session", r.Code)
	}
	var stats store.TrackerStats
	r := apiRequest(t, s, "GET", "/api/v1/stats?profile=sergey", "")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if err := json.Unmarshal(r.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Total != 8 || stats.NeedsYou != 5 || stats.Active != 3 || len(stats.Days) != 30 {
		t.Fatalf("wrong overview %+v", stats)
	}
	for _, tc := range []struct {
		query string
		want  int
	}{{"stage=active", stats.Active}, {"needs_you=true", stats.NeedsYou}, {"stage=offer", stats.Offers}} {
		r = apiRequest(t, s, "GET", "/api/v1/jobs?profile=sergey&"+tc.query, "")
		var page store.TrackerPage
		if r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
		if err := json.Unmarshal(r.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != tc.want {
			t.Errorf("%s: list %d != tile %d", tc.query, len(page.Items), tc.want)
		}
	}
	for _, query := range []string{"profile=siarhei", "stage=saved", "limit=50"} {
		if r := apiRequest(t, s, "GET", "/api/v1/stats?"+query, ""); r.Code != 400 {
			t.Errorf("invalid stats query %s accepted", query)
		}
	}
	if r := apiRequest(t, s, "POST", "/api/v1/stats", `{}`); r.Code != 405 {
		t.Fatal("stats are read only", r.Code)
	}
	if r := apiRequest(t, s, "POST", "/api/v1/jobs", `{"source_key":"bad-stage","company":"Acme","title":"Role","stage":"active"}`); r.Code != 400 {
		t.Fatal("active filter must not become a stored stage", r.Code)
	}
	r = apiRequest(t, s, "GET", "/api/v1/stats", "")
	json.Unmarshal(r.Body.Bytes(), &stats)
	if stats.Total != 9 {
		t.Fatalf("workspace total includes retired profile: %d", stats.Total)
	}
}

func TestDashboardOverviewSurvivesListFiltersAndSafeChartGeometry(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	for i := 0; i < 3; i++ {
		createTestJob(t, s, fmt.Sprintf(`{"source_key":"dash-%d","company":"Acme","title":"Role","stage":"applied"}`, i))
	}
	cookie, _ := login(t, s)
	r := getWithCookie(t, s, "/jobs?profile=sergey&stage=offer", cookie)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	html := r.Body.String()
	for _, wanted := range []string{`class="tiles"`, `class="chart"`, `data-chart-series="events"`, `class="breakdowns"`, `<span class="n">3</span>`, `No jobs match`} {
		if !strings.Contains(html, wanted) {
			t.Errorf("dashboard missing %s", wanted)
		}
	}
	if strings.Contains(html, `style=`) {
		t.Fatal("chart must not rely on inline styles forbidden by CSP")
	}
	stats, err := s.Store.TrackedStats("sergey", s.now())
	if err != nil {
		t.Fatal(err)
	}
	dash := buildDashboard(stats, "sergey", "", false)
	if len(dash.Tiles) != 6 || len(dash.JobBars) != 30 || len(dash.EventBars) != 30 {
		t.Fatal("incomplete dashboard")
	}
	for _, bar := range append(dash.JobBars, dash.EventBars...) {
		if bar.X < 0 || bar.Y < 0 || bar.Height < 0 || bar.Height > 72 || bar.Y+bar.Height != 72 {
			t.Fatalf("invalid chart geometry %+v", bar)
		}
	}
	for _, tile := range dash.Tiles {
		if !strings.Contains(tile.URL, "profile=sergey") {
			t.Fatal("KPI link loses profile", tile.URL)
		}
	}
}
