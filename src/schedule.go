package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nicklaw5/helix/v2"
	log "github.com/sirupsen/logrus"
)

// Twitch schedules ("Get Channel Stream Schedule") feed three things:
// GET /streamstatus/schedule.json and /streamstatus/schedule.ics, next_stream_at in
// status.json (nextStreamAt), and a 🟡 pre-live marker for streamers whose
// scheduled stream starts within schedulePreliveLead.
//
// Cost: one refresh resolves every roster login to an ID (ceil(n/100)
// /users calls) and then makes one /schedule call per streamer (more only if
// a schedule has over 25 segments in the next week). With ~350 streamers that
// is ~355 calls every 30 minutes, paced at scheduleChunk calls per second,
// i.e. ~20 points/second against the 800 points/minute app-token bucket.
// All Helix calls run on the worker, a chunk at a time, so events and
// reconciles interleave with a refresh and never race it.

const (
	// scheduleRefreshInterval is how often every schedule is re-fetched.
	scheduleRefreshInterval = 30 * time.Minute
	// scheduleHorizon is how far ahead segments are kept and served.
	scheduleHorizon = 7 * 24 * time.Hour
	// scheduleChunk is how many /schedule calls one worker job makes.
	scheduleChunk = 20
	// scheduleMaxPages caps /schedule pages (25 segments each) per streamer.
	scheduleMaxPages = 4
	// scheduleFailureAlert is how many refreshes in a row may fail (for one
	// streamer, or for the whole roster) before it's logged at Error level,
	// which alerts Discord. Until then the last good data is served.
	scheduleFailureAlert = 3
	// scheduleRateFloor: when Helix reports fewer remaining points than
	// this, the refresh waits for the bucket to refill.
	scheduleRateFloor = 100
	// schedulePreliveLead is how long before a scheduled start a streamer
	// is shown 🟡.
	schedulePreliveLead = 15 * time.Minute
	// schedulePreliveInterval is how often upcoming segments are checked.
	schedulePreliveInterval = time.Minute
	// scheduleCacheMaxAge is the Cache-Control max-age of both endpoints.
	scheduleCacheMaxAge = 300
	// scheduleDefaultLength is the calendar length of a segment without an
	// end time.
	scheduleDefaultLength = time.Hour
)

// schedulePause spaces out refresh chunks; swapped out in tests.
var schedulePause = time.Second

// scheduleClient is the slice of Helix the schedule code uses;
// *helix.Client satisfies it and tests substitute a fake.
type scheduleClient interface {
	GetUsers(params *helix.UsersParams) (*helix.UsersResponse, error)
	GetSchedule(params *helix.GetScheduleParams) (*helix.GetScheduleResponse, error)
	GetChannelInformation(params *helix.GetChannelInformationParams) (*helix.GetChannelInformationResponse, error)
}

// scheduledSegment is one scheduled stream of a roster streamer.
type scheduledSegment struct {
	ID            string // Twitch segment ID
	BroadcasterID string
	Start, End    time.Time // End is zero if Twitch gives none
	Title         string
	Category      string
	Canceled      bool // the streamer canceled this occurrence
	Vacation      bool // it falls in the streamer's schedule vacation
	Recurring     bool // part of a weekly recurring slot
}

// off reports whether the segment won't happen.
func (g scheduledSegment) off() bool { return g.Canceled || g.Vacation }

// rosterEntry is a roster streamer resolved to a Twitch user.
type rosterEntry struct {
	id, login, displayName string
}

// broadcasterSchedule is the last good schedule of one streamer.
type broadcasterSchedule struct {
	segments []scheduledSegment
	fetched  bool // at least one fetch succeeded
	failures int  // consecutive failed fetches
}

// scheduleState holds every roster streamer's schedule. Helix calls happen
// only on the worker; mu guards the data the HTTP handlers read.
type scheduleState struct {
	client scheduleClient

	mu          sync.RWMutex
	roster      map[string]rosterEntry // by user ID
	byID        map[string]*broadcasterSchedule
	refreshedAt time.Time // end of the last refresh; zero before the first
	rosterFails int       // consecutive refreshes that couldn't resolve the roster

	// adherence is each streamer's track record of keeping their schedule.
	adherence *adherenceTracker
	// history is each streamer's recent streams from third-party trackers.
	history *streamHistory

	// preliveDone remembers segments already turned into a 🟡 marker (or
	// skipped), so each scheduled start triggers at most one site commit.
	// Worker only.
	preliveDone map[string]time.Time
}

