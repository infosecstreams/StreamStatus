package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicklaw5/helix/v2"
)

// fakeUsers is a scripted Helix /users endpoint.
type fakeUsers struct {
	mu    sync.Mutex
	users []helix.User
	calls []helix.UsersParams
}

func (f *fakeUsers) set(users ...helix.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users = users
}

func (f *fakeUsers) GetUsers(p *helix.UsersParams) (*helix.UsersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, *p)
	r := &helix.UsersResponse{}
	r.StatusCode = 200
	for _, u := range f.users {
		for _, l := range p.Logins {
			if strings.EqualFold(l, u.Login) {
				r.Data.Users = append(r.Data.Users, u)
			}
		}
		for _, id := range p.IDs {
			if id == u.ID {
				r.Data.Users = append(r.Data.Users, u)
			}
		}
	}
	return r, nil
}

// clock is a settable timeNow for the duration of a test.
type clock struct{ now time.Time }

func useClock(t *testing.T, at time.Time) *clock {
	t.Helper()
	c := &clock{now: at}
	old := timeNow
	timeNow = func() time.Time { return c.now }
	t.Cleanup(func() { timeNow = old })
	return c
}

func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestLive(t *testing.T, path string) *liveState {
	t.Helper()
	if path == "" {
		path = filepath.Join(t.TempDir(), "test.db")
	}
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.close() })
	l, err := newLiveState(st)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// liveStreams answers /streams from a mutable set of live streams,
// whether asked by login or by ID.
type liveStreams struct {
	mu   sync.Mutex
	live map[string]helix.Stream // by user ID
}

func (f *liveStreams) set(streams ...helix.Stream) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live = map[string]helix.Stream{}
	for _, s := range streams {
		f.live[s.UserID] = s
	}
}

func (f *liveStreams) fake() *fakeStreams {
	return &fakeStreams{fn: func(_ int, p *helix.StreamsParams) (*helix.StreamsResponse, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []helix.Stream
		for _, s := range f.live {
			for _, id := range p.UserIDs {
				if id == s.UserID {
					out = append(out, s)
				}
			}
			for _, l := range p.UserLogins {
				if strings.EqualFold(l, s.UserLogin) {
					out = append(out, s)
				}
			}
		}
		return streamsResp(out...), nil
	}}
}

func decodeStatus(t *testing.T, l *liveState) (statusDoc, map[string]statusEntry) {
	t.Helper()
	doc := l.snapshot()
	if doc == nil {
		t.Fatal("no status snapshot")
	}
	var sd statusDoc
	if err := json.Unmarshal(doc.body, &sd); err != nil {
		t.Fatal(err)
	}
	byLogin := map[string]statusEntry{}
	for _, e := range sd.Streamers {
		byLogin[e.Login] = e
	}
	return sd, byLogin
}

var (
	userAlpha = helix.User{ID: "1", Login: "alpha", DisplayName: "Alpha"}
	userBeta  = helix.User{ID: "2", Login: "beta", DisplayName: "Beta"}
	userGamma = helix.User{ID: "3", Login: "gamma", DisplayName: "Gamma"}
)

