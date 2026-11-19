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

// statsNow is a Monday.
var statsNow = time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

// seedStatsHistory records, for user "1":
//
//	C Thu 2026-10-01 18-22 S&T, 5 viewers      (prev 30d window)
//	D Tue 2026-10-06 18-19 S&T, 5 viewers      (prev 30d window)
//	A Mon 2026-11-23 18-20 S&T (10 then 30 viewers), 20-21 Minecraft (100 viewers)
//	B Wed 2026-11-25 18-19 Just Chatting, 20 viewers
func seedStatsHistory(t *testing.T, st *store) {
	t.Helper()
	stream := func(start, end string, cat string, viewers int) {
		mustSeen(t, st, "1", at(start), cat, at(start))
		st.addViewerSample("1", at(start), viewers, cat)
		st.streamEnded("1", at(end))
	}
	stream("2026-10-01 18:00", "2026-10-01 22:00", "Science & Technology", 5)
	stream("2026-10-06 18:00", "2026-10-06 19:00", "Science & Technology", 5)

	mustSeen(t, st, "1", at("2026-11-23 18:00"), "Science & Technology", at("2026-11-23 18:00"))
	st.addViewerSample("1", at("2026-11-23 18:00"), 10, "Science & Technology")
	st.addViewerSample("1", at("2026-11-23 19:00"), 30, "Science & Technology")
	st.categoryChanged("1", "Minecraft", at("2026-11-23 20:00"))
	st.addViewerSample("1", at("2026-11-23 20:00"), 100, "Minecraft")
	st.streamEnded("1", at("2026-11-23 21:00"))

	stream("2026-11-25 18:00", "2026-11-25 19:00", "Just Chatting", 20)

	for d, n := range map[string]int{"2026-10-30": 880, "2026-11-01": 900, "2026-11-23": 950, "2026-11-30": 1000} {
		st.setFollowers("1", at(d+" 06:00"), n)
	}
	st.addTitle("1", at("2026-11-23 17:55"), "Reversing malware", "Science & Technology")
	st.addTitle("1", at("2026-11-25 17:59"), "Q&A", "Just Chatting")
}

