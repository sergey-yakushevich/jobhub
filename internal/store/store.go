// Package store owns the SQLite database: schema, models and every query the
// API needs. All writes go through here so ingest stays transactional.
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	// busy_timeout because more than one caller may share the file.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc/sqlite serializes writes; a single conn avoids SQLITE_BUSY races.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	return s.migrateJobs()
}

// addColumn is an idempotent ALTER TABLE: SQLite has no "IF NOT EXISTS" for
// columns, so a second run is expected to fail with "duplicate column name".
func (s *Store) addColumn(table, definition string) error {
	_, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", table, definition))
	if err != nil && strings.Contains(err.Error(), "duplicate column name") {
		return nil
	}
	return err
}

const timeFormat = time.RFC3339

func fmtTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(timeFormat, s)
	return t
}

// DayBar is one column of the chart: one day, or one week once the span is long
// enough to bucket. Quiet columns are present with zeroes, so the axis stays
// even and a gap reads as a gap.
type DayBar struct {
	Day     string `json:"day"` // YYYY-MM-DD, UTC — the column's first day
	Visits  int64  `json:"visits"`
	Devices int64  `json:"devices"`
}

// LabelStat is one row of a breakdown table. Extra carries engaged milliseconds
// for the page table and is zero everywhere else.
type LabelStat struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
	Extra int64  `json:"extra,omitempty"`
}
