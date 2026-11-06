package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func browserSessionStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	if err := s.MigrateBrowserSessions(); err != nil {
		t.Fatalf("migrate browser sessions: %v", err)
	}
	return s
}

// sessionHash builds a deterministic valid 64-lowercase-hex value.
func sessionHash(n int) string { return fmt.Sprintf("%064x", n) }

func TestBrowserSessionPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	token, scope := sessionHash(1), sessionHash(2)
	expires := t0.Add(time.Hour)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.MigrateBrowserSessions(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := s.SaveBrowserSession(token, scope, "csrf-1", expires, t0); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	// A second migration of the same file must be harmless.
	if err := s.MigrateBrowserSessions(); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	csrf, gotExpires, err := s.ReadBrowserSession(token, scope, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if csrf != "csrf-1" {
		t.Fatalf("csrf = %q", csrf)
	}
	if !gotExpires.Equal(expires) || gotExpires.Location() != time.UTC {
		t.Fatalf("expires = %v, want %v UTC", gotExpires, expires)
	}
}

func TestBrowserSessionScopeMismatchAndDeletion(t *testing.T) {
	s := browserSessionStore(t)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	token, scope, other := sessionHash(10), sessionHash(11), sessionHash(12)
	if err := s.SaveBrowserSession(token, scope, "csrf", t0.Add(time.Hour), t0); err != nil {
		t.Fatalf("save: %v", err)
	}

	if _, _, err := s.ReadBrowserSession(token, other, t0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wrong scope err = %v, want ErrNoRows", err)
	}
	// The mismatch must not have deleted the valid row.
	if _, _, err := s.ReadBrowserSession(token, scope, t0); err != nil {
		t.Fatalf("valid read after mismatch: %v", err)
	}

	if err := s.DeleteBrowserSession(token, other); err != nil {
		t.Fatalf("delete wrong scope: %v", err)
	}
	if _, _, err := s.ReadBrowserSession(token, scope, t0); err != nil {
		t.Fatalf("wrong-scope delete removed row: %v", err)
	}

	if err := s.DeleteBrowserSession(token, scope); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := s.ReadBrowserSession(token, scope, t0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read after delete err = %v, want ErrNoRows", err)
	}
}

func TestBrowserSessionExpiryAtBoundaryIsCleaned(t *testing.T) {
	s := browserSessionStore(t)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	token, scope := sessionHash(20), sessionHash(21)
	expires := t0.Add(time.Minute)
	if err := s.SaveBrowserSession(token, scope, "csrf", expires, t0); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, _, err := s.ReadBrowserSession(token, scope, expires); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read at exact expiry err = %v, want ErrNoRows", err)
	}
	// The expired token itself is gone, so its row is not left to expire later.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM browser_sessions WHERE token_hash = ?`, token).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expired row still present: %d", n)
	}
}

func TestBrowserSessionSavePrunesExpiredAndRejectsReplay(t *testing.T) {
	s := browserSessionStore(t)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	stale, scope := sessionHash(30), sessionHash(31)
	if err := s.SaveBrowserSession(stale, scope, "old", t0.Add(time.Minute), t0); err != nil {
		t.Fatalf("save stale: %v", err)
	}
	fresh := sessionHash(32)
	if err := s.SaveBrowserSession(fresh, scope, "new", t0.Add(time.Hour), t0.Add(2*time.Minute)); err != nil {
		t.Fatalf("save fresh: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM browser_sessions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired rows not pruned on save: %d rows", n)
	}

	// Same hash again is a conflict, not a silent overwrite of the CSRF secret.
	if err := s.SaveBrowserSession(fresh, scope, "replaced", t0.Add(2*time.Hour), t0); err == nil {
		t.Fatal("duplicate token save succeeded, want error")
	}
	csrf, _, err := s.ReadBrowserSession(fresh, scope, t0)
	if err != nil || csrf != "new" {
		t.Fatalf("row changed by failed replay: csrf=%q err=%v", csrf, err)
	}
}

func TestBrowserSessionBoundedEviction(t *testing.T) {
	s := browserSessionStore(t)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	scope := sessionHash(99)
	const total = browserSessionMaxRows + 1
	for i := 0; i < total; i++ {
		if err := s.SaveBrowserSession(sessionHash(1000+i), scope, "csrf",
			t0.Add(time.Duration(i+1)*time.Minute), t0); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM browser_sessions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != browserSessionMaxRows {
		t.Fatalf("rows = %d, want %d", n, browserSessionMaxRows)
	}
	// The earliest expiry lost; the newest one survived.
	if _, _, err := s.ReadBrowserSession(sessionHash(1000), scope, t0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("earliest session still present: %v", err)
	}
	if _, _, err := s.ReadBrowserSession(sessionHash(1000+total-1), scope, t0); err != nil {
		t.Fatalf("newest session missing: %v", err)
	}
}

func TestBrowserSessionRejectsInvalidInput(t *testing.T) {
	s := browserSessionStore(t)
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	good := sessionHash(7)
	expires := t0.Add(time.Hour)

	cases := []struct {
		name               string
		token, scope, csrf string
		expires            time.Time
	}{
		{"short token", "abc", good, "csrf", expires},
		{"uppercase token", strings.Repeat("AB", 32), good, "csrf", expires},
		{"non-hex token", strings.Repeat("g", 64), good, "csrf", expires},
		{"empty token", "", good, "csrf", expires},
		{"bad scope", good, "not-hex", "csrf", expires},
		{"empty csrf", good, good, "", expires},
		{"expires equal now", good, good, "csrf", t0},
		{"expires before now", good, good, "csrf", t0.Add(-time.Second)},
	}
	for _, tc := range cases {
		if err := s.SaveBrowserSession(tc.token, tc.scope, tc.csrf, tc.expires, t0); !errors.Is(err, ErrBrowserSessionValidation) {
			t.Fatalf("%s: err = %v, want ErrBrowserSessionValidation", tc.name, err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM browser_sessions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("invalid saves wrote %d rows", n)
	}
}
