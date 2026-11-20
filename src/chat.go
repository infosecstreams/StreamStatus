package main

import (
	"bufio"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nicklaw5/helix/v2"
	log "github.com/sirupsen/logrus"
)

// Chat activity. With SS_CHAT_LOGIN and SS_CHAT_REFRESH_TOKEN (a user
// token of our own bot account with the chat:read scope) the bot joins
// the Twitch chat of every roster streamer who is live, read-only, and
// counts per stream: messages, unique chatters, first-time chatters,
// subs, resubs, gift subs and bits. Raids announced in chat are recorded
// too. Only counts are stored: no message text, and chatter IDs are kept
// in memory only, to count unique chatters.

const (
	chatAddr          = "irc.chat.twitch.tv:6697"
	chatFlushInterval = time.Minute
	chatSyncInterval  = 5 * time.Second
	// Twitch allows 20 JOINs per 10 s for a normal account.
	chatJoinsPer10s = 15
	chatReadTimeout = 6 * time.Minute // Twitch PINGs about every 5 min
)

var chatBackoff = []time.Duration{5 * time.Second, 15 * time.Second, time.Minute, 5 * time.Minute}

// chatCounts are counters for one stream's chat.
type chatCounts struct {
	messages, firstTime, subs, resubs, giftSubs, bits int
}

func (c *chatCounts) add(o chatCounts) {
	c.messages += o.messages
	c.firstTime += o.firstTime
	c.subs += o.subs
	c.resubs += o.resubs
	c.giftSubs += o.giftSubs
	c.bits += o.bits
}

// chatRoom is the state of one channel we want to be in.
type chatRoom struct {
	id        string
	joined    bool
	lastFlush time.Time // or join time
	delta     chatCounts
	chatters  map[string]struct{}
	// joinedSecs is time joined since the last flush.
	joinedSecs float64
}

// chatDelta is one room's activity since the last flush.
type chatDelta struct {
	userID     string
	counts     chatCounts
	chatters   int
	joinedSecs int
}

// chatBot is a read-only Twitch IRC client.
type chatBot struct {
	s     *StreamersRepo
	login string
	addr  string
	// token returns an access token; force asks for a new one.
	token func(force bool) (string, error)
	dial  func(addr string) (net.Conn, error)
	now   func() time.Time

	mu    sync.Mutex
	rooms map[string]*chatRoom // by channel login
	conn  net.Conn
	joins []time.Time // recent JOINs, for the rate limit
}

func newChatBot(s *StreamersRepo, login string, token func(bool) (string, error)) *chatBot {
	return &chatBot{
		s: s, login: strings.ToLower(login), addr: chatAddr, token: token,
		dial: func(addr string) (net.Conn, error) {
			return tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, &tls.Config{MinVersion: tls.VersionTLS12})
		},
		now:   func() time.Time { return timeNow() },
		rooms: map[string]*chatRoom{},
	}
}

// start runs the client forever, reconnecting with backoff.
func (b *chatBot) start() {
	go func() {
		force, fails := false, 0
		for {
			err := b.session(force)
			force = errors.Is(err, errChatAuth)
			if err != nil {
				log.Warnf("chat: %s", err)
			}
			wait := chatBackoff[min(fails, len(chatBackoff)-1)]
			fails++
			if fails >= 5 && fails%5 == 0 {
				log.Errorf("chat: %d connection attempts in a row failed, last: %v", fails, err)
			}
			time.Sleep(wait)
		}
	}()
}

var errChatAuth = errors.New("chat login failed (token expired or missing chat:read?)")

// session connects, authenticates and handles one connection until it
// ends.
func (b *chatBot) session(forceToken bool) error {
	tok, err := b.token(forceToken)
	if err != nil {
		return fmt.Errorf("getting chat token: %w", err)
	}
	conn, err := b.dial(b.addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	b.mu.Lock()
	b.conn = conn
	for _, r := range b.rooms {
		r.joined = false
	}
	b.mu.Unlock()
	defer func() {
		b.flush()
		b.mu.Lock()
		b.conn = nil
		for _, r := range b.rooms {
			r.joined = false
		}
		b.mu.Unlock()
	}()

	for _, l := range []string{
		"CAP REQ :twitch.tv/tags twitch.tv/commands",
		"PASS oauth:" + tok,
		"NICK " + b.login,
	} {
		if err := b.send(l); err != nil {
			return err
		}
	}

	lines := make(chan string)
	readErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		rd := bufio.NewReaderSize(conn, 64<<10)
		for {
			conn.SetReadDeadline(time.Now().Add(chatReadTimeout))
			line, err := rd.ReadString('\n')
			if err != nil {
				readErr <- err
				close(lines)
				return
			}
			select {
			case lines <- strings.TrimRight(line, "\r\n"):
			case <-done:
				return
			}
		}
	}()

	syncT := time.NewTicker(chatSyncInterval)
	defer syncT.Stop()
	flushT := time.NewTicker(chatFlushInterval)
	defer flushT.Stop()
	welcomed := false
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return <-readErr
			}
			m := parseIRC(line)
			switch m.command {
			case "001":
				welcomed = true
				log.Infof("chat: connected as %s", b.login)
				b.syncRooms()
			case "PING":
				b.send("PONG :" + m.trailing())
			case "RECONNECT":
				return errors.New("server asked to reconnect")
			case "NOTICE":
				if !welcomed && strings.Contains(strings.ToLower(m.trailing()), "auth") {
					return errChatAuth
				}
			default:
				b.handle(m)
			}
		case <-syncT.C:
			if welcomed {
				b.syncRooms()
			}
		case <-flushT.C:
			b.flush()
		}
	}
}

