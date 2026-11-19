package main

import (
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/nicklaw5/helix/v2"
	log "github.com/sirupsen/logrus"
)

type jobKind string

const (
	jobOnline        jobKind = "stream.online"
	jobOffline       jobKind = "stream.offline"
	jobChannelUpdate jobKind = "channel.update"
	jobReconcile     jobKind = "reconcile"
	// jobSyncSubscriptions makes sure EventSub subscriptions exist.
	jobSyncSubscriptions jobKind = "sync-subscriptions"
	// jobRefreshLive refreshes viewers and titles of live streamers.
	jobRefreshLive jobKind = "refresh-live"
	// jobStats rebuilds the stats documents; jobFollowers fetches a batch
	// of daily follower totals.
	jobStats     jobKind = "stats"
	jobFollowers jobKind = "followers"
	// jobRaid records a channel.raid event; jobMedia fetches a batch of
	// VODs and clips.
	jobRaid  jobKind = "channel.raid"
	jobMedia jobKind = "media"
	// jobFunc runs fn on the worker, e.g. token renewal, which must not
	// race the Helix calls the worker makes.
	jobFunc jobKind = "func"
)

// statusJob is one unit of work for the background worker.
type statusJob struct {
	kind     jobKind
	fn       func()
	userID   string
	userName string
	game     string
	language string
	// title, userLogin and displayName come from the event as sent; they
	// feed live state (userName is what the site rows are matched on).
	title       string
	userLogin   string
	displayName string
	// raid is the channel.raid event (jobRaid).
	raid *helix.EventSubChannelRaidEvent
}

// jobQueueSize bounds how many events can wait behind a slow git push.
const jobQueueSize = 512

// onlineRetryDelays is the backoff between Helix /streams lookups after a
// stream.online event (~60 s in total). Helix often doesn't list a stream
// for a while after Twitch fires stream.online.
var onlineRetryDelays = []time.Duration{
	2 * time.Second, 4 * time.Second, 8 * time.Second,
	15 * time.Second, 15 * time.Second, 15 * time.Second,
}

// sleep and timeNow are swapped out in tests.
var (
	sleep   = time.Sleep
	timeNow = time.Now
)

// noteEvent records that an EventSub event for streamer was just handled.
// Callers must hold s.mutex.
func (s *StreamersRepo) noteEvent(streamer string) {
	if streamer == "" {
		return
	}
	if s.lastEvent == nil {
		s.lastEvent = map[string]time.Time{}
	}
	now := timeNow()
	s.lastEvent[strings.ToLower(streamer)] = now
	for k, t := range s.lastEvent {
		if now.Sub(t) > time.Hour {
			delete(s.lastEvent, k)
		}
	}
}

// startWorker starts the single goroutine that does all git work, in the
// order events arrived, so concurrent webhooks can't race on the worktree
// or on the per-event fields of StreamersRepo.
func (s *StreamersRepo) startWorker() {
	if s.jobs == nil {
		s.jobs = make(chan statusJob, jobQueueSize)
	}
	if s.seenMessages == nil {
		s.seenMessages = newMessageIDCache(2048)
	}
	if s.mutex == nil {
		s.mutex = &sync.Mutex{}
	}
	go func() {
		for job := range s.jobs {
			s.processSafely(job)
			s.pending.Done()
		}
	}()
}

// enqueue hands a job to the worker without blocking. It reports false if
// the worker isn't running or the queue is full.
func (s *StreamersRepo) enqueue(job statusJob) bool {
	if s.jobs == nil {
		return false
	}
	s.pending.Add(1)
	select {
	case s.jobs <- job:
		return true
	default:
		s.pending.Done()
		return false
	}
}

// runInWorker runs fn on the worker goroutine and waits for it. It
// reports false (without running fn) if the queue is full.
func (s *StreamersRepo) runInWorker(fn func()) bool {
	done := make(chan struct{})
	if !s.enqueue(statusJob{kind: jobFunc, fn: func() { defer close(done); fn() }}) {
		return false
	}
	<-done
	return true
}

// waitIdle blocks until every queued job has been processed.
func (s *StreamersRepo) waitIdle() { s.pending.Wait() }

// processSafely runs one job, turning a panic into an error alert so one
// bad event can't take down the process and every queued event with it.
func (s *StreamersRepo) processSafely(job statusJob) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("panic while processing %s for %s: %v\n%s", job.kind, job.userName, r, debug.Stack())
		}
	}()
	s.process(job)
}

