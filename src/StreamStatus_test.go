package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/nikoksr/notify"
)

// gitOutput runs a git command in dir and returns its output, failing the test on error.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

// runGit runs a git command in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// TestGetRepoRecoversFromDivergedClone reproduces the production wedge:
// a local commit whose push failed (e.g. a site PR merged in between)
// leaves the clone diverged from origin/main, and go-git's fast-forward-only
// Pull can never recover it. getRepo must resync the clone with origin/main.
func TestGetRepoRecoversFromDivergedClone(t *testing.T) {
	base := t.TempDir()
	seed := filepath.Join(base, "seed")
	origin := filepath.Join(base, "origin")

	if err := os.MkdirAll(seed, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "init", "-b", "main")
	runGit(t, seed, "config", "user.email", "test@test")
	runGit(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "index.md"), []byte("v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-m", "c1")
	runGit(t, base, "clone", "--bare", seed, origin)

	// getRepo derives its clone directory from the URL (everything after the
	// 4th slash) relative to the working directory, so run from a sandbox.
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatal(err)
	}
	url := "file://" + origin
	dir := strings.SplitN(url, "/", 5)[4]
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(oldWd) })

	s := &StreamersRepo{
		url:      url,
		repoPath: dir,
		mutex:    &sync.Mutex{},
	}
	if err := s.getRepo(); err != nil {
		t.Fatalf("initial getRepo (clone) failed: %v", err)
	}

	// Simulate a commit whose push failed: the clone diverges from origin.
	w, err := s.repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte("local diverged\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add("index.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("stuck local commit", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	// Upstream advances (simulates a merged PR on the site repo).
	if err := os.WriteFile(filepath.Join(seed, "index.md"), []byte("v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "commit", "-am", "c2")
	runGit(t, seed, "push", origin, "main")

	// Re-running getRepo must resync the clone with origin/main.
	if err := s.getRepo(); err != nil {
		t.Fatalf("getRepo on existing clone failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2\n" {
		t.Errorf("local clone not synced with origin/main after getRepo:\ngot index.md = %q, want %q", got, "v2\n")
	}
}

func TestGenerateStreamerLine(t *testing.T) {
	s := StreamersRepo{
		streamer: "B7H30",
		tags:     []string{"CoLearning", "CoWorking", "Linux", "CyberSecurity", "BackSeatingAllowed", "TryHackMe", "TextToSpeech", "English"},
		game:     "Software and Game Development",
		online:   true,
		language: "EN",
	}
	tests := []struct {
		name       string
		game       string
		otherInfo  string
		wantResult string
		online     bool
	}{
		{
			name:       "online with game",
			otherInfo:  "&nbsp; ",
			game:       "Software and Game Development",
			wantResult: "🟢 | `B7H30` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/B7H30 \"Software and Game Development, Tags: CoLearning, CoWorking, Linux, CyberSecurity, BackSeatingAllowed, TryHackMe, TextToSpeech, English\") &nbsp; | EN",
			online:     true,
		},
		{
			name:       "online without game",
			game:       "",
			otherInfo:  "&nbsp; [<i class=\"fab fa-youtube\" style=\"color:#C00\"></i>](https://www.youtube.com/@theo6580) ",
			wantResult: "🟢 | `B7H30` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/B7H30 \"Tags: CoLearning, CoWorking, Linux, CyberSecurity, BackSeatingAllowed, TryHackMe, TextToSpeech, English\") &nbsp; [<i class=\"fab fa-youtube\" style=\"color:#C00\"></i>](https://www.youtube.com/@theo6580) | EN",
			online:     true,
		},
		{
			name:       "offline",
			game:       "",
			otherInfo:  "&nbsp; [<i class=\"fab fa-youtube\" style=\"color:#C00\"></i>](https://www.youtube.com/@theo6580)",
			wantResult: "&nbsp; | `B7H30` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/B7H30) &nbsp; [<i class=\"fab fa-youtube\" style=\"color:#C00\"></i>](https://www.youtube.com/@theo6580)",
			online:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.online {
				s.online = false
			}
			s.game = tt.game
			if tt.online && tt.game == "" {
				s.online = true
			}
			gotResult := s.generateStreamerLine(tt.otherInfo)
			if gotResult != tt.wantResult {
				t.Errorf("\nGot:    %v\nWanted: %v\n\n", gotResult, tt.wantResult)
			}
		})
	}
}

// TestPushRepoRetriesAfterUpstreamAdvance reproduces the race that starts the
// wedge: an upstream commit (merged PR) lands between this event's sync and
// its push, so the push fails non-fast-forward. pushRepo must resync and
// replay the update so the event is not lost.
func TestPushRepoRetriesAfterUpstreamAdvance(t *testing.T) {
	base := t.TempDir()
	seed := filepath.Join(base, "seed")

	// getRepo derives its clone directory from everything after the URL's 4th
	// slash, and gitAdd assumes that path is exactly "<dir>/<file>". Give the
	// origin a path with exactly four leading segments relative to base so the
	// derived clone directory is a single path segment.
	origin := filepath.Join(base, "u0", "u1", "u2", "u3", "origin")
	if err := os.MkdirAll(filepath.Dir(origin), 0755); err != nil {
		t.Fatal(err)
	}

	offlineLine := "&nbsp; | `teststreamer` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/teststreamer) &nbsp;"
	indexMd := "# Streamers\n\n" + offlineLine + "\n\n## Credits\n"

	if err := os.MkdirAll(seed, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "init", "-b", "main")
	runGit(t, seed, "config", "user.email", "test@test")
	runGit(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "index.md"), []byte(indexMd), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "inactive.md"), []byte("# Inactive\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "sitemap.xml"), []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<urlset xmlns=\"http://www.sitemaps.org/schemas/sitemap/0.9\"><url><loc>https://example.com/</loc><lastmod>2024-01-01T00:00:00+00:00</lastmod></url></urlset>\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-m", "c1")
	runGit(t, base, "clone", "--bare", seed, origin)

	url := "u0/u1/u2/u3/origin"
	dir := strings.SplitN(url, "/", 5)[4] // "origin"
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(oldWd) })

	s := &StreamersRepo{
		url:                url,
		repoPath:           dir,
		indexFilePath:      dir + "/index.md",
		inactiveFilePath:   dir + "/inactive.md",
		streamer:           "teststreamer",
		online:             true,
		language:           "EN",
		game:               "Just Chatting",
		mutex:              &sync.Mutex{},
		notificationClient: notify.New(),
	}

	// Normal event flow: sync, update markdown, commit.
	if err := updateMarkdown(s); err != nil {
		t.Fatalf("updateMarkdown failed: %v", err)
	}
	updateRepo(s)

	// A PR merges upstream before our push: push will fail non-fast-forward.
	if err := os.WriteFile(filepath.Join(seed, "somefile.txt"), []byte("merged pr\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-m", "merged PR")
	runGit(t, seed, "push", origin, "main")

	pushRepo(s)

	// The event must not be lost: origin/main's index.md must show the
	// streamer online, and must contain the merged PR's file too.
	gotIndex := gitOutput(t, origin, "show", "main:index.md")
	if !strings.Contains(gotIndex, "🟢 | `teststreamer`") {
		t.Errorf("origin/main index.md does not show streamer online after pushRepo:\n%s", gotIndex)
	}
	files := gitOutput(t, origin, "ls-tree", "--name-only", "main")
	if !strings.Contains(files, "somefile.txt") {
		t.Errorf("origin/main lost the upstream PR's file:\n%s", files)
	}
}

// signEventSub builds a signed Twitch EventSub webhook request for the handler.
func signEventSub(t *testing.T, secret, msgID, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "/webhook/callbacks", strings.NewReader(body))
	ts := time.Now().Format(time.RFC3339)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(msgID + ts + body))
	req.Header.Set("Twitch-Eventsub-Message-Id", msgID)
	req.Header.Set("Twitch-Eventsub-Message-Timestamp", ts)
	req.Header.Set("Twitch-Eventsub-Message-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("Twitch-Eventsub-Message-Retry", "0")
	return req
}

// TestEventsubStatusSerializesConcurrentEvents delivers two concurrent
// webhook events. The handler writes per-event state (streamer, online,
// game, ...) onto the shared StreamersRepo, so unsynchronized concurrent
// deliveries corrupt each other; run with -race to detect it.
func TestEventsubStatusSerializesConcurrentEvents(t *testing.T) {
	const secret = "testsecret123"
	t.Setenv("SS_SECRETKEY", secret)

	base := t.TempDir()
	seed := filepath.Join(base, "seed")
	origin := filepath.Join(base, "u0", "u1", "u2", "u3", "origin")
	if err := os.MkdirAll(filepath.Dir(origin), 0755); err != nil {
		t.Fatal(err)
	}

	alphaOnline := "🟢 | `alpha` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/alpha \"Tags: infosec\") &nbsp;| EN"
	betaOffline := "&nbsp; | `beta` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/beta) &nbsp;"
	indexMd := "# Streamers\n\n" + alphaOnline + "\n" + betaOffline + "\n\n## Credits\n"

	if err := os.MkdirAll(seed, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "init", "-b", "main")
	runGit(t, seed, "config", "user.email", "test@test")
	runGit(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "index.md"), []byte(indexMd), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "inactive.md"), []byte("# Inactive\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "sitemap.xml"), []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<urlset xmlns=\"http://www.sitemaps.org/schemas/sitemap/0.9\"><url><loc>https://example.com/</loc><lastmod>2024-01-01T00:00:00+00:00</lastmod></url></urlset>\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-m", "c1")
	runGit(t, base, "clone", "--bare", seed, origin)

	url := "u0/u1/u2/u3/origin"
	dir := strings.SplitN(url, "/", 5)[4]
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(oldWd) })

	s := &StreamersRepo{
		url:                url,
		repoPath:           dir,
		indexFilePath:      dir + "/index.md",
		inactiveFilePath:   dir + "/inactive.md",
		mutex:              &sync.Mutex{},
		notificationClient: notify.New(),
	}

	offlineBody := func(streamer string) string {
		return `{"subscription":{"type":"stream.offline"},"event":{"broadcaster_user_name":"` + streamer + `","broadcaster_user_id":"1"}}`
	}

	var wg sync.WaitGroup
	for i, streamer := range []string{"alpha", "beta"} {
		wg.Add(1)
		go func(i int, streamer string) {
			defer wg.Done()
			body := offlineBody(streamer)
			req := signEventSub(t, secret, fmt.Sprintf("msg-%d", i), body)
			s.eventsubStatus(httptest.NewRecorder(), req)
		}(i, streamer)
	}
	wg.Wait()

	// alpha's offline event must have landed on origin/main.
	gotIndex := gitOutput(t, origin, "show", "main:index.md")
	if !strings.Contains(gotIndex, "&nbsp; | `alpha`") {
		t.Errorf("origin/main index.md does not show alpha offline after concurrent events:\n%s", gotIndex)
	}
}