func (b *chatBot) send(line string) error {
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := conn.Write([]byte(line + "\r\n"))
	return err
}

// syncRooms joins channels that went live and leaves ones that ended,
// within the JOIN rate limit.
func (b *chatBot) syncRooms() {
	want := map[string]string{}
	if b.s.live != nil {
		if w := b.s.live.chatWanted.Load(); w != nil {
			want = *w
		}
	}
	now := b.now()
	var join, part, gone []string
	b.mu.Lock()
	for login, r := range b.rooms {
		if _, ok := want[login]; !ok {
			gone = append(gone, login)
			if r.joined {
				part = append(part, login)
				r.joinedSecs += now.Sub(r.lastFlush).Seconds()
				r.joined = false
			}
		}
	}
	recent := b.joins[:0]
	for _, t := range b.joins {
		if now.Sub(t) < 10*time.Second {
			recent = append(recent, t)
		}
	}
	b.joins = recent
	logins := make([]string, 0, len(want))
	for l := range want {
		logins = append(logins, l)
	}
	sort.Strings(logins)
	for _, login := range logins {
		r := b.rooms[login]
		if r == nil {
			r = &chatRoom{id: want[login], chatters: map[string]struct{}{}}
			b.rooms[login] = r
		}
		if r.joined || len(b.joins) >= chatJoinsPer10s {
			continue
		}
		r.joined, r.lastFlush = true, now
		b.joins = append(b.joins, now)
		join = append(join, login)
	}
	b.mu.Unlock()

	for _, login := range part {
		b.send("PART #" + login)
	}
	if len(gone) > 0 {
		// Flush their last counts, then forget them (a new stream starts
		// with a new set of chatters).
		b.flush()
		b.mu.Lock()
		for _, login := range gone {
			delete(b.rooms, login)
		}
		b.mu.Unlock()
	}
	for _, login := range join {
		b.send("JOIN #" + login)
	}
}

// handle counts one chat line.
func (b *chatBot) handle(m ircMsg) {
	if len(m.params) == 0 || !strings.HasPrefix(m.params[0], "#") {
		return
	}
	login := strings.TrimPrefix(m.params[0], "#")
	b.mu.Lock()
	r := b.rooms[login]
	if r == nil {
		b.mu.Unlock()
		return
	}
	switch m.command {
	case "PRIVMSG":
		r.delta.messages++
		if uid := m.tags["user-id"]; uid != "" {
			r.chatters[uid] = struct{}{}
		}
		if m.tags["first-msg"] == "1" {
			r.delta.firstTime++
		}
		if n, err := strconv.Atoi(m.tags["bits"]); err == nil && n > 0 {
			r.delta.bits += n
		}
	case "USERNOTICE":
		switch m.tags["msg-id"] {
		case "sub":
			r.delta.subs++
		case "resub":
			r.delta.resubs++
		case "subgift", "anonsubgift":
			r.delta.giftSubs++
		case "raid":
			viewers, _ := strconv.Atoi(m.tags["msg-param-viewerCount"])
			raid := raidRecord{at: b.now(), fromID: m.tags["user-id"], fromLogin: m.tags["msg-param-login"],
				fromName: m.tags["msg-param-displayName"], toID: r.id, toLogin: login, viewers: viewers, source: "chat"}
			b.mu.Unlock()
			if b.s.live != nil {
				b.s.enqueue(statusJob{kind: jobFunc, fn: func() { b.s.live.recordRaid(raid) }})
			}
			return
		}
	}
	b.mu.Unlock()
}

// flush hands the counts since the last flush to the worker.
func (b *chatBot) flush() {
	now := b.now()
	var deltas []chatDelta
	b.mu.Lock()
	for _, r := range b.rooms {
		if r.joined && !r.lastFlush.IsZero() {
			r.joinedSecs += now.Sub(r.lastFlush).Seconds()
			r.lastFlush = now
		}
		if r.delta == (chatCounts{}) && r.joinedSecs < 1 {
			continue
		}
		deltas = append(deltas, chatDelta{userID: r.id, counts: r.delta, chatters: len(r.chatters), joinedSecs: int(r.joinedSecs)})
		r.delta, r.joinedSecs = chatCounts{}, r.joinedSecs-float64(int(r.joinedSecs))
	}
	b.mu.Unlock()
	if len(deltas) == 0 || b.s.live == nil {
		return
	}
	db := b.s.live.db
	if !b.s.enqueue(statusJob{kind: jobFunc, fn: func() {
		for _, d := range deltas {
			if err := db.addChatStats(d, now); err != nil {
				log.Errorf("chat: saving counts for %s: %s", d.userID, err)
			}
		}
	}}) {
		log.Warn("chat: counts dropped, event queue full")
	}
}

