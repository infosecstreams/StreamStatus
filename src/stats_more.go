package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// Stats from the extra recorded detail (insights.go, chat.go): followers
// gained per stream, chat activity, topics and tags, raids, VODs and
// clips. Hooked into computeStreamerStats and buildStats.

type titleAt struct {
	at    time.Time
	title string
}

type chatRow struct {
	messages, chatters, firstTime, subs, resubs, giftSubs, bits, joinedSecs int
}

// chatSummary is one stream's chat activity.
type chatSummary struct {
	Messages          int     `json:"messages"`
	MessagesPerMinute float64 `json:"messages_per_minute"`
	Chatters          int     `json:"chatters"`
	FirstTimeChatters int     `json:"first_time_chatters"`
	Subs              int     `json:"subs"`
	Resubs            int     `json:"resubs"`
	GiftSubs          int     `json:"gift_subs"`
	Bits              int     `json:"bits"`
}

func (c chatRow) summary() *chatSummary {
	s := &chatSummary{Messages: c.messages, Chatters: c.chatters, FirstTimeChatters: c.firstTime,
		Subs: c.subs, Resubs: c.resubs, GiftSubs: c.giftSubs, Bits: c.bits}
	if c.joinedSecs > 0 {
		s.MessagesPerMinute = round1(float64(c.messages) / (float64(c.joinedSecs) / 60))
	}
	return s
}

// chatTotals is chat activity over the last 30 days.
type chatTotals struct {
	Streams              int      `json:"streams"` // streams with chat data
	MessagesPerMinute    float64  `json:"messages_per_minute"`
	AvgChatters          float64  `json:"avg_chatters"`
	ChattersPerViewer    *float64 `json:"chatters_per_viewer,omitempty"`
	FirstTimeChatters    int      `json:"first_time_chatters"`
	Subs                 int      `json:"subs"`
	Resubs               int      `json:"resubs"`
	GiftSubs             int      `json:"gift_subs"`
	Bits                 int      `json:"bits"`
	messages, joinedSecs int
	chatters             int
}

type topicCount struct {
	Topic   string `json:"topic"`
	Streams int    `json:"streams"`
}

type tagCount struct {
	Tag     string `json:"tag"`
	Streams int    `json:"streams"`
}

type raidPartner struct {
	ID          string `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"display_name,omitempty"`
	RaidsOut    int    `json:"raids_out"` // this streamer raided them
	RaidsIn     int    `json:"raids_in"`  // they raided this streamer
}

// streamerExtras are the extra fields of /streamstatus/stats/<id>.json (embedded,
// so they appear at the top level).
type streamerExtras struct {
	FollowersPerStream30d *float64      `json:"followers_per_stream_30d,omitempty"`
	Chat30d               *chatTotals   `json:"chat_30d,omitempty"`
	Topics30d             []topicCount  `json:"topics_30d"`
	Tags30d               []tagCount    `json:"tags_30d"`
	RaidsOut30d           int           `json:"raids_out_30d"`
	RaidsIn30d            int           `json:"raids_in_30d"`
	RaidViewersOut30d     int           `json:"raid_viewers_out_30d"`
	RaidViewersIn30d      int           `json:"raid_viewers_in_30d"`
	RaidPartners90d       []raidPartner `json:"raid_partners_90d"`
	RecentVODs            []mediaItem   `json:"recent_vods"`
	TopClips30d           []mediaItem   `json:"top_clips_30d"`
}

// extrasAcc accumulates per-session extras while computing a streamer.
type extrasAcc struct {
	followerGains []int
	chat          chatTotals
	topics        map[string]int
	tags          map[string]int
	tagName       map[string]string
}