func newScheduleState(client scheduleClient) *scheduleState {
	return &scheduleState{
		client:      client,
		roster:      map[string]rosterEntry{},
		byID:        map[string]*broadcasterSchedule{},
		preliveDone: map[string]time.Time{},
		adherence:   newAdherenceTracker(),
		history:     newStreamHistory(defaultHistorySources()...),
	}
}

// startScheduleRefresher fetches every schedule now and then every
// interval, checks for scheduled pre-live streamers every minute, and
// tracks whether scheduled streams happen every adherencePollInterval.
func (s *StreamersRepo) startScheduleRefresher(interval time.Duration) {
	go func() {
		for {
			s.refreshSchedules(s.runInWorker)
			time.Sleep(interval)
		}
	}()
	go func() {
		t := time.NewTicker(schedulePreliveInterval)
		defer t.Stop()
		for range t.C {
			if !s.enqueue(statusJob{kind: jobFunc, fn: s.checkSchedulePrelive}) {
				log.Warn("schedule: pre-live check skipped: event queue full")
			}
		}
	}()
	go func() {
		t := time.NewTicker(adherencePollInterval)
		defer t.Stop()
		for range t.C {
			if !s.enqueue(statusJob{kind: jobFunc, fn: s.trackScheduleAdherence}) {
				log.Warn("schedule: track record poll skipped: event queue full")
			}
		}
	}()
}

// refreshSchedules re-fetches every roster streamer's schedule. run executes
// a function on the worker and reports false if it couldn't (queue full);
// refreshSchedules itself runs off the worker so events interleave.
func (s *StreamersRepo) refreshSchedules(run func(func()) bool) {
	st := s.schedule
	if st == nil || st.client == nil {
		return
	}
	var roster []rosterEntry
	var err error
	if !run(func() { roster, err = s.scheduleRoster() }) {
		err = fmt.Errorf("worker queue full")
	}
	if err != nil {
		st.mu.Lock()
		st.rosterFails++
		fails := st.rosterFails
		st.mu.Unlock()
		if fails >= scheduleFailureAlert {
			log.Errorf("schedule: refresh failed %d times in a row, serving data from %s: resolving roster: %s", fails, st.lastRefresh(), err)
		} else {
			log.Warnf("schedule: refresh failed (%d in a row), keeping last data: resolving roster: %s", fails, err)
		}
		return
	}

	type result struct {
		segments []scheduledSegment
		err      error
	}
	results := map[string]result{}
	for start := 0; start < len(roster); start += scheduleChunk {
		chunk := roster[start:min(start+scheduleChunk, len(roster))]
		var rate helix.ResponseCommon
		ok := run(func() {
			for _, e := range chunk {
				segs, rc, err := fetchSchedule(st.client, e.id, timeNow())
				results[e.id] = result{segs, err}
				if rc.Header != nil {
					rate = rc
				}
			}
		})
		if !ok {
			for _, e := range chunk {
				results[e.id] = result{err: fmt.Errorf("worker queue full")}
			}
		}
		if wait := rateLimitWait(rate, timeNow()); wait > 0 {
			log.Warnf("schedule: Helix rate limit low (%d left), pausing %s", rate.GetRateLimitRemaining(), wait)
			sleep(wait)
		} else if start+scheduleChunk < len(roster) {
			sleep(schedulePause)
		}
	}

	st.mu.Lock()
	byID := map[string]*broadcasterSchedule{}
	rosterByID := map[string]rosterEntry{}
	var failed, alerting []string
	withSchedule := 0
	for _, e := range roster {
		rosterByID[e.id] = e
		b := st.byID[e.id]
		if b == nil {
			b = &broadcasterSchedule{}
		}
		r := results[e.id]
		if r.err != nil {
			b.failures++
			failed = append(failed, e.login)
			if b.failures >= scheduleFailureAlert {
				alerting = append(alerting, fmt.Sprintf("%s (%d in a row): %s", e.login, b.failures, r.err))
			}
		} else {
			b.segments, b.fetched, b.failures = r.segments, true, 0
		}
		if len(b.segments) > 0 {
			withSchedule++
		}
		byID[e.id] = b
	}
	st.roster, st.byID = rosterByID, byID
	st.refreshedAt = timeNow()
	st.rosterFails = 0
	st.mu.Unlock()

	log.Infof("schedule: refreshed %d streamers, %d with upcoming segments, %d failed", len(roster), withSchedule, len(failed))
	if len(failed) > 0 && len(alerting) == 0 {
		log.Warnf("schedule: fetch failed for %s; keeping their last schedule", strings.Join(failed, ", "))
	}
	if len(alerting) > 0 {
		log.Errorf("schedule: fetch keeps failing for %d streamer(s), serving their last schedule: %s", len(alerting), strings.Join(alerting, "; "))
	}
	s.updateEstimates()
}

