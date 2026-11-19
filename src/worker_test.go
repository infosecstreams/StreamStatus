package main

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicklaw5/helix/v2"
	log "github.com/sirupsen/logrus"
)

// fakeStreams is a scripted stand-in for the Helix /streams endpoint.
type fakeStreams struct {
	mu    sync.Mutex
	calls []helix.StreamsParams
	fn    func(call int, p *helix.StreamsParams) (*helix.StreamsResponse, error)
}

func (f *fakeStreams) GetStreams(p *helix.StreamsParams) (*helix.StreamsResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, *p)
	n := len(f.calls)
	f.mu.Unlock()
	return f.fn(n, p)
}

func streamsResp(streams ...helix.Stream) *helix.StreamsResponse {
	r := &helix.StreamsResponse{}
	r.StatusCode = 200
	r.Data.Streams = streams
	return r
}

// noSleep records requested backoff delays instead of sleeping.
func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	old := sleep
	sleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { sleep = old })
	return &slept
}

const fixtureSitemap = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<urlset xmlns=\"http://www.sitemaps.org/schemas/sitemap/0.9\"><url><loc>https://example.com/</loc><lastmod>2024-01-01T00:00:00+00:00</lastmod></url></urlset>\n"

func onlineLine(name string) string {
	return "🟢 | `" + name + "` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/" + name + " \"Just Chatting, Tags: infosec\") &nbsp;| EN"
}

func offlineLine(name string) string {
	return "&nbsp; | `" + name + "` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/" + name + ") &nbsp;"
}

// newGitFixture creates a bare "origin" site repo holding index.md,
// inactive.md, streamers.csv and inactive_streamers.csv, chdirs into a
// sandbox, and returns a StreamersRepo pointed at it plus origin's path.
func newGitFixture(t *testing.T, indexLines, inactiveLines []string, activeCsv, inactiveCsv string) (*StreamersRepo, string) {
	t.Helper()
	base := t.TempDir()
	seed := filepath.Join(base, "seed")
	origin := filepath.Join(base, "u0", "u1", "u2", "u3", "origin")
	if err := os.MkdirAll(filepath.Dir(origin), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(seed, 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"index.md":               "# Streamers\n\n" + strings.Join(indexLines, "\n") + "\n\n## Credits\n",
		"inactive.md":            "# Inactive\n\n" + strings.Join(inactiveLines, "\n") + "\n",
		"streamers.csv":          activeCsv,
		"inactive_streamers.csv": inactiveCsv,
		"sitemap.xml":            fixtureSitemap,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, seed, "init", "-b", "main")
	runGit(t, seed, "config", "user.email", "test@test")
	runGit(t, seed, "config", "user.name", "test")
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-m", "c1")
	runGit(t, base, "clone", "--bare", seed, origin)

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(oldWd) })

	url := "u0/u1/u2/u3/origin"
	dir := "origin"
	return &StreamersRepo{
		url:              url,
		repoPath:         dir,
		indexFilePath:    dir + "/index.md",
		inactiveFilePath: dir + "/inactive.md",
		activeCsvPath:    dir + "/streamers.csv",
		inactiveCsvPath:  dir + "/inactive_streamers.csv",
		mutex:            &sync.Mutex{},
	}, origin
}

func onlineBody(name string) string {
	return `{"subscription":{"type":"stream.online"},"event":{"broadcaster_user_name":"` + name + `","broadcaster_user_id":"42"}}`
}

