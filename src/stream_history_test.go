package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func useTrackerServer(t *testing.T, h http.HandlerFunc) *int {
	t.Helper()
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	oldSG, oldTT := sullyGnomeBase, twitchTrackerBase
	sullyGnomeBase, twitchTrackerBase = srv.URL+"/sg", srv.URL+"/tt"
	t.Cleanup(func() { sullyGnomeBase, twitchTrackerBase = oldSG, oldTT })
	return &n
}

func sgRows(starts ...time.Time) string {
	var rows []string
	for _, s := range starts {
		rows = append(rows, fmt.Sprintf(`{"startDateTime":%q,"length":120,"channelurl":"x"}`, s.Format(time.RFC3339)))
	}
	return strings.Join(rows, ",")
}

func TestSullyGnomeSource(t *testing.T) {
	since := schedNow.Add(-historyWindow)
	old := since.Add(-time.Hour)
	calls := useTrackerServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sg/channel/alpha/90/streams":
			fmt.Fprint(w, `<script>var PageInfo = {"x":{"urlpart":"other","id":5},"y":{"urlpart":"alpha","id":3666580,"name":"Alpha"}};</script>`)
		case r.URL.Path == "/sg/channel/ghost/90/streams":
			fmt.Fprint(w, `<html>no channel</html>`)
		case strings.HasPrefix(r.URL.Path, "/sg/api/tables/channeltables/streams/90/3666580/ /1/1/desc/0/100"):
			fmt.Fprintf(w, `{"recordsTotal":101,"data":[%s]}`, sgRows(schedNow.Add(-time.Hour), schedNow.Add(-48*time.Hour)))
		case strings.HasPrefix(r.URL.Path, "/sg/api/tables/channeltables/streams/90/3666580/ /1/1/desc/100/100"):
			fmt.Fprintf(w, `{"recordsTotal":101,"data":[%s]}`, sgRows(old))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	noSleep(t)
	sg := &sullyGnome{ids: map[string]int{}}
	got, err := sg.fetch("alpha", since)
	if err != nil || len(got) != 2 || !got[0].Start.Equal(schedNow.Add(-time.Hour)) || got[0].Length != 2*time.Hour {
		t.Fatalf("got %+v %v", got, err)
	}
	before := *calls
	sg.fetch("alpha", since)
	if *calls-before != 2 {
		t.Errorf("channel ID not cached: %d calls", *calls-before)
	}
	if _, err := sg.fetch("ghost", since); err == nil {
		t.Error("untracked channel must be an error")
	}
}

const ttPage = `<table><tbody>
<tr>
    <td nowrap data-order="2026-11-18 21:06">
        <a href="/alpha/streams"><span>2026-11-18 21:06</span></a>
    </td>
    <td data-order="166"><span>166</span></td>
    <td><span>14</span></td>
</tr>
<tr>
    <td nowrap data-order="2019-01-03 00:50">
        <a href="/alpha/streams"><span>2019-01-03 00:50</span></a>
    </td>
    <td data-order="74"><span>74</span></td>
</tr></tbody></table>`

func TestTwitchTrackerSource(t *testing.T) {
	useTrackerServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tt/alpha/streams" {
			fmt.Fprint(w, ttPage)
			return
		}
		http.NotFound(w, r)
	})
	got, err := twitchTracker{}.fetch("alpha", schedNow.Add(-historyWindow))
	want := time.Date(2026, 11, 18, 21, 6, 0, 0, time.UTC)
	if err != nil || len(got) != 1 || !got[0].Start.Equal(want) || got[0].Length != 166*time.Minute {
		t.Errorf("got %+v %v", got, err)
	}
	if _, err := (twitchTracker{}).fetch("ghost", schedNow); err == nil {
		t.Error("404 must be an error")
	}
}

type fakeHistorySource struct {
	n       string
	streams map[string][]pastStream
	err     error
}