func (st *scheduleState) lastRefresh() string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if st.refreshedAt.IsZero() {
		return "never"
	}
	return st.refreshedAt.UTC().Format(time.RFC3339)
}

// rateLimitWait returns how long to pause before more Helix calls given the
// last response's rate-limit headers: until the bucket resets if it is
// (nearly) empty or the call was throttled, otherwise zero.
func rateLimitWait(rc helix.ResponseCommon, now time.Time) time.Duration {
	if rc.Header == nil || rc.Header.Get("RateLimit-Remaining") == "" {
		return 0
	}
	if rc.StatusCode != http.StatusTooManyRequests && rc.GetRateLimitRemaining() >= scheduleRateFloor {
		return 0
	}
	wait := time.Unix(int64(rc.GetRateLimitReset()), 0).Sub(now) + time.Second
	return min(max(wait, time.Second), time.Minute)
}

// scheduleRoster resolves the logins in both roster CSVs to Twitch users.
// Runs on the worker.
func (s *StreamersRepo) scheduleRoster() ([]rosterEntry, error) {
	if err := s.getRepo(); err != nil {
		return nil, fmt.Errorf("syncing site repo: %w", err)
	}
	logins, err := csvNames(s.activeCsvPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.activeCsvPath, err)
	}
	if s.inactiveCsvPath != "" {
		inactive, err := csvNames(s.inactiveCsvPath)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", s.inactiveCsvPath, err)
		}
		logins = append(logins, inactive...)
	}
	seen := map[string]bool{}
	var out []rosterEntry
	for start := 0; start < len(logins); start += helixLoginBatch {
		resp, err := s.schedule.client.GetUsers(&helix.UsersParams{Logins: logins[start:min(start+helixLoginBatch, len(logins))]})
		if err != nil {
			return nil, err
		}
		if err := helixError(resp.ResponseCommon); err != nil {
			return nil, err
		}
		for _, u := range resp.Data.Users {
			if u.ID == "" || seen[u.ID] {
				continue
			}
			seen[u.ID] = true
			display := u.DisplayName
			if display == "" {
				display = u.Login
			}
			out = append(out, rosterEntry{id: u.ID, login: strings.ToLower(u.Login), displayName: display})
		}
	}
	return out, nil
}

// fetchSchedule returns a broadcaster's segments starting before
// now+scheduleHorizon. A broadcaster without a schedule (404) has none.
func fetchSchedule(c scheduleClient, id string, now time.Time) ([]scheduledSegment, helix.ResponseCommon, error) {
	horizon := now.Add(scheduleHorizon)
	var out []scheduledSegment
	var rc helix.ResponseCommon
	after := ""
	for page := 0; page < scheduleMaxPages; page++ {
		resp, err := c.GetSchedule(&helix.GetScheduleParams{BroadcasterID: id, First: 25, After: after})
		if err != nil {
			return nil, rc, err
		}
		rc = resp.ResponseCommon
		if rc.StatusCode == http.StatusNotFound {
			return nil, rc, nil
		}
		if err := helixError(rc); err != nil {
			return nil, rc, err
		}
		data := resp.Data.Schedule
		vac := data.Vacation
		beyond := false
		for _, g := range data.Segments {
			if g.StartTime.IsZero() {
				continue
			}
			if !g.StartTime.Before(horizon) {
				beyond = true
				break
			}
			start, end := g.StartTime.UTC(), g.EndTime.UTC()
			if !end.IsZero() && !end.After(now) {
				continue
			}
			out = append(out, scheduledSegment{
				ID:            g.ID,
				BroadcasterID: id,
				Start:         start,
				End:           end,
				Title:         strings.TrimSpace(g.Title),
				Category:      g.Category.Name,
				Recurring:     g.IsRecurring,
				Canceled:      g.CanceledUntil != "",
				Vacation: !vac.StartTime.IsZero() && !vac.EndTime.IsZero() &&
					!start.Before(vac.StartTime.Time) && start.Before(vac.EndTime.Time),
			})
		}
		after = resp.Data.Pagination.Cursor
		if beyond || after == "" {
			break
		}
	}
	return out, rc, nil
}

