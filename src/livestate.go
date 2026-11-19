package main

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nicklaw5/helix/v2"
	log "github.com/sirupsen/logrus"
)

// Live status values in status.json.
const (
	statusOnline  = "online"
	statusPrelive = "prelive"
	statusOffline = "offline"
)

// liveRefreshInterval is how often viewer counts and titles of live
// streamers are refreshed (one batched /streams call per 100 live).
const liveRefreshInterval = 2 * time.Minute

// userRefreshInterval is how often every roster ID is looked up again, to
// pick up renames and display-name changes nobody sent an event for.
const userRefreshInterval = 24 * time.Hour

// nextStreamAt returns a streamer's next scheduled stream. The schedule
// code (bot-schedule) replaces it; it is called on the worker while
// s.mutex is held, so it must not take that mutex.
var nextStreamAt = func(id string) (time.Time, bool) { return time.Time{}, false }

// usersClient is the slice of Helix used to map logins to IDs;
// *helix.Client satisfies it.
type usersClient interface {
	GetUsers(params *helix.UsersParams) (*helix.UsersResponse, error)
}

// streamerState is what the bot knows about one streamer right now.
type streamerState struct {
	// live is true while Twitch lists a stream, in any category; status
	// is "online" only if that category is one of VALID_GAMES.
	live         bool
	status       string
	category     string
	title        string
	language     string
	tags         []string
	startedAt    time.Time
	viewers      int
	preliveSince time.Time
	// lastEvent is when an EventSub event for them was last handled.
	lastEvent time.Time
}

// liveState is the bot's in-memory view of every roster streamer, keyed
// by Twitch user ID, plus the published status.json snapshot. Everything
// except snapshot() runs on the worker goroutine.
type liveState struct {
	db *store

	users   map[string]*userRow // id -> current login and display name
	byLogin map[string]string   // any login ever seen (lowercase) -> id
	// roster is the set of IDs in streamers.csv + inactive_streamers.csv;
	// nil until it has been loaded once.
	roster           map[string]bool
	usersRefreshedAt time.Time

	states map[string]*streamerState

	snap        atomic.Pointer[apiDoc]
	lastPayload []byte

	// Statistics (stats.go).
	statsSnap  atomic.Pointer[statsDocs]
	lastSample map[string]time.Time
	lastTitle  map[string]string
}

// newLiveState loads the known users from db.
func newLiveState(db *store) (*liveState, error) {
	users, byLogin, err := db.loadUsers()
	if err != nil {
		return nil, err
	}
	l := &liveState{db: db, users: map[string]*userRow{}, byLogin: byLogin, states: map[string]*streamerState{}}
	for _, u := range users {
		u := u
		l.users[u.id] = &u
	}
	return l, nil
}

func (l *liveState) state(id string) *streamerState {
	st, ok := l.states[id]
	if !ok {
		st = &streamerState{status: statusOffline}
		l.states[id] = st
	}
	return st
}

// noteUser records id's current login and display name.
func (l *liveState) noteUser(id, login, displayName string, now time.Time) {
	login = strings.ToLower(strings.TrimSpace(login))
	if id == "" || login == "" {
		return
	}
	u, ok := l.users[id]
	if ok && u.login == login && (displayName == "" || u.displayName == displayName) && l.byLogin[login] == id {
		return
	}
	if err := l.db.upsertUser(id, login, displayName, now); err != nil {
		log.Errorf("live state: saving user %s (%s): %s", login, id, err)
		return
	}
	if !ok {
		u = &userRow{id: id}
		l.users[id] = u
	}
	if u.login != "" && u.login != login {
		log.Infof("live state: %s renamed %s -> %s", id, u.login, login)
	}
	u.login = login
	if displayName != "" {
		u.displayName = displayName
	}
	l.byLogin[login] = id
}

// noteEvent records that an EventSub event for id was just handled.
func (l *liveState) noteEvent(id string, now time.Time) {
	if id != "" {
		l.state(id).lastEvent = now
	}
}

