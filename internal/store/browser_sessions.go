package store

// Browser session tokens: the HTTP layer mints a random token, hashes it, and
// stores only the hash plus the CSRF secret and the scope it is valid for. No
// raw cookie, API key or token ever reaches this table, so a database read
// cannot be replayed as a login. Every write is transactional and every read
// fails closed: a mismatch of token or scope is a miss, never a lookup that
// keeps guessing.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrBrowserSessionValidation covers every rejected input. Callers must treat
// the session as not created; nothing is written before validation passes.
var ErrBrowserSessionValidation = errors.New("browser session: invalid")

const (
	// browserSessionMaxRows bounds the table so a token flood cannot fill the
	// disk. The oldest-expiring rows are evicted first, never the fresh one.
	browserSessionMaxRows = 1024
	browserSessionHashLen = 64
)

// MigrateBrowserSessions creates the session table. DDL is CREATE IF NOT
// EXISTS, so calling it on every boot is harmless.
func (s *Store) MigrateBrowserSessions() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS browser_sessions (
  token_hash TEXT PRIMARY KEY,
  auth_scope TEXT NOT NULL,
  csrf       TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_browser_sessions_expires ON browser_sessions(expires_at);
`); err != nil {
		return err
	}
	return tx.Commit()
}

// validSessionHash accepts exactly 64 lowercase hex characters: a SHA-256 hash
// in the one encoding the API is allowed to produce.
func validSessionHash(s string) bool {
	if len(s) != browserSessionHashLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// SaveBrowserSession stores one session hash. It prunes rows that are already
// expired, inserts (never upserts — a duplicate hash is a bug, not a rotation),
// and keeps the table bounded by evicting the earliest-expiring rows, always
// sparing the row just written.
func (s *Store) SaveBrowserSession(tokenHash, authScope, csrf string, expires, now time.Time) error {
	tokenHash = strings.TrimSpace(tokenHash)
	authScope = strings.TrimSpace(authScope)
	if !validSessionHash(tokenHash) {
		return fmt.Errorf("%w: token_hash must be 64 lowercase hex characters", ErrBrowserSessionValidation)
	}
	if !validSessionHash(authScope) {
		return fmt.Errorf("%w: auth_scope must be 64 lowercase hex characters", ErrBrowserSessionValidation)
	}
	if csrf == "" {
		return fmt.Errorf("%w: csrf is required", ErrBrowserSessionValidation)
	}
	if !expires.After(now) {
		return fmt.Errorf("%w: expires_at must be after now", ErrBrowserSessionValidation)
	}

	nowUnix := now.UTC().Unix()
	expiresUnix := expires.UTC().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM browser_sessions WHERE expires_at <= ?`, nowUnix); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO browser_sessions (token_hash, auth_scope, csrf, expires_at)
VALUES (?, ?, ?, ?)`, tokenHash, authScope, csrf, expiresUnix); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM browser_sessions`).Scan(&count); err != nil {
		return err
	}
	if count > browserSessionMaxRows {
		// Evict the nearest expiry first and never the row just inserted, so a
		// successful save is not immediately undone by its own bound.
		if _, err := tx.Exec(`
DELETE FROM browser_sessions WHERE token_hash IN (
  SELECT token_hash FROM browser_sessions
   WHERE token_hash <> ?
   ORDER BY expires_at ASC, token_hash ASC
   LIMIT ?
)`, tokenHash, count-browserSessionMaxRows); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReadBrowserSession returns the CSRF secret for a live session. Only a row
// matching both token hash and scope can be returned; a scope mismatch is a
// plain miss and never deletes the valid row, so key rotation or a stale
// caller cannot log everyone out. An expired selected token is cleaned up
// before reporting sql.ErrNoRows.
func (s *Store) ReadBrowserSession(tokenHash, authScope string, now time.Time) (string, time.Time, error) {
	tokenHash = strings.TrimSpace(tokenHash)
	authScope = strings.TrimSpace(authScope)
	if !validSessionHash(tokenHash) || !validSessionHash(authScope) {
		return "", time.Time{}, sql.ErrNoRows
	}
	var csrf string
	var expiresUnix int64
	err := s.db.QueryRow(`
SELECT csrf, expires_at FROM browser_sessions
 WHERE token_hash = ? AND auth_scope = ?`, tokenHash, authScope).Scan(&csrf, &expiresUnix)
	if err != nil {
		return "", time.Time{}, err
	}
	if expiresUnix <= now.UTC().Unix() {
		if _, err := s.db.Exec(`
DELETE FROM browser_sessions WHERE token_hash = ? AND auth_scope = ?`,
			tokenHash, authScope); err != nil {
			return "", time.Time{}, err
		}
		return "", time.Time{}, sql.ErrNoRows
	}
	return csrf, time.Unix(expiresUnix, 0).UTC(), nil
}

// DeleteBrowserSession removes one session, scoped so a caller cannot delete a
// session it does not own.
func (s *Store) DeleteBrowserSession(tokenHash, authScope string) error {
	_, err := s.db.Exec(`
DELETE FROM browser_sessions WHERE token_hash = ? AND auth_scope = ?`,
		strings.TrimSpace(tokenHash), strings.TrimSpace(authScope))
	return err
}
