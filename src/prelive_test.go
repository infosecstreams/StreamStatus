package main

import (
	"strings"
	"testing"
	"time"

	"github.com/nicklaw5/helix/v2"
)

func realSleep(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

var preliveNow = time.Date(2026, 11, 19, 14, 5, 0, 0, time.UTC)

func freezeTime(t *testing.T, at time.Time) {
	t.Helper()
	old := timeNow
	timeNow = func() time.Time { return at }
	t.Cleanup(func() { timeNow = old })
}

func notLive() *fakeStreams {
	return &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(), nil
	}}
}

func preliveLine(name, since string) string {
	return "🟡 | `" + name + "` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/" + name + " \"Getting ready: Science & Technology (since " + since + ")\") &nbsp;| EN"
}

func TestGenerateStreamerLinePrelive(t *testing.T) {
	s := StreamersRepo{streamer: "Garr_7", prelive: true, preliveSince: preliveNow, game: "Science & Technology", language: "EN"}
	got := s.generateStreamerLine("&nbsp; [<i class=\"fab fa-youtube\" style=\"color:#C00\"></i>](https://www.youtube.com/c/x) ")
	want := "🟡 | `Garr_7` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/Garr_7 \"Getting ready: Science & Technology (since 2026-11-19T14:05Z)\") &nbsp; [<i class=\"fab fa-youtube\" style=\"color:#C00\"></i>](https://www.youtube.com/c/x) | EN"
	if got != want {
		t.Errorf("\ngot:  %s\nwant: %s", got, want)
	}
}

func TestChannelUpdateWhileOfflineMarksPrelive(t *testing.T) {
	freezeTime(t, preliveNow)
	s, origin := newGitFixture(t, []string{offlineLine("Garr_7")}, nil, "Garr_7,\n", "")
	s.client = notLive()

	s.process(statusJob{kind: jobChannelUpdate, userID: "7", userName: "Garr_7", game: "Science & Technology", language: "en"})

	idx := gitOutput(t, origin, "show", "main:index.md")
	if !strings.Contains(idx, "🟡 | `Garr_7`") || !strings.Contains(idx, "(since 2026-11-19T14:05Z)") || !strings.HasSuffix(strings.Split(idx, "\n")[2], "| EN") {
		t.Errorf("want Garr_7 🟡 with timestamp and language:\n%s", idx)
	}
	if msg := gitOutput(t, origin, "log", "-1", "--format=%s", "main"); !strings.Contains(msg, "🟡 Garr_7 is getting ready") {
		t.Errorf("commit message %q", msg)
	}
	if got := preliveStreamers(idx)["garr_7"]; got.name != "Garr_7" || !got.since.Equal(preliveNow) {
		t.Errorf("round trip: got %+v", got)
	}
}

func TestPreliveThenOnlineThenOffline(t *testing.T) {
	noSleep(t)
	freezeTime(t, preliveNow)
	s, origin := newGitFixture(t, []string{preliveLine("Garr_7", "2026-11-19T14:00Z")}, nil, "Garr_7,\n", "")
	s.client = &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(helix.Stream{UserLogin: "garr_7", UserName: "Garr_7", GameName: "Science & Technology", Language: "en"}), nil
	}}

	s.process(statusJob{kind: jobOnline, userID: "7", userName: "Garr_7"})
	if idx := gitOutput(t, origin, "show", "main:index.md"); !strings.Contains(idx, "🟢 | `Garr_7`") || strings.Contains(idx, "🟡") {
		t.Fatalf("online should replace 🟡:\n%s", idx)
	}
	s.process(statusJob{kind: jobOffline, userID: "7", userName: "Garr_7"})
	if idx := gitOutput(t, origin, "show", "main:index.md"); !strings.Contains(idx, "&nbsp; | `Garr_7`") {
		t.Errorf("want offline:\n%s", idx)
	}
}

// A category change right after going live can arrive before Helix lists
// the stream; that must not downgrade 🟢 to 🟡.
func TestPreliveNeverReplacesLiveRow(t *testing.T) {
	s, origin := newGitFixture(t, []string{onlineLine("alpha")}, nil, "alpha,\n", "")
	s.client = notLive()
	before := gitOutput(t, origin, "rev-parse", "main")
	s.process(statusJob{kind: jobChannelUpdate, userID: "1", userName: "alpha", game: "Just Chatting"})
	if gitOutput(t, origin, "rev-parse", "main") != before {
		t.Errorf("🟢 row changed:\n%s", gitOutput(t, origin, "show", "main:index.md"))
	}
}

