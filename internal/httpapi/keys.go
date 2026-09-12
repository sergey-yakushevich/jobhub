package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// KeyLength is how many hex characters a link key carries. Eight keeps the
// Telegram link short enough to read in one glance:
//
//	https://board.example.com/jobs/74?k=8fe5aaf7
//
// Thirty-two bits sounds small, but the key is per page, not global: guessing
// one exposes one visitor, and guessing is rate limited below. This is a
// strict improvement on the previous scheme, where a single long VIEW_KEY
// opened every page forever once it leaked into a screenshot or a browser
// history sync.
const KeyLength = 8

// Scopes. The scope string is signed along with the id, so a key for one
// visitor cannot be replayed against another, and a visit key cannot be used
// to open the whole-site feed.
func visitScope(id int64) string   { return fmt.Sprintf("v:%d", id) }
func historyScope(id int64) string { return fmt.Sprintf("h:%d", id) }
func siteScope(slug string) string { return "s:" + strings.ToLower(slug) }

// overviewScope covers the all-sites page, and is deliberately its own scope
// rather than a reuse of any site's. The overview shows every site at once and
// links down into each of them, so it is the most privileged page here: one
// site's key must not open it, and — because a link's key rides in its href —
// no page reachable with a lesser key may ever link *up* to it.
func overviewScope() string { return "o:sites" }

// partnerScope covers one affiliate's feed on one site. Scoping by both means
// a link to one affiliate's traffic can never be replayed against another's.
func partnerScope(site, partner string) string {
	return "a:" + strings.ToLower(site) + ":" + strings.ToLower(partner)
}

// LinkKey derives the key for one scope: HMAC-SHA256 under VIEW_KEY, truncated.
func (s *Server) LinkKey(scope string) string {
	mac := hmac.New(sha256.New, []byte(s.ViewKey))
	mac.Write([]byte(scope))
	return hex.EncodeToString(mac.Sum(nil))[:KeyLength]
}

// checkKey validates ?k= for a scope. The full legacy VIEW_KEY is still
// accepted so links already sitting in the Telegram history keep working.
func (s *Server) checkKey(r *http.Request, scope string) bool {
	if s.ViewKey == "" {
		return false
	}
	given := r.URL.Query().Get("k")
	if given == "" {
		return false
	}
	scoped := s.LinkKey(scope)
	ok := subtle.ConstantTimeCompare([]byte(given), []byte(scoped)) == 1 ||
		subtle.ConstantTimeCompare([]byte(given), []byte(s.ViewKey)) == 1
	return ok
}

// guard is the front door for every view page: it rate limits by client
// address, checks the scoped key, and writes a 404 on failure. A wrong key is
// indistinguishable from a missing page, which is what we want.
func (s *Server) guard(w http.ResponseWriter, r *http.Request, scope string) bool {
	ip := clientIP(r)
	if s.keyGuard.blocked(ip, time.Now()) {
		http.NotFound(w, r)
		return false
	}
	if !s.checkKey(r, scope) {
		s.keyGuard.fail(ip, time.Now())
		http.NotFound(w, r)
		return false
	}
	s.keyGuard.pass(ip)
	return true
}

// ---- brute-force throttle ----

const (
	maxFailures  = 20
	failWindow   = time.Minute
	blockPeriod  = 10 * time.Minute
	guardMaxKeys = 4096
)

// keyThrottle blocks an address after too many wrong keys in a short window.
// Eight hex characters is 4.3 billion values; at 20 tries a minute an attacker
// needs longer than the age of the universe, so the short key is safe as long
// as this stays in front of it.
type keyThrottle struct {
	mu      sync.Mutex
	attempt map[string]*attempt
}

type attempt struct {
	count        int
	windowStart  time.Time
	blockedUntil time.Time
}

func (t *keyThrottle) blocked(ip string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.attempt[ip]
	return a != nil && now.Before(a.blockedUntil)
}

func (t *keyThrottle) fail(ip string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.attempt == nil {
		t.attempt = map[string]*attempt{}
	}
	// Cheap bound on memory: a flood from many addresses drops the table
	// rather than growing without limit.
	if len(t.attempt) > guardMaxKeys {
		t.attempt = map[string]*attempt{}
	}
	a := t.attempt[ip]
	if a == nil || now.Sub(a.windowStart) > failWindow {
		a = &attempt{windowStart: now}
		t.attempt[ip] = a
	}
	a.count++
	if a.count >= maxFailures {
		a.blockedUntil = now.Add(blockPeriod)
	}
}

func (t *keyThrottle) pass(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.attempt, ip)
}

// clientIP is the caller's address as seen past the proxy. These pages are
// only ever reached through a reverse proxy that sets X-Forwarded-For.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, ok := strings.Cut(fwd, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
