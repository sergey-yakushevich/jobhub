package httpapi

import (
	"bytes"
	"net/http"
	"testing"
)

func TestIconRoutesServeTheirFormat(t *testing.T) {
	s, _ := testServer(t)
	for _, tc := range []struct {
		path, contentType, magic string
	}{
		{"/favicon.svg", "image/svg+xml", "<svg"},
		{"/favicon.png", "image/png", "\x89PNG"},
		{"/apple-touch-icon.png", "image/png", "\x89PNG"},
	} {
		w := request(t, s, "GET", tc.path, "", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", tc.path, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != tc.contentType {
			t.Errorf("%s content type = %q, want %q", tc.path, got, tc.contentType)
		}
		if !bytes.HasPrefix(w.Body.Bytes(), []byte(tc.magic)) {
			t.Errorf("%s does not start with %q", tc.path, tc.magic)
		}
	}
}
