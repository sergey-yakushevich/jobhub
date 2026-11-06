package workspace

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

const testKey = "test-bearer-key-9f3a"

func testOrigin(t *testing.T, origin string) string {
	t.Helper()
	normalized := normalizeOrigin(origin)
	if normalized == "" {
		t.Fatalf("invalid test origin %q", origin)
	}
	return normalized
}

func newTestServer(t *testing.T, origin string) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "workspace.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st, testKey, origin, false)
}

// doReq runs one request through the real ServeHTTP entry point. Requests get a
// deterministic remote address so throttling tests are reproducible.
func doReq(t *testing.T, s *Server, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	if req.RemoteAddr == "" {
		req.RemoteAddr = "192.0.2.10:40000"
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func newRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "192.0.2.10:40000"
	return req
}

func apiRequest(t *testing.T, s *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return doReq(t, s, req)
}

func apiRequestKey(t *testing.T, s *Server, method, target, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(method, target, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return doReq(t, s, req)
}

func uiRequest(t *testing.T, s *Server, method, target, body string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(method, target, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return doReq(t, s, req)
}

// login posts the browser form and returns the session cookie plus its CSRF
// token. The same origin shape the server expects is used.
func login(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	rec := postLogin(t, s, testKey, s.origin, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("login did not set a session cookie")
	}
	req := newRequest(http.MethodGet, "/jobs", nil)
	req.AddCookie(cookie)
	csrf := s.CSRF(req)
	if csrf == "" {
		t.Fatal("login session has no CSRF token")
	}
	return cookie, csrf
}

func postLogin(t *testing.T, s *Server, key, origin, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"api_key": {key}}
	req := newRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	return doReq(t, s, req)
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not a JSON object: %v; body=%s", err, rec.Body.String())
	}
	return m
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	m := decodeMap(t, rec)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object; body=%s", rec.Body.String())
	}
	code, _ := e["code"].(string)
	return code
}

func createTestJob(t *testing.T, s *Server, body string) (int64, int64) {
	t.Helper()
	rec := apiRequest(t, s, http.MethodPost, "/api/v1/jobs", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create job status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	id, ok := m["id"].(float64)
	if !ok {
		t.Fatalf("create response has no numeric id: %s", rec.Body.String())
	}
	version, _ := m["version"].(float64)
	return int64(id), int64(version)
}

func countFailures(s *Server) int {
	s.muValue.Lock()
	defer s.muValue.Unlock()
	n := 0
	for _, a := range s.failures {
		n += a.count
	}
	return n
}

func fixedClock(s *Server, at time.Time) {
	s.now = func() time.Time { return at }
}
