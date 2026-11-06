package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func trackerStatsJob(t *testing.T, s *Store, profile, stage, key string, needs bool,
	remote, employment, waiting string, at time.Time) *TrackedJob {
	t.Helper()
	return mustCreateTracker(t, s, TrackedJob{
		Profile:    profile,
		SourceKey:  key,
		Company:    "Company " + key,
		Title:      "Role " + key,
		Stage:      stage,
		Remote:     remote,
		Employment: employment,
		WaitingOn:  waiting,
		NeedsYou:   needs,
	}, at)
}

func trackerStatsDay(t *testing.T, stats *TrackerStats, date string) TrackerDay {
	t.Helper()
	for _, d := range stats.Days {
		if d.Date == date {
			return d
		}
	}
	t.Fatalf("day %s missing from %d-day window", date, len(stats.Days))
	return TrackerDay{}
}

func trackerStatsBreakdownCount(t *testing.T, list []TrackerBreakdown, label string) int {
	t.Helper()
	for _, b := range list {
		if b.Label == label {
			return b.Count
		}
	}
	t.Fatalf("label %q missing from breakdown %v", label, list)
	return 0
}

func TestTrackerStatsBlankWorkspace(t *testing.T) {
	s := trackerStore(t)
	now := time.Date(2026, 11, 6, 12, 34, 56, 0, time.UTC)

	stats, err := s.TrackedStats("", now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Total != 0 || stats.Saved != 0 || stats.NeedsYou != 0 || stats.Active != 0 ||
		stats.Offers != 0 || stats.Hired != 0 || stats.Rejected != 0 || stats.Archived != 0 {
		t.Fatalf("blank workspace should be all zeroes: %+v", stats)
	}
	if stats.From != "2026-10-08" || stats.To != "2026-11-06" {
		t.Fatalf("window = %s..%s", stats.From, stats.To)
	}
	if len(stats.Days) != 30 || stats.Days[0].Date != "2026-10-08" || stats.Days[29].Date != "2026-11-06" {
		t.Fatalf("days not a zero-filled UTC 30-day window: %d %+v", len(stats.Days), stats.Days)
	}
	for _, d := range stats.Days {
		if d.Jobs != 0 || d.Events != 0 {
			t.Fatalf("blank day %s has activity: %+v", d.Date, d)
		}
	}
	if len(stats.Profiles) != 2 || stats.Profiles[0] != (TrackerBreakdown{Label: "sergey", Count: 0}) ||
		stats.Profiles[1] != (TrackerBreakdown{Label: "polina", Count: 0}) {
		t.Fatalf("profiles breakdown must include both live boards: %+v", stats.Profiles)
	}
	if len(stats.Remotes) != 0 || len(stats.Employment) != 0 || len(stats.WaitingOn) != 0 {
		t.Fatalf("empty board should have no labelled rows: %+v %+v %+v",
			stats.Remotes, stats.Employment, stats.WaitingOn)
	}
}

func TestTrackerStatsMixedStagesProfilesAndRetired(t *testing.T) {
	s := trackerStore(t)
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

	trackerStatsJob(t, s, "sergey", TrackerStageSaved, "sy-saved", true, "", "", "", now)
	trackerStatsJob(t, s, "sergey", TrackerStageApplied, "sy-applied", false, "", "", "", now)
	trackerStatsJob(t, s, "sergey", TrackerStageTalking, "sy-talking", false, "", "", "", now)
	trackerStatsJob(t, s, "sergey", TrackerStageInterview, "sy-interview", false, "", "", "", now)
	trackerStatsJob(t, s, "sergey", TrackerStageOffer, "sy-offer", false, "", "", "", now)
	trackerStatsJob(t, s, "sergey", TrackerStageHired, "sy-hired", true, "", "", "", now)
	trackerStatsJob(t, s, "sergey", TrackerStageRejected, "sy-rejected", true, "", "", "", now)
	// A closed row with the flag still set must not be counted as waiting.
	trackerStatsJob(t, s, "sergey", TrackerStageArchived, "sy-archived", true, "", "", "", now)

	trackerStatsJob(t, s, "polina", TrackerStageSaved, "po-saved", true, "", "", "", now)
	trackerStatsJob(t, s, "polina", TrackerStageApplied, "po-applied", false, "", "", "", now)
	trackerStatsJob(t, s, "polina", TrackerStageHired, "po-hired", false, "", "", "", now)

	retired := trackerStatsJob(t, s, "siarhei", TrackerStageOffer, "si-offer", true, "", "", "", now)
	trackerStatsJob(t, s, "siarhei", TrackerStageSaved, "si-saved", true, "", "", "", now)

	stats, err := s.TrackedStats("", now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	got := [8]int{stats.Total, stats.Saved, stats.NeedsYou, stats.Active,
		stats.Offers, stats.Hired, stats.Rejected, stats.Archived}
	want := [8]int{11, 2, 2, 4, 1, 2, 1, 1}
	if got != want {
		t.Fatalf("aggregate = %v, want %v (%+v)", got, want, stats)
	}
	if stats.Profiles[0] != (TrackerBreakdown{Label: "sergey", Count: 8}) ||
		stats.Profiles[1] != (TrackerBreakdown{Label: "polina", Count: 3}) {
		t.Fatalf("profiles = %+v (retired rows must not be counted)", stats.Profiles)
	}
	for _, b := range stats.Profiles {
		if b.Label == "siarhei" {
			t.Fatal("retired profile leaked into the workspace breakdown")
		}
	}

	one, err := s.TrackedStats(" Sergey ", now)
	if err != nil {
		t.Fatalf("scoped stats: %v", err)
	}
	if one.Total != 8 || one.Saved != 1 || one.NeedsYou != 1 || one.Active != 3 ||
		one.Offers != 1 || one.Hired != 1 || one.Rejected != 1 || one.Archived != 1 {
		t.Fatalf("sergey aggregate = %+v", one)
	}
	// Profiles stays workspace-wide on a scoped page.
	if one.Profiles[0].Count != 8 || one.Profiles[1].Count != 3 {
		t.Fatalf("scoped profiles should still be workspace-wide: %+v", one.Profiles)
	}

	// History survives even though it is out of scope.
	if _, err := s.TrackedJob(retired.ID); err != nil {
		t.Fatalf("retired row was deleted: %v", err)
	}
}

func TestTrackerStatsNeedsYouExcludesClosedStages(t *testing.T) {
	s := trackerStore(t)
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

	for i, stage := range []string{
		TrackerStageSaved, TrackerStageApplied, TrackerStageTalking, TrackerStageInterview,
		TrackerStageOffer, TrackerStageHired, TrackerStageRejected, TrackerStageArchived,
	} {
		trackerStatsJob(t, s, "sergey", stage, fmt.Sprintf("n-%d", i), true, "", "", "", now)
	}
	stats, err := s.TrackedStats("sergey", now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	// Only saved/applied/talking/interview/offer remain open.
	if stats.NeedsYou != 5 {
		t.Fatalf("needs_you = %d, want 5: %+v", stats.NeedsYou, stats)
	}
}

func TestTrackerStatsBreakdownLabelsAndOrdering(t *testing.T) {
	s := trackerStore(t)
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

	mk := func(key, remote, employment, waiting string) {
		trackerStatsJob(t, s, "sergey", TrackerStageSaved, key, false, remote, employment, waiting, now)
	}
	mk("a", TrackerRemoteRemote, "full-time", TrackerWaitingYou)
	mk("b", TrackerRemoteRemote, "full-time", TrackerWaitingYou)
	mk("c", TrackerRemoteRemote, "full-time", TrackerWaitingYou)
	mk("d", TrackerRemoteHybrid, "", "")
	mk("e", TrackerRemoteHybrid, "", "")
	mk("f", TrackerRemoteOnsite, "contract", TrackerWaitingRecruiter)
	mk("g", TrackerRemoteUnknown, "", "")

	stats, err := s.TrackedStats("sergey", now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	wantRemotes := []TrackerBreakdown{
		{Label: "remote", Count: 3}, {Label: "hybrid", Count: 2},
		{Label: "onsite", Count: 1}, {Label: "unknown", Count: 1},
	}
	if !reflect.DeepEqual(stats.Remotes, wantRemotes) {
		t.Fatalf("remotes = %+v, want %+v", stats.Remotes, wantRemotes)
	}
	wantEmployment := []TrackerBreakdown{
		{Label: "full-time", Count: 3}, {Label: "unspecified", Count: 3}, {Label: "contract", Count: 1},
	}
	if !reflect.DeepEqual(stats.Employment, wantEmployment) {
		t.Fatalf("employment = %+v, want %+v", stats.Employment, wantEmployment)
	}
	wantWaiting := []TrackerBreakdown{
		{Label: "none", Count: 3}, {Label: "you", Count: 3}, {Label: "recruiter", Count: 1},
	}
	if !reflect.DeepEqual(stats.WaitingOn, wantWaiting) {
		t.Fatalf("waiting_on = %+v, want %+v", stats.WaitingOn, wantWaiting)
	}
	if trackerStatsBreakdownCount(t, stats.Employment, "unspecified") != 3 ||
		trackerStatsBreakdownCount(t, stats.WaitingOn, "none") != 3 ||
		trackerStatsBreakdownCount(t, stats.Remotes, "unknown") != 1 {
		t.Fatal("honest empty labels were not preserved")
	}
}

func TestTrackerStatsDaysUseOccurrenceNotRecordDate(t *testing.T) {
	s := trackerStore(t)
	today := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	inWindow := trackerStatsJob(t, s, "sergey", TrackerStageSaved, "day-in", false, "", "", "",
		time.Date(2026, 10, 8, 0, 30, 0, 0, time.UTC))
	// One second before the first day of the window: visible in totals, not in
	// the chart.
	trackerStatsJob(t, s, "sergey", TrackerStageApplied, "day-before", false, "", "", "",
		time.Date(2026, 10, 7, 23, 59, 59, 0, time.UTC))

	mustAddEvent := func(key, happened string, recorded time.Time) {
		t.Helper()
		if _, _, err := s.AddTimelineEvent(inWindow.ID, TimelineEvent{
			SourceKey: key, Kind: "note", Summary: "event " + key, Actor: "sergey",
			HappenedAt: happened,
		}, recorded); err != nil {
			t.Fatalf("add event %s: %v", key, err)
		}
	}
	// Happened on the window's first day, recorded today: the chart must use
	// the occurrence date even though the row was written on 11-06.
	mustAddEvent("ev-boundary", "2026-10-08T09:00:00Z", today)
	mustAddEvent("ev-today", "2026-11-06T01:00:00Z", today)
	// Before the window: excluded from the chart.
	mustAddEvent("ev-before", "2026-10-07T22:00:00Z", today)

	stats, err := s.TrackedStats("sergey", today)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Total != 2 {
		t.Fatalf("total = %d, want 2 (outside-window job still counts)", stats.Total)
	}
	if got := trackerStatsDay(t, stats, "2026-10-08"); got.Jobs != 1 || got.Events != 1 {
		t.Fatalf("first day = %+v, want jobs 1 events 1", got)
	}
	if got := trackerStatsDay(t, stats, "2026-11-06"); got.Jobs != 0 || got.Events != 1 {
		t.Fatalf("today = %+v, want jobs 0 events 1 (recorded_at must not count)", got)
	}
	for _, d := range stats.Days {
		if d.Date == "2026-10-07" {
			t.Fatal("window contains the day before From")
		}
	}
	jobTotal, eventTotal := 0, 0
	for _, d := range stats.Days {
		jobTotal += d.Jobs
		eventTotal += d.Events
	}
	if jobTotal != 1 || eventTotal != 2 {
		t.Fatalf("window sums = jobs %d events %d, want 1 and 2", jobTotal, eventTotal)
	}
}

func TestTrackerStatsNotTruncatedToPageSize(t *testing.T) {
	s := trackerStore(t)
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	const count = 60

	for i := 0; i < count; i++ {
		trackerStatsJob(t, s, "sergey", TrackerStageSaved, fmt.Sprintf("bulk-%d", i), false, "", "", "", now)
	}
	stats, err := s.TrackedStats("sergey", now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Total != count || stats.Saved != count {
		t.Fatalf("stats truncated to a UI page: total %d saved %d, want %d", stats.Total, stats.Saved, count)
	}
	if stats.Profiles[0].Count != count {
		t.Fatalf("profiles truncated: %+v", stats.Profiles)
	}
	if got := trackerStatsDay(t, stats, "2026-11-06").Jobs; got != count {
		t.Fatalf("day jobs = %d, want %d", got, count)
	}
}

func TestTrackerStatsRejectsInvalidProfile(t *testing.T) {
	s := trackerStore(t)
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

	for _, profile := range []string{"siarhei", "someone-else", "Sergey ", "polina-x"} {
		// "Sergey " is valid once normalized; keep it out of the invalid list.
		if profile == "Sergey " {
			if _, err := s.TrackedStats(profile, now); err != nil {
				t.Fatalf("normalized %q should be valid: %v", profile, err)
			}
			continue
		}
		if _, err := s.TrackedStats(profile, now); !errors.Is(err, ErrTrackerValidation) {
			t.Fatalf("profile %q: err = %v, want ErrTrackerValidation", profile, err)
		}
	}
}

func TestTrackerStatsReadIsImmutable(t *testing.T) {
	s := trackerStore(t)
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	job := trackerStatsJob(t, s, "sergey", TrackerStageApplied, "immutable", true, "", "", "", now)
	if _, _, err := s.AddTimelineEvent(job.ID, TimelineEvent{
		SourceKey: "immutable-ev", Kind: "note", Summary: "note", Actor: "sergey",
		HappenedAt: "2026-11-06T08:00:00Z",
	}, now); err != nil {
		t.Fatalf("add event: %v", err)
	}

	before, err := s.TrackedJob(job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	beforeEvents, err := s.TimelineEvents(job.ID, 0, 0)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}

	first, err := s.TrackedStats("sergey", now)
	if err != nil {
		t.Fatalf("first stats: %v", err)
	}
	second, err := s.TrackedStats("sergey", now)
	if err != nil {
		t.Fatalf("second stats: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two reads disagree:\n%+v\n%+v", first, second)
	}

	after, err := s.TrackedJob(job.ID)
	if err != nil {
		t.Fatalf("re-read job: %v", err)
	}
	afterEvents, err := s.TimelineEvents(job.ID, 0, 0)
	if err != nil {
		t.Fatalf("re-read events: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("stats mutated the job:\nbefore %+v\nafter %+v", before, after)
	}
	if !reflect.DeepEqual(beforeEvents, afterEvents) {
		t.Fatalf("stats mutated the timeline:\nbefore %+v\nafter %+v", beforeEvents, afterEvents)
	}
}