// TestEventsubAcksBeforeGitWork: the handler must answer 200 while the
// worker is still busy, not after the git push.
func TestEventsubAcksBeforeGitWork(t *testing.T) {
	const secret = "testsecret123"
	t.Setenv("SS_SECRETKEY", secret)

	noSleep(t)
	// Helix never lists the stream, so the job ends without touching git.
	s := &StreamersRepo{mutex: &sync.Mutex{}, client: &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(), nil
	}}}
	s.startWorker()
	// Simulate a long-running git push holding the worker.
	s.mutex.Lock()

	done := make(chan *httptest.ResponseRecorder)
	go func() {
		rec := httptest.NewRecorder()
		s.eventsubStatus(rec, signEventSub(t, secret, "msg-1", onlineBody("alpha")))
		done <- rec
	}()
	select {
	case rec := <-done:
		if rec.Code != 200 || rec.Body.String() != "OK" {
			t.Errorf("got %d %q, want 200 OK", rec.Code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not respond while git work was in progress")
	}
	s.mutex.Unlock()
	s.waitIdle()
}

func TestEventsubDedupesByMessageID(t *testing.T) {
	const secret = "testsecret123"
	t.Setenv("SS_SECRETKEY", secret)

	// No worker running: jobs stay in the channel where we can count them.
	s := &StreamersRepo{jobs: make(chan statusJob, 10), seenMessages: newMessageIDCache(10)}
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		s.eventsubStatus(rec, signEventSub(t, secret, "same-id", onlineBody("alpha")))
		if rec.Code != 200 {
			t.Fatalf("delivery %d: got status %d, want 200", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	s.eventsubStatus(rec, signEventSub(t, secret, "other-id", onlineBody("beta")))
	if rec.Code != 200 {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	if got := len(s.jobs); got != 2 {
		t.Fatalf("queued %d jobs, want 2 (one per distinct message ID)", got)
	}
	if j := <-s.jobs; j.kind != jobOnline || j.userName != "alpha" || j.userID != "42" {
		t.Errorf("unexpected first job: %+v", j)
	}
}

func TestEventsubRejectsBadSignature(t *testing.T) {
	t.Setenv("SS_SECRETKEY", "right")
	s := &StreamersRepo{jobs: make(chan statusJob, 10), seenMessages: newMessageIDCache(10)}
	rec := httptest.NewRecorder()
	s.eventsubStatus(rec, signEventSub(t, "wrong", "m", onlineBody("alpha")))
	if rec.Code != 418 || len(s.jobs) != 0 {
		t.Errorf("got status %d with %d jobs, want 418 and none", rec.Code, len(s.jobs))
	}
}

// When the queue is full Twitch must get a non-2xx (so it redelivers) and
// the redelivery must not be treated as a duplicate.
func TestEventsubQueueFullAsksForRetry(t *testing.T) {
	const secret = "testsecret123"
	t.Setenv("SS_SECRETKEY", secret)
	s := &StreamersRepo{jobs: make(chan statusJob, 1), seenMessages: newMessageIDCache(10)}
	s.jobs <- statusJob{kind: jobReconcile}

	rec := httptest.NewRecorder()
	s.eventsubStatus(rec, signEventSub(t, secret, "m1", onlineBody("alpha")))
	if rec.Code != 503 {
		t.Fatalf("got status %d, want 503", rec.Code)
	}
	<-s.jobs
	rec = httptest.NewRecorder()
	s.eventsubStatus(rec, signEventSub(t, secret, "m1", onlineBody("alpha")))
	if rec.Code != 200 || len(s.jobs) != 1 {
		t.Errorf("redelivery: got status %d with %d jobs, want 200 and 1", rec.Code, len(s.jobs))
	}
}

func TestMessageIDCacheEvictsOldest(t *testing.T) {
	c := newMessageIDCache(2)
	for _, id := range []string{"a", "b", "c"} {
		if c.seenOrAdd(id) {
			t.Fatalf("%s reported as seen on first add", id)
		}
	}
	if c.seenOrAdd("a") {
		t.Error("a should have been evicted")
	}
	if !c.seenOrAdd("c") {
		t.Error("c should still be remembered")
	}
	if c.seenOrAdd("") || c.seenOrAdd("") {
		t.Error("empty IDs must never count as duplicates")
	}
}

func TestWaitForLiveStreamRetriesUntilListed(t *testing.T) {
	slept := noSleep(t)
	f := &fakeStreams{fn: func(call int, p *helix.StreamsParams) (*helix.StreamsResponse, error) {
		switch call {
		case 1, 2:
			return streamsResp(), nil // not listed yet
		case 3:
			return nil, errors.New("transient")
		}
		return streamsResp(helix.Stream{UserName: "antisyphon", GameName: "Science & Technology"}), nil
	}}
	s := &StreamersRepo{client: f}
	stream, live, err := s.waitForLiveStream("42")
	if err != nil || !live || stream.UserName != "antisyphon" {
		t.Fatalf("got live=%v err=%v stream=%+v, want the stream", live, err, stream)
	}
	if len(f.calls) != 4 || len(*slept) != 3 {
		t.Errorf("got %d calls and %d sleeps, want 4 and 3", len(f.calls), len(*slept))
	}
	if got := f.calls[0].UserIDs; len(got) != 1 || got[0] != "42" {
		t.Errorf("queried user IDs %v, want [42]", got)
	}
}

func TestWaitForLiveStreamGivesUpAfterAboutAMinute(t *testing.T) {
	slept := noSleep(t)
	f := &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(), nil
	}}
	s := &StreamersRepo{client: f}
	_, live, err := s.waitForLiveStream("42")
	if live || err != nil {
		t.Fatalf("got live=%v err=%v, want not live and no error", live, err)
	}
	var total time.Duration
	for _, d := range *slept {
		total += d
	}
	if total < 50*time.Second || total > 70*time.Second {
		t.Errorf("total backoff %s, want ~60s", total)
	}
	if len(f.calls) != len(onlineRetryDelays)+1 {
		t.Errorf("got %d calls, want %d", len(f.calls), len(onlineRetryDelays)+1)
	}
}

// Reproduces 2026-11-19 17:30 UTC: an online event arrives before Helix
// lists the stream. The bot must not commit "has gone offline".
func TestOnlineEventNotYetListedNeverMarksOffline(t *testing.T) {
	noSleep(t)
	s, origin := newGitFixture(t, []string{onlineLine("antisyphon")}, nil, "antisyphon,\n", "")
	s.client = &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(), nil
	}}
	before := gitOutput(t, origin, "rev-parse", "main")

	s.process(statusJob{kind: jobOnline, userID: "42", userName: "antisyphon"})

	if after := gitOutput(t, origin, "rev-parse", "main"); after != before {
		t.Errorf("origin/main changed after an unlisted online event:\n%s", gitOutput(t, origin, "log", "--oneline", "-3"))
	}
}

