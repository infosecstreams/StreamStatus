package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEstimateOutcomes(t *testing.T) {
	// Weekly Tuesday 18:00 UTC slot; next occurrence 2026-11-24.
	next := time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)
	segs := []scheduledSegment{
		{ID: "rec", Start: next, Recurring: true},
		{ID: "rec", Start: next.Add(7 * 24 * time.Hour), Recurring: true}, // same slot, next week
		{ID: "once", Start: next.Add(24 * time.Hour)},
	}
	var streams []pastStream
	for k := 1; k <= 13; k++ {
		occ := next.Add(-time.Duration(k) * 7 * 24 * time.Hour)
		switch {
		case k%4 == 0: // skipped weeks: 4, 8, 12
		case k == 6: // DST: an hour late counts
			streams = append(streams, pastStream{Start: occ.Add(time.Hour), Length: time.Hour})
		case k == 9: // started early and still live at the slot
			streams = append(streams, pastStream{Start: occ.Add(-3 * time.Hour), Length: 2*time.Hour + 30*time.Minute})
		default:
			streams = append(streams, pastStream{Start: occ.Add(10 * time.Minute), Length: 2 * time.Hour})
		}
	}
	// A stream far from the slot doesn't count.
	streams = append(streams, pastStream{Start: next.Add(-4*7*24*time.Hour - 5*time.Hour), Length: time.Hour})

	got := estimateOutcomes(segs, streams, schedNow)
	if len(got) != 13 { // 90 days back from 11-19 reaches 08-21; Tuesdays 08-25 .. 11-17
		t.Fatalf("%d outcomes, want 13: %+v", len(got), got)
	}
	kept := 0
	for _, o := range got {
		if !o.Estimated || o.SegmentID != "rec" {
			t.Errorf("bad outcome %+v", o)
		}
		if o.Kept {
			kept++
		}
	}
	if kept != 10 {
		t.Errorf("kept %d, want 10 (weeks 4, 8, 12 missed)", kept)
	}
}

func TestTrackRecordMergesEstimates(t *testing.T) {
	a := newAdherenceTracker()
	slot := schedNow.Add(-7 * 24 * time.Hour)
	a.setEstimated(map[string][]adherenceOutcome{"1": {
		{SegmentID: "rec", Start: slot.Add(-7 * 24 * time.Hour), Kept: true, Estimated: true},
		{SegmentID: "rec", Start: slot, Kept: false, Estimated: true},
	}})
	if tr := a.trackRecord("1", schedNow); tr != (trackRecordJSON{Kept: 1, Total: 2, Estimated: 2}) {
		t.Errorf("estimates only: %+v", tr)
	}
	// An observed outcome for the same slot wins.
	a.record("1", adherenceOutcome{SegmentID: "rec", Start: slot, Kept: true}, schedNow)
	if tr := a.trackRecord("1", schedNow); tr != (trackRecordJSON{Kept: 2, Total: 2, Estimated: 1}) {
		t.Errorf("merged: %+v", tr)
	}
}

func TestHabits(t *testing.T) {
	// Tuesdays ~18:00 UTC in 10 of the weeks, Saturdays ~14:00 in 3 (too
	// few), and a one-off on a Monday.
	var streams []pastStream
	tue := time.Date(2026, 11, 17, 18, 0, 0, 0, time.UTC)
	for k := 0; k < 13; k++ {
		if k == 2 || k == 5 || k == 7 {
			continue
		}
		jitter := time.Duration((k%3)-1) * 20 * time.Minute
		streams = append(streams, pastStream{Start: tue.Add(-time.Duration(k)*7*24*time.Hour + jitter), Length: 2 * time.Hour})
	}
	sat := time.Date(2026, 11, 14, 14, 0, 0, 0, time.UTC)
	for k := 0; k < 3; k++ {
		streams = append(streams, pastStream{Start: sat.Add(-time.Duration(k) * 7 * 24 * time.Hour), Length: time.Hour})
	}
	streams = append(streams, pastStream{Start: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), Length: time.Hour})

	got := habits(streams, schedNow)
	if len(got) != 1 {
		t.Fatalf("got %+v, want one slot", got)
	}
	h := got[0]
	if h.Weekday != time.Tuesday || h.Minute != 18*60 || h.Weeks != 10 || h.Of != 13 {
		t.Errorf("got %+v, want Tuesday 18:00 10/13", h)
	}
	next, ok := nextHabitAt(got, schedNow)
	if !ok || !next.Equal(time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("next %v", next)
	}
	if got := habits(streams[:3], schedNow); len(got) != 0 {
		t.Errorf("3 weeks is not a habit: %+v", got)
	}
}

