package store

// Bounded KPI counters for the tracker overview. One read transaction answers
// the whole dashboard, so the header, the breakdowns and the 30-day chart all
// describe the same snapshot instead of racing a concurrent write. Nothing here
// touches the legacy tables or the timeline rows that produced the numbers.

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// trackerStatsProfiles are the two boards the dashboard currently speaks for.
// Historical rows may still carry other profiles (a retired seeker); they stay
// in the database, but they are not part of a live workspace count.
var trackerStatsProfiles = []string{"sergey", "polina"}

// trackerStatsDays is the fixed width of the activity chart, today inclusive.
const trackerStatsDays = 30

// TrackerBreakdown is one labelled count in a dashboard table.
type TrackerBreakdown struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// TrackerDay is one UTC column of the 30-day chart. Jobs counts creations and
// Events counts occurrences, not records, so a late sync cannot move a moment
// to the day it was noticed.
type TrackerDay struct {
	Date   string `json:"date"`
	Jobs   int    `json:"jobs"`
	Events int    `json:"events"`
}

// TrackerStats is the tracker overview: exact funnel counts, scope breakdowns
// and a zero-filled 30-day activity window.
type TrackerStats struct {
	Total      int                `json:"total"`
	Saved      int                `json:"saved"`
	NeedsYou   int                `json:"needs_you"`
	Active     int                `json:"active"`
	Offers     int                `json:"offers"`
	Hired      int                `json:"hired"`
	Rejected   int                `json:"rejected"`
	Archived   int                `json:"archived"`
	Profiles   []TrackerBreakdown `json:"profiles"`
	Remotes    []TrackerBreakdown `json:"remotes"`
	Employment []TrackerBreakdown `json:"employment"`
	WaitingOn  []TrackerBreakdown `json:"waiting_on"`
	Days       []TrackerDay       `json:"days"`
	From       string             `json:"from"`
	To         string             `json:"to"`
}

// trackerStatsScope validates the requested board and returns the profiles the
// aggregate should read. Blank means both active boards; a retired profile is
// rejected rather than silently folded into a live count.
func trackerStatsScope(profile string) ([]string, error) {
	profile = strings.ToLower(strings.TrimSpace(profile))
	if profile == "" {
		return append([]string(nil), trackerStatsProfiles...), nil
	}
	for _, active := range trackerStatsProfiles {
		if profile == active {
			return []string{profile}, nil
		}
	}
	return nil, fmt.Errorf("%w: profile %q is not an active tracker profile", ErrTrackerValidation, profile)
}

func trackerStatsIn(scope []string) (string, []any) {
	marks := make([]string, len(scope))
	args := make([]any, len(scope))
	for i, p := range scope {
		marks[i] = "?"
		args[i] = p
	}
	return strings.Join(marks, ", "), args
}

// TrackedStats reads the overview for one profile, or for both active profiles
// when profile is blank. Every query runs inside one read transaction and each
// rows value is closed before the next statement, because the store shares a
// single SQLite connection.
func (s *Store) TrackedStats(profile string, now time.Time) (*TrackerStats, error) {
	scope, err := trackerStatsScope(profile)
	if err != nil {
		return nil, err
	}
	today := now.UTC()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -(trackerStatsDays - 1))
	fromDate, toDate := from.Format("2006-01-02"), today.Format("2006-01-02")

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	in, scopeArgs := trackerStatsIn(scope)
	stats := &TrackerStats{
		Profiles:   make([]TrackerBreakdown, 0, len(trackerStatsProfiles)),
		Remotes:    make([]TrackerBreakdown, 0, 4),
		Employment: make([]TrackerBreakdown, 0, 5),
		WaitingOn:  make([]TrackerBreakdown, 0, 4),
		Days:       make([]TrackerDay, 0, trackerStatsDays),
		From:       fromDate,
		To:         toDate,
	}

	// Exact funnel counts over every scoped row, archived included. NeedsYou is
	// a flag, but a closed row is never waiting on a human even if the flag was
	// left set by an older write.
	statArgs := []any{
		TrackerStageSaved,
		TrackerStageHired, TrackerStageRejected, TrackerStageArchived,
		TrackerStageApplied, TrackerStageTalking, TrackerStageInterview,
		TrackerStageOffer, TrackerStageHired, TrackerStageRejected, TrackerStageArchived,
	}
	statArgs = append(statArgs, scopeArgs...)
	if err := tx.QueryRow(fmt.Sprintf(`
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN stage = ? THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN needs_you = 1 AND stage NOT IN (?, ?, ?) THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN stage IN (?, ?, ?) THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN stage = ? THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN stage = ? THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN stage = ? THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN stage = ? THEN 1 ELSE 0 END), 0)
  FROM tracker_jobs
 WHERE profile IN (%s)`, in),
		statArgs...,
	).Scan(
		&stats.Total, &stats.Saved, &stats.NeedsYou, &stats.Active,
		&stats.Offers, &stats.Hired, &stats.Rejected, &stats.Archived); err != nil {
		return nil, err
	}

	// Profiles is workspace-wide even when the rest of the page is scoped: the
	// dashboard always shows both live boards, with a zero row kept visible.
	profileCounts, err := trackerStatsProfileCounts(tx)
	if err != nil {
		return nil, err
	}
	for _, p := range trackerStatsProfiles {
		stats.Profiles = append(stats.Profiles, TrackerBreakdown{Label: p, Count: profileCounts[p]})
	}

	if stats.Remotes, err = trackerStatsBreakdown(tx, scope, "remote", ""); err != nil {
		return nil, err
	}
	if stats.Employment, err = trackerStatsBreakdown(tx, scope, "employment", "unspecified"); err != nil {
		return nil, err
	}
	if stats.WaitingOn, err = trackerStatsBreakdown(tx, scope, "waiting_on", "none"); err != nil {
		return nil, err
	}

	jobDays, err := trackerStatsJobDays(tx, in, scopeArgs, fromDate, toDate)
	if err != nil {
		return nil, err
	}
	eventDays, err := trackerStatsEventDays(tx, in, scopeArgs, fromDate, toDate)
	if err != nil {
		return nil, err
	}
	for i := 0; i < trackerStatsDays; i++ {
		date := from.AddDate(0, 0, i).Format("2006-01-02")
		stats.Days = append(stats.Days, TrackerDay{
			Date:   date,
			Jobs:   jobDays[date],
			Events: eventDays[date],
		})
	}
	return stats, nil
}

