package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nicklaw5/helix/v2"
	log "github.com/sirupsen/logrus"
)

// defaultReconcileInterval is how often the bot re-checks every streamer's
// live state against Helix, to recover from EventSub deliveries that never
// arrived. One run costs ceil(streamers/100) Helix calls (~4-7), far below
// the 800 points/minute app-token rate limit; the real cost of a short
// interval is a site rebuild per correction.
const defaultReconcileInterval = 5 * time.Minute

// reconcileGrace is how long after an EventSub event reconciliation leaves
// that streamer alone. Helix /streams lags EventSub by a minute or two
// (still listing a stream that just ended, or not yet listing one that
// just started), so correcting sooner would undo a fresh event and flap.
const reconcileGrace = 5 * time.Minute

// preliveTTL is how long a 🟡 pre-live marker lasts if the streamer
// doesn't go live.
const preliveTTL = 60 * time.Minute

// Pre-live rows carry their start time in the Twitch link's hover text:
// "Getting ready: <category> (since 2026-11-19T14:05Z)".
const (
	preliveTitlePrefix = "Getting ready: "
	preliveTimeFormat  = "2006-01-02T15:04Z"
)

// helixLoginBatch is the max number of user_login values per /streams call.
const helixLoginBatch = 100

// statusChange is a correction reconciliation wants to make.
type statusChange struct {
	name   string
	online bool
	stream helix.Stream
}

// startReconciler queues a reconciliation now and then every interval.
func (s *StreamersRepo) startReconciler(interval time.Duration) {
	s.enqueue(statusJob{kind: jobReconcile})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			if !s.enqueue(statusJob{kind: jobReconcile}) {
				log.Warn("reconcile skipped: event queue full")
			}
		}
	}()
}

// reconcile compares the site's 🟢 markers with what Helix says is live and
// corrects mismatches through the normal update/commit/push path. Callers
// must hold s.mutex (the worker does).
func (s *StreamersRepo) reconcile() {
	log.Info("reconcile: starting")
	if err := s.getRepo(); err != nil {
		log.Errorf("reconcile: syncing repo failed, skipping: %s", err)
		return
	}
	if err := s.readFile(); err != nil {
		log.Errorf("reconcile: reading markdown failed, skipping: %s", err)
		return
	}
	active, err := csvNames(s.activeCsvPath)
	if err != nil {
		log.Errorf("reconcile: reading %s failed, skipping: %s", s.activeCsvPath, err)
		return
	}
	var inactive []string
	if s.reconcileInactive && s.inactiveCsvPath != "" {
		inactive, err = csvNames(s.inactiveCsvPath)
		if err != nil {
			log.Errorf("reconcile: reading %s failed, ignoring inactive streamers: %s", s.inactiveCsvPath, err)
			inactive = nil
		}
	}

	live, err := s.fetchLiveByLogin(append(append([]string{}, active...), inactive...))
	if err != nil {
		// Acting on a partial answer would mark live streamers offline.
		log.Errorf("reconcile: querying Helix failed, skipping: %s", err)
		return
	}

	changes := skipRecentlyEvented(diffLiveState(active, inactive, onlineStreamers(s.indexMdText), live), s.lastEvent, timeNow(), reconcileGrace)
	expired := expiredPrelive(preliveStreamers(s.indexMdText), live, timeNow(), preliveTTL)
	for _, name := range expired {
		log.Infof("reconcile: pre-live marker for %s expired", name)
		s.clearPrelive(name)
	}
	log.Infof("reconcile: %d active, %d inactive checked, %d live, %d corrections", len(active), len(inactive), len(live), len(changes))
	if len(changes) == 0 {
		return
	}
	var summary strings.Builder
	for _, c := range changes {
		log.Warnf("reconcile: correcting %s to online=%v", c.name, c.online)
		state := "offline"
		if c.online {
			state = "online"
		}
		fmt.Fprintf(&summary, "• %s → %s\n", c.name, state)
		s.applyStatus(c.name, c.online, c.stream.GameName, c.stream.Language, c.stream.Tags)
	}
	// Each correction is an EventSub event the bot never got (or got wrong).
	s.alert(fmt.Sprintf("Reconcile corrected %d streamer(s)", len(changes)), summary.String())
}