func TestHabitsAndEstimatesInAPI(t *testing.T) {
	s, f := newScheduleRepo(t)
	// alpha's a1 is a weekly recurring slot at 22:00 UTC Thursdays.
	page := f.schedules["1"][0]
	for i := range page.Data.Schedule.Segments {
		page.Data.Schedule.Segments[i].IsRecurring = true
	}
	var streams []pastStream
	for k := 1; k <= 12; k++ {
		if k%3 == 0 {
			continue
		}
		streams = append(streams, pastStream{Start: schedNow.Add(2*time.Hour - time.Duration(k)*7*24*time.Hour), Length: 2 * time.Hour})
	}
	s.schedule.past = func(id string, since time.Time) ([]pastStream, bool) {
		if id == "1" {
			return streams, true
		}
		return nil, true
	}
	s.refreshSchedules(direct)

	rec := get(t, s, "/streamstatus/schedule.json?login=alpha", nil)
	var got scheduleJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	tr := got.Segments[0].TrackRecord
	if tr == nil || tr.Total == 0 || tr.Estimated != tr.Total || tr.Kept >= tr.Total {
		t.Errorf("track record %+v", tr)
	}
	if len(got.Habits) != 1 || got.Habits[0].Login != "alpha" || got.Habits[0].Slots[0].Weekday != "Thursday" ||
		got.Habits[0].Slots[0].Time != "22:00" || got.Habits[0].Next != "2026-11-19T22:00:00Z" {
		t.Errorf("habits %s", rec.Body)
	}
	lines := unfoldICS(t, get(t, s, "/streamstatus/schedule.ics?login=alpha", nil).Body.String())
	if ev := eventLines(lines, "UID:a1"); !containsPrefix(ev, "DESCRIPTION:") || !strings.Contains(strings.Join(ev, "\n"), "estimated from stream history") {
		t.Errorf("ICS: %q", ev)
	}
}

// Past streams come from bot-live-data's database: the imported tracker
// backfill and streams recorded live.
func TestEstimatesAndHabitsFromDatabase(t *testing.T) {
	s, f := newScheduleRepo(t)
	f.schedules["1"][0].Data.Schedule.Segments[1].IsRecurring = true // a1, Thursdays 22:00
	db := newTestStore(t)
	var back []backfillStream
	for k := 1; k <= 12; k++ {
		if k%3 != 0 {
			back = append(back, backfillStream{Start: schedNow.Add(2*time.Hour - time.Duration(k)*7*24*time.Hour), Length: 2 * time.Hour})
		}
	}
	if _, err := db.importBackfill(map[string][]backfillStream{"1": back}, schedNow); err != nil {
		t.Fatal(err)
	}
	s.live = &liveState{db: db}
	s.refreshSchedules(direct)

	if h := s.streamHabits("1"); len(h) != 1 || h[0].Weekday != time.Thursday || h[0].Minute != 22*60 {
		t.Errorf("habits %+v", h)
	}
	if tr := s.scheduleTrackRecord("1"); tr.Total < 12 || tr.Estimated != tr.Total || tr.Kept != 8 {
		t.Errorf("track record %+v, want 8 kept, all estimated", tr)
	}
	// No database: nothing, and no crash.
	s.live = nil
	s.refreshSchedules(direct)
	if h := s.streamHabits("1"); h != nil {
		t.Errorf("habits without a database: %+v", h)
	}
}