func TestComputeStreamerStats(t *testing.T) {
	st := newTestStore(t)
	seedStatsHistory(t, st)
	d, err := st.loadStatsData(statsNow)
	if err != nil {
		t.Fatal(err)
	}
	s := computeStreamerStats("1", d, statsNow)

	if want := (hoursBlock{D7: 3, D30: 3, D90: 8, All: 8}); s.Hours != want {
		t.Errorf("hours = %+v, want %+v", s.Hours, want)
	}
	if s.InfosecShare30d == nil || *s.InfosecShare30d != 0.75 {
		t.Errorf("infosec share = %v, want 0.75", s.InfosecShare30d)
	}
	wantCats := []categoryHours{
		{"Science & Technology", 2, true}, {"Just Chatting", 1, true}, {"Minecraft", 1, false},
	}
	if !reflect.DeepEqual(s.Categories30d, wantCats) {
		t.Errorf("categories = %+v", s.Categories30d)
	}
	if s.Streams30d != 2 || *s.AvgStreamHours30d != 2 || *s.MedianStreamHours30d != 2 ||
		*s.LongestStreamHours30d != 3 || *s.LongestStreamHoursAll != 4 {
		t.Errorf("stream lengths: n=%d avg=%v median=%v longest=%v all=%v", s.Streams30d,
			*s.AvgStreamHours30d, *s.MedianStreamHours30d, *s.LongestStreamHours30d, *s.LongestStreamHoursAll)
	}
	// 2 days in 30 = 0.47/week.
	if s.StreamDaysPerWeek30d != 0.5 {
		t.Errorf("days/week = %v", s.StreamDaysPerWeek30d)
	}
	// No stream yet this week; last week counts. Sep 28 and Oct 5 weeks run.
	if s.CurrentStreakWeeks != 1 || s.LongestStreakWeeks != 2 {
		t.Errorf("streaks = %d, %d", s.CurrentStreakWeeks, s.LongestStreakWeeks)
	}
	if s.LastLiveAt != "2026-11-25T19:00:00Z" {
		t.Errorf("last live = %s", s.LastLiveAt)
	}

	// Viewers: S&T 1h@10 + 1h@30, JC 1h@20; the Minecraft hour is excluded.
	if s.AvgViewers30d == nil || *s.AvgViewers30d != 20 || *s.PeakViewers30d != 30 || s.HoursWatched30d != 60 {
		t.Errorf("viewers: avg=%v peak=%v watched=%v", s.AvgViewers30d, s.PeakViewers30d, s.HoursWatched30d)
	}
	if s.AvgViewersPrev30d == nil || *s.AvgViewersPrev30d != 5 {
		t.Errorf("prev avg = %v", s.AvgViewersPrev30d)
	}
	if s.PeakViewersAll == nil || *s.PeakViewersAll != 30 {
		t.Errorf("all-time peak = %v (Minecraft's 100 must not count)", s.PeakViewersAll)
	}

	if *s.Followers != 1000 || *s.FollowersGained7d != 50 || *s.FollowersGained30d != 120 {
		t.Errorf("followers %d +%d/7d +%d/30d", *s.Followers, *s.FollowersGained7d, *s.FollowersGained30d)
	}

	// 1 h per slot; recording began Oct 1, so 60 days (8.6 weeks) = 0.12.
	for _, c := range []struct{ wd, hr int }{{0, 18}, {0, 19}, {1, 18}, {2, 18}, {3, 18}, {3, 21}} {
		if got := s.Heatmap90d[c.wd][c.hr]; got != 0.12 {
			t.Errorf("heatmap[%d][%d] = %v, want 0.12", c.wd, c.hr, got)
		}
	}
	if s.Heatmap90d[0][20] != 0 {
		t.Error("Minecraft time in heatmap")
	}

	if len(s.RecentStreams) != 4 {
		t.Fatalf("recent streams = %+v", s.RecentStreams)
	}
	a := s.RecentStreams[1]
	if s.RecentStreams[0].StartedAt != "2026-11-25T18:00:00Z" || a.Hours != 3 ||
		!reflect.DeepEqual(a.Categories, []string{"Science & Technology", "Minecraft"}) ||
		*a.AvgViewers != 46.7 || *a.PeakViewers != 100 || a.EndedAt != "2026-11-23T21:00:00Z" {
		t.Errorf("recent = %+v / %+v", s.RecentStreams[0], a)
	}
	if len(s.RecentTitles) != 2 || s.RecentTitles[0].Title != "Q&A" {
		t.Errorf("titles = %+v", s.RecentTitles)
	}
}

// A stream in progress counts up to now and has no ended_at.
func TestStatsOpenStream(t *testing.T) {
	st := newTestStore(t)
	mustSeen(t, st, "1", statsNow.Add(-90*time.Minute), "Just Chatting", statsNow.Add(-90*time.Minute))
	st.addViewerSample("1", statsNow.Add(-90*time.Minute), 12, "Just Chatting")
	d, _ := st.loadStatsData(statsNow)
	s := computeStreamerStats("1", d, statsNow)
	if s.Hours.D7 != 1.5 || len(s.RecentStreams) != 1 || s.RecentStreams[0].EndedAt != "" || *s.AvgViewers30d != 12 {
		t.Errorf("open stream: %+v", s)
	}
	// Nothing at all: zeros and omitted fields, never nulls.
	empty := computeStreamerStats("nobody", d, statsNow)
	body, _ := json.Marshal(empty)
	var m map[string]any
	json.Unmarshal(body, &m)
	for _, k := range []string{"avg_viewers_30d", "followers", "last_live_at", "infosec_share_30d", "median_stream_hours_30d"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s present without data", k)
		}
	}
	if m["recent_streams"] == nil || m["categories_30d"] == nil {
		t.Errorf("lists must be [] not null: %s", body)
	}
}

