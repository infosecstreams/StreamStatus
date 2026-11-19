package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func setRecordingSince(t *testing.T, st *store, at time.Time) {
	t.Helper()
	if _, err := st.db.Exec(`UPDATE meta SET value = ? WHERE key = 'recording_since'`, strconv.FormatInt(at.Unix(), 10)); err != nil {
		t.Fatal(err)
	}
}

func TestImportBackfill(t *testing.T) {
	st := newTestStore(t)
	cutoff := t0 // recording started at t0
	setRecordingSince(t, st, cutoff)
	// A stream the bot recorded itself, right after it started.
	mustSeen(t, st, "1", t0.Add(time.Hour), "Just Chatting", t0.Add(time.Hour))
	st.streamEnded("1", t0.Add(3*time.Hour))

	streams := map[string][]backfillStream{
		"1": {
			{Start: t0.Add(-48 * time.Hour), Length: 2 * time.Hour},       // before: imported
			{Start: t0.Add(-30 * time.Minute), Length: 2 * time.Hour},     // straddles: clipped to 30 min
			{Start: t0.Add(time.Hour), Length: 2 * time.Hour},             // after recording began: skipped
			{Start: t0.Add(-100 * 24 * time.Hour), Length: 3 * time.Hour}, // old but kept (all-time stats)
		},
		"2": {{Start: t0.Add(-24 * time.Hour), Length: 90 * time.Minute}},
	}
	n, err := st.importBackfill(streams, cutoff)
	if err != nil || n != 4 {
		t.Fatalf("imported %d, %v; want 4", n, err)
	}
	if n, _ := st.importBackfill(streams, cutoff); n != 0 {
		t.Errorf("re-import added %d", n)
	}

	now := t0.Add(4 * time.Hour)
	// user 1: 2h + 0.5h backfilled + 2h recorded in the last 30 days.
	wantHours(t, hoursAt(t, st, "1", now), 4.5)
	wantHours(t, hoursAt(t, st, "2", now), 1.5)

	got, err := st.streamsSince("1", t0.Add(-200*24*time.Hour))
	if err != nil || len(got) != 4 {
		t.Fatalf("streamsSince = %+v, %v", got, err)
	}
	if !got[0].Backfilled || got[0].Length != 3*time.Hour || got[3].Backfilled || got[3].Length != 2*time.Hour {
		t.Errorf("streams = %+v", got)
	}

	d, _ := st.loadStatsData(now)
	s := computeStreamerStats("1", d, now)
	if s.Hours.All != 7.5 || s.Streams30d != 3 || s.Categories30d[0].Category != backfillCategory || !s.Categories30d[0].Infosec {
		t.Errorf("stats with backfill: hours=%+v streams=%d cats=%+v", s.Hours, s.Streams30d, s.Categories30d)
	}
	// Backfill moves the start of history back, so rates aren't inflated.
	if !d.since.Equal(t0.Add(-100 * 24 * time.Hour)) {
		t.Errorf("history since %s", d.since)
	}
	if live, _ := st.liveRecordingSince(); !live.Equal(cutoff) {
		t.Errorf("live recording since %s", live)
	}
}

func TestImportBackfillFile(t *testing.T) {
	l := newTestLive(t, "")
	setRecordingSince(t, l.db, t0)
	l.importBackfillFile() // no path: nothing
	l.backfillPath = filepath.Join(t.TempDir(), "stream_history.json")
	l.importBackfillFile() // missing file: nothing

	// bot-schedule's file format.
	body, _ := json.Marshal(map[string]any{
		"version":  1,
		"last_run": t0,
		"streams":  map[string][]backfillStream{"1": {{Start: t0.Add(-5 * time.Hour), Length: time.Hour}}},
	})
	if err := os.WriteFile(l.backfillPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	l.importBackfillFile()
	got, _ := l.db.streamsSince("1", t0.Add(-day))
	if len(got) != 1 || !got[0].Backfilled {
		t.Fatalf("after import: %+v", got)
	}
	// Unchanged file: not read again (a deleted row stays deleted).
	l.db.db.Exec(`DELETE FROM segments`)
	l.db.db.Exec(`DELETE FROM sessions`)
	l.importBackfillFile()
	if got, _ := l.db.streamsSince("1", t0.Add(-day)); len(got) != 0 {
		t.Errorf("unchanged file re-imported: %+v", got)
	}
}