func TestPreliveDoesNotReactivateInactiveStreamers(t *testing.T) {
	s, origin := newGitFixture(t, []string{offlineLine("alpha")},
		[]string{"`dormant` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/dormant) &nbsp; |"}, "alpha,\n", "dormant,\n")
	s.client = notLive()
	before := gitOutput(t, origin, "rev-parse", "main")
	s.process(statusJob{kind: jobChannelUpdate, userID: "9", userName: "dormant", game: "Just Chatting"})
	if gitOutput(t, origin, "rev-parse", "main") != before {
		t.Error("an inactive streamer's title change modified the site")
	}
}

func TestChannelUpdateToOtherCategoryClearsOnlyPrelive(t *testing.T) {
	s, origin := newGitFixture(t, []string{preliveLine("alpha", "2026-11-19T14:00Z"), onlineLine("beta")}, nil, "alpha,\nbeta,\n", "")
	s.client = notLive()

	s.process(statusJob{kind: jobChannelUpdate, userID: "1", userName: "alpha", game: "Minecraft"})
	s.process(statusJob{kind: jobChannelUpdate, userID: "2", userName: "beta", game: "Minecraft"})

	idx := gitOutput(t, origin, "show", "main:index.md")
	if !strings.Contains(idx, "&nbsp; | `alpha`") {
		t.Errorf("alpha's 🟡 should be cleared:\n%s", idx)
	}
	if !strings.Contains(idx, "🟢 | `beta`") {
		t.Errorf("beta's 🟢 must be left to events/reconcile:\n%s", idx)
	}
}

func TestReconcileExpiresStalePrelive(t *testing.T) {
	freezeTime(t, preliveNow.Add(61*time.Minute))
	s, origin := newGitFixture(t, []string{
		preliveLine("Stale", "2026-11-19T14:05Z"),
		preliveLine("Fresh", "2026-11-19T14:50Z"),
		preliveLine("WentLive", "2026-11-19T14:05Z"),
		"🟡 | `Garbled` | no timestamp here | EN",
	}, nil, "Stale,\nFresh,\nWentLive,\nGarbled,\n", "")
	s.client = &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(helix.Stream{UserLogin: "wentlive", UserName: "WentLive", GameName: "Just Chatting"}), nil
	}}

	s.reconcile()

	idx := gitOutput(t, origin, "show", "main:index.md")
	for _, want := range []string{"&nbsp; | `Stale`", "🟡 | `Fresh`", "🟢 | `WentLive`"} {
		if !strings.Contains(idx, want) {
			t.Errorf("missing %q:\n%s", want, idx)
		}
	}
	if strings.Contains(idx, "`stale`") {
		t.Errorf("expiry lowercased the name:\n%s", idx)
	}
}

func TestExpiredPrelive(t *testing.T) {
	now := preliveNow.Add(2 * time.Hour)
	rows := map[string]preliveRow{
		"old":    {name: "Old", since: preliveNow},
		"new":    {name: "New", since: now.Add(-10 * time.Minute)},
		"nots":   {name: "NoTS"},
		"gaming": {name: "Gaming", since: preliveNow},
		"live":   {name: "Live", since: preliveNow},
	}
	live := map[string]helix.Stream{
		"gaming": {GameName: "Minecraft"},
		"live":   {GameName: "Just Chatting"},
	}
	got := strings.Join(expiredPrelive(rows, live, now, preliveTTL), ",")
	if got != "Gaming,NoTS,Old" {
		t.Errorf("got %s, want Gaming,NoTS,Old", got)
	}
}

// withLiveState gives a git fixture an in-memory status.json for users.
func withLiveState(t *testing.T, s *StreamersRepo, now time.Time, users ...helix.User) {
	t.Helper()
	fu := &fakeUsers{}
	fu.set(users...)
	s.users = fu
	s.live = newTestLive(t, "")
	logins := make([]string, 0, len(users))
	for _, u := range users {
		logins = append(logins, u.Login)
	}
	s.resolveRoster(logins, now)
}

