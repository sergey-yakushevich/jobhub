package workspace

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAPIRejectsNonBearerCredentials(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")

	cases := []struct {
		name string
		req  func() *http.Request
	}{
		{"no credentials", func() *http.Request {
			return newRequest(http.MethodGet, "/api/v1/jobs", nil)
		}},
		{"query key", func() *http.Request {
			return newRequest(http.MethodGet, "/api/v1/jobs?api_key="+testKey, nil)
		}},
		{"wrong bearer", func() *http.Request {
			req := newRequest(http.MethodGet, "/api/v1/jobs", nil)
			req.Header.Set("Authorization", "Bearer nope")
			return req
		}},
		{"basic scheme", func() *http.Request {
			req := newRequest(http.MethodGet, "/api/v1/jobs", nil)
			req.Header.Set("Authorization", "Basic "+testKey)
			return req
		}},
		{"bearer without token", func() *http.Request {
			req := newRequest(http.MethodGet, "/api/v1/jobs", nil)
			req.Header.Set("Authorization", "Bearer")
			return req
		}},
		{"key header", func() *http.Request {
			req := newRequest(http.MethodGet, "/api/v1/jobs", nil)
			req.Header.Set("X-Api-Key", testKey)
			return req
		}},
		{"arbitrary cookie", func() *http.Request {
			req := newRequest(http.MethodGet, "/api/v1/jobs", nil)
			req.Header.Set("Cookie", sessionCookie+"="+testKey)
			return req
		}},
		{"forwarded credentials", func() *http.Request {
			req := newRequest(http.MethodGet, "/api/v1/jobs", nil)
			req.Header.Set("X-Forwarded-User", testKey)
			req.Header.Set("X-Forwarded-Authorization", "Bearer "+testKey)
			return req
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, s, tc.req())
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
			if code := errorCode(t, rec); code != "unauthorized" {
				t.Errorf("error code = %q, want unauthorized", code)
			}
		})
	}

	t.Run("valid bearer case-insensitive", func(t *testing.T) {
		req := newRequest(http.MethodGet, "/api/v1/jobs", nil)
		req.Header.Set("Authorization", "bearer "+testKey)
		rec := doReq(t, s, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("bearer plus query key still rejected", func(t *testing.T) {
		rec := apiRequest(t, s, http.MethodGet, "/api/v1/jobs?api_key="+testKey, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for unknown query parameter; body=%s", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "invalid_query" {
			t.Errorf("error code = %q, want invalid_query", code)
		}
	})

	t.Run("blank configured key fails closed", func(t *testing.T) {
		blank := New(s.Store, "", "http://127.0.0.1:8096", false)
		rec := apiRequestKey(t, blank, http.MethodGet, "/api/v1/jobs", "", testKey)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
		}
	})
}

func TestSessionCookieCannotAuthenticateAPI(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	cookie, csrf := login(t, s)

	req := newRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.AddCookie(cookie)
	rec := doReq(t, s, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("API with session cookie status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}

	// The same cookie is valid on the UI surface, proving the cookie itself is
	// fine and only the API is bearer-only.
	req = newRequest(http.MethodGet, "/ui/jobs", nil)
	req.AddCookie(cookie)
	req.Header.Set("Origin", s.origin)
	req.Header.Set("X-CSRF-Token", csrf)
	if rec := doReq(t, s, req); rec.Code != http.StatusOK {
		t.Fatalf("UI with session cookie status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestLoginCookieAttributesFollowOriginScheme(t *testing.T) {
	for _, tc := range []struct {
		origin string
		secure bool
	}{
		{"http://127.0.0.1:8096", false},
		{"https://jobs.example.test", true},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			s := newTestServer(t, tc.origin)
			rec := postLogin(t, s, testKey, s.origin, "")
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != "/jobs" {
				t.Errorf("Location = %q, want /jobs", loc)
			}
			cookies := rec.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("got %d cookies, want 1", len(cookies))
			}
			c := cookies[0]
			if c.Name != sessionCookie {
				t.Errorf("cookie name = %q, want %q", c.Name, sessionCookie)
			}
			if !c.HttpOnly {
				t.Error("session cookie must be HttpOnly")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("SameSite = %v, want Strict", c.SameSite)
			}
			if c.Secure != tc.secure {
				t.Errorf("Secure = %v, want %v", c.Secure, tc.secure)
			}
			if c.Path != "/" {
				t.Errorf("Path = %q, want /", c.Path)
			}
			if c.MaxAge != int(sessionTTL/time.Second) {
				t.Errorf("MaxAge = %d, want %d", c.MaxAge, int(sessionTTL/time.Second))
			}
			if c.Value == "" || c.Value == testKey {
				t.Error("cookie value must be an opaque token, not the key")
			}
		})
	}

	t.Run("forwarded proto does not make http origin secure", func(t *testing.T) {
		s := newTestServer(t, "http://127.0.0.1:8096")
		req := newRequest(http.MethodPost, "/login", strings.NewReader("api_key="+testKey))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", s.origin)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "jobs.example.test")
		rec := doReq(t, s, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", rec.Code)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookie && c.Secure {
				t.Error("cookie became Secure from a spoofed X-Forwarded-Proto header")
			}
		}
	})
}

func TestLoginRequiresConfiguredOrigin(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")

	cases := []struct {
		name   string
		origin string
		extra  map[string]string
	}{
		{"missing origin", "", nil},
		{"foreign origin", "https://evil.example", nil},
		{"foreign origin with forwarded host", "https://evil.example", map[string]string{
			"X-Forwarded-Host": "127.0.0.1:8096", "X-Forwarded-Proto": "http",
		}},
		{"suffix confusion", "http://127.0.0.1:8096.evil.example", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newRequest(http.MethodPost, "/login", strings.NewReader("api_key="+testKey))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			for k, v := range tc.extra {
				req.Header.Set(k, v)
			}
			rec := doReq(t, s, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
			}
			for _, c := range rec.Result().Cookies() {
				if c.Name == sessionCookie && c.Value != "" {
					t.Fatal("forbidden login issued a session")
				}
			}
		})
	}

	t.Run("unconfigured origin fails closed", func(t *testing.T) {
		none := New(s.Store, testKey, "", false)
		rec := postLogin(t, none, testKey, "http://127.0.0.1:8096", "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
}

func TestLoginThrottlePerRemoteAddress(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	fixedClock(s, now)

	const ip = "198.51.100.7:5555"
	for i := 0; i < loginFailureLimit; i++ {
		rec := postLogin(t, s, "wrong-key", s.origin, ip)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("failure %d status = %d, want 303; body=%s", i+1, rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/login?error=1" {
			t.Fatalf("failure %d Location = %q, want generic error redirect", i+1, loc)
		}
	}
	if got := countFailures(s); got != loginFailureLimit {
		t.Fatalf("recorded failures = %d, want %d", got, loginFailureLimit)
	}

	// Correct key is now refused too: throttling is checked before validation.
	rec := postLogin(t, s, testKey, s.origin, ip)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}

	// A spoofed X-Forwarded-For must not move the throttle to another bucket.
	req := newRequest(http.MethodPost, "/login", strings.NewReader("api_key="+testKey))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.origin)
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	req.Header.Set("X-Real-IP", "203.0.113.99")
	req.RemoteAddr = ip
	if rec := doReq(t, s, req); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed forwarded headers bypassed throttle: status = %d, want 429", rec.Code)
	}

	// Another address has its own window.
	if rec := postLogin(t, s, testKey, s.origin, "198.51.100.8:6666"); rec.Code != http.StatusSeeOther {
		t.Fatalf("other IP status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}

	// The window expires after the failure TTL.
	fixedClock(s, now.Add(loginFailureTTL+time.Second))
	if rec := postLogin(t, s, testKey, s.origin, ip); rec.Code != http.StatusSeeOther {
		t.Fatalf("after TTL status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if got := countFailures(s); got != 0 {
		t.Errorf("failures after successful login = %d, want 0", got)
	}
}

func TestSuccessfulLoginClearsFailures(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	fixedClock(s, time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC))
	const ip = "198.51.100.9:5555"
	for i := 0; i < 3; i++ {
		postLogin(t, s, "wrong", s.origin, ip)
	}
	if got := countFailures(s); got != 3 {
		t.Fatalf("failures = %d, want 3", got)
	}
	if rec := postLogin(t, s, testKey, s.origin, ip); rec.Code != http.StatusSeeOther {
		t.Fatalf("valid login status = %d, want 303", rec.Code)
	}
	if got := countFailures(s); got != 0 {
		t.Errorf("failures after success = %d, want 0", got)
	}
}

func TestSessionExpiryOnRead(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	start := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	fixedClock(s, start)
	cookie, csrf := login(t, s)

	if rec := getWithCookie(t, s, "/jobs", cookie); rec.Code != http.StatusOK {
		t.Fatalf("fresh session status = %d, want 200", rec.Code)
	}

	fixedClock(s, start.Add(sessionTTL+time.Second))

	rec := getWithCookie(t, s, "/jobs", cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expired page status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("expired redirect = %q, want /login", loc)
	}

	req := newRequest(http.MethodGet, "/ui/jobs", nil)
	req.AddCookie(cookie)
	req.Header.Set("Origin", s.origin)
	req.Header.Set("X-CSRF-Token", csrf)
	if rec := doReq(t, s, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired UI status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}

	// The expired session is removed on read.
	s.muValue.Lock()
	n := len(s.sessions)
	s.muValue.Unlock()
	if n != 0 {
		t.Errorf("sessions remaining after expiry = %d, want 0", n)
	}
}

func TestLogoutRetiresSession(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	cookie, csrf := login(t, s)

	t.Run("missing csrf leaves session alive", func(t *testing.T) {
		rec := uiRequest(t, s, http.MethodPost, "/logout", "csrf=", cookie, "", s.origin)
		rec = postLogout(t, s, cookie, "", s.origin)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if rec := getWithCookie(t, s, "/jobs", cookie); rec.Code != http.StatusOK {
			t.Fatalf("session gone after rejected logout: %d", rec.Code)
		}
	})

	t.Run("wrong origin leaves session alive", func(t *testing.T) {
		rec := postLogout(t, s, cookie, csrf, "https://evil.example")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if rec := getWithCookie(t, s, "/jobs", cookie); rec.Code != http.StatusOK {
			t.Fatalf("session gone after wrong-origin logout: %d", rec.Code)
		}
	})

	t.Run("valid logout clears cookie and session", func(t *testing.T) {
		rec := postLogout(t, s, cookie, csrf, s.origin)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "/login" {
			t.Errorf("Location = %q, want /login", loc)
		}
		var cleared *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == sessionCookie {
				cleared = c
			}
		}
		if cleared == nil {
			t.Fatal("logout did not clear the session cookie")
		}
		if cleared.Value != "" || cleared.MaxAge >= 0 {
			t.Errorf("clearing cookie = %q maxage=%d, want empty and negative", cleared.Value, cleared.MaxAge)
		}
		if !cleared.HttpOnly || cleared.SameSite != http.SameSiteStrictMode {
			t.Error("clearing cookie must stay HttpOnly and SameSite=Strict")
		}
		if rec := getWithCookie(t, s, "/jobs", cookie); rec.Code != http.StatusSeeOther {
			t.Fatalf("retired session status = %d, want 303", rec.Code)
		}
	})
}

func TestUIWritesRequireOriginAndCSRF(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	cookie, csrf := login(t, s)
	body := `{"source_key":"ui-1","company":"Acme","title":"Engineer"}`

	cases := []struct {
		name   string
		origin string
		csrf   string
		want   int
	}{
		{"no origin or csrf", "", "", http.StatusForbidden},
		{"foreign origin", "https://evil.example", csrf, http.StatusForbidden},
		{"origin ok csrf missing", s.origin, "", http.StatusForbidden},
		{"origin ok csrf wrong", s.origin, "not-the-token", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := uiRequest(t, s, http.MethodPost, "/ui/jobs", body, cookie, tc.csrf, tc.origin)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "forbidden" {
				t.Errorf("error code = %q, want forbidden", code)
			}
		})
	}

	t.Run("spoofed forwarded origin is ignored", func(t *testing.T) {
		req := newRequest(http.MethodPost, "/ui/jobs", strings.NewReader(body))
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("X-Forwarded-Host", "127.0.0.1:8096")
		req.Header.Set("X-Forwarded-Proto", "http")
		rec := doReq(t, s, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("valid UI write succeeds", func(t *testing.T) {
		rec := uiRequest(t, s, http.MethodPost, "/ui/jobs", body, cookie, csrf, s.origin)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("referer is a same-origin fallback only", func(t *testing.T) {
		req := newRequest(http.MethodPost, "/ui/jobs", strings.NewReader(`{"source_key":"ui-2","company":"Beta","title":"Dev"}`))
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("Referer", s.origin+"/jobs")
		if rec := doReq(t, s, req); rec.Code != http.StatusCreated {
			t.Fatalf("same-origin referer status = %d, want 201; body=%s", rec.Code, rec.Body.String())
		}

		req = newRequest(http.MethodPost, "/ui/jobs", strings.NewReader(`{"source_key":"ui-3","company":"Gamma","title":"Dev"}`))
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("Referer", "https://evil.example/jobs")
		if rec := doReq(t, s, req); rec.Code != http.StatusForbidden {
			t.Fatalf("foreign referer status = %d, want 403", rec.Code)
		}
	})

	t.Run("UI GET without cookie is unauthorized", func(t *testing.T) {
		rec := doReq(t, s, newRequest(http.MethodGet, "/ui/jobs", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("bearer does not authorize UI", func(t *testing.T) {
		req := newRequest(http.MethodGet, "/ui/jobs", nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rec := doReq(t, s, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
		}
	})
}

func getWithCookie(t *testing.T, s *Server, target string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(http.MethodGet, target, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return doReq(t, s, req)
}

func postLogout(t *testing.T, s *Server, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(http.MethodPost, "/logout", strings.NewReader("csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return doReq(t, s, req)
}