// upcoming returns the segments that haven't ended and start within the
// horizon, sorted by start; login (case-insensitive) limits it to one
// streamer. ok is false before the first refresh.
func (st *scheduleState) upcoming(now time.Time, login string) (segs []scheduledSegment, roster map[string]rosterEntry, generated time.Time, ok bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if st.refreshedAt.IsZero() {
		return nil, nil, time.Time{}, false
	}
	horizon := now.Add(scheduleHorizon)
	login = strings.ToLower(login)
	for id, b := range st.byID {
		e := st.roster[id]
		if login != "" && e.login != login {
			continue
		}
		for _, g := range b.segments {
			end := g.End
			if end.IsZero() {
				end = g.Start.Add(scheduleDefaultLength)
			}
			if !end.After(now) || !g.Start.Before(horizon) {
				continue
			}
			segs = append(segs, g)
		}
	}
	sort.Slice(segs, func(i, j int) bool {
		a, b := segs[i], segs[j]
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		if la, lb := st.roster[a.BroadcasterID].login, st.roster[b.BroadcasterID].login; la != lb {
			return la < lb
		}
		return a.ID < b.ID
	})
	return segs, st.roster, st.refreshedAt, true
}

// hasLogin reports whether login is a resolved roster streamer.
func (st *scheduleState) hasLogin(login string) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	login = strings.ToLower(login)
	for _, e := range st.roster {
		if e.login == login {
			return true
		}
	}
	return false
}

// nextStreamAt returns the start of streamer id's next scheduled stream that
// isn't canceled or on vacation, if one is known within the horizon.
func (s *StreamersRepo) nextStreamAt(id string) (time.Time, bool) {
	st := s.schedule
	if st == nil {
		return time.Time{}, false
	}
	now := timeNow()
	st.mu.RLock()
	defer st.mu.RUnlock()
	b := st.byID[id]
	if b == nil {
		return time.Time{}, false
	}
	var next time.Time
	for _, g := range b.segments {
		if g.off() || g.Start.Before(now) {
			continue
		}
		if next.IsZero() || g.Start.Before(next) {
			next = g.Start
		}
	}
	return next, !next.IsZero()
}