var userDormant = helix.User{ID: "9", Login: "dormant", DisplayName: "dormant"}

// Ash_F0x on 2026-11-21: an inactive streamer was shown 🟡 in status.json
// (and so on the site) although the markdown path refused them.
func TestChannelUpdateNeverMarksInactiveStreamerPrelive(t *testing.T) {
	freezeTime(t, preliveNow)
	s, origin := newGitFixture(t, []string{offlineLine("alpha")},
		[]string{"`dormant` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/dormant) &nbsp; |"}, "alpha,\n", "dormant,\n")
	s.client = notLive()
	withLiveState(t, s, preliveNow, userAlpha, userDormant)

	s.process(statusJob{kind: jobChannelUpdate, userID: "9", userName: "dormant", game: "Science & Technology", language: "en"})
	s.process(statusJob{kind: jobChannelUpdate, userID: "1", userName: "alpha", game: "Science & Technology", language: "en"})

	_, got := decodeStatus(t, s.live)
	if got["dormant"].Status == statusPrelive {
		t.Errorf("inactive streamer shown pre-live: %+v", got["dormant"])
	}
	if got["alpha"].Status != statusPrelive {
		t.Errorf("active streamer not pre-live: %+v", got["alpha"])
	}
	if strings.Contains(gitOutput(t, origin, "show", "main:index.md"), "dormant") {
		t.Error("inactive streamer reached index.md")
	}
}

func TestSchedulePreliveNeverMarksInactiveStreamer(t *testing.T) {
	s, _ := newSchedulePreliveRepo(t, map[string]scheduledSegment{"dormant": soon(5*time.Minute, "Science & Technology")})
	withLiveState(t, s, schedNow, userAlpha, userDormant)

	s.process(statusJob{kind: jobFunc, fn: s.checkSchedulePrelive})
	s.live.rebuild(schedNow)

	if _, got := decodeStatus(t, s.live); got["dormant"].Status == statusPrelive {
		t.Errorf("inactive streamer shown pre-live from their schedule: %+v", got["dormant"])
	}
}

// A 🟡 from the schedule ends schedulePreliveGrace after the scheduled
// start if they don't go live, not a full preliveTTL after it was set.
func TestSchedulePreliveEndsAfterNoShow(t *testing.T) {
	s, _ := newSchedulePreliveRepo(t, map[string]scheduledSegment{"alpha": soon(10*time.Minute, "Science & Technology")})
	withLiveState(t, s, schedNow, userAlpha, userDormant)

	s.process(statusJob{kind: jobFunc, fn: s.checkSchedulePrelive})
	if _, got := decodeStatus(t, s.live); got["alpha"].Status != statusPrelive {
		t.Fatalf("alpha not pre-live: %+v", got["alpha"])
	}

	deadline := schedNow.Add(10*time.Minute + schedulePreliveGrace)
	s.live.expirePrelive(deadline.Add(-time.Second))
	s.live.rebuild(deadline.Add(-time.Second))
	if _, got := decodeStatus(t, s.live); got["alpha"].Status != statusPrelive {
		t.Errorf("ended before the grace period: %+v", got["alpha"])
	}
	s.live.expirePrelive(deadline)
	s.live.rebuild(deadline)
	if _, got := decodeStatus(t, s.live); got["alpha"].Status == statusPrelive {
		t.Errorf("still pre-live %s after a no-show: %+v", schedulePreliveGrace, got["alpha"])
	}
}

// channel.update markers keep the full preliveTTL.
func TestChannelUpdatePreliveKeepsFullTTL(t *testing.T) {
	c := useClock(t, preliveNow)
	s, _ := newGitFixture(t, []string{offlineLine("alpha")}, nil, "alpha,\n", "")
	s.client = notLive()
	withLiveState(t, s, c.now, userAlpha)
	s.process(statusJob{kind: jobChannelUpdate, userID: "1", userName: "alpha", game: "Science & Technology", language: "en"})

	at := preliveNow.Add(preliveTTL - time.Minute)
	s.live.expirePrelive(at)
	s.live.rebuild(at)
	if _, got := decodeStatus(t, s.live); got["alpha"].Status != statusPrelive {
		t.Errorf("channel.update marker ended early: %+v", got["alpha"])
	}
}
