package httpapi

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

const (
	apiToken = "test-api-token"
	viewKey  = "test-view-key"
)

func testServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, apiToken, viewKey), st
}

func request(t *testing.T, s *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}