// checkSchedulePrelive marks 🟡 every active streamer whose next scheduled
// segment (in an infosec category, or with none) starts within
// schedulePreliveLead, through the same applyPrelive path as channel.update:
// it never overrides 🟢, never reactivates inactive streamers, and expires
// after preliveTTL in reconcile. Runs on the worker with s.mutex held.
func (s *StreamersRepo) checkSchedulePrelive() {
	st := s.schedule
	if st == nil {
		return
	}
	now := timeNow()
	for k, t := range st.preliveDone {
		if now.Sub(t) > 2*time.Hour {
			delete(st.preliveDone, k)
		}
	}

	type candidate struct {
		seg   scheduledSegment
		entry rosterEntry
		key   string
	}
	var cands []candidate
	st.mu.RLock()
	for id, b := range st.byID {
		for _, g := range b.segments {
			key := g.ID + "@" + g.Start.Format(time.RFC3339)
			if _, done := st.preliveDone[key]; done || g.off() {
				continue
			}
			if g.Start.Before(now) || g.Start.Sub(now) > schedulePreliveLead {
				continue
			}
			if g.Category != "" && !contains(VALID_GAMES, g.Category) {
				continue
			}
			cands = append(cands, candidate{g, st.roster[id], key})
			break // one per streamer
		}
	}
	st.mu.RUnlock()
	if len(cands) == 0 {
		return
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].entry.login < cands[j].entry.login })

	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.entry.id)
	}
	// Someone already live (in any category) isn't "getting ready".
	live := map[string]bool{}
	for start := 0; start < len(ids); start += helixLoginBatch {
		resp, err := s.client.GetStreams(&helix.StreamsParams{UserIDs: ids[start:min(start+helixLoginBatch, len(ids))], First: helixLoginBatch})
		if err == nil {
			err = helixError(resp.ResponseCommon)
		}
		if err != nil {
			log.Warnf("schedule: pre-live check: fetching streams failed, retrying next minute: %s", err)
			return
		}
		for _, x := range resp.Data.Streams {
			live[x.UserID] = true
		}
	}
	channels := map[string]helix.ChannelInformation{}
	for start := 0; start < len(ids); start += helixLoginBatch {
		resp, err := st.client.GetChannelInformation(&helix.GetChannelInformationParams{BroadcasterIDs: ids[start:min(start+helixLoginBatch, len(ids))]})
		if err == nil {
			err = helixError(resp.ResponseCommon)
		}
		if err != nil {
			log.Warnf("schedule: pre-live check: channel info failed, marking without language: %s", err)
			break
		}
		for _, ch := range resp.Data.Channels {
			channels[ch.BroadcasterID] = ch
		}
	}

	// Leave existing 🟡/🟢 rows alone: a channel.update marker keeps its own
	// start time and category.
	if err := s.getRepo(); err != nil {
		log.Warnf("schedule: pre-live check: syncing site repo failed, retrying next minute: %s", err)
		return
	}
	if err := s.readFile(); err != nil {
		log.Warnf("schedule: pre-live check: reading markdown failed, retrying next minute: %s", err)
		return
	}
	marked := map[string]bool{}
	for k := range preliveStreamers(s.indexMdText) {
		marked[k] = true
	}
	for k := range onlineStreamers(s.indexMdText) {
		marked[k] = true
	}

	for _, c := range cands {
		st.preliveDone[c.key] = now
		if live[c.entry.id] || marked[c.entry.login] {
			continue
		}
		ch := channels[c.entry.id]
		game := c.seg.Category
		if game == "" && contains(VALID_GAMES, ch.GameName) {
			game = ch.GameName
		}
		if game == "" {
			game = "scheduled stream"
		}
		log.Infof("schedule: %s starts %q at %s: marking pre-live", c.entry.login, c.seg.Title, c.seg.Start.Format(time.RFC3339))
		s.applyPrelive(c.entry.login, game, ch.BroadcasterLanguage, now)
	}
}

// scheduleHandlers returns the schedule endpoints by path, for mounting on
// the /streamstatus/ mux.
func (s *StreamersRepo) scheduleHandlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/streamstatus/schedule.json": s.serveScheduleJSON,
		"/streamstatus/schedule.ics":  s.serveScheduleICS,
	}
}

type scheduleSegmentJSON struct {
	ID          string `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"display_name"`
	SegmentID   string `json:"segment_id"`
	Start       string `json:"start"`
	End         string `json:"end,omitempty"`
	Title       string `json:"title,omitempty"`
	Category    string `json:"category,omitempty"`
	Canceled    bool   `json:"canceled"`
	Vacation    bool   `json:"vacation,omitempty"`
	// TrackRecord is how many of the streamer's recent scheduled streams
	// they went live for (within adherenceTolerance); omitted if none
	// were judged yet.
	TrackRecord *trackRecordJSON `json:"track_record,omitempty"`
}

type scheduleJSON struct {
	GeneratedAt string                `json:"generated_at"`
	Segments    []scheduleSegmentJSON `json:"segments"`
	// Habits are the weekly UTC slots streamers usually stream in, from
	// 90 days of stream history, whether or not they publish a schedule.
	Habits []habitsJSON `json:"habits"`
}

type habitsJSON struct {
	ID          string          `json:"id"`
	Login       string          `json:"login"`
	DisplayName string          `json:"display_name"`
	Next        string          `json:"next,omitempty"`
	Slots       []habitSlotJSON `json:"slots"`
}

type habitSlotJSON struct {
	Weekday string `json:"weekday"` // "Tuesday"
	Time    string `json:"time"`    // "18:00", UTC
	Weeks   int    `json:"weeks"`   // weeks they streamed around then
	Of      int    `json:"of"`      // weeks of history
}

