package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Stream history is when each roster streamer actually streamed over the
// last 90 days, from SullyGnome (falling back to TwitchTracker). Twitch
// keeps no past schedules and its API has no stream history beyond VODs, so
// the third-party trackers are the only source. It feeds two things:
//
//   - estimated track records: each streamer's current *recurring*
//     segments projected back 13 weeks and judged against the history
//     with the same ±adherenceTolerance rule as observed outcomes. Real
//     observed outcomes replace estimates for the same slot as they come in.
//   - habits: weekly UTC slots a streamer usually streams in, schedule or
//     not ("Tuesdays ~18:00 UTC, 11 of the last 13 weeks").
//
// Cost: one SullyGnome JSON request per streamer per day (plus a one-time
// channel page fetch to learn SullyGnome's channel ID), historyPause apart,
// on its own goroutine: no Helix and no git, so it never touches the worker.

const (
	historyWindow          = 90 * 24 * time.Hour
	historyRefreshInterval = 24 * time.Hour
	historyMaxPages        = 5   // 100 streams per SullyGnome page
	historyFailureAlert    = 2   // runs in a row with most fetches failing
	habitMinWeeks          = 4   // a slot needs a stream in this many weeks
	habitMinShare          = 0.5 // and in at least this share of the weeks
	habitSlotRound         = 15 * time.Minute
)

// historyPause spaces out requests to the trackers; swapped out in tests.
var historyPause = 3 * time.Second

// historyHTTPClient fetches the trackers; tests point the base URLs at a
// fake server instead.
var historyHTTPClient = &http.Client{Timeout: 30 * time.Second}

var (
	sullyGnomeBase    = "https://sullygnome.com"
	twitchTrackerBase = "https://twitchtracker.com"
)

// pastStream is one stream a streamer did.
type pastStream struct {
	Start  time.Time     `json:"start"`
	Length time.Duration `json:"length"`
}

// historySource fetches a streamer's streams since a time.
type historySource interface {
	name() string
	fetch(login string, since time.Time) ([]pastStream, error)
}

// streamHistory holds every roster streamer's last 90 days of streams.
type streamHistory struct {
	sources []historySource

	mu        sync.RWMutex
	streams   map[string][]pastStream // by broadcaster ID, oldest first
	fetchedAt map[string]time.Time    // last successful fetch, by ID
	lastRun   time.Time
	badRuns   int
	path      string
}

func newStreamHistory(sources ...historySource) *streamHistory {
	return &streamHistory{sources: sources, streams: map[string][]pastStream{}, fetchedAt: map[string]time.Time{}}
}

func defaultHistorySources() []historySource {
	return []historySource{&sullyGnome{ids: map[string]int{}}, twitchTracker{}}
}

// streamHistoryPath is where stream history is cached, next to the
// schedule track record.
func streamHistoryPath() string {
	return filepath.Join(filepath.Dir(scheduleHistoryPath()), "stream_history.json")
}

type streamHistoryFile struct {
	Version   int                     `json:"version"`
	LastRun   time.Time               `json:"last_run"`
	Streams   map[string][]pastStream `json:"streams"`
	FetchedAt map[string]time.Time    `json:"fetched_at"`
}

// load reads cached history from path and keeps saving there. A missing
// directory keeps it in memory (re-fetched after every restart).
func (h *streamHistory) load(path string) error {
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || !fi.IsDir() {
		return fmt.Errorf("directory of %s not available, keeping stream history in memory only", path)
	}
	h.path = path
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f streamHistoryFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastRun = f.LastRun
	for id, s := range f.Streams {
		h.streams[id] = s
	}
	for id, t := range f.FetchedAt {
		h.fetchedAt[id] = t
	}
	return nil
}

func (h *streamHistory) save() {
	if h.path == "" {
		return
	}
	h.mu.RLock()
	b, err := json.Marshal(streamHistoryFile{Version: 1, LastRun: h.lastRun, Streams: h.streams, FetchedAt: h.fetchedAt})
	h.mu.RUnlock()
	if err == nil {
		tmp := h.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0644); err == nil {
			err = os.Rename(tmp, h.path)
		}
	}
	if err != nil {
		log.Warnf("stream history: saving to %s failed: %s", h.path, err)
	}
}

// get returns a streamer's streams and whether they were ever fetched.
func (h *streamHistory) get(id string) ([]pastStream, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.fetchedAt[id]
	return h.streams[id], ok
}

