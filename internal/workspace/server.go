// Package workspace is the local-only web workspace for JobHub: a cookie
// authenticated board for people, and a bearer-token JSON API for agents.
// Everything is same-origin, no-store and fail-closed; there is deliberately
// no legacy surface mounted here.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

// Server owns the HTTP surface. Store and Demo are read by the page layer;
// the auth state stays private.
type Server struct {
	Store *store.Store
	Demo  bool

	key       string
	keyHash   [sha256.Size]byte
	origin    string
	originOK  bool
	secure    bool
	authScope string
	failures  map[string]*attempt
	muValue   sync.Mutex
	now       func() time.Time
}

// New builds a server. A blank key means nothing authenticates (fail closed).
// origin is the single public origin this workspace trusts, for example
// http://127.0.0.1:8096; browser writes require it and its scheme decides
// whether the session cookie is Secure.
func New(st *store.Store, key, origin string, demo bool) *Server {
	s := &Server{
		Store:    st,
		Demo:     demo,
		key:      key,
		origin:   normalizeOrigin(origin),
		failures: make(map[string]*attempt),
		now:      timeNow,
	}
	s.originOK = s.origin != ""
	s.secure = strings.HasPrefix(s.origin, "https://")
	if key != "" {
		s.keyHash = sha256.Sum256([]byte(key))
		scope := sha256.Sum256([]byte("jobhub browser sessions v1\x00" + s.origin + "\x00" + key))
		s.authScope = hex.EncodeToString(scope[:])
	}
	return s
}

// normalizeOrigin accepts only a bare http(s) origin. Anything with a path,
// query, fragment or user info is refused rather than papered over.
func normalizeOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	if u.Path != "" && u.Path != "/" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// ServeHTTP is the only entry point: it applies security headers before any
// routing, then dispatches by exact path so /api/v1 can answer every error as
// JSON without leaking the mux's plain-text defaults.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.securityHeaders(w)
	path := r.URL.Path

	switch {
	case path == "/up":
		if r.Method != http.MethodGet {
			plainMethodNotAllowed(w, http.MethodGet)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return

	case path == "/":
		if r.Method != http.MethodGet {
			plainMethodNotAllowed(w, http.MethodGet)
			return
		}
		s.requirePage(w, r, s.handleBoard)
		return

	case path == "/jobs":
		if r.Method != http.MethodGet {
			plainMethodNotAllowed(w, http.MethodGet)
			return
		}
		s.requirePage(w, r, s.handleBoard)
		return

	case strings.HasPrefix(path, "/jobs/"):
		if r.Method != http.MethodGet {
			plainMethodNotAllowed(w, http.MethodGet)
			return
		}
		if id := strings.TrimPrefix(path, "/jobs/"); id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		r.SetPathValue("id", strings.TrimPrefix(path, "/jobs/"))
		s.requirePage(w, r, s.handleJob)
		return

	case path == "/login":
		switch r.Method {
		case http.MethodGet:
			s.handleLoginPage(w, r)
		case http.MethodPost:
			s.handleLogin(w, r)
		default:
			plainMethodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
		return

	case path == "/logout":
		if r.Method != http.MethodPost {
			plainMethodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleLogout(w, r)
		return

	case path == "/api-docs":
		if r.Method != http.MethodGet {
			plainMethodNotAllowed(w, http.MethodGet)
			return
		}
		s.requirePage(w, r, s.handleDocs)
		return

	case strings.HasPrefix(path, "/assets/"):
		if r.Method != http.MethodGet {
			plainMethodNotAllowed(w, http.MethodGet)
			return
		}
		name := strings.TrimPrefix(path, "/assets/")
		if !publicAsset(name) {
			http.NotFound(w, r)
			return
		}
		r.SetPathValue("name", name)
		s.handleAssets(w, r)
		return

	case strings.HasPrefix(path, "/api/v1"):
		s.serveAPI(w, r)
		return

	case strings.HasPrefix(path, "/ui/"):
		s.serveUI(w, r)
		return

	case strings.HasPrefix(path, "/api/"):
		// Old weak endpoints are gone, not aliased.
		writeAPIError(w, errNotFound("unknown endpoint"))
		return
	}

	http.NotFound(w, r)
}

// securityHeaders is applied to every response, including errors and redirects.
func (s *Server) securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	// Native POST forms inherit this policy. no-referrer makes browsers send
	// Origin: null, breaking our strict login/logout origin checks. same-origin
	// retains same-site form provenance without leaking URLs to external sites.
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; "+
			"frame-ancestors 'none'")
}

// publicAsset allows only a single file name with a css or js suffix: no
// traversal, no directories, nothing executable beyond the page's own assets.
func publicAsset(name string) bool {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return false
	}
	return strings.HasSuffix(name, ".css") || strings.HasSuffix(name, ".js")
}

// requirePage gates server-rendered pages behind a live session cookie.
func (s *Server) requirePage(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	if s.session(r) == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	next(w, r)
}

func plainMethodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func cleanSegment(v string) string {
	if v == "" || strings.Contains(v, "/") {
		return ""
	}
	return v
}

func setJobETag(w http.ResponseWriter, version int64) {
	w.Header().Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
}
