package main

import (
	"database/sql"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nicklaw5/helix/v2"
	log "github.com/sirupsen/logrus"
)

// Statistics built from the recorded sessions, viewer samples, daily
// follower totals and title changes:
//
//	GET /api/stats.json           community totals plus one summary row per streamer
//	GET /api/stats/<id|login>.json one streamer's full statistics
//
// Everything is recomputed on the worker every statsInterval into
// immutable documents, like status.json. "Infosec" time is time in
// VALID_GAMES; hours, streams and viewer figures count only that time,
// so they agree with hours_30d.

const (
	statsInterval = 10 * time.Minute
	statsMaxAge   = 300
	// followersBatch is how many follower totals one followers job
	// fetches; the job runs every statsInterval until every roster
	// streamer has today's total.
	followersBatch = 50
	// sampleEvery is the minimum gap between stored viewer samples.
	sampleEvery = time.Minute
	// recentCount is how many recent streams and titles are listed.
	recentCount = 10

	day  = 24 * time.Hour
	week = 7 * day
)

// followersClient is the slice of Helix used for follower totals. With an
// app token, Get Channel Followers returns only the total, which is all we
// need; *helix.Client satisfies it.
type followersClient interface {
	GetChannelFollows(params *helix.GetChannelFollowsParams) (*helix.GetChannelFollowersResponse, error)
}

// --- storage

// addViewerSample stores a viewer count seen at at.
func (st *store) addViewerSample(userID string, at time.Time, viewers int, category string) error {
	_, err := st.db.Exec(`INSERT OR REPLACE INTO viewer_samples (user_id, at, viewers, category) VALUES (?, ?, ?, ?)`,
		userID, at.Unix(), viewers, category)
	return err
}

// lastTitle returns the most recently stored title of userID.
func (st *store) lastTitle(userID string) (string, error) {
	var t string
	err := st.db.QueryRow(`SELECT title FROM titles WHERE user_id = ? ORDER BY at DESC, id DESC LIMIT 1`, userID).Scan(&t)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return t, err
}

func (st *store) addTitle(userID string, at time.Time, title, category string) error {
	_, err := st.db.Exec(`INSERT INTO titles (user_id, at, title, category) VALUES (?, ?, ?, ?)`, userID, at.Unix(), title, category)
	return err
}

// setFollowers stores userID's follower total for at's UTC day.
func (st *store) setFollowers(userID string, at time.Time, total int) error {
	_, err := st.db.Exec(`INSERT OR REPLACE INTO follower_counts (user_id, day, at, total) VALUES (?, ?, ?, ?)`,
		userID, at.UTC().Format(time.DateOnly), at.Unix(), total)
	return err
}