// startStreamHistory refreshes stream history daily (sooner after a start
// if the cache is older than a day) and recomputes estimates after each run.
func (s *StreamersRepo) startStreamHistory() {
	go func() {
		for {
			st := s.schedule
			// Wait for the first schedule refresh: it resolves the roster.
			for st.lastRefresh() == "never" {
				time.Sleep(10 * time.Second)
			}
			st.history.mu.RLock()
			wait := st.history.lastRun.Add(historyRefreshInterval).Sub(timeNow())
			st.history.mu.RUnlock()
			if wait > 0 {
				s.updateEstimates()
				time.Sleep(wait)
			}
			s.refreshStreamHistory()
		}
	}()
}

// refreshStreamHistory fetches every roster streamer's recent streams. A
// failed fetch keeps the previous data.
func (s *StreamersRepo) refreshStreamHistory() {
	st := s.schedule
	if st == nil || st.history == nil {
		return
	}
	h := st.history
	st.mu.RLock()
	roster := make([]rosterEntry, 0, len(st.roster))
	for _, e := range st.roster {
		roster = append(roster, e)
	}
	st.mu.RUnlock()
	sort.Slice(roster, func(i, j int) bool { return roster[i].login < roster[j].login })
	if len(roster) == 0 {
		return
	}

	now := timeNow()
	since := now.Add(-historyWindow)
	var failed []string
	bySource := map[string]int{}
	for i, e := range roster {
		if i > 0 {
			sleep(historyPause)
		}
		var streams []pastStream
		var errs []string
		ok := false
		for _, src := range h.sources {
			got, err := src.fetch(e.login, since)
			if err != nil {
				errs = append(errs, src.name()+": "+err.Error())
				continue
			}
			streams, ok = got, true
			bySource[src.name()]++
			break
		}
		if !ok {
			failed = append(failed, e.login+" ("+strings.Join(errs, "; ")+")")
			continue
		}
		sort.Slice(streams, func(i, j int) bool { return streams[i].Start.Before(streams[j].Start) })
		h.mu.Lock()
		h.streams[e.id] = streams
		h.fetchedAt[e.id] = now
		h.mu.Unlock()
	}

	h.mu.Lock()
	// Forget streamers who left the roster.
	ids := map[string]bool{}
	for _, e := range roster {
		ids[e.id] = true
	}
	for id := range h.streams {
		if !ids[id] {
			delete(h.streams, id)
			delete(h.fetchedAt, id)
		}
	}
	h.lastRun = now
	if len(failed)*2 > len(roster) {
		h.badRuns++
	} else {
		h.badRuns = 0
	}
	bad := h.badRuns
	h.mu.Unlock()
	h.save()

	log.Infof("stream history: fetched %d of %d streamers (%v), %d failed", len(roster)-len(failed), len(roster), bySource, len(failed))
	switch {
	case bad >= historyFailureAlert:
		log.Errorf("stream history: most fetches failed %d runs in a row, keeping old data; first: %s", bad, failed[0])
	case len(failed) > 0:
		log.Warnf("stream history: fetch failed for %d streamers, keeping their old data; first: %s", len(failed), failed[0])
	}
	s.updateEstimates()
}

// updateEstimates recomputes estimated outcomes from stream history and
// the current recurring segments.
func (s *StreamersRepo) updateEstimates() {
	st := s.schedule
	if st == nil || st.history == nil || st.adherence == nil {
		return
	}
	now := timeNow()
	est := map[string][]adherenceOutcome{}
	st.mu.RLock()
	for id, b := range st.byID {
		streams, ok := st.history.get(id)
		if !ok {
			continue
		}
		if o := estimateOutcomes(b.segments, streams, now); len(o) > 0 {
			est[id] = o
		}
	}
	st.mu.RUnlock()
	st.adherence.setEstimated(est)
}

