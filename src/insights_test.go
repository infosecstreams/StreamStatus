package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nicklaw5/helix/v2"
)

func TestRaidSubscriptionsPlanned(t *testing.T) {
	existing := []helix.EventSubSubscription{
		{ID: "r1", Type: "channel.raid", Status: "enabled", Condition: helix.EventSubCondition{FromBroadcasterUserID: "1"},
			Transport: helix.EventSubTransport{Method: "webhook", Callback: testCallback}},
	}
	var got []string
	for _, c := range planSubscriptionsWith([]string{"1"}, existing, testCallback, "s3cret", true).create {
		got = append(got, c.Type+"/"+c.Condition.BroadcasterUserID+"/"+c.Condition.FromBroadcasterUserID+"/"+c.Condition.ToBroadcasterUserID)
	}
	want := []string{"stream.online/1//", "stream.offline/1//", "channel.update/1//", "channel.raid///1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("create = %v, want %v", got, want)
	}
	// Without live state: as before.
	if p := planSubscriptionsWith([]string{"1"}, nil, testCallback, "s", false); len(p.create) != 3 {
		t.Errorf("raids planned without live state: %d", len(p.create))
	}
}

func raidNotification(from, to string, viewers int) eventSubNotification {
	body, _ := json.Marshal(helix.EventSubChannelRaidEvent{
		FromBroadcasterUserID: from, FromBroadcasterUserLogin: "u" + from, FromBroadcasterUserName: "U" + from,
		ToBroadcasterUserID: to, ToBroadcasterUserLogin: "u" + to, ToBroadcasterUserName: "U" + to, Viewers: viewers,
	})
	return eventSubNotification{Subscription: helix.EventSubSubscription{Type: "channel.raid"}, Event: body}
}

// The raider's and the target's subscriptions both deliver the same raid,
// and chat may announce it too: it is stored once.
func TestRaidRecordedOnce(t *testing.T) {
	c := useClock(t, statsNow)
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, "")}
	for i := 0; i < 2; i++ {
		job, ok := jobFromNotification(raidNotification("1", "2", 42))
		if !ok || job.kind != jobRaid {
			t.Fatalf("job = %+v", job)
		}
		s.process(job)
	}
	c.advance(time.Minute)
	s.live.recordRaid(raidRecord{at: c.now, fromID: "1", fromLogin: "u1", toID: "2", toLogin: "u2", viewers: 40, source: "chat"})
	c.advance(time.Hour)
	job, _ := jobFromNotification(raidNotification("1", "2", 7))
	s.process(job)

	raids, err := s.live.db.raidsSince(statsNow.Add(-day))
	if err != nil || len(raids) != 2 {
		t.Fatalf("raids = %+v, %v", raids, err)
	}
	if r := raids[0]; r.viewers != 42 || r.fromLogin != "u1" || r.toName != "U2" || r.source != "eventsub" {
		t.Errorf("raid = %+v", r)
	}
}