// Events drive live state and the recorded sessions, with no git work
// when status commits are off.
func TestLiveStateFollowsEvents(t *testing.T) {
	noSleep(t)
	c := useClock(t, t0)
	streams := &liveStreams{}
	users := &fakeUsers{}
	users.set(userAlpha, userBeta)
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, ""), users: users, client: streams.fake(), statusCommitsOff: true}
	s.resolveRoster([]string{"alpha", "Beta"}, c.now)
	s.live.rebuild(c.now)

	sd, got := decodeStatus(t, s.live)
	if sd.LiveCount != 0 || len(sd.Streamers) != 2 || got["alpha"].Status != statusOffline || got["beta"].DisplayName != "Beta" {
		t.Fatalf("initial status = %+v", sd)
	}

	alphaLive := helix.Stream{UserID: "1", UserLogin: "alpha", UserName: "Alpha", GameName: "Science & Technology",
		Title: "Hacking", Tags: []string{"English", "infosec"}, Language: "en", ViewerCount: 42, StartedAt: t0.Add(-10 * time.Minute)}
	streams.set(alphaLive)
	s.process(statusJob{kind: jobOnline, userID: "1", userName: "alpha"})
	sd, got = decodeStatus(t, s.live)
	a := got["alpha"]
	if sd.LiveCount != 1 || a.Status != statusOnline || a.Category != "Science & Technology" || a.Title != "Hacking" ||
		a.Viewers == nil || *a.Viewers != 42 || a.StartedAt != "2026-11-01T17:50:00Z" || a.Language != "en" || len(a.Tags) != 2 {
		t.Fatalf("after online: %+v", a)
	}

	// Into a non-infosec category: shown offline, still recorded.
	c.advance(time.Hour)
	alphaLive.GameName = "Minecraft"
	streams.set(alphaLive)
	s.process(statusJob{kind: jobChannelUpdate, userID: "1", userName: "alpha", game: "Minecraft", language: "en", title: "Mining"})
	if _, got = decodeStatus(t, s.live); got["alpha"].Status != statusOffline || got["alpha"].Category != "" || got["alpha"].Viewers != nil {
		t.Fatalf("live in Minecraft should be offline with no live fields: %+v", got["alpha"])
	}

	c.advance(30 * time.Minute)
	s.process(statusJob{kind: jobChannelUpdate, userID: "1", userName: "alpha", game: "Just Chatting", language: "en", title: "Back"})
	if _, got = decodeStatus(t, s.live); got["alpha"].Status != statusOnline || got["alpha"].Title != "Back" {
		t.Fatalf("back in Just Chatting: %+v", got["alpha"])
	}

	c.advance(30 * time.Minute)
	streams.set()
	s.process(statusJob{kind: jobOffline, userID: "1", userName: "alpha"})
	sd, got = decodeStatus(t, s.live)
	// 1h10m in Science & Technology + 30m in Just Chatting.
	if sd.LiveCount != 0 || got["alpha"].Status != statusOffline || got["alpha"].Hours30d != 1.7 {
		t.Fatalf("after offline: %+v", got["alpha"])
	}

	// Pre-live, then expiry after preliveTTL.
	s.process(statusJob{kind: jobChannelUpdate, userID: "2", userName: "beta", game: "Science & Technology", language: "de", title: "Soon"})
	_, got = decodeStatus(t, s.live)
	b := got["beta"]
	if b.Status != statusPrelive || b.PreliveSince != rfc3339(c.now) || b.Category != "Science & Technology" || b.Title != "Soon" || b.StartedAt != "" {
		t.Fatalf("pre-live: %+v", b)
	}
	c.advance(preliveTTL - time.Minute)
	s.process(statusJob{kind: jobRefreshLive})
	if _, got = decodeStatus(t, s.live); got["beta"].Status != statusPrelive {
		t.Fatalf("pre-live expired early: %+v", got["beta"])
	}
	c.advance(2 * time.Minute)
	s.process(statusJob{kind: jobRefreshLive})
	if _, got = decodeStatus(t, s.live); got["beta"].Status != statusOffline || got["beta"].Category != "" {
		t.Fatalf("pre-live should expire after %s: %+v", preliveTTL, got["beta"])
	}
}

// Refresh updates viewers and titles with one /streams call by ID, and
// leaves a streamer Helix stopped listing alone.
func TestRefreshLiveUpdatesViewers(t *testing.T) {
	c := useClock(t, t0)
	streams := &liveStreams{}
	fake := streams.fake()
	users := &fakeUsers{}
	users.set(userAlpha, userBeta)
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, ""), users: users, client: fake, statusCommitsOff: true}
	s.resolveRoster([]string{"alpha", "beta"}, c.now)
	a := helix.Stream{UserID: "1", UserLogin: "alpha", GameName: "Just Chatting", ViewerCount: 5, Title: "one", StartedAt: t0}
	b := helix.Stream{UserID: "2", UserLogin: "beta", GameName: "Just Chatting", ViewerCount: 7, StartedAt: t0}
	s.live.online("1", a, c.now)
	s.live.online("2", b, c.now)

	c.advance(2 * time.Minute)
	a.ViewerCount, a.Title = 50, "two"
	streams.set(a) // beta not listed (Helix lag)
	before := len(fake.calls)
	s.process(statusJob{kind: jobRefreshLive})
	if n := len(fake.calls) - before; n != 1 || len(fake.calls[before].UserIDs) != 2 {
		t.Fatalf("want one /streams call by ID for both, got %d: %+v", n, fake.calls[before:])
	}
	_, got := decodeStatus(t, s.live)
	if *got["alpha"].Viewers != 50 || got["alpha"].Title != "two" {
		t.Errorf("alpha not refreshed: %+v", got["alpha"])
	}
	if got["beta"].Status != statusOnline || *got["beta"].Viewers != 7 {
		t.Errorf("beta should be untouched: %+v", got["beta"])
	}
}

