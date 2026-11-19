package main

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicklaw5/helix/v2"
)

// adherenceRig is a repo with one streamer (id 1) and a controllable clock
// and live state.
type adherenceRig struct {
	s    *StreamersRepo
	now  time.Time
	mu   sync.Mutex
	live bool
	fail bool
}

func newAdherenceRig(t *testing.T, segs ...scheduledSegment) *adherenceRig {
	t.Helper()
	r := &adherenceRig{now: schedNow}
	old := timeNow
	timeNow = func() time.Time { return r.now }
	t.Cleanup(func() { timeNow = old })
	st := newScheduleState(nil)
	st.roster["1"] = rosterEntry{id: "1", login: "alpha", displayName: "Alpha"}
	for i := range segs {
		segs[i].BroadcasterID = "1"
	}
	st.byID["1"] = &broadcasterSchedule{segments: segs, fetched: true}
	st.refreshedAt = schedNow
	r.s = &StreamersRepo{schedule: st, client: &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.fail {
			return nil, errors.New("helix down")
		}
		if r.live {
			return streamsResp(helix.Stream{UserID: "1", UserLogin: "alpha"}), nil
		}
		return streamsResp(), nil
	}}}
	return r
}

// run polls every adherencePollInterval from now until until.
func (r *adherenceRig) run(until time.Time) {
	for ; !r.now.After(until); r.now = r.now.Add(adherencePollInterval) {
		r.s.trackScheduleAdherence()
	}
}

func (r *adherenceRig) record() (int, int) {
	tr := r.s.scheduleTrackRecord("1")
	return tr.Kept, tr.Total
}

func at(d time.Duration, id string) scheduledSegment {
	return scheduledSegment{ID: id, Start: schedNow.Add(d), End: schedNow.Add(d + 2*time.Hour)}
}

func TestTrackRecordKept(t *testing.T) {
	start := schedNow.Add(2 * time.Hour)
	r := newAdherenceRig(t, at(2*time.Hour, "s1"))
	r.run(start.Add(20 * time.Minute)) // 70 min late is still kept
	if k, n := r.record(); n != 0 {
		t.Fatalf("judged before going live: %d/%d", k, n)
	}
	r.live = true
	r.run(start.Add(40 * time.Minute))
	if k, n := r.record(); k != 1 || n != 1 {
		t.Fatalf("got %d/%d, want 1/1", k, n)
	}
	// Still live for the rest of the window: counted once.
	r.run(start.Add(3 * time.Hour))
	if k, n := r.record(); k != 1 || n != 1 {
		t.Errorf("got %d/%d after the window, want 1/1", k, n)
	}
	if o := r.s.schedule.adherence.outcomes["1"][0]; !o.LiveAt.Equal(start.Add(25 * time.Minute)) {
		t.Errorf("live_at %v", o.LiveAt)
	}
}

func TestTrackRecordEarlyCounts(t *testing.T) {
	start := schedNow.Add(2 * time.Hour)
	r := newAdherenceRig(t, at(2*time.Hour, "s1"))
	r.run(start.Add(-80 * time.Minute))
	r.live = true
	r.run(start.Add(-75 * time.Minute))
	if k, n := r.record(); k != 1 || n != 1 {
		t.Errorf("live 75 min early: got %d/%d, want 1/1", k, n)
	}
}

func TestTrackRecordMissed(t *testing.T) {
	start := schedNow.Add(2 * time.Hour)
	r := newAdherenceRig(t, at(2*time.Hour, "s1"))
	r.run(start.Add(adherenceTolerance - time.Minute))
	if _, n := r.record(); n != 0 {
		t.Fatal("judged before the window closed")
	}
	r.run(start.Add(adherenceTolerance + 5*time.Minute))
	if k, n := r.record(); k != 0 || n != 1 {
		t.Errorf("got %d/%d, want 0/1", k, n)
	}
	// Going live after the window doesn't change it.
	r.live = true
	r.run(start.Add(3 * time.Hour))
	if k, n := r.record(); k != 0 || n != 1 {
		t.Errorf("after the window: got %d/%d, want 0/1", k, n)
	}
}

// Windows the bot didn't watch properly are not held against anyone.
func TestTrackRecordUnwatchedWindowsAreNotJudged(t *testing.T) {
	start := schedNow.Add(2 * time.Hour)

	// Started watching after the scheduled start (e.g. a restart).
	r := newAdherenceRig(t, at(2*time.Hour, "s1"))
	r.now = start.Add(10 * time.Minute)
	r.run(start.Add(3 * time.Hour))
	if _, n := r.record(); n != 0 {
		t.Errorf("late start: judged %d", n)
	}

	// Helix down for 20 minutes mid-window.
	r = newAdherenceRig(t, at(2*time.Hour, "s1"))
	r.run(start)
	r.fail = true
	r.run(start.Add(20 * time.Minute))
	r.fail = false
	r.run(start.Add(3 * time.Hour))
	if _, n := r.record(); n != 0 {
		t.Errorf("gap: judged %d", n)
	}
	if len(r.s.schedule.adherence.watching) != 0 {
		t.Error("closed windows must stop being watched")
	}
}