func TestRaidStatsAndGraph(t *testing.T) {
	c := useClock(t, statsNow)
	users := &fakeUsers{}
	users.set(userAlpha, userBeta)
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, ""), users: users}
	s.resolveRoster([]string{"alpha", "beta"}, c.now)
	raid := func(ago time.Duration, from, fromLogin, to, toLogin string, viewers int) {
		s.live.recordRaid(raidRecord{at: statsNow.Add(-ago), fromID: from, fromLogin: fromLogin, toID: to, toLogin: toLogin, viewers: viewers, source: "eventsub"})
	}
	raid(day, "1", "alpha", "2", "beta", 10)
	raid(2*day, "1", "alpha", "2", "beta", 20)
	raid(3*day, "2", "beta", "1", "alpha", 5)
	raid(4*day, "9", "outsider", "1", "alpha", 3)
	raid(50*day, "1", "alpha", "8", "other", 1)   // outside 30d, inside 90d
	raid(5*day, "8", "other", "9", "outsider", 1) // no roster streamer: ignored

	s.process(statusJob{kind: jobStats})
	mux := http.NewServeMux()
	s.registerAPI(mux)

	var one streamerStats
	json.Unmarshal(apiGet(mux, "GET", "/api/stats/alpha.json", nil).Body.Bytes(), &one)
	if one.RaidsOut30d != 2 || one.RaidViewersOut30d != 30 || one.RaidsIn30d != 2 || one.RaidViewersIn30d != 8 {
		t.Errorf("alpha raids: %+v", one.streamerExtras)
	}
	if len(one.RaidPartners90d) != 3 || one.RaidPartners90d[0].Login != "beta" || one.RaidPartners90d[0].RaidsOut != 2 || one.RaidPartners90d[0].RaidsIn != 1 {
		t.Errorf("partners = %+v", one.RaidPartners90d)
	}

	var comm communityStats
	json.Unmarshal(apiGet(mux, "GET", "/api/stats.json", nil).Body.Bytes(), &comm)
	if comm.Raids30d != 4 {
		t.Errorf("community raids = %d, want 4", comm.Raids30d)
	}

	rec := apiGet(mux, "GET", "/api/raids.json", nil)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("raids.json: %d", rec.Code)
	}
	var g raidGraph
	json.Unmarshal(rec.Body.Bytes(), &g)
	var edges []string
	for _, e := range g.Edges {
		edges = append(edges, e.From+">"+e.To+":"+itoa(e.Raids)+"/"+itoa(e.Viewers))
	}
	want := []string{"1>2:2/30", "1>8:1/1", "2>1:1/5", "9>1:1/3"}
	if !reflect.DeepEqual(edges, want) {
		t.Errorf("edges = %v, want %v", edges, want)
	}
	roster := map[string]bool{}
	for _, n := range g.Nodes {
		roster[n.Login] = n.Roster
	}
	if len(g.Nodes) != 4 || !roster["alpha"] || roster["outsider"] {
		t.Errorf("nodes = %+v", g.Nodes)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// Followers at stream start and end are snapshotted by the live refresh.
func TestFollowersGainedPerStream(t *testing.T) {
	c := useClock(t, statsNow)
	follows := &fakeFollows{}
	users := &fakeUsers{}
	users.set(userAlpha)
	streams := &liveStreams{}
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, ""), users: users, follows: follows, client: streams.fake()}
	s.resolveRoster([]string{"alpha"}, c.now)

	stream := helix.Stream{UserID: "1", UserLogin: "alpha", GameName: "Just Chatting", StartedAt: c.now}
	streams.set(stream)
	s.live.online("1", stream, c.now)
	s.process(statusJob{kind: jobRefreshLive}) // start snapshot: total 101
	s.process(statusJob{kind: jobRefreshLive}) // nothing more to do
	if len(follows.calls) != 1 {
		t.Fatalf("calls = %v", follows.calls)
	}
	c.advance(2 * time.Hour)
	s.live.offline("1", false, c.now)
	s.process(statusJob{kind: jobRefreshLive}) // end snapshot: total 102
	if len(follows.calls) != 2 {
		t.Fatalf("calls = %v", follows.calls)
	}
	d, _ := s.live.db.loadStatsData(c.now)
	st := computeStreamerStats("1", d, c.now)
	if g := st.RecentStreams[0].FollowersGained; g == nil || *g != 1 || st.FollowersPerStream30d == nil || *st.FollowersPerStream30d != 1 {
		t.Errorf("gained = %v, per stream %v", g, st.FollowersPerStream30d)
	}
	// Streams ended long ago are not snapshotted late.
	c.advance(time.Hour)
	s.live.db.db.Exec(`UPDATE sessions SET followers_end = NULL`)
	s.process(statusJob{kind: jobRefreshLive})
	if len(follows.calls) != 2 {
		t.Errorf("late snapshot taken: %v", follows.calls)
	}
}

func TestStreamTopics(t *testing.T) {
	for _, c := range []struct {
		tags, titles []string
		want         []string
	}{
		{[]string{"HackTheBox", "English"}, []string{"Rooting boxes"}, []string{"Hack The Box"}},
		{nil, []string{"Reverse Engineering a router's firmware w/ Ghidra"}, []string{"Reverse engineering", "Hardware & radio"}},
		{nil, []string{"Retail therapy, said nobody"}, nil}, // "ai" only as a whole word
		{nil, []string{"AI red teaming: prompt injection"}, []string{"Pentesting & red team", "AI security"}},
		{[]string{"Capture The Flag"}, nil, []string{"CTF"}},
		{nil, []string{"Studying for Security+ !"}, []string{"Certs & careers"}},
		{[]string{"bug bounty"}, []string{"Bug Bounty Q&A"}, []string{"Bug bounty"}},
	} {
		if got := streamTopics(c.tags, c.titles); !reflect.DeepEqual(got, c.want) {
			t.Errorf("streamTopics(%q, %q) = %q, want %q", c.tags, c.titles, got, c.want)
		}
	}
}

func TestTopicsAndTagsInStats(t *testing.T) {
	c := useClock(t, statsNow)
	l := newTestLive(t, "")
	live := func(start time.Time, title string, tags ...string) {
		s := helix.Stream{UserID: "1", UserLogin: "alpha", GameName: "Science & Technology", Title: title, Tags: tags, StartedAt: start}
		l.online("1", s, start)
		l.offline("1", false, start.Add(time.Hour))
	}
	live(c.now.Add(-3*day), "Malware Monday", "English", "Malware")
	live(c.now.Add(-2*day), "CTF practice", "english", "CTF")
	live(c.now.Add(-1*day), "More malware", "English")
	d, _ := l.db.loadStatsData(c.now)
	s := computeStreamerStats("1", d, c.now)
	wantTopics := []topicCount{{"Malware analysis", 2}, {"CTF", 1}}
	if !reflect.DeepEqual(s.Topics30d, wantTopics) {
		t.Errorf("topics = %+v", s.Topics30d)
	}
	if len(s.Tags30d) != 3 || s.Tags30d[0] != (tagCount{"English", 3}) {
		t.Errorf("tags = %+v", s.Tags30d)
	}
}