// followersOn returns the users that have a follower total for day.
func (st *store) followersOn(dayStr string) (map[string]bool, error) {
	rows, err := st.db.Query(`SELECT user_id FROM follower_counts WHERE day = ?`, dayStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// recordingSince is when stream history starts: the database's creation
// or the first recorded session, whichever is earlier.
func (st *store) recordingSince() (time.Time, error) {
	var meta sql.NullString
	var first sql.NullInt64
	if err := st.db.QueryRow(`SELECT (SELECT value FROM meta WHERE key = 'recording_since'), (SELECT MIN(started_at) FROM sessions)`).Scan(&meta, &first); err != nil {
		return time.Time{}, err
	}
	since := int64(math.MaxInt64)
	if meta.Valid {
		if v, err := strconv.ParseInt(meta.String, 10, 64); err == nil {
			since = v
		}
	}
	if first.Valid && first.Int64 < since {
		since = first.Int64
	}
	if since == math.MaxInt64 {
		return time.Time{}, nil
	}
	return time.Unix(since, 0).UTC(), nil
}

type sessionRow struct {
	id         int64
	userID     string
	start, end time.Time // end is zero while open
}

type viewerSample struct {
	at       time.Time
	viewers  int
	category string
}

type followerPoint struct {
	day   string
	total int
}

type titleEntry struct {
	At       string `json:"at"`
	Title    string `json:"title"`
	Category string `json:"category,omitempty"`
}

// statsData is everything stats are computed from.
type statsData struct {
	since     time.Time
	sessions  map[string][]sessionRow // by user, oldest first
	segments  map[int64][]segment     // by session, oldest first
	samples   map[string][]viewerSample
	peakAll   map[string]int
	followers map[string][]followerPoint // oldest first
	titles    map[string][]titleEntry    // newest first
}

func unixOrZero(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}

// loadStatsData reads the history needed for stats as of now: all sessions
// and segments, viewer samples for the last 60 days, follower totals for
// the last 31 days and the latest titles.
func (st *store) loadStatsData(now time.Time) (*statsData, error) {
	d := &statsData{
		sessions:  map[string][]sessionRow{},
		segments:  map[int64][]segment{},
		samples:   map[string][]viewerSample{},
		peakAll:   map[string]int{},
		followers: map[string][]followerPoint{},
		titles:    map[string][]titleEntry{},
	}
	var err error
	if d.since, err = st.recordingSince(); err != nil {
		return nil, err
	}

	rows, err := st.db.Query(`SELECT id, user_id, started_at, ended_at FROM sessions ORDER BY started_at, id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r sessionRow
		var start int64
		var end sql.NullInt64
		if err := rows.Scan(&r.id, &r.userID, &start, &end); err != nil {
			rows.Close()
			return nil, err
		}
		r.start, r.end = time.Unix(start, 0).UTC(), unixOrZero(end)
		d.sessions[r.userID] = append(d.sessions[r.userID], r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = st.db.Query(`SELECT session_id, user_id, category, started_at, ended_at FROM segments ORDER BY started_at, id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sid, start int64
		var g segment
		var end sql.NullInt64
		if err := rows.Scan(&sid, &g.userID, &g.category, &start, &end); err != nil {
			rows.Close()
			return nil, err
		}
		g.start, g.end = time.Unix(start, 0).UTC(), unixOrZero(end)
		d.segments[sid] = append(d.segments[sid], g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = st.db.Query(`SELECT user_id, at, viewers, category FROM viewer_samples WHERE at >= ? ORDER BY at`, now.Add(-60*day).Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var at int64
		var s viewerSample
		if err := rows.Scan(&id, &at, &s.viewers, &s.category); err != nil {
			rows.Close()
			return nil, err
		}
		s.at = time.Unix(at, 0).UTC()
		d.samples[id] = append(d.samples[id], s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// All-time peak, in infosec categories only.
	ph := make([]string, len(VALID_GAMES))
	args := make([]any, len(VALID_GAMES))
	for i, g := range VALID_GAMES {
		ph[i], args[i] = "?", strings.ToLower(g)
	}
	rows, err = st.db.Query(`SELECT user_id, MAX(viewers) FROM viewer_samples WHERE lower(category) IN (`+strings.Join(ph, ",")+`) GROUP BY user_id`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var peak int
		if err := rows.Scan(&id, &peak); err != nil {
			rows.Close()
			return nil, err
		}
		d.peakAll[id] = peak
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = st.db.Query(`SELECT user_id, day, total FROM follower_counts WHERE day >= ? ORDER BY day`, now.Add(-31*day).UTC().Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var p followerPoint
		if err := rows.Scan(&id, &p.day, &p.total); err != nil {
			rows.Close()
			return nil, err
		}
		d.followers[id] = append(d.followers[id], p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = st.db.Query(`SELECT user_id, at, title, category FROM (
		SELECT user_id, at, title, category, ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY at DESC, id DESC) AS n FROM titles
	) WHERE n <= ? ORDER BY user_id, at DESC`, recentCount)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var at int64
		var t titleEntry
		if err := rows.Scan(&id, &at, &t.Title, &t.Category); err != nil {
			rows.Close()
			return nil, err
		}
		t.At = rfc3339(time.Unix(at, 0))
		d.titles[id] = append(d.titles[id], t)
	}
	rows.Close()
	return d, rows.Err()
}

// --- recording (worker)

// recordSample stores a viewer sample, at most one per sampleEvery.
func (l *liveState) recordSample(id string, now time.Time, viewers int, category string) {
	if l.lastSample == nil {
		l.lastSample = map[string]time.Time{}
	}
	if last, ok := l.lastSample[id]; ok && now.Sub(last) < sampleEvery {
		return
	}
	if err := l.db.addViewerSample(id, now, viewers, category); err != nil {
		log.Errorf("stats: saving viewer sample for %s: %s", id, err)
		return
	}
	l.lastSample[id] = now
}

// recordTitle stores title if it differs from the last one stored.
func (l *liveState) recordTitle(id string, now time.Time, title, category string) {
	title = strings.TrimSpace(title)
	if title == "" {
		return
	}
	if l.lastTitle == nil {
		l.lastTitle = map[string]string{}
	}
	last, ok := l.lastTitle[id]
	if !ok {
		var err error
		if last, err = l.db.lastTitle(id); err != nil {
			log.Errorf("stats: reading last title of %s: %s", id, err)
			return
		}
	}
	if last == title {
		l.lastTitle[id] = title
		return
	}
	if err := l.db.addTitle(id, now, title, category); err != nil {
		log.Errorf("stats: saving title of %s: %s", id, err)
		return
	}
	l.lastTitle[id] = title
}

// fetchFollowers stores today's follower total for up to followersBatch
// roster streamers that don't have one yet.
func (s *StreamersRepo) fetchFollowers() {
	l := s.live
	if l == nil || s.follows == nil || l.roster == nil {
		return
	}
	now := timeNow()
	done, err := l.db.followersOn(now.UTC().Format(time.DateOnly))
	if err != nil {
		log.Errorf("stats: reading follower totals: %s", err)
		return
	}
	var todo []string
	for id := range l.roster {
		if !done[id] {
			todo = append(todo, id)
		}
	}
	sort.Strings(todo)
	if len(todo) > followersBatch {
		todo = todo[:followersBatch]
	}
	failed := 0
	var lastErr error
	for _, id := range todo {
		resp, err := s.follows.GetChannelFollows(&helix.GetChannelFollowsParams{BroadcasterID: id, First: 1})
		if err == nil {
			err = helixError(resp.ResponseCommon)
		}
		if err != nil {
			failed++
			lastErr = err
			if failed >= 3 {
				break
			}
			continue
		}
		if err := l.db.setFollowers(id, now, resp.Data.Total); err != nil {
			log.Errorf("stats: saving follower total of %s: %s", id, err)
			return
		}
	}
	if failed > 0 {
		log.Warnf("stats: %d follower lookups failed, last: %s", failed, lastErr)
	}
}

// startStatsJobs queues follower fetching and a stats rebuild now (after
// the first reconcile, which loads the roster) and every statsInterval.
func (s *StreamersRepo) startStatsJobs(interval time.Duration) {
	queue := func() {
		if !s.enqueue(statusJob{kind: jobFollowers}) || !s.enqueue(statusJob{kind: jobStats}) {
			log.Warn("stats skipped: event queue full")
		}
	}
	queue()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			queue()
		}
	}()
}

// --- computation

type hoursBlock struct {
	D7  float64 `json:"7d"`
	D30 float64 `json:"30d"`
	D90 float64 `json:"90d"`
	All float64 `json:"all"`
}

type categoryHours struct {
	Category string  `json:"category"`
	Hours    float64 `json:"hours"`
	Infosec  bool    `json:"infosec"`
}

type recentStream struct {
	StartedAt   string   `json:"started_at"`
	EndedAt     string   `json:"ended_at,omitempty"` // absent while live
	Hours       float64  `json:"hours"`
	Categories  []string `json:"categories"`
	AvgViewers  *float64 `json:"avg_viewers,omitempty"`
	PeakViewers *int     `json:"peak_viewers,omitempty"`
}

// heatmap is time live per UTC weekday (Monday first) and hour, as the
// average fraction of that hour spent live per week (0..1).
type heatmap [7][24]float64

// streamerStats is /api/stats/<id>.json. Optional figures are omitted
// until there is data for them.
type streamerStats struct {
	ID             string `json:"id"`
	Login          string `json:"login"`
	DisplayName    string `json:"display_name"`
	GeneratedAt    string `json:"generated_at"`
	RecordingSince string `json:"recording_since,omitempty"`
	LastLiveAt     string `json:"last_live_at,omitempty"`

	Hours           hoursBlock      `json:"hours"`
	InfosecShare30d *float64        `json:"infosec_share_30d,omitempty"`
	Categories30d   []categoryHours `json:"categories_30d"`

	Streams30d            int      `json:"streams_30d"`
	AvgStreamHours30d     *float64 `json:"avg_stream_hours_30d,omitempty"`
	MedianStreamHours30d  *float64 `json:"median_stream_hours_30d,omitempty"`
	LongestStreamHours30d *float64 `json:"longest_stream_hours_30d,omitempty"`
	LongestStreamHoursAll *float64 `json:"longest_stream_hours_all,omitempty"`
	StreamDaysPerWeek30d  float64  `json:"stream_days_per_week_30d"`
	CurrentStreakWeeks    int      `json:"current_streak_weeks"`
	LongestStreakWeeks    int      `json:"longest_streak_weeks"`

	AvgViewers30d     *float64 `json:"avg_viewers_30d,omitempty"`
	AvgViewersPrev30d *float64 `json:"avg_viewers_prev_30d,omitempty"`
	PeakViewers30d    *int     `json:"peak_viewers_30d,omitempty"`
	PeakViewersAll    *int     `json:"peak_viewers_all,omitempty"`
	HoursWatched30d   float64  `json:"hours_watched_30d"`

	Followers          *int `json:"followers,omitempty"`
	FollowersGained7d  *int `json:"followers_gained_7d,omitempty"`
	FollowersGained30d *int `json:"followers_gained_30d,omitempty"`

	Heatmap90d    heatmap        `json:"heatmap_90d"`
	RecentStreams []recentStream `json:"recent_streams"`
	RecentTitles  []titleEntry   `json:"recent_titles"`
}

// statsSummary is one streamer's row in /api/stats.json.
type statsSummary struct {
	ID                 string   `json:"id"`
	Login              string   `json:"login"`
	DisplayName        string   `json:"display_name"`
	Hours7d            float64  `json:"hours_7d"`
	Hours30d           float64  `json:"hours_30d"`
	Streams30d         int      `json:"streams_30d"`
	AvgViewers30d      *float64 `json:"avg_viewers_30d,omitempty"`
	PeakViewers30d     *int     `json:"peak_viewers_30d,omitempty"`
	Followers          *int     `json:"followers,omitempty"`
	FollowersGained30d *int     `json:"followers_gained_30d,omitempty"`
	CurrentStreakWeeks int      `json:"current_streak_weeks"`
	LastLiveAt         string   `json:"last_live_at,omitempty"`
}

type communityTotals struct {
	Hours7d          float64 `json:"hours_7d"`
	Hours30d         float64 `json:"hours_30d"`
	Streams30d       int     `json:"streams_30d"`
	ActiveStreamers  int     `json:"active_streamers_30d"`
	HoursWatched30d  float64 `json:"hours_watched_30d"`
	NewFollowers30d  int     `json:"followers_gained_30d"`
	TrackedStreamers int     `json:"tracked_streamers"`
}

// communityStats is /api/stats.json.
type communityStats struct {
	GeneratedAt    string          `json:"generated_at"`
	RecordingSince string          `json:"recording_since,omitempty"`
	Totals         communityTotals `json:"totals"`
	Categories30d  []categoryHours `json:"categories_30d"`
	// LiveHeatmap90d is the average number of roster streamers live in
	// infosec categories per UTC weekday (Monday first) and hour.
	LiveHeatmap90d heatmap        `json:"live_heatmap_90d"`
	Streamers      []statsSummary `json:"streamers"`
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
func round2(x float64) float64 { return math.Round(x*100) / 100 }
func ptrF(x float64) *float64  { return &x }
func ptrI(x int) *int          { return &x }

// overlap returns how much of [a, b) lies in [from, to).
func overlap(a, b, from, to time.Time) time.Duration {
	if a.Before(from) {
		a = from
	}
	if b.After(to) {
		b = to
	}
	if !b.After(a) {
		return 0
	}
	return b.Sub(a)
}

// weekIndex numbers UTC weeks starting on Monday.
func weekIndex(t time.Time) int64 {
	const monday = 4 * 24 * 3600 // 1970-01-05 was a Monday
	return int64(math.Floor(float64(t.Unix()-monday) / float64(week/time.Second)))
}

// weekday returns 0 for Monday .. 6 for Sunday.
func weekday(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// addHeat spreads [a, b) over the heatmap, in seconds.
func (h *heatmap) addHeat(a, b time.Time) {
	for a.Before(b) {
		next := a.Truncate(time.Hour).Add(time.Hour)
		if next.After(b) {
			next = b
		}
		h[weekday(a)][a.Hour()] += next.Sub(a).Seconds()
		a = next
	}
}

// observedSpan is how much of the window ending at now has been recorded.
func observedSpan(since, now time.Time, window time.Duration) time.Duration {
	if since.IsZero() || now.Sub(since) >= window {
		return window
	}
	if span := now.Sub(since); span > time.Hour {
		return span
	}
	return time.Hour
}

// viewerPiece is a stretch of a stream with one sampled viewer count.
type viewerPiece struct {
	from, to time.Time
	viewers  int
	category string
}

// viewerPieces splits [start, end) at each viewer sample taken in it; a
// sample's count holds until the next one, and the first one also covers
// the time before it.
func viewerPieces(samples []viewerSample, start, end time.Time) []viewerPiece {
	i := sort.Search(len(samples), func(i int) bool { return !samples[i].at.Before(start) })
	var in []viewerSample
	for ; i < len(samples) && samples[i].at.Before(end.Add(time.Second)); i++ {
		in = append(in, samples[i])
	}
	pieces := make([]viewerPiece, 0, len(in))
	for k, s := range in {
		from, to := s.at, end
		if k == 0 {
			from = start
		}
		if k+1 < len(in) {
			to = in[k+1].at
		}
		if to.After(from) {
			pieces = append(pieces, viewerPiece{from: from, to: to, viewers: s.viewers, category: s.category})
		}
	}
	return pieces
}

// computeStreamerStats derives one streamer's statistics from d as of now.
func computeStreamerStats(id string, d *statsData, now time.Time) streamerStats {
	out := streamerStats{ID: id, GeneratedAt: rfc3339(now), RecordingSince: rfc3339(d.since)}
	w7, w30, w60, w90 := now.Add(-7*day), now.Add(-30*day), now.Add(-60*day), now.Add(-90*day)

	var h7, h30, h90, hAll, all30 time.Duration
	catSecs := map[string]time.Duration{}
	catName := map[string]string{}
	weeks := map[int64]bool{}
	days30 := map[string]bool{}
	var heat heatmap
	var lastLive time.Time
	var lengths30 []time.Duration
	var longestAll time.Duration
	var vs30, vd30, vsPrev, vdPrev float64 // viewer-seconds and seconds
	peak30 := -1
	var recent []recentStream

	for _, ss := range d.sessions[id] {
		end := ss.end
		if end.IsZero() {
			end = now
		}
		var validAny, valid30 bool
		var cats []string
		for _, g := range d.segments[ss.id] {
			gEnd := g.end
			if gEnd.IsZero() {
				gEnd = end
			}
			if len(cats) == 0 || cats[len(cats)-1] != g.category {
				cats = append(cats, g.category)
			}
			o30 := overlap(g.start, gEnd, w30, now)
			all30 += o30
			key := strings.ToLower(g.category)
			if o30 > 0 {
				catSecs[key] += o30
				if _, ok := catName[key]; !ok {
					catName[key] = g.category
				}
			}
			if !countsAsInfosec(g.category) || !gEnd.After(g.start) {
				continue
			}
			validAny = true
			hAll += gEnd.Sub(g.start)
			h90 += overlap(g.start, gEnd, w90, now)
			h7 += overlap(g.start, gEnd, w7, now)
			if o30 > 0 {
				h30 += o30
				valid30 = true
			}
			if o := overlap(g.start, gEnd, w90, now); o > 0 {
				a := g.start
				if a.Before(w90) {
					a = w90
				}
				heat.addHeat(a, a.Add(o))
			}
			for wk := weekIndex(g.start); wk <= weekIndex(gEnd.Add(-time.Second)); wk++ {
				weeks[wk] = true
			}
			from := g.start
			if from.Before(w30) {
				from = w30
			}
			for t := from; t.Before(gEnd); t = t.Truncate(day).Add(day) {
				days30[t.Format(time.DateOnly)] = true
			}
			if gEnd.After(lastLive) {
				lastLive = gEnd
			}
		}
		if !validAny {
			continue
		}
		length := end.Sub(ss.start)
		if length > longestAll {
			longestAll = length
		}
		if valid30 {
			lengths30 = append(lengths30, length)
		}

		// Viewers. Averages count time in infosec categories only.
		var sessVS, sessVD float64
		sessPeak := -1
		for _, p := range viewerPieces(d.samples[id], ss.start, end) {
			secs := p.to.Sub(p.from).Seconds()
			sessVS += float64(p.viewers) * secs
			sessVD += secs
			if p.viewers > sessPeak {
				sessPeak = p.viewers
			}
			if !countsAsInfosec(p.category) {
				continue
			}
			if o := overlap(p.from, p.to, w30, now).Seconds(); o > 0 {
				vs30 += float64(p.viewers) * o
				vd30 += o
				if p.viewers > peak30 {
					peak30 = p.viewers
				}
			}
			if o := overlap(p.from, p.to, w60, w30).Seconds(); o > 0 {
				vsPrev += float64(p.viewers) * o
				vdPrev += o
			}
		}
		rs := recentStream{StartedAt: rfc3339(ss.start), Hours: round1(length.Hours()), Categories: cats}
		if !ss.end.IsZero() {
			rs.EndedAt = rfc3339(ss.end)
		}
		if sessVD > 0 {
			rs.AvgViewers = ptrF(round1(sessVS / sessVD))
			rs.PeakViewers = ptrI(sessPeak)
		}
		recent = append(recent, rs)
	}

	out.Hours = hoursBlock{D7: round1(h7.Hours()), D30: round1(h30.Hours()), D90: round1(h90.Hours()), All: round1(hAll.Hours())}
	if all30 > 0 {
		out.InfosecShare30d = ptrF(round2(float64(h30) / float64(all30)))
	}
	out.Categories30d = []categoryHours{}
	for key, secs := range catSecs {
		out.Categories30d = append(out.Categories30d, categoryHours{Category: catName[key], Hours: round1(secs.Hours()), Infosec: countsAsInfosec(key)})
	}
	sort.Slice(out.Categories30d, func(i, j int) bool {
		a, b := out.Categories30d[i], out.Categories30d[j]
		if a.Hours != b.Hours {
			return a.Hours > b.Hours
		}
		return a.Category < b.Category
	})

	out.Streams30d = len(lengths30)
	if n := len(lengths30); n > 0 {
		var sum, longest time.Duration
		for _, l := range lengths30 {
			sum += l
			longest = max(longest, l)
		}
		sorted := append([]time.Duration{}, lengths30...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		median := sorted[n/2]
		if n%2 == 0 {
			median = (sorted[n/2-1] + sorted[n/2]) / 2
		}
		out.AvgStreamHours30d = ptrF(round1((sum / time.Duration(n)).Hours()))
		out.MedianStreamHours30d = ptrF(round1(median.Hours()))
		out.LongestStreamHours30d = ptrF(round1(longest.Hours()))
	}
	if longestAll > 0 {
		out.LongestStreamHoursAll = ptrF(round1(longestAll.Hours()))
	}
	out.StreamDaysPerWeek30d = round1(float64(len(days30)) / (observedSpan(d.since, now, 30*day).Hours() / week.Hours()))
	out.CurrentStreakWeeks, out.LongestStreakWeeks = streaks(weeks, weekIndex(now))
	if !lastLive.IsZero() {
		out.LastLiveAt = rfc3339(lastLive)
	}

	if vd30 > 0 {
		out.AvgViewers30d = ptrF(round1(vs30 / vd30))
		out.PeakViewers30d = ptrI(peak30)
	}
	if vdPrev > 0 {
		out.AvgViewersPrev30d = ptrF(round1(vsPrev / vdPrev))
	}
	if p, ok := d.peakAll[id]; ok {
		out.PeakViewersAll = ptrI(p)
	}
	out.HoursWatched30d = round1(vs30 / 3600)

	if pts := d.followers[id]; len(pts) > 0 {
		latest := pts[len(pts)-1]
		out.Followers = ptrI(latest.total)
		out.FollowersGained7d = followersGained(pts, now, 7)
		out.FollowersGained30d = followersGained(pts, now, 30)
	}

	weeksSpan := observedSpan(d.since, now, 90*day).Hours() / week.Hours()
	for wd := range heat {
		for hr := range heat[wd] {
			out.Heatmap90d[wd][hr] = round2(math.Min(1, heat[wd][hr]/3600/math.Max(1, weeksSpan)))
		}
	}

	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}
	if len(recent) > recentCount {
		recent = recent[:recentCount]
	}
	out.RecentStreams = append([]recentStream{}, recent...)
	out.RecentTitles = append([]titleEntry{}, d.titles[id]...)
	return out
}

// streaks returns the current streak (ending this week, or last week if
// there's been no stream yet this week) and the longest, in weeks.
func streaks(weeks map[int64]bool, current int64) (cur, longest int) {
	wk := current
	if !weeks[wk] {
		wk--
	}
	for weeks[wk] {
		cur++
		wk--
	}
	keys := make([]int64, 0, len(weeks))
	for k := range weeks {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	run := 0
	for i, k := range keys {
		if i > 0 && k == keys[i-1]+1 {
			run++
		} else {
			run = 1
		}
		longest = max(longest, run)
	}
	return cur, longest
}

// followersGained is the change from the latest total to the last total
// recorded at least n days before now, if there is one.
func followersGained(pts []followerPoint, now time.Time, n int) *int {
	cutoff := now.Add(-time.Duration(n) * day).UTC().Format(time.DateOnly)
	for i := len(pts) - 1; i >= 0; i-- {
		if pts[i].day <= cutoff {
			return ptrI(pts[len(pts)-1].total - pts[i].total)
		}
	}
	return nil
}

// statsDocs are the published stats documents.
type statsDocs struct {
	all     *apiDoc
	byID    map[string]*apiDoc
	byLogin map[string]string // lowercase login (current or roster) -> id
}

// buildStats recomputes every stats document. Runs on the worker.
func (l *liveState) buildStats(now time.Time) {
	if l.roster == nil {
		return
	}
	d, err := l.db.loadStatsData(now)
	if err != nil {
		log.Errorf("stats: loading history: %s", err)
		return
	}
	docs := &statsDocs{byID: map[string]*apiDoc{}, byLogin: map[string]string{}}
	comm := communityStats{GeneratedAt: rfc3339(now), RecordingSince: rfc3339(d.since), Streamers: []statsSummary{}}
	commCats := map[string]*categoryHours{}
	var commHeat heatmap

	ids := make([]string, 0, len(l.roster))
	for id := range l.roster {
		if l.users[id] != nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return l.users[ids[i]].login < l.users[ids[j]].login })
	for _, id := range ids {
		u := l.users[id]
		s := computeStreamerStats(id, d, now)
		s.Login, s.DisplayName = u.login, u.displayName
		if s.DisplayName == "" {
			s.DisplayName = u.login
		}
		doc, err := newJSONDoc(s, statsMaxAge)
		if err != nil {
			log.Errorf("stats: encoding %s: %s", u.login, err)
			continue
		}
		docs.byID[id] = doc
		docs.byLogin[u.login] = id

		comm.Streamers = append(comm.Streamers, statsSummary{
			ID: id, Login: s.Login, DisplayName: s.DisplayName,
			Hours7d: s.Hours.D7, Hours30d: s.Hours.D30, Streams30d: s.Streams30d,
			AvgViewers30d: s.AvgViewers30d, PeakViewers30d: s.PeakViewers30d,
			Followers: s.Followers, FollowersGained30d: s.FollowersGained30d,
			CurrentStreakWeeks: s.CurrentStreakWeeks, LastLiveAt: s.LastLiveAt,
		})
		t := &comm.Totals
		t.TrackedStreamers++
		t.Hours7d += s.Hours.D7
		t.Hours30d += s.Hours.D30
		t.Streams30d += s.Streams30d
		t.HoursWatched30d += s.HoursWatched30d
		if s.Hours.D30 > 0 {
			t.ActiveStreamers++
		}
		if s.FollowersGained30d != nil && *s.FollowersGained30d > 0 {
			t.NewFollowers30d += *s.FollowersGained30d
		}
		for _, c := range s.Categories30d {
			key := strings.ToLower(c.Category)
			if commCats[key] == nil {
				commCats[key] = &categoryHours{Category: c.Category, Infosec: c.Infosec}
			}
			commCats[key].Hours += c.Hours
		}
		for wd := range s.Heatmap90d {
			for hr := range s.Heatmap90d[wd] {
				commHeat[wd][hr] += s.Heatmap90d[wd][hr]
			}
		}
	}
	t := &comm.Totals
	t.Hours7d, t.Hours30d, t.HoursWatched30d = round1(t.Hours7d), round1(t.Hours30d), round1(t.HoursWatched30d)
	comm.Categories30d = []categoryHours{}
	for _, c := range commCats {
		c.Hours = round1(c.Hours)
		comm.Categories30d = append(comm.Categories30d, *c)
	}
	sort.Slice(comm.Categories30d, func(i, j int) bool {
		a, b := comm.Categories30d[i], comm.Categories30d[j]
		if a.Hours != b.Hours {
			return a.Hours > b.Hours
		}
		return a.Category < b.Category
	})
	for wd := range commHeat {
		for hr := range commHeat[wd] {
			comm.LiveHeatmap90d[wd][hr] = round2(commHeat[wd][hr])
		}
	}
	// Roster spellings of renamed streamers resolve too.
	for login, id := range l.byLogin {
		if l.roster[id] {
			if _, taken := docs.byLogin[login]; !taken {
				docs.byLogin[login] = id
			}
		}
	}
	all, err := newJSONDoc(comm, statsMaxAge)
	if err != nil {
		log.Errorf("stats: encoding community stats: %s", err)
		return
	}
	docs.all = all
	l.statsSnap.Store(docs)
}

// --- HTTP

// handleStats serves /api/stats.json.
func (s *StreamersRepo) handleStats(w http.ResponseWriter, r *http.Request) {
	var doc *apiDoc
	if s.live != nil {
		if d := s.live.statsSnap.Load(); d != nil {
			doc = d.all
		}
	}
	serveDoc(w, r, doc)
}

// handleStreamerStats serves /api/stats/<id or login>.json.
func (s *StreamersRepo) handleStreamerStats(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/api/stats/"), ".json")
	if !ok || key == "" || strings.Contains(key, "/") {
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	}
	var docs *statsDocs
	if s.live != nil {
		docs = s.live.statsSnap.Load()
	}
	if docs == nil {
		serveDoc(w, r, nil)
		return
	}
	doc := docs.byID[key]
	if doc == nil {
		if id, ok := docs.byLogin[strings.ToLower(key)]; ok {
			doc = docs.byID[id]
		}
	}
	if doc == nil {
		writeAPIError(w, http.StatusNotFound, "unknown streamer")
		return
	}
	serveDoc(w, r, doc)
}
