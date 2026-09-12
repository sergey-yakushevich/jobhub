// Package httpapi is the HTTP surface of jobhub: the /api/jobs JSON endpoints
// (Bearer-token gated), the /jobs HTML board (view-key gated), and the /up
// health check.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

type Server struct {
	Store    *store.Store
	APIToken string
	ViewKey  string
	// BasePath is the public URL prefix the app is mounted under (e.g. "/trk"),
	// stripped by the proxy before requests reach us. In-page links must carry
	// it or the browser resolves them against the domain root. Empty = root.
	BasePath string
	// Origin is the public scheme+host (e.g. "https://board.example.com").
	// Only links that leave the site need it; in-page links stay relative.
	Origin   string
	mux      *http.ServeMux
	keyGuard keyThrottle
}

func New(st *store.Store, apiToken, viewKey string) *Server {
	s := &Server{Store: st, APIToken: apiToken, ViewKey: viewKey}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /up", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /favicon.svg", s.handleFaviconSVG)
	mux.HandleFunc("GET /favicon.png", s.handleFaviconPNG)
	mux.HandleFunc("GET /apple-touch-icon.png", s.handleAppleIcon)
	mux.HandleFunc("GET /icons/{name}", s.handleIcon)
	mux.HandleFunc("GET /robots.txt", s.handleRobots)
	mux.Handle("POST /api/jobs", s.auth(http.HandlerFunc(s.handleJobsIngest)))
	mux.Handle("GET /api/jobs", s.auth(http.HandlerFunc(s.handleJobsJSON)))
	mux.Handle("DELETE /api/jobs", s.auth(http.HandlerFunc(s.handleJobsPurge)))
	mux.Handle("DELETE /api/jobs/{id}", s.auth(http.HandlerFunc(s.handleJobDelete)))
	mux.Handle("POST /api/jobs/{id}/applied", s.auth(http.HandlerFunc(s.handleJobApplied)))
	mux.Handle("POST /api/jobs/{id}/rejected", s.auth(http.HandlerFunc(s.handleJobRejected)))
	mux.Handle("POST /api/jobs/{id}/approved", s.auth(http.HandlerFunc(s.handleJobApproved)))
	mux.Handle("POST /api/jobs/{id}/prep", s.auth(http.HandlerFunc(s.handleJobPrep)))
	mux.Handle("POST /api/jobs/{id}/duplicate", s.auth(http.HandlerFunc(s.handleJobDuplicate)))
	mux.HandleFunc("GET /jobs", s.handleJobsBoard)
	mux.HandleFunc("GET /jobs/{id}", s.handleJobShow)
	mux.HandleFunc("GET /jobs/{id}/go", s.handleJobGo)
	mux.HandleFunc("POST /jobs/{id}/applied", s.handleJobAppliedToggle)
	mux.HandleFunc("POST /jobs/{id}/approved", s.handleJobApproveToggle)
	mux.HandleFunc("POST /jobs/{id}/notes", s.handleJobNotes)
	s.mux = mux
	return s
}

// ServeHTTP stamps every response as off-limits to crawlers before routing.
// The board is a private tool behind link keys; no page here has any business
// in a search index, and the header covers even the public bits like /up.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	s.mux.ServeHTTP(w, r)
}

// handleRobots tells well-behaved crawlers the same thing the header does:
// nothing here is for them.
func (s *Server) handleRobots(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
}

// auth requires `Authorization: Bearer <API_TOKEN>`.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.APIToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.APIToken)) != 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// wantsJSON reports whether the caller is the board's own fetch() rather than
// a plain form post; the toggles answer JSON to the first and redirect the
// second, so the buttons keep working with JavaScript off.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