// online records that id is live in stream (seen by an event, reconcile
// or refresh).
func (l *liveState) online(id string, stream helix.Stream, now time.Time) {
	if id == "" {
		return
	}
	stale, err := l.db.streamSeen(id, stream.StartedAt, stream.GameName, now)
	if err != nil {
		log.Errorf("live state: recording stream of %s: %s", id, err)
	}
	if stale {
		log.Infof("live state: ignoring stale stream listing for %s (started %s, already ended)", id, stream.StartedAt.Format(time.RFC3339))
		return
	}
	l.noteUser(id, stream.UserLogin, stream.UserName, now)
	st := l.state(id)
	st.live = true
	st.category, st.title, st.language = stream.GameName, stream.Title, strings.ToLower(stream.Language)
	st.tags = stream.Tags
	st.startedAt = stream.StartedAt.UTC()
	if st.startedAt.IsZero() {
		st.startedAt = now.UTC()
	}
	st.viewers = stream.ViewerCount
	st.preliveSince = time.Time{}
	st.status = statusOffline
	if contains(VALID_GAMES, stream.GameName) {
		st.status = statusOnline
	}
	l.recordSample(id, now, stream.ViewerCount, stream.GameName)
	l.recordTitle(id, now, stream.Title, stream.GameName)
}

// offline records that id's stream ended. missed means it was noticed by
// reconciliation rather than a stream.offline event, so the session is
// closed at the last time it was seen live rather than now.
func (l *liveState) offline(id string, missed bool, now time.Time) {
	if id == "" {
		return
	}
	var err error
	if missed {
		err = l.db.streamMissing(id)
	} else {
		err = l.db.streamEnded(id, now)
	}
	if err != nil {
		log.Errorf("live state: closing stream of %s: %s", id, err)
	}
	*l.state(id) = streamerState{status: statusOffline, lastEvent: l.state(id).lastEvent}
}

// preliveUpdate handles channel.update for an offline channel: switching to
// an infosec category marks them pre-live, anything else clears it.
func (l *liveState) preliveUpdate(id, category, title, language string, now time.Time) {
	if id == "" {
		return
	}
	st := l.state(id)
	if st.live {
		// Helix just stopped listing them; the offline event decides.
		return
	}
	if !contains(VALID_GAMES, category) {
		*st = streamerState{status: statusOffline, lastEvent: st.lastEvent}
		return
	}
	l.recordTitle(id, now, title, category)
	st.status = statusPrelive
	st.preliveSince = now.UTC()
	st.category, st.title, st.language = category, title, strings.ToLower(language)
	st.tags, st.startedAt, st.viewers = nil, time.Time{}, 0
}

// expirePrelive drops pre-live markers older than preliveTTL.
func (l *liveState) expirePrelive(now time.Time) {
	for _, st := range l.states {
		if st.status == statusPrelive && now.Sub(st.preliveSince) >= preliveTTL {
			*st = streamerState{status: statusOffline, lastEvent: st.lastEvent}
		}
	}
}

// reconcile brings every roster streamer (and anyone with a session still
// open) in line with Helix. live holds every live stream found, any key;
// streamers who had an event within reconcileGrace are left alone.
func (l *liveState) reconcile(live map[string]helix.Stream, now time.Time) (opened, closed int) {
	byID := map[string]helix.Stream{}
	for _, st := range live {
		if st.UserID != "" {
			byID[st.UserID] = st
		}
	}
	ids := map[string]bool{}
	for id := range l.roster {
		ids[id] = true
	}
	open, err := l.db.openSessionUsers()
	if err != nil {
		log.Errorf("live state: listing open sessions: %s", err)
	}
	openSet := map[string]bool{}
	for _, id := range open {
		ids[id] = true
		openSet[id] = true
	}
	for id := range ids {
		st := l.state(id)
		if !st.lastEvent.IsZero() && now.Sub(st.lastEvent) < reconcileGrace {
			continue
		}
		if stream, ok := byID[id]; ok {
			if !st.live {
				opened++
			}
			l.online(id, stream, now)
			continue
		}
		if st.live || openSet[id] {
			closed++
			l.offline(id, true, now)
		}
	}
	return opened, closed
}