// trackerStatsProfileCounts counts every scoped row per active board, so a
// board with no jobs still reports zero instead of disappearing.
func trackerStatsProfileCounts(tx *sql.Tx) (map[string]int, error) {
	in, args := trackerStatsIn(trackerStatsProfiles)
	rows, err := tx.Query(fmt.Sprintf(
		`SELECT profile, COUNT(*) FROM tracker_jobs WHERE profile IN (%s) GROUP BY profile`, in),
		args...)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(trackerStatsProfiles))
	for rows.Next() {
		var profile string
		var count int
		if err := rows.Scan(&profile, &count); err != nil {
			rows.Close()
			return nil, err
		}
		counts[profile] = count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

// trackerStatsBreakdown groups one column and sorts busiest first, then label,
// so equal counts never reorder between requests. emptyLabel is only applied
// when non-empty: remote's "unknown" stays exactly as stored.
func trackerStatsBreakdown(tx *sql.Tx, scope []string, column, emptyLabel string) ([]TrackerBreakdown, error) {
	in, args := trackerStatsIn(scope)
	rows, err := tx.Query(fmt.Sprintf(
		`SELECT %s, COUNT(*) FROM tracker_jobs WHERE profile IN (%s) GROUP BY %s`,
		column, in, column), args...)
	if err != nil {
		return nil, err
	}
	out := make([]TrackerBreakdown, 0)
	for rows.Next() {
		var label string
		var count int
		if err := rows.Scan(&label, &count); err != nil {
			rows.Close()
			return nil, err
		}
		if label == "" && emptyLabel != "" {
			label = emptyLabel
		}
		out = append(out, TrackerBreakdown{Label: label, Count: count})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	return out, nil
}

func trackerStatsJobDays(tx *sql.Tx, in string, scopeArgs []any, from, to string) (map[string]int, error) {
	args := append(append([]any(nil), scopeArgs...), from, to)
	rows, err := tx.Query(fmt.Sprintf(`
SELECT substr(created_at, 1, 10), COUNT(*)
  FROM tracker_jobs
 WHERE profile IN (%s)
   AND substr(created_at, 1, 10) BETWEEN ? AND ?
 GROUP BY 1`, in), args...)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, trackerStatsDays)
	for rows.Next() {
		var day string
		var count int
		if err := rows.Scan(&day, &count); err != nil {
			rows.Close()
			return nil, err
		}
		out[day] = count
	}
	rows.Close()
	return out, rows.Err()
}

// trackerStatsEventDays counts events by the day they happened, joined to the
// job only to scope the profile. recorded_at is deliberately ignored: a late
// monitoring pass must not inflate the day it ran.
func trackerStatsEventDays(tx *sql.Tx, in string, scopeArgs []any, from, to string) (map[string]int, error) {
	args := append(append([]any(nil), scopeArgs...), from, to)
	rows, err := tx.Query(fmt.Sprintf(`
SELECT substr(e.happened_at, 1, 10), COUNT(*)
  FROM tracker_events e
  JOIN tracker_jobs j ON j.id = e.job_id
 WHERE j.profile IN (%s)
   AND substr(e.happened_at, 1, 10) BETWEEN ? AND ?
 GROUP BY 1`, in), args...)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, trackerStatsDays)
	for rows.Next() {
		var day string
		var count int
		if err := rows.Scan(&day, &count); err != nil {
			rows.Close()
			return nil, err
		}
		out[day] = count
	}
	rows.Close()
	return out, rows.Err()
}