func (s *StreamersRepo) process(job statusJob) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	switch job.kind {
	case jobOnline, jobOffline, jobChannelUpdate:
		s.noteEvent(job.userName)
		s.liveEvent(job, timeNow())
	}

	switch job.kind {
	case jobOffline:
		log.Printf("processing offline event for: %s (%s)", job.userName, job.userID)
		s.liveOffline(job.userID)
		s.applyStatus(job.userName, false, "", "", nil)

	case jobOnline:
		log.Printf("processing online event for: %s (%s)", job.userName, job.userID)
		stream, live, err := s.waitForLiveStream(job.userID)
		if err != nil {
			log.Errorf("Error fetching stream info for %s (uid: %s). error: %s", job.userName, job.userID, err)
			return
		}
		if !live {
			// Never mark someone offline because of an online event; if they
			// really are live, the next reconciliation picks them up.
			log.Warnf("%s (%s) not listed by Helix after online event; leaving status unchanged", job.userName, job.userID)
			return
		}
		s.liveOnline(job.userID, stream)
		name := job.userName
		if name == "" {
			name = stream.UserLogin
		}
		// Streamers outside the infosec categories are shown as offline.
		s.applyStatus(name, contains(VALID_GAMES, stream.GameName), stream.GameName, stream.Language, stream.Tags)

	case jobChannelUpdate:
		log.Printf("processing channel update event for: %s (%s)", job.userName, job.userID)
		// channel.update also fires for offline channels (title or category
		// edits), so only touch status if the streamer is actually live.
		stream, live, err := s.fetchStreamInfo(job.userID)
		if err != nil {
			log.Errorf("channel update for %s: fetching stream info failed, leaving status unchanged: %s", job.userName, err)
			return
		}
		if !live {
			// An offline streamer editing their title or category is about
			// to go live: show them 🟡 if it's an infosec category.
			s.livePrelive(job)
			if contains(VALID_GAMES, job.game) {
				log.Infof("channel update for offline channel %s (category %q): marking pre-live", job.userName, job.game)
				s.applyPrelive(job.userName, job.game, job.language, timeNow())
			} else {
				log.Infof("channel update for offline channel %s (category %q): clearing any pre-live marker", job.userName, job.game)
				s.clearPrelive(job.userName)
			}
			return
		}
		// The event's category, title and language are newer than Helix's.
		updated := stream
		updated.GameName, updated.Language = job.game, job.language
		if job.title != "" {
			updated.Title = job.title
		}
		s.liveOnline(job.userID, updated)
		s.applyStatus(job.userName, contains(VALID_GAMES, job.game), job.game, job.language, stream.Tags)

	case jobReconcile:
		s.reconcile()

	case jobSyncSubscriptions:
		s.syncSubscriptions()

	case jobRefreshLive:
		s.refreshLive()

	case jobStats:
		if s.live != nil {
			s.live.importBackfillFile()
			s.live.buildStats(timeNow())
		}

	case jobFollowers:
		s.fetchFollowers()

	case jobRaid:
		if s.live != nil && job.raid != nil {
			r := job.raid
			s.live.recordRaid(raidRecord{at: timeNow(), fromID: r.FromBroadcasterUserID, fromLogin: r.FromBroadcasterUserLogin,
				fromName: r.FromBroadcasterUserName, toID: r.ToBroadcasterUserID, toLogin: r.ToBroadcasterUserLogin,
				toName: r.ToBroadcasterUserName, viewers: r.Viewers, source: "eventsub"})
		}

	case jobMedia:
		s.fetchMedia()

	case jobFunc:
		job.fn()
	}
}

// waitForLiveStream polls Helix for userID's stream, backing off per
// onlineRetryDelays. It returns live=false only after every attempt saw no
// stream, and an error only if the last attempt failed.
func (s *StreamersRepo) waitForLiveStream(userID string) (helix.Stream, bool, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		stream, live, err := s.fetchStreamInfo(userID)
		if err == nil && live {
			return stream, true, nil
		}
		lastErr = err
		if attempt >= len(onlineRetryDelays) {
			break
		}
		if err != nil {
			log.Warnf("attempt %d fetching stream info for uid %s failed: %s", attempt+1, userID, err)
		} else {
			log.Infof("uid %s not listed as live yet (attempt %d), retrying in %s", userID, attempt+1, onlineRetryDelays[attempt])
		}
		sleep(onlineRetryDelays[attempt])
	}
	return helix.Stream{}, false, lastErr
}

// messageIDCache remembers the most recent EventSub message IDs so that
// Twitch's redeliveries of the same message are acked but not reprocessed.
type messageIDCache struct {
	mu    sync.Mutex
	ids   map[string]struct{}
	order []string
	max   int
}

func newMessageIDCache(max int) *messageIDCache {
	return &messageIDCache{ids: make(map[string]struct{}, max), max: max}
}

// seenOrAdd reports whether id was already recorded, recording it if not.
// An empty id is never treated as a duplicate.
func (c *messageIDCache) seenOrAdd(id string) bool {
	if c == nil || id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.ids[id]; ok {
		return true
	}
	c.ids[id] = struct{}{}
	c.order = append(c.order, id)
	if len(c.order) > c.max {
		delete(c.ids, c.order[0])
		c.order = c.order[1:]
	}
	return false
}

// forget drops id so a redelivery of it is processed.
func (c *messageIDCache) forget(id string) {
	if c == nil || id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.ids, id)
	for i, v := range c.order {
		if v == id {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}
