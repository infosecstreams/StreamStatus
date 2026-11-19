package main

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)

func newTestStore(t *testing.T) *store {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.close() })
	return st
}

// hoursAt returns userID's hours_30d as of now.
func hoursAt(t *testing.T, st *store, userID string, now time.Time) float64 {
	t.Helper()
	segs, err := st.segmentsSince(now.Add(-hoursWindow))
	if err != nil {
		t.Fatal(err)
	}
	return streamedHours(segs, now, hoursWindow)[userID]
}

func wantHours(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("hours = %v, want %v", got, want)
	}
}

func mustSeen(t *testing.T, st *store, id string, started time.Time, cat string, now time.Time) {
	t.Helper()
	stale, err := st.streamSeen(id, started, cat, now)
	if err != nil || stale {
		t.Fatalf("streamSeen(%s, %s, %s): stale=%v err=%v", id, started, cat, stale, err)
	}
}

func h(n float64) time.Duration { return time.Duration(n * float64(time.Hour)) }

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	for i := 0; i < 2; i++ {
		st, err := openStore(path)
		if err != nil {
			t.Fatalf("open %d: %s", i, err)
		}
		var v int
		st.db.QueryRow(`PRAGMA user_version`).Scan(&v)
		if v != len(migrations) {
			t.Errorf("user_version = %d, want %d", v, len(migrations))
		}
		if i == 0 {
			mustSeen(t, st, "1", t0, "Just Chatting", t0)
		}
		st.close()
	}
	st, _ := openStore(path)
	defer st.close()
	if open, _ := st.hasOpenSession("1"); !open {
		t.Error("session lost across reopen")
	}
}

func TestSessionOpenClose(t *testing.T) {
	st := newTestStore(t)
	mustSeen(t, st, "1", t0, "Science & Technology", t0.Add(time.Minute))
	wantHours(t, hoursAt(t, st, "1", t0.Add(h(1))), 1) // open: counts until now
	if err := st.streamEnded("1", t0.Add(h(2))); err != nil {
		t.Fatal(err)
	}
	if open, _ := st.hasOpenSession("1"); open {
		t.Error("session still open after stream.offline")
	}
	wantHours(t, hoursAt(t, st, "1", t0.Add(h(10))), 2)
	wantHours(t, hoursAt(t, st, "2", t0.Add(h(10))), 0)
}

// Twitch's started_at, not the time the bot noticed, starts the session:
// a missed stream.online loses nothing.
func TestMissedOnlineCountsFromStartedAt(t *testing.T) {
	st := newTestStore(t)
	mustSeen(t, st, "1", t0, "Just Chatting", t0.Add(30*time.Minute))
	st.streamEnded("1", t0.Add(h(1.5)))
	wantHours(t, hoursAt(t, st, "1", t0.Add(h(5))), 1.5)
}

func TestSameBroadcastIsOneSession(t *testing.T) {
	st := newTestStore(t)
	for m := 0; m <= 60; m += 2 {
		mustSeen(t, st, "1", t0, "Just Chatting", t0.Add(time.Duration(m)*time.Minute))
	}
	var n int
	st.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}
	st.db.QueryRow(`SELECT COUNT(*) FROM segments`).Scan(&n)
	if n != 1 {
		t.Errorf("%d segments, want 1", n)
	}
}

// Only time in VALID_GAMES counts; channel.update splits the session.
func TestCategoryChangesSplitSession(t *testing.T) {
	st := newTestStore(t)
	mustSeen(t, st, "1", t0, "Science & Technology", t0)
	st.categoryChanged("1", "Minecraft", t0.Add(h(1)))
	st.categoryChanged("1", "Minecraft", t0.Add(h(1.2))) // no change, no split
	st.categoryChanged("1", "just chatting", t0.Add(h(1.5)))
	st.streamEnded("1", t0.Add(h(3)))
	wantHours(t, hoursAt(t, st, "1", t0.Add(h(4))), 2.5)

	var n int
	st.db.QueryRow(`SELECT COUNT(*) FROM segments`).Scan(&n)
	if n != 3 {
		t.Errorf("%d segments, want 3", n)
	}
	// A category change for someone not live is ignored.
	if err := st.categoryChanged("2", "Just Chatting", t0); err != nil {
		t.Fatal(err)
	}
	st.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}
}