// diffLiveState returns the corrections needed so the site matches Helix.
// An active streamer should be 🟢 exactly when they are live in one of
// VALID_GAMES (the same rule the event handlers use). An inactive streamer
// who is live in VALID_GAMES is reactivated. live is keyed by lowercased
// login; onlineNow by lowercased name.
func diffLiveState(active, inactive []string, onlineNow map[string]bool, live map[string]helix.Stream) []statusChange {
	var changes []statusChange
	for _, name := range active {
		key := strings.ToLower(name)
		stream, isLive := live[key]
		want := isLive && contains(VALID_GAMES, stream.GameName)
		if want == onlineNow[key] {
			continue
		}
		c := statusChange{name: name, online: want}
		if want {
			c.stream = stream
			if stream.UserName != "" {
				c.name = stream.UserName
			}
		}
		changes = append(changes, c)
	}
	for _, name := range inactive {
		stream, isLive := live[strings.ToLower(name)]
		if !isLive || !contains(VALID_GAMES, stream.GameName) {
			continue
		}
		c := statusChange{name: name, online: true, stream: stream}
		if stream.UserName != "" {
			c.name = stream.UserName
		}
		changes = append(changes, c)
	}
	return changes
}

// onlineStreamers returns the lowercased names of streamers marked 🟢 in
// index.md.
func onlineStreamers(indexMd string) map[string]bool {
	online := map[string]bool{}
	for _, line := range strings.Split(indexMd, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "🟢") {
			continue
		}
		parts := strings.SplitN(line, "`", 3)
		if len(parts) == 3 && parts[1] != "" {
			online[strings.ToLower(parts[1])] = true
		}
	}
	return online
}

// fetchLiveByLogin returns the live streams among logins, keyed by
// lowercased login. Any failed batch fails the whole call.
func (s *StreamersRepo) fetchLiveByLogin(logins []string) (map[string]helix.Stream, error) {
	live := map[string]helix.Stream{}
	for start := 0; start < len(logins); start += helixLoginBatch {
		end := start + helixLoginBatch
		if end > len(logins) {
			end = len(logins)
		}
		resp, err := s.client.GetStreams(&helix.StreamsParams{
			UserLogins: logins[start:end],
			First:      helixLoginBatch,
		})
		if err != nil {
			return nil, err
		}
		if err := helixError(resp.ResponseCommon); err != nil {
			return nil, err
		}
		for _, st := range resp.Data.Streams {
			live[strings.ToLower(st.UserLogin)] = st
		}
	}
	return live, nil
}

// csvNames returns the streamer names (first column) from a streamers CSV.
func csvNames(path string) ([]string, error) {
	lines, err := readCSVLines(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(lines))
	for _, l := range lines {
		if n := csvName(l); n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// skipRecentlyEvented drops changes for streamers that had an EventSub
// event within grace of now; the event is fresher than Helix.
func skipRecentlyEvented(changes []statusChange, lastEvent map[string]time.Time, now time.Time, grace time.Duration) []statusChange {
	kept := changes[:0:0]
	for _, c := range changes {
		if t, ok := lastEvent[strings.ToLower(c.name)]; ok && now.Sub(t) < grace {
			log.Infof("reconcile: %s had an event %s ago, not overriding it", c.name, now.Sub(t).Round(time.Second))
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

// preliveRow is a 🟡 row in index.md.
type preliveRow struct {
	name  string    // as written in the row
	since time.Time // zero if it can't be read
}

// preliveStreamers returns the 🟡 rows in index.md keyed by lowercased name.
func preliveStreamers(indexMd string) map[string]preliveRow {
	out := map[string]preliveRow{}
	for _, line := range strings.Split(indexMd, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "🟡") {
			continue
		}
		parts := strings.SplitN(line, "`", 3)
		if len(parts) != 3 || parts[1] == "" {
			continue
		}
		row := preliveRow{name: parts[1]}
		if i := strings.LastIndex(line, "(since "); i >= 0 {
			rest := line[i+len("(since "):]
			if j := strings.Index(rest, ")"); j >= 0 {
				row.since, _ = time.Parse(preliveTimeFormat, rest[:j])
			}
		}
		out[strings.ToLower(parts[1])] = row
	}
	return out
}

// expiredPrelive returns the names of pre-live streamers whose marker is
// older than ttl (or unreadable), unless they're live in an infosec
// category, in which case the normal diff turns them 🟢.
func expiredPrelive(prelive map[string]preliveRow, live map[string]helix.Stream, now time.Time, ttl time.Duration) []string {
	var out []string
	for key, row := range prelive {
		if st, ok := live[key]; ok && contains(VALID_GAMES, st.GameName) {
			continue
		}
		if row.since.IsZero() || now.Sub(row.since) >= ttl {
			out = append(out, row.name)
		}
	}
	sort.Strings(out)
	return out
}
