package workspace

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

type dashboardTile struct {
	Label, URL string
	Count      int
	Selected   bool
}

type dashboardBar struct {
	X, Y, Width, Height int
	Title               string
}

type dashboardView struct {
	Stats              *store.TrackerStats
	Tiles              []dashboardTile
	JobBars, EventBars []dashboardBar
	From, To           string
}

func buildDashboard(stats *store.TrackerStats, profile, stage string, needsYou bool) *dashboardView {
	d := &dashboardView{Stats: stats}
	for _, item := range []struct {
		label, stage string
		count        int
		needsYou     bool
	}{
		{"jobs", "", stats.Total, false},
		{"needs you", "", stats.NeedsYou, true}, {"in progress", "active", stats.Active, false},
		{"offers", "offer", stats.Offers, false}, {"rejected", "rejected", stats.Rejected, false},
		{"hired", "hired", stats.Hired, false},
	} {
		q := url.Values{"profile": {profile}}
		if item.stage != "" {
			q.Set("stage", item.stage)
		}
		if item.needsYou {
			q.Set("needs_you", "true")
		}
		d.Tiles = append(d.Tiles, dashboardTile{Label: item.label, URL: "/jobs?" + q.Encode(), Count: item.count, Selected: stage == item.stage && needsYou == item.needsYou})
	}
	maxJobs, maxEvents := 1, 1
	for _, day := range stats.Days {
		if day.Jobs > maxJobs {
			maxJobs = day.Jobs
		}
		if day.Events > maxEvents {
			maxEvents = day.Events
		}
	}
	for i, day := range stats.Days {
		makeBar := func(n, max int, unit string) dashboardBar {
			height := n * 72 / max
			if n > 0 && height < 2 {
				height = 2
			}
			return dashboardBar{X: i * 20, Y: 72 - height, Width: 16, Height: height, Title: fmt.Sprintf("%s · %d %s", day.Date, n, unit)}
		}
		d.JobBars = append(d.JobBars, makeBar(day.Jobs, maxJobs, "jobs"))
		d.EventBars = append(d.EventBars, makeBar(day.Events, maxEvents, "events"))
	}
	shortDate := func(raw string) string {
		date, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return raw
		}
		return date.Format("01-02")
	}
	d.From, d.To = shortDate(stats.From), shortDate(stats.To)
	return d
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiMethodNotAllowed(w, http.MethodGet)
		return
	}
	q := r.URL.Query()
	for key := range q {
		if key != "profile" {
			writeAPIError(w, invalidQuery("unknown query parameter: "+key))
			return
		}
	}
	profile := strings.ToLower(strings.TrimSpace(q.Get("profile")))
	if profile != "" && !isActiveProfile(profile) {
		writeAPIError(w, invalidQuery("profile must be sergey or polina"))
		return
	}
	stats, err := s.Store.TrackedStats(profile, s.now())
	if err != nil {
		writeAPIError(w, storeError(err))
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