func TestOnlineEventMarksOnlineOnceListed(t *testing.T) {
	noSleep(t)
	s, origin := newGitFixture(t, []string{offlineLine("antisyphon")}, nil, "antisyphon,\n", "")
	s.client = &fakeStreams{fn: func(call int, _ *helix.StreamsParams) (*helix.StreamsResponse, error) {
		if call < 3 {
			return streamsResp(), nil
		}
		return streamsResp(helix.Stream{UserLogin: "antisyphon", UserName: "antisyphon", GameName: "Just Chatting", Language: "en"}), nil
	}}

	s.process(statusJob{kind: jobOnline, userID: "42", userName: "antisyphon"})

	if idx := gitOutput(t, origin, "show", "main:index.md"); !strings.Contains(idx, "🟢 | `antisyphon`") {
		t.Errorf("antisyphon not online on origin/main:\n%s", idx)
	}
	if msg := gitOutput(t, origin, "log", "-1", "--format=%s", "main"); !strings.Contains(msg, "gone online") {
		t.Errorf("last commit %q, want an online commit", msg)
	}
}

// Events must be applied in arrival order: an offline that arrives while an
// online is still waiting on Helix must win.
func TestWorkerKeepsEventOrder(t *testing.T) {
	noSleep(t)
	s, origin := newGitFixture(t, []string{offlineLine("alpha")}, nil, "alpha,\n", "")
	s.client = &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(helix.Stream{UserLogin: "alpha", UserName: "alpha", GameName: "Just Chatting"}), nil
	}}
	s.startWorker()
	s.enqueue(statusJob{kind: jobOnline, userID: "1", userName: "alpha"})
	s.enqueue(statusJob{kind: jobOffline, userID: "1", userName: "alpha"})
	s.waitIdle()

	if idx := gitOutput(t, origin, "show", "main:index.md"); !strings.Contains(idx, "&nbsp; | `alpha`") {
		t.Errorf("alpha should end offline:\n%s", idx)
	}
	log := gitOutput(t, origin, "log", "--format=%s", "main")
	if !strings.Contains(log, "alpha has gone online") || !strings.Contains(log, "alpha has gone offline") {
		t.Errorf("want both commits, got:\n%s", log)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if code != 0 {
		fmt.Fprintln(os.Stderr, "note: git fixture tests need the git CLI on PATH")
	}
	os.Exit(code)
}

// channel.update fires for offline channels too (title/category edits);
// that must not mark anyone 🟢.
func TestChannelUpdateWhileOfflineChangesNothing(t *testing.T) {
	s, origin := newGitFixture(t, []string{offlineLine("alpha")}, nil, "alpha,\n", "")
	s.client = &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(), nil
	}}
	before := gitOutput(t, origin, "rev-parse", "main")

	// (An infosec category would mark them 🟡 pre-live instead; see prelive_test.go.)
	s.process(statusJob{kind: jobChannelUpdate, userID: "1", userName: "alpha", game: "Minecraft", language: "en"})

	if gitOutput(t, origin, "rev-parse", "main") != before {
		t.Errorf("offline channel.update changed the site:\n%s", gitOutput(t, origin, "show", "main:index.md"))
	}
}

func TestChannelUpdateWhileLiveUsesEventCategory(t *testing.T) {
	s, origin := newGitFixture(t, []string{offlineLine("alpha")}, nil, "alpha,\n", "")
	s.client = &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		// Helix still shows the old category; the event is newer.
		return streamsResp(helix.Stream{UserLogin: "alpha", UserName: "alpha", GameName: "Minecraft", Tags: []string{"infosec"}}), nil
	}}

	s.process(statusJob{kind: jobChannelUpdate, userID: "1", userName: "alpha", game: "Science & Technology", language: "en"})

	idx := gitOutput(t, origin, "show", "main:index.md")
	if !strings.Contains(idx, "🟢 | `alpha`") || !strings.Contains(idx, "Science & Technology, Tags: infosec") {
		t.Errorf("want alpha online in the event's category:\n%s", idx)
	}
}