// scheduleRequest validates the method and ?login= of a schedule request
// and returns the segments to serve. It writes the response and reports
// false if there is nothing more to do.
func (s *StreamersRepo) scheduleRequest(w http.ResponseWriter, r *http.Request) ([]scheduledSegment, map[string]rosterEntry, time.Time, string, bool) {
	scheduleCommonHeaders(w)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodOptions:
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "If-None-Match")
		w.Header().Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusNoContent)
		return nil, nil, time.Time{}, "", false
	default:
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, nil, time.Time{}, "", false
	}
	st := s.schedule
	login := strings.TrimSpace(r.URL.Query().Get("login"))
	var segs []scheduledSegment
	var roster map[string]rosterEntry
	var generated time.Time
	ok := false
	if st != nil {
		segs, roster, generated, ok = st.upcoming(timeNow(), login)
	}
	if !ok {
		// An empty answer would make subscribed calendars drop every event
		// after a restart; ask them to come back instead.
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "schedule not loaded yet", http.StatusServiceUnavailable)
		return nil, nil, time.Time{}, "", false
	}
	if login != "" && !st.hasLogin(login) {
		http.Error(w, "unknown streamer", http.StatusNotFound)
		return nil, nil, time.Time{}, "", false
	}
	return segs, roster, generated, strings.ToLower(login), true
}

func (s *StreamersRepo) serveScheduleJSON(w http.ResponseWriter, r *http.Request) {
	segs, roster, generated, login, ok := s.scheduleRequest(w, r)
	if !ok {
		return
	}
	out := renderScheduleJSON(segs, roster, generated, s.scheduleTrackRecord)
	out.Habits = s.habitsJSON(roster, login)
	b, _ := json.Marshal(out)
	writeScheduleBody(w, r, "application/json; charset=utf-8", append(b, '\n'))
}

// habitsJSON lists the habits of every roster streamer (or just login)
// who has any, by login.
func (s *StreamersRepo) habitsJSON(roster map[string]rosterEntry, login string) []habitsJSON {
	out := []habitsJSON{}
	now := timeNow()
	for id, e := range roster {
		if login != "" && e.login != login {
			continue
		}
		slots := s.streamHabits(id)
		if len(slots) == 0 {
			continue
		}
		h := habitsJSON{ID: id, Login: e.login, DisplayName: e.displayName}
		if next, ok := nextHabitAt(slots, now); ok {
			h.Next = next.Format(time.RFC3339)
		}
		for _, sl := range slots {
			h.Slots = append(h.Slots, habitSlotJSON{Weekday: sl.Weekday.String(), Time: fmt.Sprintf("%02d:%02d", sl.Minute/60, sl.Minute%60), Weeks: sl.Weeks, Of: sl.Of})
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Login < out[j].Login })
	return out
}

// trackRecordFunc returns a streamer's track record.
type trackRecordFunc func(id string) trackRecordJSON

func renderScheduleJSON(segs []scheduledSegment, roster map[string]rosterEntry, generated time.Time, record trackRecordFunc) scheduleJSON {
	out := scheduleJSON{GeneratedAt: generated.UTC().Format(time.RFC3339), Segments: []scheduleSegmentJSON{}, Habits: []habitsJSON{}}
	for _, g := range segs {
		e := roster[g.BroadcasterID]
		j := scheduleSegmentJSON{
			ID:          g.BroadcasterID,
			Login:       e.login,
			DisplayName: e.displayName,
			SegmentID:   g.ID,
			Start:       g.Start.UTC().Format(time.RFC3339),
			Title:       g.Title,
			Category:    g.Category,
			Canceled:    g.Canceled,
			Vacation:    g.Vacation,
		}
		if !g.End.IsZero() {
			j.End = g.End.UTC().Format(time.RFC3339)
		}
		if tr := record(g.BroadcasterID); tr.Total > 0 {
			j.TrackRecord = &tr
		}
		out.Segments = append(out.Segments, j)
	}
	return out
}

func (s *StreamersRepo) serveScheduleICS(w http.ResponseWriter, r *http.Request) {
	segs, roster, generated, login, ok := s.scheduleRequest(w, r)
	if !ok {
		return
	}
	name := "InfoSec Streams schedule"
	if login != "" {
		for _, e := range roster {
			if e.login == login {
				name = e.displayName + " on Twitch (InfoSec Streams)"
			}
		}
	}
	writeScheduleBody(w, r, "text/calendar; charset=utf-8", renderScheduleICS(segs, roster, generated, name, s.scheduleTrackRecord))
}

const icsTime = "20060102T150405Z"

