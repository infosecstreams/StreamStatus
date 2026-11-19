package main

import (
	"context"
	"fmt"
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
)

// statusJob is one unit of work for the background worker.
type statusJob struct {
	kind     jobKind
	userID   string
	userName string
	game     string
	language string
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

// sleep is swapped out in tests.
var sleep = time.Sleep

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
			s.process(job)
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

// waitIdle blocks until every queued job has been processed.
func (s *StreamersRepo) waitIdle() { s.pending.Wait() }

func (s *StreamersRepo) process(job statusJob) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	switch job.kind {
	case jobOffline:
		log.Printf("processing offline event for: %s (%s)", job.userName, job.userID)
		s.applyStatus(job.userName, false, "", "", nil)

	case jobOnline:
		log.Printf("processing online event for: %s (%s)", job.userName, job.userID)
		stream, live, err := s.waitForLiveStream(job.userID)
		if err != nil {
			msg := fmt.Sprintf("Error fetching stream info for %s (uid: %s). error: %s", job.userName, job.userID, err)
			log.Error(msg)
			s.notify("failed to get stream info", msg)
			return
		}
		if !live {
			// Never mark someone offline because of an online event; if they
			// really are live, the next reconciliation picks them up.
			log.Warnf("%s (%s) not listed by Helix after online event; leaving status unchanged", job.userName, job.userID)
			return
		}
		name := job.userName
		if name == "" {
			name = stream.UserName
		}
		// Streamers outside the infosec categories are shown as offline.
		s.applyStatus(name, contains(VALID_GAMES, stream.GameName), stream.GameName, stream.Language, stream.Tags)

	case jobChannelUpdate:
		log.Printf("processing channel update event for: %s (%s)", job.userName, job.userID)
		s.applyStatus(job.userName, contains(VALID_GAMES, job.game), job.game, job.language, nil)

	case jobReconcile:
		s.reconcile()
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

func (s *StreamersRepo) notify(subject, msg string) {
	if s.notificationClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.notificationClient.Send(ctx, subject, msg)
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