// loadMoreStats adds the extra detail to d.
func (st *store) loadMoreStats(d *statsData, now time.Time) error {
	d.tags = map[int64][]string{}
	d.titlesSince = map[string][]titleAt{}
	d.chat = map[int64]chatRow{}

	rows, err := st.db.Query(`SELECT t.session_id, t.tag FROM session_tags t JOIN sessions s ON s.id = t.session_id
		WHERE COALESCE(s.ended_at, s.last_seen) >= ?`, now.Add(-31*day).Unix())
	if err != nil {
		return err
	}
	for rows.Next() {
		var sid int64
		var tag string
		if err := rows.Scan(&sid, &tag); err != nil {
			rows.Close()
			return err
		}
		d.tags[sid] = append(d.tags[sid], tag)
	}
	rows.Close()

	rows, err = st.db.Query(`SELECT user_id, at, title FROM titles WHERE at >= ? ORDER BY at`, now.Add(-31*day).Unix())
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var at int64
		var t titleAt
		if err := rows.Scan(&id, &at, &t.title); err != nil {
			rows.Close()
			return err
		}
		t.at = time.Unix(at, 0).UTC()
		d.titlesSince[id] = append(d.titlesSince[id], t)
	}
	rows.Close()

	rows, err = st.db.Query(`SELECT session_id, messages, chatters, first_time, subs, resubs, gift_subs, bits, joined_seconds FROM chat_stats`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var sid int64
		var c chatRow
		if err := rows.Scan(&sid, &c.messages, &c.chatters, &c.firstTime, &c.subs, &c.resubs, &c.giftSubs, &c.bits, &c.joinedSecs); err != nil {
			rows.Close()
			return err
		}
		d.chat[sid] = c
	}
	rows.Close()

	if d.raids, err = st.raidsSince(now.Add(-90 * day)); err != nil {
		return err
	}
	if d.videos, err = st.loadMedia("videos"); err != nil {
		return err
	}
	d.clips, err = st.loadMedia("clips")
	return err
}

