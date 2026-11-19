package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicklaw5/helix/v2"
)

func signedHeaders(secret, id string, sent time.Time, body string) http.Header {
	ts := sent.UTC().Format(time.RFC3339Nano)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(id + ts + body))
	h := http.Header{}
	h.Set("Twitch-Eventsub-Message-Id", id)
	h.Set("Twitch-Eventsub-Message-Timestamp", ts)
	h.Set("Twitch-Eventsub-Message-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return h
}

func TestVerifyEventSub(t *testing.T) {
	now := time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC)
	body := []byte(`{"x":1}`)
	if err := verifyEventSub("s3cret", signedHeaders("s3cret", "m1", now.Add(-time.Minute), string(body)), body, now); err != nil {
		t.Errorf("valid message rejected: %v", err)
	}
	cases := map[string]http.Header{
		"wrong secret":    signedHeaders("other", "m1", now, string(body)),
		"replayed (old)":  signedHeaders("s3cret", "m1", now.Add(-11*time.Minute), string(body)),
		"from the future": signedHeaders("s3cret", "m1", now.Add(11*time.Minute), string(body)),
		"no headers":      {},
	}
	for name, h := range cases {
		if err := verifyEventSub("s3cret", h, body, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	tampered := signedHeaders("s3cret", "m1", now, string(body))
	if err := verifyEventSub("s3cret", tampered, []byte(`{"x":2}`), now); err == nil {
		t.Error("tampered body accepted")
	}
	if err := verifyEventSub("", signedHeaders("", "m1", now, string(body)), body, now); err == nil {
		t.Error("empty secret accepted")
	}
}

func TestWebhookRejectsHugeBody(t *testing.T) {
	t.Setenv("SS_SECRETKEY", "s3cret")
	s := &StreamersRepo{jobs: make(chan statusJob, 1), seenMessages: newMessageIDCache(10)}
	req := httptest.NewRequest("POST", "/webhook/callbacks", strings.NewReader(strings.Repeat("x", maxWebhookBody+1)))
	rec := httptest.NewRecorder()
	s.eventsubStatus(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("got %d, want 413", rec.Code)
	}
}

// Events are matched by login; the row keeps the name as written.
func TestEventMatchesByLoginAndKeepsRowName(t *testing.T) {
	s, origin := newGitFixture(t, []string{offlineLine("TheLaluka")}, nil, "TheLaluka,\n", "")
	s.client = &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(helix.Stream{UserLogin: "thelaluka", UserName: "ラルカ", GameName: "Just Chatting", Language: "fr"}), nil
	}}
	job, ok := jobFromNotification(eventSubNotification{
		Subscription: helix.EventSubSubscription{Type: "stream.online"},
		Event:        []byte(`{"broadcaster_user_id":"1","broadcaster_user_login":"thelaluka","broadcaster_user_name":"ラルカ"}`),
	})
	if !ok || job.userName != "thelaluka" {
		t.Fatalf("job = %+v, want matched by login", job)
	}
	s.process(job)
	idx := gitOutput(t, origin, "show", "main:index.md")
	if !strings.Contains(idx, "🟢 | `TheLaluka` |") || !strings.Contains(idx, "twitch.tv/TheLaluka ") {
		t.Errorf("want the row's own name kept:\n%s", idx)
	}
}

// A failed sync must skip the update, not edit stale files or panic.
func TestUpdateSkippedWhenRepoCannotSync(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	os.Chdir(dir)
	t.Cleanup(func() { os.Chdir(old) })
	s := &StreamersRepo{url: "u0/u1/u2/u3/missing", repoPath: "missing", indexFilePath: "missing/index.md",
		inactiveFilePath: "missing/inactive.md", mutex: &sync.Mutex{}, streamer: "alpha", online: true}
	if s.applyStatus("alpha", true, "Just Chatting", "en", nil) {
		t.Error("reported a change although the repo couldn't be synced")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing", "index.md")); err == nil {
		t.Error("wrote files without a synced repo")
	}
}

func TestWorkerSurvivesPanickingJob(t *testing.T) {
	s := &StreamersRepo{mutex: &sync.Mutex{}}
	s.startWorker()
	s.enqueue(statusJob{kind: jobFunc, fn: func() { panic("boom") }})
	ran := false
	if !s.runInWorker(func() { ran = true }) || !ran {
		t.Fatal("worker died after a panicking job")
	}
	// The mutex must have been released by the panicking job.
	if !s.mutex.TryLock() {
		t.Fatal("mutex still held after panic")
	}
	s.mutex.Unlock()
}

func TestSitemapNotEmptiedWhenUnparseable(t *testing.T) {
	s, origin := newGitFixture(t, []string{offlineLine("alpha")}, nil, "alpha,\n", "")
	s.client = notLive()
	// Break the sitemap upstream, then make a real change.
	seed := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(origin))))), "seed")
	os.WriteFile(filepath.Join(seed, "sitemap.xml"), []byte("<urlset></urlset>"), 0644)
	runGit(t, seed, "commit", "-qam", "break sitemap")
	runGit(t, seed, "push", "-q", origin, "main")

	if !s.applyStatus("alpha", true, "Just Chatting", "en", nil) {
		t.Fatal("status change not pushed")
	}
	if got := gitOutput(t, origin, "show", "main:sitemap.xml"); got != "<urlset></urlset>" {
		t.Errorf("sitemap.xml was rewritten to %q", got)
	}
}

func TestReconcileClearsGreenRowMissingFromCSV(t *testing.T) {
	s, origin := newGitFixture(t, []string{onlineLine("Ghost"), offlineLine("alpha")}, nil, "alpha,\n", "")
	s.client = notLive()
	s.reconcile()
	if idx := gitOutput(t, origin, "show", "main:index.md"); !strings.Contains(idx, "&nbsp; | `Ghost`") {
		t.Errorf("🟢 row not in streamers.csv was left green:\n%s", idx)
	}
}