func TestStreaks(t *testing.T) {
	w := func(ks ...int64) map[int64]bool {
		m := map[int64]bool{}
		for _, k := range ks {
			m[k] = true
		}
		return m
	}
	for _, c := range []struct {
		weeks        map[int64]bool
		cur, longest int
		current      int64
		name         string
	}{
		{w(), 0, 0, 10, "none"},
		{w(8, 9, 10), 3, 3, 10, "through this week"},
		{w(8, 9), 2, 2, 10, "last week still counts"},
		{w(7, 8), 0, 2, 10, "broken"},
		{w(1, 2, 3, 4, 9, 10), 2, 4, 10, "longest earlier"},
	} {
		cur, longest := streaks(c.weeks, c.current)
		if cur != c.cur || longest != c.longest {
			t.Errorf("%s: got %d/%d, want %d/%d", c.name, cur, longest, c.cur, c.longest)
		}
	}
}

func TestWeekIndexStartsMonday(t *testing.T) {
	if weekIndex(at("2026-11-29 23:59")) == weekIndex(at("2026-11-30 00:00")) {
		t.Error("Sunday and Monday in the same week")
	}
	if weekIndex(at("2026-11-30 00:00")) != weekIndex(at("2026-12-06 23:59")) {
		t.Error("Monday..Sunday split")
	}
	if weekday(at("2026-11-30 00:00")) != 0 || weekday(at("2026-11-29 00:00")) != 6 {
		t.Error("weekday numbering")
	}
}

func TestSamplesAndTitlesAreRecordedOncePerChange(t *testing.T) {
	l := newTestLive(t, "")
	s := helix.Stream{UserID: "1", UserLogin: "alpha", GameName: "Just Chatting", Title: "One", ViewerCount: 3, StartedAt: t0}
	l.online("1", s, t0)
	l.online("1", s, t0.Add(30*time.Second)) // within sampleEvery
	s.ViewerCount = 4
	l.online("1", s, t0.Add(2*time.Minute))
	s.Title = "Two"
	l.online("1", s, t0.Add(4*time.Minute))
	l.offline("1", false, t0.Add(time.Hour))
	l.preliveUpdate("1", "Science & Technology", "Three", "en", t0.Add(2*time.Hour))
	l.preliveUpdate("1", "Science & Technology", "Three", "en", t0.Add(3*time.Hour))

	var n int
	l.db.db.QueryRow(`SELECT COUNT(*) FROM viewer_samples`).Scan(&n)
	if n != 3 {
		t.Errorf("%d samples, want 3", n)
	}
	d, _ := l.db.loadStatsData(t0.Add(4 * time.Hour))
	var got []string
	for _, e := range d.titles["1"] {
		got = append(got, e.Title)
	}
	if !reflect.DeepEqual(got, []string{"Three", "Two", "One"}) {
		t.Errorf("titles = %v", got)
	}
}

type fakeFollows struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]bool
}

func (f *fakeFollows) GetChannelFollows(p *helix.GetChannelFollowsParams) (*helix.GetChannelFollowersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p.BroadcasterID)
	r := &helix.GetChannelFollowersResponse{}
	if f.fail[p.BroadcasterID] {
		r.StatusCode, r.Error = 500, "Internal Server Error"
		return r, nil
	}
	r.StatusCode = 200
	r.Data.Total = 100 + len(f.calls)
	return r, nil
}