// sessionExtras fills rs and acc from one stream (one with infosec time).
func sessionExtras(rs *recentStream, acc *extrasAcc, ss sessionRow, end time.Time, valid30 bool, d *statsData) {
	if ss.followersStart.Valid && ss.followersEnd.Valid {
		g := int(ss.followersEnd.Int64 - ss.followersStart.Int64)
		rs.FollowersGained = &g
		if valid30 {
			acc.followerGains = append(acc.followerGains, g)
		}
	}
	if c, ok := d.chat[ss.id]; ok {
		rs.Chat = c.summary()
		if valid30 {
			t := &acc.chat
			t.Streams++
			t.messages += c.messages
			t.joinedSecs += c.joinedSecs
			t.chatters += c.chatters
			t.FirstTimeChatters += c.firstTime
			t.Subs += c.subs
			t.Resubs += c.resubs
			t.GiftSubs += c.giftSubs
			t.Bits += c.bits
		}
	}
	if !valid30 {
		return
	}
	var titles []string
	for _, t := range d.titlesSince[ss.userID] {
		if !t.at.Before(ss.start.Add(-30*time.Minute)) && !t.at.After(end) {
			titles = append(titles, t.title)
		}
	}
	tags := d.tags[ss.id]
	for _, tp := range streamTopics(tags, titles) {
		acc.topics[tp]++
	}
	seen := map[string]bool{}
	for _, t := range tags {
		k := strings.ToLower(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		acc.tags[k]++
		if _, ok := acc.tagName[k]; !ok {
			acc.tagName[k] = t
		}
	}
}

// finishExtras computes the streamer-wide extras.
func finishExtras(id string, acc *extrasAcc, avgViewers *float64, d *statsData, now time.Time) streamerExtras {
	var x streamerExtras
	if n := len(acc.followerGains); n > 0 {
		sum := 0
		for _, g := range acc.followerGains {
			sum += g
		}
		x.FollowersPerStream30d = ptrF(round1(float64(sum) / float64(n)))
	}
	if c := acc.chat; c.Streams > 0 {
		if c.joinedSecs > 0 {
			c.MessagesPerMinute = round1(float64(c.messages) / (float64(c.joinedSecs) / 60))
		}
		c.AvgChatters = round1(float64(c.chatters) / float64(c.Streams))
		if avgViewers != nil && *avgViewers > 0 {
			c.ChattersPerViewer = ptrF(round2(c.AvgChatters / *avgViewers))
		}
		x.Chat30d = &c
	}

	x.Topics30d = []topicCount{}
	for _, tp := range topics { // keeps a stable order among equal counts
		if n := acc.topics[tp.name]; n > 0 {
			x.Topics30d = append(x.Topics30d, topicCount{Topic: tp.name, Streams: n})
		}
	}
	sort.SliceStable(x.Topics30d, func(i, j int) bool { return x.Topics30d[i].Streams > x.Topics30d[j].Streams })
	x.Tags30d = []tagCount{}
	for k, n := range acc.tags {
		x.Tags30d = append(x.Tags30d, tagCount{Tag: acc.tagName[k], Streams: n})
	}
	sort.Slice(x.Tags30d, func(i, j int) bool {
		if x.Tags30d[i].Streams != x.Tags30d[j].Streams {
			return x.Tags30d[i].Streams > x.Tags30d[j].Streams
		}
		return strings.ToLower(x.Tags30d[i].Tag) < strings.ToLower(x.Tags30d[j].Tag)
	})
	if len(x.Tags30d) > 10 {
		x.Tags30d = x.Tags30d[:10]
	}

	w30 := now.Add(-30 * day)
	partners := map[string]*raidPartner{}
	partner := func(pid, login, name string) *raidPartner {
		p := partners[pid]
		if p == nil {
			p = &raidPartner{ID: pid, Login: login, DisplayName: name}
			partners[pid] = p
		}
		return p
	}
	for _, r := range d.raids {
		switch id {
		case r.fromID:
			partner(r.toID, r.toLogin, r.toName).RaidsOut++
			if !r.at.Before(w30) {
				x.RaidsOut30d++
				x.RaidViewersOut30d += r.viewers
			}
		case r.toID:
			partner(r.fromID, r.fromLogin, r.fromName).RaidsIn++
			if !r.at.Before(w30) {
				x.RaidsIn30d++
				x.RaidViewersIn30d += r.viewers
			}
		}
	}
	x.RaidPartners90d = []raidPartner{}
	for _, p := range partners {
		x.RaidPartners90d = append(x.RaidPartners90d, *p)
	}
	sort.Slice(x.RaidPartners90d, func(i, j int) bool {
		a, b := x.RaidPartners90d[i], x.RaidPartners90d[j]
		if a.RaidsOut+a.RaidsIn != b.RaidsOut+b.RaidsIn {
			return a.RaidsOut+a.RaidsIn > b.RaidsOut+b.RaidsIn
		}
		return a.Login < b.Login
	})
	if len(x.RaidPartners90d) > 5 {
		x.RaidPartners90d = x.RaidPartners90d[:5]
	}

	x.RecentVODs = append([]mediaItem{}, d.videos[id]...) // newest first
	if len(x.RecentVODs) > 5 {
		x.RecentVODs = x.RecentVODs[:5]
	}
	x.TopClips30d = topClips(d.clips[id], w30, 5)
	return x
}

// topClips returns the n most viewed clips created since from.
func topClips(clips []mediaItem, from time.Time, n int) []mediaItem {
	out := []mediaItem{}
	for _, c := range clips {
		if !parseHelixTime(c.CreatedAt).Before(from) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Views > out[j].Views })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// communityExtras are the extra fields of /streamstatus/stats.json.
type communityExtras struct {
	Raids30d    int          `json:"raids_30d"`  // involving a roster streamer
	Topics30d   []topicCount `json:"topics_30d"` // streams counts summed over streamers
	TopClips30d []mediaItem  `json:"top_clips_30d"`
}

func buildCommunityExtras(all []streamerStats, d *statsData, roster map[string]bool, now time.Time) communityExtras {
	var x communityExtras
	w30 := now.Add(-30 * day)
	for _, r := range d.raids {
		if !r.at.Before(w30) && (roster[r.fromID] || roster[r.toID]) {
			x.Raids30d++
		}
	}
	counts := map[string]int{}
	var clips []mediaItem
	for _, s := range all {
		for _, t := range s.Topics30d {
			counts[t.Topic] += t.Streams
		}
		for _, c := range d.clips[s.ID] {
			c.StreamerID, c.StreamerLogin = s.ID, s.Login
			clips = append(clips, c)
		}
	}
	x.Topics30d = []topicCount{}
	for _, tp := range topics {
		if n := counts[tp.name]; n > 0 {
			x.Topics30d = append(x.Topics30d, topicCount{Topic: tp.name, Streams: n})
		}
	}
	sort.SliceStable(x.Topics30d, func(i, j int) bool { return x.Topics30d[i].Streams > x.Topics30d[j].Streams })
	x.TopClips30d = topClips(clips, w30, 10)
	return x
}

// --- raid graph: /streamstatus/raids.json

type raidNode struct {
	ID          string `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"display_name,omitempty"`
	Roster      bool   `json:"roster"`
}

type raidEdge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Raids   int    `json:"raids"`
	Viewers int    `json:"viewers"`
	LastAt  string `json:"last_at"`
}