// A category change seen only by a refresh/reconcile (channel.update was
// missed) still splits, at the time it was seen.
func TestCategoryChangeSeenByPollingSplits(t *testing.T) {
	st := newTestStore(t)
	mustSeen(t, st, "1", t0, "Minecraft", t0)
	mustSeen(t, st, "1", t0, "Software and Game Development", t0.Add(h(1)))
	st.streamEnded("1", t0.Add(h(2)))
	wantHours(t, hoursAt(t, st, "1", t0.Add(h(3))), 1)
}

// Reconcile finds them offline: the session ends when last seen live.
func TestMissedOfflineEndsAtLastSeen(t *testing.T) {
	st := newTestStore(t)
	mustSeen(t, st, "1", t0, "Just Chatting", t0)
	mustSeen(t, st, "1", t0, "Just Chatting", t0.Add(h(1)))
	if err := st.streamMissing("1"); err != nil {
		t.Fatal(err)
	}
	wantHours(t, hoursAt(t, st, "1", t0.Add(h(6))), 1)
}

// stream.offline missed, then a new broadcast: the old session closes at
// its last sighting and the new one starts at its own started_at.
func TestMissedOfflineThenNewBroadcast(t *testing.T) {
	st := newTestStore(t)
	mustSeen(t, st, "1", t0, "Just Chatting", t0)
	mustSeen(t, st, "1", t0, "Just Chatting", t0.Add(h(1)))
	mustSeen(t, st, "1", t0.Add(h(3)), "Just Chatting", t0.Add(h(3.25)))
	wantHours(t, hoursAt(t, st, "1", t0.Add(h(4))), 2)
	ids, _ := st.openSessionUsers()
	if len(ids) != 1 {
		t.Errorf("open sessions for %v, want just one", ids)
	}
}

// Helix keeps listing a stream for a bit after stream.offline; that must
// not reopen it, and an older broadcast must not close the current one.
func TestStaleListingDoesNotReopen(t *testing.T) {
	st := newTestStore(t)
	mustSeen(t, st, "1", t0, "Just Chatting", t0)
	st.streamEnded("1", t0.Add(h(1)))
	stale, err := st.streamSeen("1", t0, "Just Chatting", t0.Add(h(1)+time.Minute))
	if err != nil || !stale {
		t.Fatalf("stale=%v err=%v, want stale", stale, err)
	}
	if open, _ := st.hasOpenSession("1"); open {
		t.Error("stale listing reopened the session")
	}

	mustSeen(t, st, "1", t0.Add(h(2)), "Just Chatting", t0.Add(h(2)))
	stale, _ = st.streamSeen("1", t0, "Just Chatting", t0.Add(h(2.5)))
	if !stale {
		t.Error("older broadcast not treated as stale")
	}
	wantHours(t, hoursAt(t, st, "1", t0.Add(h(3))), 2)
}

func TestStreamedHoursWindow(t *testing.T) {
	now := t0
	day := 24 * time.Hour
	segs := []segment{
		// Straddles the window start: only the last day counts.
		{userID: "a", category: "Just Chatting", start: now.Add(-31 * day), end: now.Add(-29 * day)},
		// Entirely before the window.
		{userID: "a", category: "Just Chatting", start: now.Add(-40 * day), end: now.Add(-35 * day)},
		// Open: counts until now.
		{userID: "a", category: "Talk Shows & Podcasts", start: now.Add(-h(2))},
		// Wrong category.
		{userID: "a", category: "Minecraft", start: now.Add(-h(5)), end: now.Add(-h(3))},
		// Case-insensitive category match.
		{userID: "b", category: "SCIENCE & TECHNOLOGY", start: now.Add(-h(1)), end: now},
	}
	got := streamedHours(segs, now, hoursWindow)
	wantHours(t, got["a"], 24+2)
	wantHours(t, got["b"], 1)
}

func TestUserRenameKeepsOldLogin(t *testing.T) {
	st := newTestStore(t)
	st.upsertUser("1", "OldName", "OldName", t0)
	st.upsertUser("1", "newname", "", t0.Add(time.Hour))
	users, byLogin, err := st.loadUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].login != "newname" || users[0].displayName != "OldName" {
		t.Errorf("users = %+v", users)
	}
	if byLogin["oldname"] != "1" || byLogin["newname"] != "1" {
		t.Errorf("byLogin = %v", byLogin)
	}
	// Someone else takes the old login later: the newest owner wins, but
	// user 1's current login still maps to user 1.
	st.upsertUser("2", "oldname", "", t0.Add(2*time.Hour))
	_, byLogin, _ = st.loadUsers()
	if byLogin["oldname"] != "2" || byLogin["newname"] != "1" {
		t.Errorf("byLogin = %v", byLogin)
	}
}