func TestTrackRecordIgnoresCanceledAndRemoved(t *testing.T) {
	c := at(2*time.Hour, "c")
	c.Canceled = true
	v := at(5*time.Hour, "v")
	v.Vacation = true
	r := newAdherenceRig(t, c, v, at(8*time.Hour, "gone"))
	r.run(schedNow.Add(7 * time.Hour))
	// Removed from the schedule before it starts.
	r.s.schedule.byID["1"].segments = []scheduledSegment{c, v}
	r.run(schedNow.Add(12 * time.Hour))
	if _, n := r.record(); n != 0 {
		t.Errorf("judged %d canceled/vacation/removed segments", n)
	}
}

func TestTrackRecordTrimAndPersist(t *testing.T) {
	r := newAdherenceRig(t)
	a := r.s.schedule.adherence
	path := filepath.Join(t.TempDir(), "schedule_history.json")
	if err := a.loadHistory(path); err != nil {
		t.Fatal(err)
	}
	a.record("1", adherenceOutcome{SegmentID: "ancient", Start: schedNow.Add(-100 * 24 * time.Hour), Kept: true}, schedNow)
	for i := 0; i < adherenceKeep+5; i++ {
		a.record("1", adherenceOutcome{SegmentID: "s", Start: schedNow.Add(-time.Duration(i) * 24 * time.Hour), Kept: i%2 == 0}, schedNow)
	}
	if a.record("1", adherenceOutcome{SegmentID: "s", Start: schedNow}, schedNow) {
		t.Error("duplicate recorded")
	}
	k, n := r.record()
	if n != adherenceKeep || k != 10 {
		t.Errorf("got %d/%d, want 10/%d (newest kept)", k, n, adherenceKeep)
	}
	a.save()

	b := newAdherenceTracker()
	if err := b.loadHistory(path); err != nil {
		t.Fatal(err)
	}
	if tr := b.trackRecord("1", schedNow); tr.Kept != k || tr.Total != n {
		t.Errorf("reloaded %+v, want %d/%d", tr, k, n)
	}
	if err := newAdherenceTracker().loadHistory("/nonexistent-dir/x.json"); err == nil {
		t.Error("missing directory should be reported")
	}
}

func TestScheduleHistoryPath(t *testing.T) {
	t.Setenv("SS_SCHEDULE_HISTORY", "")
	t.Setenv("SS_DB_PATH", "")
	if p := scheduleHistoryPath(); p != "/data/schedule_history.json" {
		t.Errorf("default %s", p)
	}
	t.Setenv("SS_DB_PATH", "/var/lib/ss/db.sqlite")
	if p := scheduleHistoryPath(); p != "/var/lib/ss/schedule_history.json" {
		t.Errorf("next to DB: %s", p)
	}
	t.Setenv("SS_SCHEDULE_HISTORY", "/x/h.json")
	if p := scheduleHistoryPath(); p != "/x/h.json" {
		t.Errorf("override: %s", p)
	}
}

func TestTrackRecordInAPI(t *testing.T) {
	s, _ := newScheduleRepo(t)
	s.refreshSchedules(direct)
	a := s.schedule.adherence
	a.record("1", adherenceOutcome{SegmentID: "x", Start: schedNow.Add(-24 * time.Hour), Kept: true}, schedNow)
	a.record("1", adherenceOutcome{SegmentID: "y", Start: schedNow.Add(-48 * time.Hour)}, schedNow)

	body := get(t, s, "/api/schedule.json", nil).Body.String()
	if !strings.Contains(body, `"segment_id":"a1","start":"2026-11-19T22:00:00Z","end":"2026-11-20T00:00:00Z","title":"CTF night","category":"Just Chatting","canceled":false,"track_record":{"kept":1,"total":2}`) {
		t.Errorf("alpha's track record missing:\n%s", body)
	}
	if strings.Count(body, "track_record") != 2 {
		t.Errorf("only alpha's two segments should carry one:\n%s", body)
	}
	lines := unfoldICS(t, get(t, s, "/api/schedule.ics", nil).Body.String())
	if ev := eventLines(lines, "UID:a1"); !containsLine(ev, `DESCRIPTION:Category: Just Chatting\nhttps://www.twitch.tv/alpha\nWent live for 1 of their last 2 scheduled streams`) {
		t.Errorf("ICS description: %q", ev)
	}
}