// renderScheduleICS renders segments as an RFC 5545 calendar. Times are
// UTC, so no VTIMEZONE is needed.
func renderScheduleICS(segs []scheduledSegment, roster map[string]rosterEntry, stamp time.Time, name string, record trackRecordFunc) []byte {
	var b bytes.Buffer
	line := func(s string) { b.WriteString(foldICSLine(s)) }
	line("BEGIN:VCALENDAR")
	line("VERSION:2.0")
	line("PRODID:-//InfoSec Streams//StreamStatus " + icsEscape(version) + "//EN")
	line("CALSCALE:GREGORIAN")
	line("METHOD:PUBLISH")
	line("X-WR-CALNAME:" + icsEscape(name))
	line("X-WR-CALDESC:" + icsEscape("Scheduled Twitch streams of InfoSec Streams streamers (https://infosecstreams.com)"))
	line("REFRESH-INTERVAL;VALUE=DURATION:PT30M")
	line("X-PUBLISHED-TTL:PT30M")
	dtstamp := stamp.UTC().Format(icsTime)
	for _, g := range segs {
		e := roster[g.BroadcasterID]
		end := g.End
		if end.IsZero() {
			end = g.Start.Add(scheduleDefaultLength)
		}
		title := g.Title
		if title == "" {
			title = "Live on Twitch"
		}
		url := "https://www.twitch.tv/" + e.login
		desc := url
		if g.Category != "" {
			desc = "Category: " + g.Category + "\n" + url
		}
		if tr := record(g.BroadcasterID); tr.Total > 0 {
			desc += fmt.Sprintf("\nWent live for %d of their last %d scheduled streams", tr.Kept, tr.Total)
			if tr.Estimated > 0 {
				desc += fmt.Sprintf(" (%d estimated from stream history)", tr.Estimated)
			}
		}
		if g.Vacation {
			desc = "On vacation\n" + desc
		}
		line("BEGIN:VEVENT")
		line("UID:" + icsEscape(g.ID))
		line("DTSTAMP:" + dtstamp)
		line("DTSTART:" + g.Start.UTC().Format(icsTime))
		line("DTEND:" + end.UTC().Format(icsTime))
		line("SUMMARY:" + icsEscape(e.displayName+": "+title))
		line("DESCRIPTION:" + icsEscape(desc))
		line("URL:" + url)
		line("LOCATION:" + icsEscape(url))
		if g.Category != "" {
			line("CATEGORIES:" + icsEscape(g.Category))
		}
		if g.off() {
			line("STATUS:CANCELLED")
		} else {
			line("STATUS:CONFIRMED")
		}
		line("TRANSP:TRANSPARENT")
		line("END:VEVENT")
	}
	line("END:VCALENDAR")
	return b.Bytes()
}

// icsEscape escapes a TEXT value (RFC 5545 3.3.11).
func icsEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`)
	s = r.Replace(s)
	// Other control characters aren't allowed in TEXT.
	return strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return -1
		}
		return c
	}, s)
}

// foldICSLine terminates a content line with CRLF, folding it so no line
// exceeds 75 octets and no UTF-8 sequence is split (RFC 5545 3.1).
func foldICSLine(s string) string {
	var b strings.Builder
	limit := 75
	for len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		b.WriteString(s[:cut])
		b.WriteString("\r\n ")
		s = s[cut:]
		limit = 74 // the leading space counts
	}
	b.WriteString(s)
	b.WriteString("\r\n")
	return b.String()
}

func scheduleCommonHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "ETag")
	h.Set("Vary", "Accept-Encoding")
	h.Set("X-Content-Type-Options", "nosniff")
}

// writeScheduleBody writes body with caching headers, a strong ETag (304 on
// a match), and gzip if the client accepts it.
func writeScheduleBody(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", scheduleCacheMaxAge))
	h.Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if acceptsGzip(r.Header.Get("Accept-Encoding")) && len(body) > 512 {
		var z bytes.Buffer
		zw := gzip.NewWriter(&z)
		zw.Write(body)
		zw.Close()
		body = z.Bytes()
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Length", fmt.Sprint(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body)
}

func etagMatches(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimSpace(t)
		if t == "*" || t == etag || strings.TrimPrefix(t, "W/") == etag {
			return true
		}
	}
	return false
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(fields[0]), "gzip") {
			continue
		}
		for _, p := range fields[1:] {
			if q := strings.ReplaceAll(strings.TrimSpace(p), " ", ""); q == "q=0" || q == "q=0.0" || q == "q=0.00" || q == "q=0.000" {
				return false
			}
		}
		return true
	}
	return false
}
