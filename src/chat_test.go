package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicklaw5/helix/v2"
)

func TestParseIRC(t *testing.T) {
	m := parseIRC(`@badge-info=;display-name=A\sB;user-id=7 :ab!ab@ab.tmi.twitch.tv PRIVMSG #chan :hi there :)`)
	if m.command != "PRIVMSG" || m.prefix != "ab!ab@ab.tmi.twitch.tv" || m.tags["display-name"] != "A B" || m.tags["user-id"] != "7" ||
		len(m.params) != 2 || m.params[0] != "#chan" || m.trailing() != "hi there :)" {
		t.Errorf("parsed %+v", m)
	}
	if m := parseIRC("PING :tmi.twitch.tv"); m.command != "PING" || m.trailing() != "tmi.twitch.tv" {
		t.Errorf("PING parsed %+v", m)
	}
	if m := parseIRC(":tmi.twitch.tv 001 bot :Welcome, GLHF!"); m.command != "001" || m.params[0] != "bot" {
		t.Errorf("001 parsed %+v", m)
	}
	if m := parseIRC(":tmi.twitch.tv RECONNECT"); m.command != "RECONNECT" || len(m.params) != 0 {
		t.Errorf("RECONNECT parsed %+v", m)
	}
}

// chatFixture is a bot talking to a fake Twitch IRC server over a pipe.
type chatFixture struct {
	t    *testing.T
	s    *StreamersRepo
	b    *chatBot
	srv  net.Conn
	rd   *bufio.Reader
	errc chan error
}

func newChatFixture(t *testing.T) (*chatFixture, *clock) {
	t.Helper()
	c := useClock(t, statsNow)
	users := &fakeUsers{}
	users.set(userAlpha, userBeta)
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, ""), users: users}
	s.startWorker()
	s.runInWorker(func() {
		s.resolveRoster([]string{"alpha", "beta"}, c.now)
		s.live.online("1", helix.Stream{UserID: "1", UserLogin: "alpha", GameName: "Just Chatting", StartedAt: c.now, ViewerCount: 10}, c.now)
		s.live.rebuild(c.now)
	})
	srv, cli := net.Pipe()
	f := &chatFixture{t: t, s: s, srv: srv, rd: bufio.NewReader(srv), errc: make(chan error, 1)}
	f.b = newChatBot(s, "ISSBot", func(bool) (string, error) { return "tok", nil })
	f.b.dial = func(string) (net.Conn, error) { return cli, nil }
	t.Cleanup(func() { srv.Close() })
	return f, c
}

func (f *chatFixture) expect(want string) {
	f.t.Helper()
	f.srv.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := f.rd.ReadString('\n')
	if err != nil {
		f.t.Fatalf("waiting for %q: %s", want, err)
	}
	if got := strings.TrimRight(line, "\r\n"); got != want {
		f.t.Fatalf("got %q, want %q", got, want)
	}
}

func (f *chatFixture) say(lines ...string) {
	f.t.Helper()
	for _, l := range lines {
		f.srv.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := f.srv.Write([]byte(l + "\r\n")); err != nil {
			f.t.Fatalf("writing %q: %s", l, err)
		}
	}
}

// sync waits until the bot has handled everything sent so far.
func (f *chatFixture) sync() {
	f.t.Helper()
	f.say("PING :sync")
	f.expect("PONG :sync")
}

func (f *chatFixture) login() {
	f.t.Helper()
	go func() { f.errc <- f.b.session(false) }()
	f.expect("CAP REQ :twitch.tv/tags twitch.tv/commands")
	f.expect("PASS oauth:tok")
	f.expect("NICK issbot")
}