func TestFetchFollowersOncePerDay(t *testing.T) {
	c := useClock(t, statsNow)
	users := &fakeUsers{}
	users.set(userAlpha, userBeta, userGamma)
	follows := &fakeFollows{fail: map[string]bool{"2": true}}
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, ""), users: users, follows: follows}
	s.fetchFollowers() // no roster yet
	if len(follows.calls) != 0 {
		t.Fatal("fetched before the roster was loaded")
	}
	s.resolveRoster([]string{"alpha", "beta", "gamma"}, c.now)

	s.process(statusJob{kind: jobFollowers})
	if !reflect.DeepEqual(follows.calls, []string{"1", "2", "3"}) {
		t.Fatalf("calls = %v", follows.calls)
	}
	s.process(statusJob{kind: jobFollowers}) // only the failed one again
	if !reflect.DeepEqual(follows.calls[3:], []string{"2"}) {
		t.Errorf("retry calls = %v", follows.calls[3:])
	}
	follows.fail = nil
	c.advance(day)
	follows.calls = nil
	s.process(statusJob{kind: jobFollowers})
	if len(follows.calls) != 3 {
		t.Errorf("next day calls = %v", follows.calls)
	}
}

func TestStatsEndpoints(t *testing.T) {
	c := useClock(t, statsNow)
	users := &fakeUsers{}
	users.set(helix.User{ID: "1", Login: "alpha", DisplayName: "Alpha"}, userBeta)
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, ""), users: users}
	mux := http.NewServeMux()
	s.registerAPI(mux)

	if rec := apiGet(mux, "GET", "/api/stats.json", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("before first build: %d", rec.Code)
	}
	if rec := apiGet(mux, "GET", "/api/stats/1.json", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("before first build: %d", rec.Code)
	}

	seedStatsHistory(t, s.live.db)
	s.resolveRoster([]string{"alpha", "beta"}, c.now)
	s.process(statusJob{kind: jobStats})

	rec := apiGet(mux, "GET", "/api/stats.json", nil)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=300" || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("stats.json: %d %v", rec.Code, rec.Header())
	}
	var comm communityStats
	if err := json.Unmarshal(rec.Body.Bytes(), &comm); err != nil {
		t.Fatal(err)
	}
	if comm.Totals.TrackedStreamers != 2 || comm.Totals.ActiveStreamers != 1 || comm.Totals.Hours30d != 3 ||
		comm.Totals.Streams30d != 2 || comm.Totals.HoursWatched30d != 60 || comm.Totals.NewFollowers30d != 120 {
		t.Errorf("totals = %+v", comm.Totals)
	}
	if len(comm.Streamers) != 2 || comm.Streamers[0].Login != "alpha" || *comm.Streamers[0].AvgViewers30d != 20 || comm.Streamers[1].Hours30d != 0 {
		t.Errorf("rows = %+v", comm.Streamers)
	}
	if comm.LiveHeatmap90d[0][18] != 0.12 || comm.Categories30d[0].Category != "Science & Technology" {
		t.Errorf("community heatmap/categories: %v %+v", comm.LiveHeatmap90d[0][18], comm.Categories30d)
	}

	byID := apiGet(mux, "GET", "/api/stats/1.json", nil)
	for _, path := range []string{"/api/stats/alpha.json", "/api/stats/ALPHA.json"} {
		r := apiGet(mux, "GET", path, nil)
		if r.Code != 200 || r.Body.String() != byID.Body.String() {
			t.Errorf("%s: %d", path, r.Code)
		}
	}
	var one streamerStats
	json.Unmarshal(byID.Body.Bytes(), &one)
	if one.ID != "1" || one.Login != "alpha" || one.DisplayName != "Alpha" || one.Hours.D30 != 3 {
		t.Errorf("stats/1 = %+v", one)
	}
	for _, path := range []string{"/api/stats/nobody.json", "/api/stats/1", "/api/stats/a/b.json", "/api/stats/.json"} {
		if r := apiGet(mux, "GET", path, nil); r.Code != 404 {
			t.Errorf("%s: %d, want 404", path, r.Code)
		}
	}
	if r := apiGet(mux, "GET", "/api/stats/1.json", map[string]string{"If-None-Match": byID.Header().Get("ETag")}); r.Code != 304 {
		t.Errorf("conditional: %d", r.Code)
	}
}