// estimateOutcomes projects each recurring segment back week by week over
// the history window and judges each past occurrence against streams: kept
// if a stream overlapped start ± adherenceTolerance. Occurrences are only
// judged once their window has closed.
func estimateOutcomes(segs []scheduledSegment, streams []pastStream, now time.Time) []adherenceOutcome {
	week := 7 * 24 * time.Hour
	seen := map[int64]bool{}
	var out []adherenceOutcome
	for _, g := range segs {
		if !g.Recurring {
			continue
		}
		for k := 1; ; k++ {
			occ := g.Start.Add(-time.Duration(k) * week)
			if now.Sub(occ) > historyWindow {
				break
			}
			if occ.Add(adherenceTolerance).After(now) || seen[occ.Unix()] {
				continue
			}
			seen[occ.Unix()] = true
			o := adherenceOutcome{SegmentID: g.ID, Start: occ, Estimated: true}
			from, to := occ.Add(-adherenceTolerance), occ.Add(adherenceTolerance)
			for _, p := range streams {
				if p.Start.Before(to) && p.Start.Add(p.Length).After(from) {
					o.Kept, o.LiveAt = true, p.Start
					break
				}
			}
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// habitSlot is a weekly UTC time a streamer usually streams at.
type habitSlot struct {
	Weekday time.Weekday
	Minute  int // minutes after UTC midnight
	Weeks   int // weeks with a stream starting within ±adherenceTolerance
	Of      int // weeks of history
}

// streamHabits returns streamer id's usual weekly slots, most regular first.
func (s *StreamersRepo) streamHabits(id string) []habitSlot {
	if s.schedule == nil || s.schedule.history == nil {
		return nil
	}
	streams, ok := s.schedule.history.get(id)
	if !ok {
		return nil
	}
	return habits(streams, timeNow())
}

// habits finds weekly slots: for each candidate minute-of-week (every
// stream start), count the weeks of the history window that have a stream
// starting within adherenceTolerance of it. Slots in at least habitMinWeeks
// weeks and habitMinShare of them are kept, greedily, so that no two are
// closer than twice the tolerance.
func habits(streams []pastStream, now time.Time) []habitSlot {
	const weekMin = 7 * 24 * 60
	tol := int(adherenceTolerance / time.Minute)
	start := now.Add(-historyWindow)
	weekOf := func(t time.Time) int { return int(t.Sub(start) / (7 * 24 * time.Hour)) }
	mow := func(t time.Time) int {
		t = t.UTC()
		return int(t.Weekday())*24*60 + t.Hour()*60 + t.Minute()
	}
	dist := func(a, b int) int {
		d := (a - b + weekMin) % weekMin
		return min(d, weekMin-d)
	}
	var recent []pastStream
	for _, p := range streams {
		if !p.Start.Before(start) && p.Start.Before(now) {
			recent = append(recent, p)
		}
	}
	type cand struct {
		minute, weeks, of int
	}
	var cands []cand
	for _, c := range recent {
		m := mow(c.Start)
		weeks := map[int]bool{}
		var offsets []int
		for _, p := range recent {
			if d := mow(p.Start); dist(d, m) <= tol {
				weeks[weekOf(p.Start)] = true
				off := (d - m + weekMin) % weekMin
				if off > weekMin/2 {
					off -= weekMin
				}
				offsets = append(offsets, off)
			}
		}
		sort.Ints(offsets)
		center := (m + offsets[len(offsets)/2] + weekMin) % weekMin
		// Occurrences of this minute-of-week inside the window.
		of := 0
		first := start.Truncate(24 * time.Hour).Add(-7 * 24 * time.Hour)
		for t := first; t.Before(now); t = t.Add(24 * time.Hour) {
			occ := t.Add(time.Duration(center%(24*60)) * time.Minute)
			if int(occ.Weekday()) == center/(24*60) && !occ.Before(start) && occ.Before(now) {
				of++
			}
		}
		cands = append(cands, cand{center, len(weeks), of})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].weeks != cands[j].weeks {
			return cands[i].weeks > cands[j].weeks
		}
		return cands[i].minute < cands[j].minute
	})
	var out []habitSlot
	for _, c := range cands {
		if c.weeks < habitMinWeeks || c.of == 0 || float64(c.weeks) < habitMinShare*float64(c.of) {
			continue
		}
		clash := false
		for _, h := range out {
			if dist(int(h.Weekday)*24*60+h.Minute, c.minute) <= 2*tol {
				clash = true
				break
			}
		}
		if clash {
			continue
		}
		minute := c.minute % (24 * 60)
		r := int(habitSlotRound / time.Minute)
		minute = (minute + r/2) / r * r % (24 * 60)
		out = append(out, habitSlot{Weekday: time.Weekday(c.minute / (24 * 60)), Minute: minute, Weeks: min(c.weeks, c.of), Of: c.of})
	}
	return out
}

// nextHabitAt returns the next start of one of a streamer's habit slots.
func nextHabitAt(slots []habitSlot, now time.Time) (time.Time, bool) {
	var next time.Time
	day := now.UTC().Truncate(24 * time.Hour)
	for _, h := range slots {
		for d := 0; d <= 7; d++ {
			t := day.Add(time.Duration(d)*24*time.Hour + time.Duration(h.Minute)*time.Minute)
			if t.Weekday() != h.Weekday || !t.After(now) {
				continue
			}
			if next.IsZero() || t.Before(next) {
				next = t
			}
			break
		}
	}
	return next, !next.IsZero()
}

// --- sources ---

func historyGet(rawURL string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "StreamStatus/"+version)
	resp, err := historyHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return body, resp.StatusCode, err
}

