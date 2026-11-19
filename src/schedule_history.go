package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nicklaw5/helix/v2"
	log "github.com/sirupsen/logrus"
)

// A streamer's track record says how often they actually stream when their
// Twitch schedule says they will, so the site can say how likely a
// scheduled stream is to happen.
//
// Every non-canceled, non-vacation segment is watched from
// adherenceTolerance before its start until adherenceTolerance after it,
// polling Helix /streams every adherencePollInterval. Being live (in any
// category) at any poll in that window means the segment was kept. A
// window that closes without seeing them live is a miss, but only if it
// was watched properly: from before the scheduled start, with no gap
// between polls over adherenceMaxGap and up to near its end. Anything else
// (a restart or a Helix outage mid-window) is unknown and not counted, so
// the bot's own downtime never counts against a streamer.
//
// Cost: one /streams call (up to 100 IDs) per poll, only while some
// segment's window is open.

const (
	adherenceTolerance    = 90 * time.Minute
	adherencePollInterval = 5 * time.Minute
	adherenceMaxGap       = 15 * time.Minute
	// adherenceKeep is how many outcomes are kept per streamer, and
	// adherenceMaxAge how old they may get.
	adherenceKeep   = 20
	adherenceMaxAge = 90 * 24 * time.Hour
)

// adherenceOutcome is whether one scheduled segment was kept.
type adherenceOutcome struct {
	SegmentID string    `json:"segment_id"`
	Start     time.Time `json:"start"`
	Kept      bool      `json:"kept"`
	LiveAt    time.Time `json:"live_at,omitzero"`
}

// watchedSegment is a segment whose window is open. Worker only.
type watchedSegment struct {
	seg                 scheduledSegment
	firstPoll, lastPoll time.Time
	maxGap              time.Duration
	liveAt              time.Time
}

// adherenceTracker holds every streamer's outcomes. mu guards outcomes,
// which handlers read; watching is worker only.
type adherenceTracker struct {
	mu       sync.RWMutex
	outcomes map[string][]adherenceOutcome // by broadcaster ID, oldest first

	watching map[string]*watchedSegment // by segmentKey
	path     string                     // JSON file; "" keeps history in memory
}

func newAdherenceTracker() *adherenceTracker {
	return &adherenceTracker{outcomes: map[string][]adherenceOutcome{}, watching: map[string]*watchedSegment{}}
}

func segmentKey(g scheduledSegment) string { return g.ID + "@" + g.Start.UTC().Format(time.RFC3339) }

type adherenceFile struct {
	Version  int                           `json:"version"`
	Outcomes map[string][]adherenceOutcome `json:"outcomes"`
}

// scheduleHistoryPath is where the track record is kept: SS_SCHEDULE_HISTORY
// if set, else next to the SQLite DB (SS_DB_PATH, default /data).
func scheduleHistoryPath() string {
	if p := os.Getenv("SS_SCHEDULE_HISTORY"); p != "" {
		return p
	}
	db := os.Getenv("SS_DB_PATH")
	if db == "" {
		db = "/data/streamstatus.db"
	}
	return filepath.Join(filepath.Dir(db), "schedule_history.json")
}

// loadHistory reads the track record from path and keeps saving it there.
// If path's directory doesn't exist (no volume mounted), history stays in
// memory and is lost on restart.
func (a *adherenceTracker) loadHistory(path string) error {
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || !fi.IsDir() {
		return fmt.Errorf("directory of %s not available, keeping schedule history in memory only", path)
	}
	a.path = path
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f adherenceFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, o := range f.Outcomes {
		a.outcomes[id] = o
	}
	return nil
}

// save writes the track record atomically. Worker only.
func (a *adherenceTracker) save() {
	if a.path == "" {
		return
	}
	a.mu.RLock()
	b, err := json.Marshal(adherenceFile{Version: 1, Outcomes: a.outcomes})
	a.mu.RUnlock()
	if err == nil {
		tmp := a.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0644); err == nil {
			err = os.Rename(tmp, a.path)
		}
	}
	if err != nil {
		log.Warnf("schedule: saving track record to %s failed: %s", a.path, err)
	}
}

// record adds an outcome, dropping old ones. It reports false if the
// segment was already recorded.
func (a *adherenceTracker) record(id string, o adherenceOutcome, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	list := a.outcomes[id]
	for _, x := range list {
		if x.SegmentID == o.SegmentID && x.Start.Equal(o.Start) {
			return false
		}
	}
	list = append(list, o)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Start.Before(list[j].Start) })
	kept := list[:0]
	for _, x := range list {
		if now.Sub(x.Start) <= adherenceMaxAge {
			kept = append(kept, x)
		}
	}
	if len(kept) > adherenceKeep {
		kept = kept[len(kept)-adherenceKeep:]
	}
	a.outcomes[id] = kept
	return true
}

