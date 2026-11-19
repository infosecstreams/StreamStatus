package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// discordWebhook posts messages to a Discord channel webhook
// (SS_DISCORD_WEBHOOK). It is the bot's only notification channel.
type discordWebhook struct {
	url    string
	client *http.Client
}

func newDiscordWebhook(url string) *discordWebhook {
	return &discordWebhook{url: url, client: &http.Client{Timeout: 10 * time.Second}}
}

// discordMaxContent is Discord's message length limit.
const discordMaxContent = 2000

func (d *discordWebhook) Send(ctx context.Context, subject, message string) error {
	content := fmt.Sprintf("**%s**\n%s", subject, message)
	if r := []rune(content); len(r) > discordMaxContent {
		content = string(r[:discordMaxContent-1]) + "…"
	}
	body, _ := json.Marshal(map[string]any{
		"username": "StreamStatus",
		"content":  content,
		// Never ping anyone from log text.
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("discord webhook: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// alertSender is what alerts go to (the Discord webhook in production).
type alertSender interface {
	Send(ctx context.Context, subject, message string) error
}

// alertHook forwards Error-and-worse log lines to an alertSender, at most
// once per throttle window per distinct message, so a failure that repeats
// every reconcile doesn't flood the channel.
type alertHook struct {
	sender   alertSender
	throttle time.Duration

	mu   sync.Mutex
	last map[string]time.Time
	wg   sync.WaitGroup
}

func newAlertHook(sender alertSender, throttle time.Duration) *alertHook {
	return &alertHook{sender: sender, throttle: throttle, last: map[string]time.Time{}}
}

func (h *alertHook) Levels() []log.Level {
	return []log.Level{log.PanicLevel, log.FatalLevel, log.ErrorLevel}
}

func (h *alertHook) Fire(e *log.Entry) error {
	now := timeNow()
	h.mu.Lock()
	if t, ok := h.last[e.Message]; ok && now.Sub(t) < h.throttle {
		h.mu.Unlock()
		return nil
	}
	h.last[e.Message] = now
	for k, t := range h.last {
		if now.Sub(t) > h.throttle {
			delete(h.last, k)
		}
	}
	h.mu.Unlock()

	subject := "StreamStatus " + e.Level.String()
	send := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := h.sender.Send(ctx, subject, e.Message); err != nil {
			// Plain stderr: logging at Error here would recurse.
			fmt.Printf("alert delivery failed: %s\n", err)
		}
	}
	if e.Level <= log.FatalLevel {
		// The process exits right after a Fatal; deliver first.
		send()
		return nil
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		send()
	}()
	return nil
}

// alert sends an operational message (not an error) to the alert channel,
// if one is configured.
func (s *StreamersRepo) alert(subject, msg string) {
	if s.alerts == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.alerts.Send(ctx, subject, msg); err != nil {
			fmt.Printf("alert delivery failed: %s\n", err)
		}
	}()
}
