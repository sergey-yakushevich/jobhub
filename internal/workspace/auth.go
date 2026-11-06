package workspace

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "jobhub_session"
	sessionTTL    = 365 * 24 * time.Hour

	// Login throttling: ten failures per quarter hour, per remote address.
	loginFailureLimit = 10
	loginFailureTTL   = 15 * time.Minute
	maxFailureEntries = 1024
)

var timeNow = time.Now

type session struct {
	csrf    string
	expires time.Time
}

type attempt struct {
	count int
	reset time.Time
	last  time.Time
}

// validKey compares a presented key to the configured one in constant time
// over hashes. A blank configured key never matches, whatever is presented.
func (s *Server) validKey(presented string) bool {
	if s.key == "" || presented == "" {
		return false
	}
	sum := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(s.keyHash[:], sum[:]) == 1
}

func sessionKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable for auth; panic beats issuing a
		// predictable token.
		panic("workspace: crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func (s *Server) cookieValue(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// session resolves and expires-on-read the current cookie session.
func (s *Server) session(r *http.Request) *session {
	token := s.cookieValue(r)
	if token == "" || s.key == "" {
		return nil
	}
	csrf, expires, err := s.Store.ReadBrowserSession(sessionKey(token), s.authScope, s.now())
	if err != nil {
		return nil
	}
	return &session{csrf: csrf, expires: expires}
}

// CSRF returns the signed-in session's CSRF token, or "" when there is no
// live session. Pages embed it in forms and scripts send it in a header.
func (s *Server) CSRF(r *http.Request) string {
	if sess := s.session(r); sess != nil {
		return sess.csrf
	}
	return ""
}

func (s *Server) newSession(w http.ResponseWriter) error {
	token := newToken()
	csrf := newToken()
	now := s.now()
	if err := s.Store.SaveBrowserSession(sessionKey(token), s.authScope, csrf, now.Add(sessionTTL), now); err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL / time.Second),
		Expires:  now.Add(sessionTTL),
	})
	return nil
}

func (s *Server) destroySession(w http.ResponseWriter, r *http.Request) error {
	if token := s.cookieValue(r); token != "" {
		if err := s.Store.DeleteBrowserSession(sessionKey(token), s.authScope); err != nil {
			return err
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
	return nil
}

// originMatches enforces the fixed public origin. Origin wins when present;
// a Referer is accepted only as a same-origin fallback. Forwarded host/proto
// headers are never consulted.
func (s *Server) originMatches(r *http.Request) bool {
	if !s.originOK {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		return o == s.origin
	}
	ref := r.Header.Get("Referer")
	if ref == "" {
		return false
	}
	u, err := url.Parse(ref)
	if err != nil {
		return false
	}
	return u.Scheme+"://"+u.Host == s.origin
}

func csrfMatches(presented, want string) bool {
	if presented == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

// bearerToken extracts an Authorization: Bearer token. Tokens in cookies or
// query parameters are never accepted for the API.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func (s *Server) apiAuthorized(r *http.Request) bool {
	return s.validKey(bearerToken(r))
}

// clientIP strips the port from RemoteAddr. X-Forwarded-For is deliberately
// ignored: this is a local, single-origin service and the header is spoofable.
func clientIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return strings.TrimSpace(remoteAddr)
}

// allowLogin reports whether another failed attempt may be recorded.
func (s *Server) allowLogin(ip string) bool {
	s.muValue.Lock()
	defer s.muValue.Unlock()
	a, ok := s.failures[ip]
	if !ok {
		return true
	}
	now := s.now()
	if !now.Before(a.reset) {
		delete(s.failures, ip)
		return true
	}
	return a.count < loginFailureLimit
}

func (s *Server) recordLoginFailure(ip string) {
	s.muValue.Lock()
	defer s.muValue.Unlock()
	now := s.now()
	a, ok := s.failures[ip]
	if !ok || !now.Before(a.reset) {
		if len(s.failures) >= maxFailureEntries {
			s.pruneFailuresLocked(now)
		}
		a = &attempt{reset: now.Add(loginFailureTTL)}
		s.failures[ip] = a
	}
	a.count++
	a.last = now
}

func (s *Server) clearLoginFailures(ip string) {
	s.muValue.Lock()
	defer s.muValue.Unlock()
	delete(s.failures, ip)
}

// pruneFailuresLocked drops expired windows, then the least recently seen
// entry if the map is still full. Everything stays bounded.
func (s *Server) pruneFailuresLocked(now time.Time) {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, v := range s.failures {
		if !now.Before(v.reset) {
			delete(s.failures, k)
			continue
		}
		if first || v.last.Before(oldest) {
			oldestKey, oldest, first = k, v.last, false
		}
	}
	if len(s.failures) >= maxFailureEntries && oldestKey != "" {
		delete(s.failures, oldestKey)
	}
}

// handleLogin authenticates the browser form. Failures are generic, redirect
// back to the login page with ?error=1, and are throttled per remote address.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.originMatches(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ip := clientIP(r.RemoteAddr)
	if !s.allowLogin(ip) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		s.recordLoginFailure(ip)
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	if !s.validKey(r.PostForm.Get("api_key")) {
		s.recordLoginFailure(ip)
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}

	s.clearLoginFailures(ip)
	if err := s.newSession(w); err != nil {
		http.Error(w, "Unable to sign in", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/jobs", http.StatusSeeOther)
}

// handleLogout needs the live cookie, the fixed origin and the session CSRF
// token (form field or header), then retires the session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !s.originMatches(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	token := r.PostFormValue("csrf")
	if token == "" {
		token = r.Header.Get("X-CSRF-Token")
	}
	if !csrfMatches(token, sess.csrf) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := s.destroySession(w, r); err != nil {
		http.Error(w, "Unable to sign out", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// csrfOK is the browser-JSON write gate: header token must match the session.
func (s *Server) csrfOK(r *http.Request, sess *session) bool {
	return csrfMatches(r.Header.Get("X-CSRF-Token"), sess.csrf)
}

// isTooLarge reports MaxBytesReader overflow as a 413.
func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// parseLimit parses a positive limit in [1,100]. Empty means the store default.
func parseLimit(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 100 {
		return 0, errors.New("limit must be an integer between 1 and 100")
	}
	return n, nil
}