// recorded reports whether g already has an outcome.
func (a *adherenceTracker) recorded(g scheduledSegment) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, o := range a.outcomes[g.BroadcasterID] {
		if o.SegmentID == g.ID && o.Start.Equal(g.Start) {
			return true
		}
	}
	return false
}

// trackRecord returns how many of a streamer's recent scheduled streams
// they kept, out of how many were judged.
func (a *adherenceTracker) trackRecord(id string, now time.Time) (kept, total int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, o := range a.outcomes[id] {
		if now.Sub(o.Start) > adherenceMaxAge {
			continue
		}
		total++
		if o.Kept {
			kept++
		}
	}
	return kept, total
}

// scheduleTrackRecord returns how many of streamer id's recent scheduled
// streams (up to adherenceKeep in the last 90 days) they went live for.
func (s *StreamersRepo) scheduleTrackRecord(id string) (kept, total int) {
	if s.schedule == nil || s.schedule.adherence == nil {
		return 0, 0
	}
	return s.schedule.adherence.trackRecord(id, timeNow())
}

// trackScheduleAdherence polls whether streamers with an open window are
// live and records the outcome of windows that closed. Runs on the worker.
func (s *StreamersRepo) trackScheduleAdherence() {
	st := s.schedule
	if st == nil || st.adherence == nil {
		return
	}
	a := st.adherence
	now := timeNow()

	// Start watching segments whose window opened; stop watching ones that
	// were canceled or removed before they started.
	current := map[string]bool{}
	st.mu.RLock()
	for _, b := range st.byID {
		for _, g := range b.segments {
			if g.off() {
				continue
			}
			key := segmentKey(g)
			current[key] = true
			if now.Before(g.Start.Add(-adherenceTolerance)) || !now.Before(g.Start.Add(adherenceTolerance)) {
				continue
			}
			if _, ok := a.watching[key]; !ok && !a.recorded(g) {
				a.watching[key] = &watchedSegment{seg: g}
			}
		}
	}
	st.mu.RUnlock()
	for key, w := range a.watching {
		if !current[key] && now.Before(w.seg.Start) {
			delete(a.watching, key)
		}
	}
	if len(a.watching) == 0 {
		return
	}

	// Poll the ones still open.
	var ids []string
	seen := map[string]bool{}
	for _, w := range a.watching {
		if now.Before(w.seg.Start.Add(adherenceTolerance)) && !seen[w.seg.BroadcasterID] {
			seen[w.seg.BroadcasterID] = true
			ids = append(ids, w.seg.BroadcasterID)
		}
	}
	sort.Strings(ids)
	live := map[string]bool{}
	polled := true
	for start := 0; start < len(ids); start += helixLoginBatch {
		resp, err := s.client.GetStreams(&helix.StreamsParams{UserIDs: ids[start:min(start+helixLoginBatch, len(ids))], First: helixLoginBatch})
		if err == nil {
			err = helixError(resp.ResponseCommon)
		}
		if err != nil {
			// The gap this leaves can make the window unknown, never a miss.
			log.Warnf("schedule: track record poll failed: %s", err)
			polled = false
			break
		}
		for _, x := range resp.Data.Streams {
			live[x.UserID] = true
		}
	}
	if polled {
		for _, w := range a.watching {
			if !now.Before(w.seg.Start.Add(adherenceTolerance)) {
				continue
			}
			if !w.lastPoll.IsZero() {
				w.maxGap = max(w.maxGap, now.Sub(w.lastPoll))
			}
			if w.firstPoll.IsZero() {
				w.firstPoll = now
			}
			w.lastPoll = now
			if live[w.seg.BroadcasterID] && w.liveAt.IsZero() {
				w.liveAt = now
			}
		}
	}

	// Judge: kept as soon as they're seen live; missed once a well-watched
	// window closes without it.
	changed := false
	for key, w := range a.watching {
		end := w.seg.Start.Add(adherenceTolerance)
		o := adherenceOutcome{SegmentID: w.seg.ID, Start: w.seg.Start}
		switch {
		case !w.liveAt.IsZero():
			o.Kept, o.LiveAt = true, w.liveAt
		case now.Before(end):
			continue
		case w.firstPoll.IsZero() || w.firstPoll.After(w.seg.Start) ||
			w.maxGap > adherenceMaxGap || end.Sub(w.lastPoll) > adherenceMaxGap:
			log.Infof("schedule: not judging segment %s of %s: window not watched fully", w.seg.ID, w.seg.BroadcasterID)
			delete(a.watching, key)
			continue
		}
		delete(a.watching, key)
		if a.record(w.seg.BroadcasterID, o, now) {
			changed = true
			log.Infof("schedule: %s scheduled for %s: kept=%v", w.seg.BroadcasterID, w.seg.Start.Format(time.RFC3339), o.Kept)
		}
	}
	if changed {
		a.save()
	}
}

// trackRecordJSON is a streamer's track record as served in schedule.json.
type trackRecordJSON struct {
	Kept  int `json:"kept"`
	Total int `json:"total"`
}