// liveIDs returns the IDs Twitch currently lists as live, sorted.
func (l *liveState) liveIDs() []string {
	var ids []string
	for id, st := range l.states {
		if st.live {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// statusEntry is one streamer in status.json. Absent values are omitted.
type statusEntry struct {
	ID           string   `json:"id"`
	Login        string   `json:"login"`
	DisplayName  string   `json:"display_name,omitempty"`
	Status       string   `json:"status"`
	Category     string   `json:"category,omitempty"`
	Title        string   `json:"title,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Language     string   `json:"language,omitempty"`
	StartedAt    string   `json:"started_at,omitempty"`
	Viewers      *int     `json:"viewers,omitempty"`
	PreliveSince string   `json:"prelive_since,omitempty"`
	NextStreamAt string   `json:"next_stream_at,omitempty"`
	Hours30d     float64  `json:"hours_30d"`
}

type statusDoc struct {
	GeneratedAt string        `json:"generated_at"`
	LiveCount   int           `json:"live_count"`
	Streamers   []statusEntry `json:"streamers"`
}

// rebuild publishes a new status.json snapshot if anything in it changed.
func (l *liveState) rebuild(now time.Time) {
	if l.roster == nil {
		return
	}
	segs, err := l.db.segmentsSince(now.Add(-hoursWindow))
	if err != nil {
		log.Errorf("live state: reading sessions: %s", err)
	}
	hours := streamedHours(segs, now, hoursWindow)

	entries := make([]statusEntry, 0, len(l.roster))
	liveCount := 0
	for id := range l.roster {
		u := l.users[id]
		if u == nil {
			continue
		}
		e := statusEntry{ID: id, Login: u.login, DisplayName: u.displayName, Status: statusOffline}
		if e.DisplayName == "" {
			e.DisplayName = u.login
		}
		e.Hours30d = math.Round(hours[id]*10) / 10
		if t, ok := nextStreamAt(id); ok && !t.IsZero() {
			e.NextStreamAt = rfc3339(t)
		}
		if st := l.states[id]; st != nil {
			e.Status = st.status
			switch st.status {
			case statusOnline:
				liveCount++
				e.Category, e.Title, e.Tags, e.Language = st.category, st.title, st.tags, st.language
				e.StartedAt = rfc3339(st.startedAt)
				v := st.viewers
				e.Viewers = &v
			case statusPrelive:
				e.Category, e.Title, e.Language = st.category, st.title, st.language
				e.PreliveSince = rfc3339(st.preliveSince)
			}
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Login != entries[j].Login {
			return entries[i].Login < entries[j].Login
		}
		return entries[i].ID < entries[j].ID
	})

	payload, err := json.Marshal(entries)
	if err != nil {
		log.Errorf("live state: encoding status: %s", err)
		return
	}
	if l.snap.Load() != nil && bytes.Equal(payload, l.lastPayload) {
		return
	}
	doc, err := newJSONDoc(statusDoc{GeneratedAt: rfc3339(now), LiveCount: liveCount, Streamers: entries}, statusMaxAge)
	if err != nil {
		log.Errorf("live state: encoding status: %s", err)
		return
	}
	l.lastPayload = payload
	l.snap.Store(doc)
}

// snapshot returns the current status.json, or nil before the first one.
// Safe to call from any goroutine.
func (l *liveState) snapshot() *apiDoc { return l.snap.Load() }

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// --- StreamersRepo glue. All of it runs on the worker with s.mutex held,
// and is a no-op when live state is off (s.live == nil).

// resolveRoster maps the roster logins to user IDs and installs them as
// the roster. Logins already seen (including old logins of renamed
// accounts) come from the database; new ones are looked up on Helix. Once
// a day every roster ID is looked up again to refresh logins and display
// names. It returns the current logins of roster streamers that differ
// from the roster's spelling (renamed accounts), so they can be queried.
func (s *StreamersRepo) resolveRoster(logins []string, now time.Time) []string {
	l := s.live
	if l == nil {
		return nil
	}
	inRoster := map[string]bool{}
	ids := map[string]bool{}
	var unknown []string
	for _, login := range logins {
		key := strings.ToLower(strings.TrimSpace(login))
		if key == "" || inRoster[key] {
			continue
		}
		inRoster[key] = true
		if id, ok := l.byLogin[key]; ok {
			ids[id] = true
		} else {
			unknown = append(unknown, key)
		}
	}
	if s.users != nil && len(unknown) > 0 {
		found, err := s.lookupUsers(unknown, false, now)
		if err != nil {
			log.Warnf("live state: looking up %d new logins failed: %s", len(unknown), err)
		}
		var missing []string
		for _, login := range unknown {
			if id, ok := found[login]; ok {
				ids[id] = true
			} else if err == nil {
				missing = append(missing, login)
			}
		}
		if len(missing) > 0 {
			log.Infof("live state: %d roster logins unknown to Twitch: %s", len(missing), strings.Join(missing, ", "))
		}
	}
	if s.users != nil && now.Sub(l.usersRefreshedAt) >= userRefreshInterval && len(ids) > 0 {
		all := make([]string, 0, len(ids))
		for id := range ids {
			all = append(all, id)
		}
		sort.Strings(all)
		if _, err := s.lookupUsers(all, true, now); err != nil {
			log.Warnf("live state: refreshing roster users failed: %s", err)
		} else {
			l.usersRefreshedAt = now
		}
	}
	l.roster = ids

	var extra []string
	for id := range ids {
		if u := l.users[id]; u != nil && !inRoster[u.login] {
			extra = append(extra, u.login)
		}
	}
	sort.Strings(extra)
	return extra
}

// lookupUsers calls Helix GetUsers for keys (logins, or IDs if byID), 100
// at a time, records what it finds and returns lowercased login -> ID.
func (s *StreamersRepo) lookupUsers(keys []string, byID bool, now time.Time) (map[string]string, error) {
	found := map[string]string{}
	for start := 0; start < len(keys); start += helixLoginBatch {
		end := min(start+helixLoginBatch, len(keys))
		p := &helix.UsersParams{}
		if byID {
			p.IDs = keys[start:end]
		} else {
			p.Logins = keys[start:end]
		}
		resp, err := s.users.GetUsers(p)
		if err != nil {
			return found, err
		}
		if err := helixError(resp.ResponseCommon); err != nil {
			return found, err
		}
		for _, u := range resp.Data.Users {
			s.live.noteUser(u.ID, u.Login, u.DisplayName, now)
			found[strings.ToLower(u.Login)] = u.ID
		}
	}
	return found, nil
}

// liveEvent records an EventSub event's broadcaster and time.
func (s *StreamersRepo) liveEvent(job statusJob, now time.Time) {
	if s.live == nil || job.userID == "" {
		return
	}
	s.live.noteEvent(job.userID, now)
	if job.userLogin != "" {
		s.live.noteUser(job.userID, job.userLogin, job.displayName, now)
	}
}

// liveOnline feeds a stream found live after an event into live state.
func (s *StreamersRepo) liveOnline(id string, stream helix.Stream) {
	if s.live == nil {
		return
	}
	now := timeNow()
	s.live.online(id, stream, now)
	s.live.rebuild(now)
}

// liveOffline feeds stream.offline into live state.
func (s *StreamersRepo) liveOffline(id string) {
	if s.live == nil {
		return
	}
	now := timeNow()
	s.live.offline(id, false, now)
	s.live.rebuild(now)
}

// livePrelive feeds channel.update for an offline channel into live state.
func (s *StreamersRepo) livePrelive(job statusJob) {
	if s.live == nil {
		return
	}
	now := timeNow()
	s.live.preliveUpdate(job.userID, job.game, job.title, job.language, now)
	s.live.rebuild(now)
}

// livePreliveLogin marks a streamer, by login, pre-live in live state
// unless they already are. applyPrelive calls it, so every pre-live
// source (channel.update, scheduled streams) reaches status.json even
// when status commits are off.
func (s *StreamersRepo) livePreliveLogin(login, category, language string, since time.Time) {
	if s.live == nil {
		return
	}
	id, ok := s.live.byLogin[strings.ToLower(login)]
	if !ok {
		return
	}
	if st := s.live.states[id]; st != nil && (st.live || st.status == statusPrelive) {
		return
	}
	s.live.preliveUpdate(id, category, "", language, since)
	s.live.rebuild(timeNow())
}

// reconcileLive feeds a reconciliation's Helix answer into live state.
func (s *StreamersRepo) reconcileLive(live map[string]helix.Stream) {
	if s.live == nil {
		return
	}
	now := timeNow()
	opened, closed := s.live.reconcile(live, now)
	if opened+closed > 0 {
		log.Infof("live state: reconcile opened %d and closed %d sessions", opened, closed)
	}
	s.live.expirePrelive(now)
	s.live.rebuild(now)
}

// startLiveRefresh queues a live refresh every interval.
func (s *StreamersRepo) startLiveRefresh(interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			if !s.enqueue(statusJob{kind: jobRefreshLive}) {
				log.Warn("live refresh skipped: event queue full")
			}
		}
	}()
}

// refreshLive updates viewer counts, titles and categories of everyone
// currently live with one /streams call per 100. A streamer missing from
// the answer is left alone: Helix lags, and stream.offline or the next
// reconcile decides.
func (s *StreamersRepo) refreshLive() {
	if s.live == nil {
		return
	}
	now := timeNow()
	ids := s.live.liveIDs()
	for start := 0; start < len(ids); start += helixLoginBatch {
		end := min(start+helixLoginBatch, len(ids))
		resp, err := s.client.GetStreams(&helix.StreamsParams{UserIDs: ids[start:end], First: helixLoginBatch})
		if err == nil {
			err = helixError(resp.ResponseCommon)
		}
		if err != nil {
			log.Warnf("live refresh: querying Helix failed: %s", err)
			break
		}
		for _, st := range resp.Data.Streams {
			if cur := s.live.states[st.UserID]; cur != nil && cur.live {
				s.live.online(st.UserID, st, now)
			}
		}
	}
	s.live.expirePrelive(now)
	s.live.rebuild(now)
}