// Reconcile opens sessions for missed stream.online (inactive streamers
// included), closes them for missed stream.offline, and leaves streamers
// with a fresh event alone.
func TestReconcileFeedsLiveState(t *testing.T) {
	noSleep(t)
	c := useClock(t, t0)
	s, origin := newGitFixture(t, []string{offlineLine("alpha"), offlineLine("beta")},
		[]string{"`gamma` | [x](https://www.twitch.tv/gamma) &nbsp; |"}, "alpha,\nbeta,\n", "gamma,\n")
	streams := &liveStreams{}
	users := &fakeUsers{}
	users.set(userAlpha, userBeta, userGamma)
	s.client, s.users, s.live, s.statusCommitsOff = streams.fake(), users, newTestLive(t, ""), true
	before := gitOutput(t, origin, "rev-parse", "main")

	streams.set(
		helix.Stream{UserID: "1", UserLogin: "alpha", GameName: "Just Chatting", StartedAt: t0.Add(-time.Hour), ViewerCount: 3},
		helix.Stream{UserID: "3", UserLogin: "gamma", GameName: "Science & Technology", StartedAt: t0.Add(-time.Hour)},
	)
	s.process(statusJob{kind: jobReconcile})
	sd, got := decodeStatus(t, s.live)
	if sd.LiveCount != 2 || got["alpha"].Status != statusOnline || got["gamma"].Status != statusOnline || got["beta"].Status != statusOffline {
		t.Fatalf("after reconcile: %+v", sd)
	}

	// beta goes live by event; Helix doesn't list it yet. alpha's offline
	// event never arrives.
	c.advance(time.Hour)
	betaLive := helix.Stream{UserID: "2", UserLogin: "beta", GameName: "Just Chatting", StartedAt: c.now}
	streams.set(betaLive, helix.Stream{UserID: "3", UserLogin: "gamma", GameName: "Science & Technology", StartedAt: t0.Add(-time.Hour)})
	s.process(statusJob{kind: jobOnline, userID: "2", userName: "beta"})
	streams.set(helix.Stream{UserID: "3", UserLogin: "gamma", GameName: "Science & Technology", StartedAt: t0.Add(-time.Hour)})
	c.advance(time.Minute)
	s.process(statusJob{kind: jobReconcile})
	sd, got = decodeStatus(t, s.live)
	if got["alpha"].Status != statusOffline {
		t.Errorf("alpha should be closed by reconcile: %+v", got["alpha"])
	}
	if got["beta"].Status != statusOnline {
		t.Errorf("beta had a fresh event; reconcile must leave it: %+v", got["beta"])
	}
	// alpha: from started_at (t0-1h) to last seen (t0).
	if got["alpha"].Hours30d != 1 {
		t.Errorf("alpha hours = %v, want 1", got["alpha"].Hours30d)
	}
	// gamma: t0-1h until now (t0+1h1m), still live.
	if got["gamma"].Hours30d != 2 {
		t.Errorf("gamma hours = %v, want 2", got["gamma"].Hours30d)
	}
	if gitOutput(t, origin, "rev-parse", "main") != before {
		t.Errorf("status commits are off but the site changed:\n%s", gitOutput(t, origin, "log", "--oneline", "-3", "main"))
	}
}