// raidGraph is who raided whom in the last 90 days, for every raid with a
// roster streamer on either side.
type raidGraph struct {
	GeneratedAt string     `json:"generated_at"`
	Since       string     `json:"since"`
	Nodes       []raidNode `json:"nodes"`
	Edges       []raidEdge `json:"edges"`
}

func (l *liveState) buildRaidGraph(d *statsData, now time.Time) raidGraph {
	g := raidGraph{GeneratedAt: rfc3339(now), Since: rfc3339(now.Add(-90 * day)), Nodes: []raidNode{}, Edges: []raidEdge{}}
	nodes := map[string]*raidNode{}
	node := func(id, login, name string) {
		if nodes[id] != nil {
			return
		}
		n := &raidNode{ID: id, Login: login, DisplayName: name, Roster: l.roster[id]}
		if u := l.users[id]; u != nil {
			n.Login = u.login
			if u.displayName != "" {
				n.DisplayName = u.displayName
			}
		}
		nodes[id] = n
	}
	edges := map[[2]string]*raidEdge{}
	for _, r := range d.raids {
		if !l.roster[r.fromID] && !l.roster[r.toID] {
			continue
		}
		node(r.fromID, r.fromLogin, r.fromName)
		node(r.toID, r.toLogin, r.toName)
		k := [2]string{r.fromID, r.toID}
		e := edges[k]
		if e == nil {
			e = &raidEdge{From: r.fromID, To: r.toID}
			edges[k] = e
		}
		e.Raids++
		e.Viewers += r.viewers
		e.LastAt = rfc3339(r.at) // raids are oldest first
	}
	for _, n := range nodes {
		g.Nodes = append(g.Nodes, *n)
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].Login < g.Nodes[j].Login })
	for _, e := range edges {
		g.Edges = append(g.Edges, *e)
	}
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		return g.Edges[i].To < g.Edges[j].To
	})
	return g
}

// handleRaids serves /streamstatus/raids.json.
func (s *StreamersRepo) handleRaids(w http.ResponseWriter, r *http.Request) {
	var doc *apiDoc
	if s.live != nil {
		if d := s.live.statsSnap.Load(); d != nil {
			doc = d.raids
		}
	}
	serveDoc(w, r, doc)
}

// topTopicNames returns the names of the first n topics.
func topTopicNames(ts []topicCount, n int) []string {
	out := []string{}
	for i := 0; i < len(ts) && i < n; i++ {
		out = append(out, ts[i].Topic)
	}
	return out
}