// addChatStats adds d to the stream it belongs to: the one in progress,
// or one that ended in the last 10 minutes.
func (st *store) addChatStats(d chatDelta, now time.Time) error {
	var sid int64
	err := st.db.QueryRow(`SELECT id FROM sessions WHERE user_id = ? AND (ended_at IS NULL OR ended_at >= ?)
		ORDER BY started_at DESC LIMIT 1`, d.userID, now.Add(-10*time.Minute).Unix()).Scan(&sid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	c := d.counts
	_, err = st.db.Exec(`INSERT INTO chat_stats (session_id, messages, chatters, first_time, subs, resubs, gift_subs, bits, joined_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (session_id) DO UPDATE SET
			messages = messages + excluded.messages,
			chatters = MAX(chatters, excluded.chatters),
			first_time = first_time + excluded.first_time,
			subs = subs + excluded.subs,
			resubs = resubs + excluded.resubs,
			gift_subs = gift_subs + excluded.gift_subs,
			bits = bits + excluded.bits,
			joined_seconds = joined_seconds + excluded.joined_seconds`,
		sid, c.messages, d.chatters, c.firstTime, c.subs, c.resubs, c.giftSubs, c.bits, d.joinedSecs)
	return err
}

// ircMsg is a parsed IRC line.
type ircMsg struct {
	tags    map[string]string
	prefix  string
	command string
	params  []string // the trailing parameter is last
}

func (m ircMsg) trailing() string {
	if len(m.params) == 0 {
		return ""
	}
	return m.params[len(m.params)-1]
}

var tagUnescaper = strings.NewReplacer(`\:`, ";", `\s`, " ", `\\`, `\`, `\r`, "\r", `\n`, "\n")

// parseIRC parses "@tags :prefix COMMAND params :trailing".
func parseIRC(line string) ircMsg {
	m := ircMsg{tags: map[string]string{}}
	if strings.HasPrefix(line, "@") {
		raw, rest, _ := strings.Cut(line[1:], " ")
		for _, kv := range strings.Split(raw, ";") {
			k, v, _ := strings.Cut(kv, "=")
			m.tags[k] = tagUnescaper.Replace(v)
		}
		line = rest
	}
	if strings.HasPrefix(line, ":") {
		m.prefix, line, _ = strings.Cut(line[1:], " ")
	}
	var trailing string
	hasTrailing := false
	if i := strings.Index(line, " :"); i >= 0 {
		trailing, hasTrailing = line[i+2:], true
		line = line[:i]
	} else if strings.HasPrefix(line, ":") {
		trailing, hasTrailing, line = line[1:], true, ""
	}
	fields := strings.Fields(line)
	if len(fields) > 0 {
		m.command, m.params = fields[0], fields[1:]
	}
	if hasTrailing {
		m.params = append(m.params, trailing)
	}
	return m
}

// --- token

// tokenRefresher renews user tokens; *helix.Client satisfies it.
type tokenRefresher interface {
	RefreshUserAccessToken(refreshToken string) (*helix.RefreshTokenResponse, error)
}

// chatToken keeps the bot account's user token fresh. Twitch may rotate
// the refresh token, so the latest one is kept in the database (meta
// chat_refresh_token) and preferred over SS_CHAT_REFRESH_TOKEN.
type chatToken struct {
	s       *StreamersRepo
	client  tokenRefresher
	envRT   string
	access  string
	expires time.Time
}

// get returns a valid access token, renewing it on the worker (the Helix
// client and database live there) when it's near expiry or force is set.
func (t *chatToken) get(force bool) (string, error) {
	var tok string
	var err error
	if !t.s.runInWorker(func() { tok, err = t.renew(force) }) {
		return "", errors.New("worker queue full")
	}
	return tok, err
}

func (t *chatToken) renew(force bool) (string, error) {
	now := timeNow()
	if !force && t.access != "" && t.expires.After(now.Add(5*time.Minute)) {
		return t.access, nil
	}
	rt := t.envRT
	db := t.s.live.db
	var stored string
	if err := db.db.QueryRow(`SELECT value FROM meta WHERE key = 'chat_refresh_token'`).Scan(&stored); err == nil && stored != "" {
		rt = stored
	}
	resp, err := t.client.RefreshUserAccessToken(rt)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 || resp.Data.AccessToken == "" {
		return "", fmt.Errorf("refreshing chat token: status=%d %s %s", resp.StatusCode, resp.Error, resp.ErrorMessage)
	}
	t.access = resp.Data.AccessToken
	t.expires = now.Add(time.Duration(resp.Data.ExpiresIn) * time.Second)
	if nrt := resp.Data.RefreshToken; nrt != "" && nrt != rt {
		if _, err := db.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('chat_refresh_token', ?)`, nrt); err != nil {
			log.Errorf("chat: saving the new refresh token: %s", err)
		}
	}
	return t.access, nil
}