// History is keyed by user ID: a rename, before or after the roster is
// updated, keeps the same streamer and their hours.
func TestRosterIDMappingAcrossRename(t *testing.T) {
	c := useClock(t, t0)
	path := filepath.Join(t.TempDir(), "test.db")
	users := &fakeUsers{}
	users.set(helix.User{ID: "1", Login: "oldname", DisplayName: "OldName"})

	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, path), users: users}
	s.resolveRoster([]string{"OldName"}, c.now)
	s.live.online("1", helix.Stream{UserID: "1", UserLogin: "oldname", GameName: "Just Chatting", StartedAt: t0}, c.now)
	c.advance(2 * time.Hour)
	s.live.offline("1", false, c.now)
	s.live.db.close()

	// Renamed on Twitch; the CSV still has the old login. Restart.
	users.set(helix.User{ID: "1", Login: "newname", DisplayName: "NewName"})
	c.advance(time.Hour)
	s = &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, path), users: users}
	extra := s.resolveRoster([]string{"OldName"}, c.now)
	if len(extra) != 1 || extra[0] != "newname" {
		t.Errorf("want the new login queried too, got %v", extra)
	}
	s.live.rebuild(c.now)
	sd, _ := decodeStatus(t, s.live)
	if len(sd.Streamers) != 1 {
		t.Fatalf("streamers = %+v", sd.Streamers)
	}
	if e := sd.Streamers[0]; e.ID != "1" || e.Login != "newname" || e.DisplayName != "NewName" || e.Hours30d != 2 {
		t.Errorf("after rename: %+v", e)
	}

	// The roster catches up: same ID, no new lookup needed.
	users.calls = nil
	extra = s.resolveRoster([]string{"NewName"}, c.now)
	if len(extra) != 0 || len(users.calls) != 0 || !s.live.roster["1"] {
		t.Errorf("extra=%v calls=%v roster=%v", extra, users.calls, s.live.roster)
	}

	// A rename seen in an event updates the login at once.
	s.liveEvent(statusJob{kind: jobOffline, userID: "1", userLogin: "third", displayName: "Third"}, c.now)
	s.live.rebuild(c.now)
	if sd, _ = decodeStatus(t, s.live); sd.Streamers[0].Login != "third" || sd.Streamers[0].Hours30d != 2 {
		t.Errorf("after event rename: %+v", sd.Streamers[0])
	}
}

// SS_COMMIT_STATUS=false: status changes don't touch git, roster moves
// (reactivation from inactive) still do.
func TestStatusCommitsOff(t *testing.T) {
	noSleep(t)
	s, origin := newGitFixture(t, []string{offlineLine("alpha")},
		[]string{"`dormant` | [x](https://www.twitch.tv/dormant) &nbsp; |"}, "alpha,\n", "dormant,\n")
	s.statusCommitsOff = true
	streams := &liveStreams{}
	s.client = streams.fake()
	before := gitOutput(t, origin, "rev-parse", "main")

	streams.set(helix.Stream{UserID: "1", UserLogin: "alpha", GameName: "Just Chatting"})
	s.process(statusJob{kind: jobOnline, userID: "1", userName: "alpha"})
	s.process(statusJob{kind: jobChannelUpdate, userID: "1", userName: "alpha", game: "Science & Technology"})
	streams.set()
	s.process(statusJob{kind: jobOffline, userID: "1", userName: "alpha"})
	s.process(statusJob{kind: jobChannelUpdate, userID: "1", userName: "alpha", game: "Science & Technology"})
	if gitOutput(t, origin, "rev-parse", "main") != before {
		t.Fatalf("status commits off but the site changed:\n%s", gitOutput(t, origin, "log", "--oneline", "-5", "main"))
	}

	streams.set(helix.Stream{UserID: "9", UserLogin: "dormant", GameName: "Just Chatting"})
	s.process(statusJob{kind: jobOnline, userID: "9", userName: "dormant"})
	if csv := gitOutput(t, origin, "show", "main:streamers.csv"); !strings.Contains(csv, "dormant") {
		t.Errorf("reactivation should still be committed; streamers.csv:\n%s", csv)
	}
}

// Pre-live set through applyPrelive by login (e.g. a scheduled stream)
// reaches status.json even with status commits off, without overwriting
// a channel.update marker.
func TestApplyPreliveFeedsLiveState(t *testing.T) {
	c := useClock(t, t0)
	users := &fakeUsers{}
	users.set(userAlpha, userBeta)
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, ""), users: users, statusCommitsOff: true}
	s.resolveRoster([]string{"alpha", "beta"}, c.now)

	s.applyPrelive("Alpha", "Science & Technology", "en", c.now)
	_, got := decodeStatus(t, s.live)
	if got["alpha"].Status != statusPrelive || got["alpha"].PreliveSince != rfc3339(t0) {
		t.Fatalf("alpha = %+v", got["alpha"])
	}

	s.live.preliveUpdate("2", "Just Chatting", "Soon", "en", c.now)
	c.advance(time.Minute)
	s.applyPrelive("beta", "Just Chatting", "en", c.now)
	s.live.rebuild(c.now)
	if _, got = decodeStatus(t, s.live); got["beta"].Title != "Soon" || got["beta"].PreliveSince != rfc3339(t0) {
		t.Errorf("existing marker overwritten: %+v", got["beta"])
	}
}