// sullyGnome reads the 90-day stream table SullyGnome's channel pages use.
type sullyGnome struct {
	mu  sync.Mutex
	ids map[string]int // SullyGnome channel ID by login
}

func (*sullyGnome) name() string { return "sullygnome" }

var sullyGnomeIDRe = regexp.MustCompile(`"urlpart":"([^"]*)","id":(\d+)`)

func (sg *sullyGnome) channelID(login string) (int, error) {
	sg.mu.Lock()
	id, ok := sg.ids[login]
	sg.mu.Unlock()
	if ok {
		return id, nil
	}
	body, code, err := historyGet(sullyGnomeBase + "/channel/" + url.PathEscape(login) + "/90/streams")
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("channel page status %d", code)
	}
	for _, m := range sullyGnomeIDRe.FindAllSubmatch(body, -1) {
		if strings.EqualFold(string(m[1]), login) {
			id, _ = strconv.Atoi(string(m[2]))
		}
	}
	if id == 0 {
		return 0, errors.New("channel not tracked")
	}
	sg.mu.Lock()
	sg.ids[login] = id
	sg.mu.Unlock()
	return id, nil
}

func (sg *sullyGnome) fetch(login string, since time.Time) ([]pastStream, error) {
	id, err := sg.channelID(login)
	if err != nil {
		return nil, err
	}
	var out []pastStream
	for page := 0; page < historyMaxPages; page++ {
		body, code, err := historyGet(fmt.Sprintf("%s/api/tables/channeltables/streams/90/%d/%%20/1/1/desc/%d/100", sullyGnomeBase, id, page*100))
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, fmt.Errorf("streams table status %d", code)
		}
		var t struct {
			RecordsTotal int `json:"recordsTotal"`
			Data         []struct {
				Start  string `json:"startDateTime"`
				Length int    `json:"length"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &t); err != nil {
			return nil, fmt.Errorf("streams table: %w", err)
		}
		for _, d := range t.Data {
			start, err := time.Parse(time.RFC3339, d.Start)
			if err != nil || start.Before(since) {
				continue
			}
			out = append(out, pastStream{Start: start.UTC(), Length: time.Duration(d.Length) * time.Minute})
		}
		if (page+1)*100 >= t.RecordsTotal || len(t.Data) == 0 {
			return out, nil
		}
		sleep(historyPause)
	}
	return out, nil
}

// twitchTracker parses the stream list on a TwitchTracker channel page
// (allowed by its robots.txt). Times are UTC.
type twitchTracker struct{}

func (twitchTracker) name() string { return "twitchtracker" }

var twitchTrackerRowRe = regexp.MustCompile(`<td nowrap data-order="(\d{4}-\d\d-\d\d \d\d:\d\d)">[\s\S]*?<td data-order="(\d+)">`)

func (twitchTracker) fetch(login string, since time.Time) ([]pastStream, error) {
	body, code, err := historyGet(twitchTrackerBase + "/" + url.PathEscape(login) + "/streams")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("status %d", code)
	}
	rows := twitchTrackerRowRe.FindAllSubmatch(body, -1)
	if len(rows) == 0 && !strings.Contains(string(body), "/streams") {
		return nil, errors.New("unrecognised page")
	}
	var out []pastStream
	for _, m := range rows {
		start, err := time.Parse("2006-01-02 15:04", string(m[1]))
		if err != nil || start.Before(since) {
			continue
		}
		mins, _ := strconv.Atoi(string(m[2]))
		out = append(out, pastStream{Start: start, Length: time.Duration(mins) * time.Minute})
	}
	return out, nil
}