// Reconciliation must not undo a fresh event because Helix lags behind it.
func TestReconcileLeavesFreshEventsAlone(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	old := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = old })

	s, origin := newGitFixture(t, []string{offlineLine("alpha"), onlineLine("beta")}, nil, "alpha,\nbeta,\n", "")
	// Helix still lists alpha, who just went offline, and doesn't list beta.
	s.client = &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(helix.Stream{UserLogin: "alpha", UserName: "alpha", GameName: "Just Chatting"}), nil
	}}
	s.noteEvent("Alpha")
	before := gitOutput(t, origin, "rev-parse", "main")

	timeNow = func() time.Time { return now.Add(2 * time.Minute) }
	s.reconcile()
	idx := gitOutput(t, origin, "show", "main:index.md")
	if !strings.Contains(idx, "&nbsp; | `alpha`") {
		t.Errorf("reconcile flipped alpha back online 2m after its offline event:\n%s", idx)
	}
	if !strings.Contains(idx, "&nbsp; | `beta`") {
		t.Errorf("beta (no recent event) should still be corrected:\n%s", idx)
	}
	if gitOutput(t, origin, "rev-parse", "main") == before {
		t.Error("expected beta's correction to be committed")
	}

	timeNow = func() time.Time { return now.Add(6 * time.Minute) }
	s.reconcile()
	if idx := gitOutput(t, origin, "show", "main:index.md"); !strings.Contains(idx, "🟢 | `alpha`") {
		t.Errorf("after the grace period Helix should win:\n%s", idx)
	}
}

// Accounts subscribed to EventSub but not on the site (removed or renamed)
// must not produce a commit, a push or an error alert.
func TestEventForStreamerNotOnSiteChangesNothing(t *testing.T) {
	noSleep(t)
	s, origin := newGitFixture(t, []string{offlineLine("alpha")}, []string{"`dormant` | [x](https://www.twitch.tv/dormant) &nbsp; |"}, "alpha,\n", "dormant,\n")
	s.client = &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(helix.Stream{UserLogin: "ckellerlive", UserName: "CKellerLive", GameName: "Just Chatting"}), nil
	}}
	alerts := &fakeAlerts{}
	h := newAlertHook(alerts, time.Minute)
	log.AddHook(h)
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(log.LevelHooks{}) })
	before := gitOutput(t, origin, "rev-parse", "main")

	s.process(statusJob{kind: jobOnline, userID: "242020938", userName: "CKellerLive"})
	s.process(statusJob{kind: jobOffline, userID: "242020938", userName: "CKellerLive"})
	h.wg.Wait()

	if gitOutput(t, origin, "rev-parse", "main") != before {
		t.Errorf("site changed for an off-list streamer:\n%s", gitOutput(t, origin, "log", "--oneline", "-3", "main"))
	}
	if got := alerts.all(); len(got) != 0 {
		t.Errorf("unexpected alerts: %+v", got)
	}
}

// An offline event for an inactive streamer must not move them to the
// active list (only someone shown live comes back).
func TestOfflineEventDoesNotReactivate(t *testing.T) {
	s, origin := newGitFixture(t, []string{offlineLine("alpha")}, []string{"`dormant` | [x](https://www.twitch.tv/dormant) &nbsp; |"}, "alpha,\n", "dormant,\n")
	before := gitOutput(t, origin, "rev-parse", "main")
	s.process(statusJob{kind: jobOffline, userID: "9", userName: "dormant"})
	if gitOutput(t, origin, "rev-parse", "main") != before {
		t.Errorf("offline event reactivated an inactive streamer:\n%s", gitOutput(t, origin, "show", "main:index.md"))
	}
}

func TestUpdateStreamStatusRejectsMalformedPagesWithoutPanicking(t *testing.T) {
	tests := []struct{ name, index, inactive string }{
		{"index row without columns", "# S\n`alpha` with no table columns\n## Credits\n", ""},
		{"inactive row without columns", "# S\n\n## Credits\n", "`alpha`"},
		{"no Credits heading", "# S\n", "`alpha` | [x](https://www.twitch.tv/alpha) &nbsp; |"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &StreamersRepo{streamer: "alpha", online: true, indexMdText: tt.index, inactiveMdText: tt.inactive}
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panicked: %v", r)
					}
				}()
				err = s.updateStreamStatus()
			}()
			if err == nil {
				t.Error("want an error for a malformed page")
			}
		})
	}
}