type fakeMedia struct {
	mu     sync.Mutex
	calls  int
	videos map[string][]helix.Video
	clips  map[string][]helix.Clip
}

func (f *fakeMedia) GetVideos(p *helix.VideosParams) (*helix.VideosResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	r := &helix.VideosResponse{}
	r.StatusCode = 200
	r.Data.Videos = f.videos[p.UserID]
	return r, nil
}

func (f *fakeMedia) GetClips(p *helix.ClipsParams) (*helix.ClipsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	r := &helix.ClipsResponse{}
	r.StatusCode = 200
	for _, c := range f.clips[p.BroadcasterID] {
		if !parseHelixTime(c.CreatedAt).Before(p.StartedAt.Time) {
			r.Data.Clips = append(r.Data.Clips, c)
		}
	}
	return r, nil
}

func TestVODsAndClips(t *testing.T) {
	c := useClock(t, statsNow)
	users := &fakeUsers{}
	users.set(userAlpha, userBeta)
	ts := func(ago time.Duration) string { return statsNow.Add(-ago).Format(time.RFC3339) }
	media := &fakeMedia{
		videos: map[string][]helix.Video{"1": {
			{ID: "v2", Title: "Newer", URL: "https://twitch.tv/videos/2", CreatedAt: ts(day), ViewCount: 5, Duration: "1h30m0s"},
			{ID: "v1", Title: "Older", URL: "https://twitch.tv/videos/1", CreatedAt: ts(5 * day), ViewCount: 50, Duration: "2h0m"},
		}},
		clips: map[string][]helix.Clip{
			"1": {
				{ID: "c1", Title: "Small", CreatedAt: ts(day), ViewCount: 3, Duration: 20, CreatorName: "fan"},
				{ID: "c2", Title: "Big", CreatedAt: ts(2 * day), ViewCount: 300, Duration: 30, CreatorName: "fan2"},
				{ID: "c0", Title: "Ancient", CreatedAt: ts(60 * day), ViewCount: 9999},
			},
			"2": {{ID: "c3", Title: "Beta's", CreatedAt: ts(day), ViewCount: 100}},
		},
	}
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, ""), users: users, media: media}
	s.resolveRoster([]string{"alpha", "beta"}, c.now)
	s.process(statusJob{kind: jobMedia})
	if media.calls != 4 {
		t.Fatalf("calls = %d, want 4", media.calls)
	}
	s.process(statusJob{kind: jobMedia}) // done for today
	if media.calls != 4 {
		t.Errorf("refetched the same day: %d", media.calls)
	}
	s.process(statusJob{kind: jobStats})
	mux := http.NewServeMux()
	s.registerAPI(mux)

	var one streamerStats
	json.Unmarshal(apiGet(mux, "GET", "/api/stats/1.json", nil).Body.Bytes(), &one)
	if len(one.RecentVODs) != 2 || one.RecentVODs[0].ID != "v2" || one.RecentVODs[0].DurationS != 5400 {
		t.Errorf("vods = %+v", one.RecentVODs)
	}
	var clips []string
	for _, c := range one.TopClips30d {
		clips = append(clips, c.ID)
	}
	if !reflect.DeepEqual(clips, []string{"c2", "c1"}) || one.TopClips30d[0].CreatorName != "fan2" {
		t.Errorf("clips = %v", one.TopClips30d)
	}

	var comm communityStats
	json.Unmarshal(apiGet(mux, "GET", "/api/stats.json", nil).Body.Bytes(), &comm)
	clips = nil
	for _, c := range comm.TopClips30d {
		clips = append(clips, c.ID+"@"+c.StreamerLogin)
	}
	if !reflect.DeepEqual(clips, []string{"c2@alpha", "c3@beta", "c1@alpha"}) {
		t.Errorf("community clips = %v", clips)
	}

	c.advance(day)
	s.process(statusJob{kind: jobMedia})
	if media.calls != 8 {
		t.Errorf("next day calls = %d", media.calls)
	}
}

func TestCommunityTopicsAndSummary(t *testing.T) {
	c := useClock(t, statsNow)
	users := &fakeUsers{}
	users.set(userAlpha, userBeta)
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, ""), users: users}
	s.resolveRoster([]string{"alpha", "beta"}, c.now)
	for i, id := range []string{"1", "2"} {
		start := c.now.Add(-time.Duration(i+2) * time.Hour)
		s.live.online(id, helix.Stream{UserID: id, GameName: "Just Chatting", Title: "OSINT hour", StartedAt: start}, start)
		s.live.offline(id, false, start.Add(time.Hour))
	}
	s.process(statusJob{kind: jobStats})
	var comm communityStats
	json.Unmarshal(s.live.statsSnap.Load().all.body, &comm)
	if !reflect.DeepEqual(comm.Topics30d, []topicCount{{"OSINT", 2}}) {
		t.Errorf("community topics = %+v", comm.Topics30d)
	}
	if !reflect.DeepEqual(comm.Streamers[0].Topics, []string{"OSINT"}) {
		t.Errorf("summary topics = %+v", comm.Streamers[0].Topics)
	}
}