func TestChatCountsPerStream(t *testing.T) {
	f, c := newChatFixture(t)
	f.login()
	f.say(":tmi.twitch.tv 001 issbot :Welcome, GLHF!")
	f.expect("JOIN #alpha") // live; beta is not

	f.say(
		`@user-id=100;first-msg=1;room-id=1 :u100!u100@u100.tmi.twitch.tv PRIVMSG #alpha :hello`,
		`@user-id=101;first-msg=0;bits=50;room-id=1 :u101!u101@u101.tmi.twitch.tv PRIVMSG #alpha :cheer50 nice`,
		`@user-id=100;room-id=1 :u100!u100@u100.tmi.twitch.tv PRIVMSG #alpha :again`,
		`@msg-id=sub;user-id=102;room-id=1 :tmi.twitch.tv USERNOTICE #alpha`,
		`@msg-id=resub;user-id=103;room-id=1 :tmi.twitch.tv USERNOTICE #alpha :3 months!`,
		`@msg-id=submysterygift;msg-param-mass-gift-count=2;user-id=104;room-id=1 :tmi.twitch.tv USERNOTICE #alpha`,
		`@msg-id=subgift;user-id=104;room-id=1 :tmi.twitch.tv USERNOTICE #alpha`,
		`@msg-id=subgift;user-id=104;room-id=1 :tmi.twitch.tv USERNOTICE #alpha`,
		`@msg-id=raid;user-id=200;msg-param-login=raider;msg-param-displayName=Raider;msg-param-viewerCount=12;room-id=1 :tmi.twitch.tv USERNOTICE #alpha`,
		`@user-id=300;room-id=5 :x!x@x.tmi.twitch.tv PRIVMSG #notjoined :ignored`,
	)
	f.sync()
	c.advance(10 * time.Minute)
	f.b.flush()
	f.s.waitIdle()

	var row chatRow
	err := f.s.live.db.db.QueryRow(`SELECT messages, chatters, first_time, subs, resubs, gift_subs, bits, joined_seconds FROM chat_stats`).
		Scan(&row.messages, &row.chatters, &row.firstTime, &row.subs, &row.resubs, &row.giftSubs, &row.bits, &row.joinedSecs)
	if err != nil {
		t.Fatal(err)
	}
	want := chatRow{messages: 3, chatters: 2, firstTime: 1, subs: 1, resubs: 1, giftSubs: 2, bits: 50, joinedSecs: 600}
	if row != want {
		t.Errorf("chat_stats = %+v, want %+v", row, want)
	}
	raids, _ := f.s.live.db.raidsSince(statsNow.Add(-day))
	if len(raids) != 1 || raids[0].fromLogin != "raider" || raids[0].toID != "1" || raids[0].viewers != 12 || raids[0].source != "chat" {
		t.Errorf("raids = %+v", raids)
	}

	// More chat, then the stream ends: counts add up, unique chatters don't
	// double, and the bot leaves.
	f.say(`@user-id=101;room-id=1 :u101!u101@u101.tmi.twitch.tv PRIVMSG #alpha :bye`)
	f.sync()
	c.advance(5 * time.Minute)
	f.s.runInWorker(func() {
		f.s.live.offline("1", false, c.now)
		f.s.live.rebuild(c.now)
	})
	parted := make(chan struct{})
	go func() { f.b.syncRooms(); close(parted) }()
	f.expect("PART #alpha")
	<-parted // syncRooms has queued the final flush
	f.s.waitIdle()
	f.s.live.db.db.QueryRow(`SELECT messages, chatters, joined_seconds FROM chat_stats`).Scan(&row.messages, &row.chatters, &row.joinedSecs)
	if row.messages != 4 || row.chatters != 2 || row.joinedSecs != 900 {
		t.Errorf("after stream end: %+v", row)
	}

	// Stats show it.
	f.s.runInWorker(func() { f.s.live.buildStats(c.now) })
	var st streamerStats
	decodeJSON(t, f.s.live.statsSnap.Load().byID["1"].body, &st)
	ch := st.RecentStreams[0].Chat
	if ch == nil || ch.Messages != 4 || ch.MessagesPerMinute != 0.3 || ch.Bits != 50 || st.Chat30d == nil || st.Chat30d.AvgChatters != 2 ||
		st.Chat30d.ChattersPerViewer == nil || *st.Chat30d.ChattersPerViewer != 0.2 {
		t.Errorf("chat stats: %+v / %+v", ch, st.Chat30d)
	}

	f.srv.Close()
	if err := <-f.errc; err == nil {
		t.Error("session should end with an error when the server hangs up")
	}
}

func TestChatAuthFailure(t *testing.T) {
	f, _ := newChatFixture(t)
	f.login()
	f.say(":tmi.twitch.tv NOTICE * :Login authentication failed")
	if err := <-f.errc; !errors.Is(err, errChatAuth) {
		t.Errorf("err = %v, want errChatAuth", err)
	}
}

func TestChatReconnectRequest(t *testing.T) {
	f, _ := newChatFixture(t)
	f.login()
	f.say(":tmi.twitch.tv 001 issbot :Welcome", ":tmi.twitch.tv RECONNECT")
	f.expect("JOIN #alpha")
	if err := <-f.errc; err == nil || errors.Is(err, errChatAuth) {
		t.Errorf("err = %v", err)
	}
}

type fakeRefresher struct {
	calls []string
	n     int
}

func (f *fakeRefresher) RefreshUserAccessToken(rt string) (*helix.RefreshTokenResponse, error) {
	f.calls = append(f.calls, rt)
	f.n++
	r := &helix.RefreshTokenResponse{}
	r.StatusCode = 200
	r.Data.AccessToken = "access" + itoa(f.n)
	r.Data.RefreshToken = "rotated" + itoa(f.n)
	r.Data.ExpiresIn = 4 * 3600
	return r, nil
}

func TestChatTokenRotation(t *testing.T) {
	c := useClock(t, statsNow)
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, "")}
	s.startWorker()
	ref := &fakeRefresher{}
	tok := &chatToken{s: s, client: ref, envRT: "fromenv"}

	if a, err := tok.get(false); err != nil || a != "access1" {
		t.Fatalf("first token %q %v", a, err)
	}
	if a, _ := tok.get(false); a != "access1" || len(ref.calls) != 1 {
		t.Errorf("cached token not reused: %q %v", a, ref.calls)
	}
	c.advance(4*time.Hour - time.Minute) // near expiry
	if a, _ := tok.get(false); a != "access2" {
		t.Errorf("near expiry: %q", a)
	}
	if a, _ := tok.get(true); a != "access3" {
		t.Errorf("forced: %q", a)
	}
	want := []string{"fromenv", "rotated1", "rotated2"}
	if strings.Join(ref.calls, ",") != strings.Join(want, ",") {
		t.Errorf("refresh tokens used = %v, want %v", ref.calls, want)
	}
}

func decodeJSON(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatal(err)
	}
}
