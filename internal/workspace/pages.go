package workspace

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

//go:embed templates/*.html assets/* openapi.json
var webFiles embed.FS

var pageTemplates = template.Must(template.New("pages").Funcs(template.FuncMap{
	"label": func(s string) string { return strings.ReplaceAll(s, "_", " ") },
	"initial": func(s string) string {
		for _, r := range s {
			return strings.ToUpper(string(r))
		}
		return "?"
	},
	"salary": salaryLabel,
	"fit": func(n *float64) string {
		if n == nil {
			return "—"
		}
		return strconv.FormatFloat(*n, 'f', -1, 64)
	},
	"when": func(s string) string {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return "Not set"
		}
		return t.Format("02 Jan · 15:04 MST")
	},
	"day": func(s string) string {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return "Earlier"
		}
		return t.Format("Monday, 02 January 2006")
	},
	"clock": func(s string) string {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return ""
		}
		return t.Format("15:04")
	},
	"selected": func(a, b string) bool { return a == b },
	"stages": func() []string {
		return []string{"saved", "applied", "talking", "interview", "offer", "hired", "rejected", "archived"}
	},
	"remotes":  func() []string { return []string{"unknown", "remote", "hybrid", "onsite"} },
	"profiles": activeProfiles,
	"boardURL": profileBoardURL,
	"jobURL": func(id int64, profile string) string {
		path := "/jobs/" + strconv.FormatInt(id, 10)
		if profile == "all" {
			path += "?profile=all"
		}
		return path
	},
	"join": func(v []string) string { return strings.Join(v, "\n") },
	"json": func(v any) string { b, _ := json.Marshal(v); return string(b) },
}).ParseFS(webFiles, "templates/*.html"))

func activeProfiles() []string { return []string{"sergey", "polina"} }

func isActiveProfile(profile string) bool {
	return profile == "sergey" || profile == "polina"
}

func selectedProfile(raw string) string {
	profile := strings.ToLower(strings.TrimSpace(raw))
	if isActiveProfile(profile) || profile == "all" {
		return profile
	}
	return "sergey"
}

func profileBoardURL(profile string) string {
	return "/jobs?profile=" + url.QueryEscape(selectedProfile(profile))
}

type pageData struct {
	Title, CSRF, Error            string
	Demo                          bool
	Jobs                          []store.TrackedJob
	Job                           *store.TrackedJob
	Events                        []store.TimelineEvent
	NextCursor, NextURL           string
	Query, Profile, Stage, Remote string
	NeedsYou                      bool
	Dashboard                     *dashboardView
}

func salaryLabel(j store.TrackedJob) string {
	if j.SalaryMin == nil && j.SalaryMax == nil {
		return "Salary not listed"
	}
	format := func(n *float64) string {
		if n == nil {
			return ""
		}
		v := strconv.FormatFloat(*n, 'f', 0, 64)
		for i := len(v) - 3; i > 0; i -= 3 {
			v = v[:i] + "," + v[i:]
		}
		return v
	}
	amount := format(j.SalaryMin)
	if j.SalaryMin == nil {
		amount = "Up to " + format(j.SalaryMax)
	} else if j.SalaryMax == nil {
		amount += "+"
	} else if *j.SalaryMin != *j.SalaryMax {
		amount += "–" + format(j.SalaryMax)
	}
	if j.Currency != "" {
		amount += " " + j.Currency
	}
	if j.SalaryPeriod != "" {
		amount += " / " + j.SalaryPeriod
	}
	return amount
}

func (s *Server) render(w http.ResponseWriter, name string, d pageData) {
	d.Demo = s.Demo
	d.Profile = selectedProfile(d.Profile)
	var body bytes.Buffer
	if err := pageTemplates.ExecuteTemplate(&body, name, d); err != nil {
		http.Error(w, "Unable to render page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body.Bytes())
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	profile := selectedProfile(q.Get("profile"))
	if q.Get("profile") != "" && q.Get("profile") != profile {
		q.Set("profile", profile)
		q.Del("cursor")
		http.Redirect(w, r, "/jobs?"+q.Encode(), http.StatusSeeOther)
		return
	}
	q.Set("profile", profile)
	d := pageData{Title: "Jobs", CSRF: s.CSRF(r), Query: q.Get("q"), Profile: profile, Stage: q.Get("stage"), Remote: q.Get("remote"), NeedsYou: q.Get("needs_you") == "true"}
	storeProfile := profile
	if profile == "all" {
		storeProfile = ""
	}
	before, _ := strconv.ParseInt(q.Get("cursor"), 10, 64)
	p, err := s.Store.ListTrackedJobs(store.TrackerFilter{Profile: storeProfile, ActiveProfilesOnly: true, ExcludeArchived: d.Stage != "archived", Stage: d.Stage, Remote: d.Remote, Query: d.Query, NeedsYou: d.NeedsYou, Limit: 50, BeforeID: before})
	if err != nil {
		http.Error(w, "Unable to load jobs", 500)
		return
	}
	d.Jobs = p.Items
	stats, err := s.Store.TrackedStats(storeProfile, s.now())
	if err != nil {
		http.Error(w, "Unable to load statistics", http.StatusInternalServerError)
		return
	}
	d.Dashboard = buildDashboard(stats, profile, d.Stage, d.NeedsYou)
	if p.NextCursor != "" {
		q.Set("cursor", p.NextCursor)
		d.NextURL = "/jobs?" + q.Encode()
	}
	s.render(w, "board", d)
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	j, err := s.Store.TrackedJob(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	p, err := s.Store.TimelineEvents(id, 100, before)
	if err != nil {
		http.Error(w, "Unable to load timeline", 500)
		return
	}
	sort.SliceStable(p.Items, func(i, k int) bool {
		a, _ := time.Parse(time.RFC3339, p.Items[i].HappenedAt)
		b, _ := time.Parse(time.RFC3339, p.Items[k].HappenedAt)
		return a.After(b)
	})
	d := pageData{Title: j.Company + " · " + j.Title, CSRF: s.CSRF(r), Profile: j.Profile, Job: j, Events: p.Items, NextCursor: p.NextCursor}
	if r.URL.Query().Get("profile") == "all" {
		d.Profile = "all"
	}
	if p.NextCursor != "" {
		d.NextURL = "/jobs/" + strconv.FormatInt(id, 10) + "?cursor=" + url.QueryEscape(p.NextCursor)
		if d.Profile == "all" {
			d.NextURL += "&profile=all"
		}
	}
	s.render(w, "job", d)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	d := pageData{Title: "Sign in"}
	if r.URL.Query().Get("error") != "" {
		d.Error = "That key didn't work. Please try again."
	}
	s.render(w, "login", d)
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	s.render(w, "docs", pageData{Title: "Agent API", CSRF: s.CSRF(r), Profile: r.URL.Query().Get("profile")})
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != "app.css" && name != "app.js" {
		http.NotFound(w, r)
		return
	}
	b, err := webFiles.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	b, err := webFiles.ReadFile("openapi.json")
	if err != nil {
		http.Error(w, "Specification unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}
