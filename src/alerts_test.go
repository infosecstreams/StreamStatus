package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

type recordedAlert struct{ subject, message string }

type fakeAlerts struct {
	mu   sync.Mutex
	sent []recordedAlert
}

func (f *fakeAlerts) Send(_ context.Context, subject, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, recordedAlert{subject, message})
	return nil
}

func (f *fakeAlerts) all() []recordedAlert {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedAlert(nil), f.sent...)
}

func TestDiscordWebhookPostsMessage(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	long := strings.Repeat("x", 3000)
	if err := newDiscordWebhook(srv.URL).Send(context.Background(), "Subject @everyone", long); err != nil {
		t.Fatal(err)
	}
	content, _ := got["content"].(string)
	if !strings.HasPrefix(content, "**Subject @everyone**\nxxx") || len([]rune(content)) != discordMaxContent {
		t.Errorf("content %q... (%d runes), want subject + truncated body of %d runes", content[:30], len([]rune(content)), discordMaxContent)
	}
	am, _ := got["allowed_mentions"].(map[string]any)
	if parse, ok := am["parse"].([]any); !ok || len(parse) != 0 {
		t.Errorf("allowed_mentions = %v, want mentions disabled", got["allowed_mentions"])
	}
}

func TestDiscordWebhookReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message": "Unknown Webhook"}`))
	}))
	defer srv.Close()
	if err := newDiscordWebhook(srv.URL).Send(context.Background(), "s", "m"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("got %v, want a 404 error", err)
	}
}

func TestAlertHookThrottlesRepeats(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	old := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = old })

	f := &fakeAlerts{}
	h := newAlertHook(f, 30*time.Minute)
	logger := log.New()
	logger.SetOutput(io.Discard)
	logger.AddHook(h)

	logger.Error("push failed")
	logger.Error("push failed")
	logger.Error("something else")
	logger.Warn("just a warning")
	timeNow = func() time.Time { return now.Add(31 * time.Minute) }
	logger.Error("push failed")
	h.wg.Wait()

	got := f.all()
	if len(got) != 3 {
		t.Fatalf("got %d alerts %+v, want 3 (repeat throttled, warning ignored)", len(got), got)
	}
	for _, a := range got {
		if a.subject != "StreamStatus error" {
			t.Errorf("subject %q, want %q", a.subject, "StreamStatus error")
		}
	}
}

func TestRevocationIsAlertedNotQueued(t *testing.T) {
	const secret = "testsecret123"
	t.Setenv("SS_SECRETKEY", secret)
	f := &fakeAlerts{}
	h := newAlertHook(f, time.Minute)
	log.AddHook(h)
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(log.LevelHooks{}) })

	s := &StreamersRepo{jobs: make(chan statusJob, 10), seenMessages: newMessageIDCache(10)}
	body := `{"subscription":{"id":"sub1","type":"stream.offline","status":"notification_failures_exceeded","condition":{"broadcaster_user_id":"42"}}}`
	req := signEventSub(t, secret, "rev-1", body)
	req.Header.Set("Twitch-Eventsub-Message-Type", "revocation")
	rec := httptest.NewRecorder()
	s.eventsubStatus(rec, req)
	h.wg.Wait()

	if rec.Code != 200 || len(s.jobs) != 0 {
		t.Errorf("got status %d with %d jobs, want 200 and none", rec.Code, len(s.jobs))
	}
	got := f.all()
	if len(got) != 1 || !strings.Contains(got[0].message, "notification_failures_exceeded") || !strings.Contains(got[0].message, "42") {
		t.Errorf("alerts %+v, want one naming the status and broadcaster", got)
	}
}