func (f *fakeHistorySource) name() string { return f.n }
func (f *fakeHistorySource) fetch(login string, _ time.Time) ([]pastStream, error) {
	if f.err != nil {
		return nil, f.err
	}
	if s, ok := f.streams[login]; ok {
		return s, nil
	}
	return nil, errors.New("not found")
}

func TestRefreshStreamHistoryFallsBackAndKeepsOldData(t *testing.T) {
	s, _ := newScheduleRepo(t)
	s.refreshSchedules(direct)
	logs := captureLogs(t)
	primary := &fakeHistorySource{n: "sg", streams: map[string][]pastStream{"alpha": {{Start: schedNow.Add(-time.Hour), Length: time.Hour}}}}
	fallback := &fakeHistorySource{n: "tt", streams: map[string][]pastStream{"beta": {{Start: schedNow.Add(-2 * time.Hour), Length: time.Hour}}}}
	h := newStreamHistory(primary, fallback)
	h.path = filepath.Join(t.TempDir(), "stream_history.json")
	s.schedule.history = h

	s.refreshStreamHistory()
	if got, ok := h.get("1"); !ok || len(got) != 1 {
		t.Errorf("alpha from primary: %v %v", got, ok)
	}
	if got, ok := h.get("2"); !ok || len(got) != 1 {
		t.Errorf("beta from fallback: %v %v", got, ok)
	}
	if _, ok := h.get("9"); ok {
		t.Error("dormant is in neither source")
	}
	if logs.count(log.WarnLevel) != 1 || logs.count(log.ErrorLevel) != 0 {
		t.Errorf("one failure should warn: %d warn %d error", logs.count(log.WarnLevel), logs.count(log.ErrorLevel))
	}

	// Both trackers down: keep the old data; Error only on the 2nd run.
	primary.err, fallback.err = errors.New("down"), errors.New("down")
	s.refreshStreamHistory()
	if logs.count(log.ErrorLevel) != 0 {
		t.Error("first bad run must not alert")
	}
	s.refreshStreamHistory()
	if logs.count(log.ErrorLevel) != 1 {
		t.Error("second bad run must alert")
	}
	if got, _ := h.get("1"); len(got) != 1 {
		t.Error("failed fetch dropped old data")
	}

	// The cache survives a restart.
	h2 := newStreamHistory()
	if err := h2.load(h.path); err != nil {
		t.Fatal(err)
	}
	if got, ok := h2.get("2"); !ok || len(got) != 1 || h2.lastRun.IsZero() {
		t.Errorf("reloaded %v %v", got, ok)
	}
}

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
	s.schedule.history = newStreamHistory(&fakeHistorySource{n: "sg", streams: map[string][]pastStream{"alpha": streams}})
	s.refreshSchedules(direct)
	s.refreshStreamHistory()

	rec := get(t, s, "/api/schedule.json?login=alpha", nil)
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
	lines := unfoldICS(t, get(t, s, "/api/schedule.ics?login=alpha", nil).Body.String())
	if ev := eventLines(lines, "UID:a1"); !containsPrefix(ev, "DESCRIPTION:") || !strings.Contains(strings.Join(ev, "\n"), "estimated from stream history") {
		t.Errorf("ICS: %q", ev)
	}
}

// TestLiveTrackers fetches real data; run with SS_LIVE_HISTORY=login1,login2.
func TestLiveTrackers(t *testing.T) {
	logins := os.Getenv("SS_LIVE_HISTORY")
	if logins == "" {
		t.Skip("set SS_LIVE_HISTORY to fetch from SullyGnome and TwitchTracker")
	}
	since := time.Now().Add(-historyWindow)
	for _, l := range strings.Split(logins, ",") {
		for _, src := range defaultHistorySources() {
			got, err := src.fetch(l, since)
			slots := habits(got, time.Now())
			t.Logf("%-14s %-13s streams=%-3d err=%v habits=%+v", l, src.name(), len(got), err, slots)
			time.Sleep(2 * time.Second)
		}
	}
}
