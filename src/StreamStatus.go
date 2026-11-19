package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	httpauth "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/nicklaw5/helix/v2"

	log "github.com/sirupsen/logrus"
)

var VALID_GAMES = []string{
	"just chatting",
	"science \u0026 technology",
	"software and game development",
	"talk shows \u0026 podcasts",
}

// Currently not using this due to showing gamers as online who leave infosec tags.
// Need to scope the tags down, maybe just ISS related.
var OPTIONAL_TAGS = []string{
	"ctf", "capturetheflag",
	"htb", "hackthebox",
	"thm", "tryhackme",
	"infosec", "cybersecurity", "informationsecurity",
	"hacker", "hacking", "bugbounty", "pentest", "security",
	"reverseengineering", "malware", "malwareanalysis",
	"iss", "infosecstream", "infosecstreams", "infosecstreamer", "infosecstreamers",
}

// StreamersRepo struct represents fields to hold various data while updating status.
type StreamersRepo struct {
	auth             *httpauth.BasicAuth
	activeCsvPath    string
	inactiveCsvPath  string
	inactiveFilePath string
	inactiveMdText   string
	indexFilePath    string
	indexMdText      string
	online           bool
	reactivated      bool
	repo             *git.Repository
	repoPath         string
	streamer         string
	url              string
	language         string
	game             string
	tags             []string
	// prelive marks the streamer 🟡 (offline, but just set an infosec
	// category or title) instead of 🟢/offline; preliveSince is when.
	prelive      bool
	preliveSince time.Time
	// onlyIfPrelive restricts an offline update to rows currently 🟡.
	onlyIfPrelive bool
	// alerts receives operational messages (SS_DISCORD_WEBHOOK); may be nil.
	alerts alertSender
	// subs, callbackURL and secret manage EventSub subscriptions
	// (SS_CALLBACK_URL); subscription management is off when unset.
	subs        subscriptionsClient
	callbackURL string
	secret      string
	client      streamsClient
	mutex       *sync.Mutex

	jobs         chan statusJob
	pending      sync.WaitGroup
	seenMessages *messageIDCache
	// reconcileInactive lets reconciliation reactivate live streamers that
	// are on the inactive list (SS_RECONCILE_INACTIVE).
	reconcileInactive bool
	// lastEvent is when each streamer (lowercased) last had an EventSub
	// event handled; reconciliation leaves them alone for a while after.
	lastEvent map[string]time.Time

	// live is the in-memory live state behind /api/status.json, backed by
	// the stream history database; nil turns it off.
	live *liveState
	// users maps roster logins to Twitch user IDs for live state.
	users usersClient
	// follows fetches daily follower totals for stats.
	follows followersClient
	// statusCommitsOff (SS_COMMIT_STATUS=false) stops committing status
	// changes to the site repo. Roster moves (reactivating an inactive
	// streamer who went live) are still committed.
	statusCommitsOff bool
	// schedule holds Twitch stream schedules (schedule.go); nil disables them.
	schedule *scheduleState
}

// streamsClient is the slice of the Helix API the bot uses; *helix.Client
// satisfies it and tests substitute a fake.
type streamsClient interface {
	GetStreams(params *helix.StreamsParams) (*helix.StreamsResponse, error)
}

// maxWebhookBody caps the size of an EventSub request body.
const maxWebhookBody = 1 << 20

// maxMessageAge is how old (or how far in the future) a signed EventSub
// message may be; older ones are replays. Twitch recommends 10 minutes.
const maxMessageAge = 10 * time.Minute

// verifyEventSub checks an EventSub request's HMAC-SHA256 signature in
// constant time, and rejects stale timestamps so a captured message can't
// be replayed once it has left the message-ID cache.
func verifyEventSub(secret string, h http.Header, body []byte, now time.Time) error {
	id := h.Get("Twitch-Eventsub-Message-Id")
	ts := h.Get("Twitch-Eventsub-Message-Timestamp")
	sig := h.Get("Twitch-Eventsub-Message-Signature")
	if secret == "" || id == "" || ts == "" || !strings.HasPrefix(sig, "sha256=") {
		return errors.New("missing EventSub signature headers")
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
	if err != nil {
		return errors.New("malformed signature")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(id + ts))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return errors.New("invalid signature")
	}
	sent, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return fmt.Errorf("malformed timestamp %q", ts)
	}
	if age := now.Sub(sent); age > maxMessageAge || age < -maxMessageAge {
		return fmt.Errorf("stale message %s (sent %s)", id, ts)
	}
	return nil
}

// gitTimeout bounds every git network operation, so a hung connection to
// GitHub can't wedge the worker (and with it every queued event).
const gitTimeout = 2 * time.Minute

// NoChangeNeededError is a struct for a custom error handler
// when no changes are needed to the git repository.
type NoChangeNeededError struct {
	err string
}

// Error returns a string for the NoChangeNeededError struct.
func (e *NoChangeNeededError) Error() string {
	return e.err
}

// gitPush pushes the repository to github and return and error.
func (s *StreamersRepo) gitPush() error {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	err := s.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		Auth:       s.auth,
	})
	if err != nil {
		return err
	}
	log.Println("remote repo updated.", s.indexFilePath)
	return nil
}

// gitCommit makes a commit to the repository and returns an error.
func (s *StreamersRepo) gitCommit() error {
	w, err := s.repo.Worktree()
	if err != nil {
		return err
	}
	commitMessage := ""
	if s.prelive {
		commitMessage = fmt.Sprintf("🟡 %s is getting ready to stream", s.streamer)
	} else if s.online {
		commitMessage = fmt.Sprintf("🟢 %s has gone online!", s.streamer)
	} else {
		commitMessage = fmt.Sprintf("☠️  %s has gone offline!", s.streamer)
	}
	_, err = w.Commit(commitMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "🤖 STATUSS (Seriously Totally Automated Twitch Updating StreamStatus)",
			Email: "goproslowyo+statuss@users.noreply.github.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		return err
	}
	commit, err := s.getHeadCommit()
	if err != nil {
		return err
	}
	log.Printf("Current HEAD commit: %s", commit)
	return nil
}

// gitAdd adds the index file to the repository and returns an error.
func (s *StreamersRepo) gitAdd() error {
	// Update the sitemap.xml file for Google.
	sitemapPath := filepath.Join(s.repoPath, "sitemap.xml")
	sitemap, err := os.ReadFile(sitemapPath)
	if err != nil {
		log.Errorf("Error reading sitemap.xml: %s", err)
	}
	// Only touch sitemap.xml if it could be read and updated; writing the
	// nil result of a failed parse used to commit an empty sitemap.
	if err == nil {
		if newXML, err := updateSitemapXML(sitemap); err != nil {
			log.Errorf("Error updating sitemap.xml: %s", err)
		} else if err := os.WriteFile(sitemapPath, newXML, 0644); err != nil {
			log.Errorf("Error writing sitemap.xml: %s", err)
		}
	}

	w, err := s.repo.Worktree()
	if err != nil {
		return err
	}
	_, err = w.Add(strings.Split(s.indexFilePath, "/")[1])
	if err != nil {
		log.Errorf("Error adding index.md: %s", err)
		return err
	}
	_, err = w.Add(strings.Split(s.inactiveFilePath, "/")[1])
	if err != nil {
		log.Errorf("Error adding inactive.md: %s", err)
	}
	if s.activeCsvPath != "" {
		_, err = w.Add(strings.Split(s.activeCsvPath, "/")[1])
		if err != nil {
			log.Errorf("Error adding streamers.csv: %s", err)
		}
	}
	if s.inactiveCsvPath != "" {
		_, err = w.Add(strings.Split(s.inactiveCsvPath, "/")[1])
		if err != nil {
			log.Errorf("Error adding inactive_streamers.csv: %s", err)
		}
	}
	_, err = w.Add("sitemap.xml")
	if err != nil {
		log.Errorf("Error adding sitemap.xml: %s", err)
	}
	return nil
}

// getHeadCommit gets the commit at HEAD.
func (s *StreamersRepo) getHeadCommit() (string, error) {
	// Get repo head.
	ref, err := s.repo.Head()
	if err != nil {
		return "", err
	}
	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return "", err
	}
	return commit.String(), nil
}

// getRepo clones a repo to pwd and returns an error.
func (s *StreamersRepo) getRepo() error {
	directory := strings.SplitN(s.url, "/", 5)[4]
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	repo, err := git.PlainCloneContext(ctx, directory, false, &git.CloneOptions{
		// The intended use of a GitHub personal access token is to replace passwords because
		// access tokens can easily be revoked.
		// https://help.github.com/articles/creating-a-personal-access-token-for-the-command-line/
		Auth: s.auth,
		URL:  s.url,
		// We're discarding the stdout out here. If you'd like to see it toggle
		// `Progress` to something like os.Stdout.
		Progress: io.Discard,
	})

	if err == nil {
		s.repo = repo
		return nil
	}
	// Check if the error is that the repo exists and if it is on disk open it.
	errStr := fmt.Sprint(err)
	// Otherwise return error
	if !strings.Contains(errStr, "exists") {
		return err
	}
	repo, err = git.PlainOpen(s.repoPath)
	if err != nil {
		return err
	}
	log.Warn("Syncing existing clone with origin/main")
	// A plain pull is fast-forward-only in go-git, so it can never recover a
	// clone that diverged from origin (e.g. a local commit whose push failed
	// because a PR merged upstream first). Fetch and hard-reset instead: any
	// discarded local commit failed to push anyway, and the status is
	// regenerated from scratch on every event.
	err = repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		Auth:       s.auth,
		Force:      true,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return err
	}
	remoteRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "main"), true)
	if err != nil {
		return err
	}
	w, err := repo.Worktree()
	if err != nil {
		return err
	}
	err = w.Reset(&git.ResetOptions{
		Commit: remoteRef.Hash(),
		Mode:   git.HardReset,
	})
	if err != nil {
		return err
	}
	s.repo = repo
	return nil
}

// writeFile writes given text and returns an error.
func (s *StreamersRepo) writefile(activeText, inactiveText string) error {
	bytesToWrite := []byte(activeText)
	err := os.WriteFile(s.indexFilePath, bytesToWrite, 0644)
	if err != nil {
		return err
	}

	bytesToWrite = []byte(inactiveText)
	err = os.WriteFile(s.inactiveFilePath, bytesToWrite, 0644)
	if err != nil {
		return err
	}
	return nil
}

// updateStreamStatus toggles the streamers status online/offline based on the boolean online.
// this function returns the strings in text replaced or an error.
func (s *StreamersRepo) updateStreamStatus() error {
	streamerFormatted := fmt.Sprintf("`%s`", strings.ToLower(s.streamer))
	s.reactivated = false

	indexMdLines := strings.Split(s.indexMdText, "\n")
	inactiveMdLines := strings.Split(s.inactiveMdText, "\n")
	var streamerFound bool

	for i, v := range indexMdLines {
		if strings.Contains(strings.ToLower(v), streamerFormatted) {
			streamerFound = true
			status := strings.TrimSpace(strings.Split(v, "|")[0])
			// Pre-live never overrides a live row (Helix may simply not
			// list a stream that just started), and a pre-live clear only
			// touches 🟡 rows.
			if (s.prelive && status == "🟢") || (s.onlyIfPrelive && status != "🟡") {
				return &NoChangeNeededError{err: fmt.Sprintf("no change needed for: %s (row is %q)", s.streamer, status)}
			}
			cols := strings.Split(v, "|")
			if len(cols) < 3 {
				return fmt.Errorf("malformed index.md row for %s: %q", s.streamer, v)
			}
			s.streamer = rowName(v, s.streamer)
			newLine := s.generateStreamerLine(cols[2])
			if newLine != v {
				indexMdLines[i] = newLine
			} else {
				err := &NoChangeNeededError{}
				err.err = fmt.Sprintf("no change needed for: %s, online: %v", s.streamer, s.online)
				return err
			}
			break
		}
	}

	if !streamerFound && (s.prelive || s.onlyIfPrelive) {
		// Only live streamers get reactivated from inactive.md.
		return &NoChangeNeededError{err: fmt.Sprintf("no change needed for: %s (not an active streamer)", s.streamer)}
	}
	if !streamerFound {
		// Only someone being shown live comes back from inactive.md; an
		// offline event (or a live stream outside the infosec categories)
		// leaves an inactive streamer where they are.
		if !s.online {
			return &NoChangeNeededError{err: fmt.Sprintf("no change needed for: %s (not an active streamer, online: false)", s.streamer)}
		}
		log.Warnf("streamer not found in index.md, checking inactive.md for %s", s.streamer)
		reactivated := false
		for i, v := range inactiveMdLines {
			if !strings.Contains(strings.ToLower(v), streamerFormatted) {
				continue
			}
			cols := strings.Split(v, "|")
			if len(cols) < 2 {
				return fmt.Errorf("malformed inactive.md row for %s: %q", s.streamer, v)
			}
			// Insert before the blank line that precedes the Credits heading,
			// i.e. after the last streamer row.
			credits := lineIndex(indexMdLines, "Credits")
			if credits < 1 {
				return fmt.Errorf("index.md has no Credits heading; can't place %s", s.streamer)
			}
			end := credits - 1
			s.streamer = rowName(v, s.streamer)
			newLine := s.generateStreamerLine(cols[1])
			inactiveMdLines = append(inactiveMdLines[:i], inactiveMdLines[i+1:]...)
			indexMdLines = append(indexMdLines[:end], append([]string{newLine}, indexMdLines[end:]...)...)
			s.reactivated = true
			reactivated = true
			break
		}
		if !reactivated {
			// Not on the site at all (e.g. an account subscribed to EventSub
			// but removed from the lists, or a renamed login): nothing to do.
			return &NoChangeNeededError{err: fmt.Sprintf("no change needed for: %s (not on the site)", s.streamer)}
		}
	}
	s.indexMdText = strings.Join(indexMdLines, "\n")
	s.inactiveMdText = strings.Join(inactiveMdLines, "\n")

	return nil
}

// rowName returns the streamer name as written in a page row (the first
// backticked word), so rewriting a row keeps its capitalisation even though
// events are matched by lowercase login.
func rowName(row, fallback string) string {
	parts := strings.SplitN(row, "`", 3)
	if len(parts) == 3 && parts[1] != "" {
		return parts[1]
	}
	return fallback
}

func (s *StreamersRepo) generateStreamerLine(otherInfo string) string {
	tw := fmt.Sprintf("[<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/%s", s.streamer)
	// The links column is "<twitch link> &nbsp;<youtube link or nothing>";
	// tolerate a hand-edited row without the separator instead of panicking.
	yt := " "
	if parts := strings.SplitN(otherInfo, "&nbsp;", 2); len(parts) == 2 {
		yt = parts[1]
	}
	tags := strings.Join(s.tags, ", ")
	if s.prelive {
		return fmt.Sprintf("%s | `%s` | %s \"%s%s (since %s)\") &nbsp;%s| %s",
			"🟡",
			s.streamer,
			tw,
			preliveTitlePrefix,
			s.game,
			s.preliveSince.UTC().Format(preliveTimeFormat),
			yt,
			s.language,
		)
	}
	if s.online && s.game != "" {
		return fmt.Sprintf("%s | `%s` | %s \"%s, Tags: %s\") &nbsp;%s| %s",
			"🟢",
			s.streamer,
			tw,
			s.game,
			tags,
			yt,
			s.language,
		)
	} else if s.online {
		return fmt.Sprintf("%s | `%s` | %s \"Tags: %s\") &nbsp;%s| %s",
			"🟢",
			s.streamer,
			tw,
			tags,
			yt,
			s.language,
		)
	}
	return fmt.Sprintf("%s | `%s` | %s) &nbsp;%s",
		"&nbsp;",
		s.streamer,
		tw,
		yt,
	)
}

// readFile reads in a slice of bytes from the provided path and returns a string or an error.
func (s *StreamersRepo) readFile() error {
	// Read index.md
	markdownText, err := os.ReadFile(s.indexFilePath)
	if err != nil {
		return err
	}
	s.indexMdText = string(markdownText)

	// Read inactive.md
	iMarkdownText, err := os.ReadFile(s.inactiveFilePath)
	if err != nil {
		return err
	}
	s.inactiveMdText = string(iMarkdownText)

	return nil
}

// updateMarkdown reads index.md, updates the streamer's status,
// then writes the change back to index.md and returns an error.
func updateMarkdown(repo *StreamersRepo) error {
	// Never edit a worktree that isn't synced with origin: the change would
	// be built on stale files (or, with no clone yet, crash in gitAdd).
	if err := repo.getRepo(); err != nil {
		log.Errorf("error syncing site repo, skipping update for %s: %s", repo.streamer, err)
		return err
	}
	var err error

	err = repo.readFile()
	if err != nil {
		log.Errorf("error reading file: %+s", err)
		return err
	}

	err = repo.updateStreamStatus()
	if err != nil {
		var noChange *NoChangeNeededError
		if !errors.As(err, &noChange) {
			log.Errorf("error updating status: %s", err)
		}
		return err
	}
	if repo.statusCommitsOff && !repo.reactivated {
		return &NoChangeNeededError{err: fmt.Sprintf("status commits off, not committing %s", repo.streamer)}
	}
	err = repo.writefile(repo.indexMdText, repo.inactiveMdText)
	if err != nil {
		log.Printf("error writing file: %s", err)
	}
	if repo.reactivated {
		err = repo.moveCsvEntry()
		if err != nil {
			log.Printf("Warning: Could not move CSV entry: %v", err)
		}
	}
	return nil
}

func (s *StreamersRepo) moveCsvEntry() error {
	if s.activeCsvPath == "" || s.inactiveCsvPath == "" {
		return fmt.Errorf("csv paths not configured")
	}
	inactiveLines, err := readCSVLines(s.inactiveCsvPath)
	if err != nil {
		return err
	}
	activeLines, err := readCSVLines(s.activeCsvPath)
	if err != nil {
		return err
	}

	var movedLine string
	for i, line := range inactiveLines {
		if strings.EqualFold(csvName(line), s.streamer) {
			movedLine = line
			inactiveLines = append(inactiveLines[:i], inactiveLines[i+1:]...)
			break
		}
	}
	if movedLine == "" {
		return fmt.Errorf("streamer not found in inactive_streamers.csv: %s", s.streamer)
	}
	activeLines = append(activeLines, movedLine)
	sortCSVLines(activeLines)
	sortCSVLines(inactiveLines)

	if err := writeCSVLines(s.inactiveCsvPath, inactiveLines); err != nil {
		return err
	}
	if err := writeCSVLines(s.activeCsvPath, activeLines); err != nil {
		return err
	}
	return nil
}

// updateRepo adds and commits the chanages to the repository.
func updateRepo(repo *StreamersRepo) error {
	if err := repo.gitAdd(); err != nil {
		log.Errorf("error staging changes for %s: %s", repo.streamer, err)
		return err
	}
	if err := repo.gitCommit(); err != nil {
		log.Errorf("error making commit for %s: %s", repo.streamer, err)
		return err
	}
	return nil
}

// pushRepo pushes the committed changes to GitHub and reports whether the
// change reached origin. If the push fails (e.g. non-fast-forward because a
// PR merged upstream between our sync and push), it resyncs the clone with
// origin and replays the update once so the event isn't lost.
func pushRepo(repo *StreamersRepo) bool {
	err := repo.gitPush()
	if err == nil {
		return true
	}
	log.Printf("error pushing repo to GitHub: %s -- resyncing with origin and retrying once", err)

	retryErr := updateMarkdown(repo)
	if retryErr != nil {
		var noChange *NoChangeNeededError
		if errors.As(retryErr, &noChange) {
			// After resyncing, origin already reflects the desired state.
			log.Printf("no push needed after resync for %s", repo.streamer)
			return true
		}
		notifyPushError(repo, retryErr)
		return false
	}
	if err := updateRepo(repo); err != nil {
		return false
	}
	if err := repo.gitPush(); err != nil {
		notifyPushError(repo, err)
		return false
	}
	return true
}

// notifyPushError reports a push failure; Error level also alerts Discord.
func notifyPushError(repo *StreamersRepo, err error) {
	log.Errorf("error pushing repo to GitHub for %s: %s", repo.streamer, err)
}

// eventSubNotification is a struct to hold the eventSub webhook request from Twitch.
type eventSubNotification struct {
	Challenge    string                     `json:"challenge"`
	Event        json.RawMessage            `json:"event"`
	Subscription helix.EventSubSubscription `json:"subscription"`
}

// eventsubStatus handles an incoming EventSub webhook. It verifies the
// signature, de-duplicates on the message ID, queues the event for the
// background worker and acknowledges immediately: Twitch counts a delivery
// as failed (and retries it) if the response takes more than a few seconds,
// and the git sync/commit/push for an event takes far longer than that.
func (s *StreamersRepo) eventsubStatus(w http.ResponseWriter, r *http.Request) {
	// EventSub messages are a few KB; don't let anyone make us buffer more
	// than this before the signature is checked.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		log.Warnf("rejecting webhook body: %s", err)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	defer r.Body.Close()

	// Verify that the notification came from twitch using the secret.
	if err := verifyEventSub(os.Getenv("SS_SECRETKEY"), r.Header, body, timeNow()); err != nil {
		log.Warnf("rejecting webhook: %s", err)
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("I am, unfortunately for you, a teapot.\n"))
		return
	}
	log.Println("verified signature on message")

	var vals eventSubNotification
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&vals); err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// If there's a challenge in the request respond with only the challenge to verify the eventsubscription.
	if vals.Challenge != "" {
		// Plain text stops browsers rendering a reflected challenge as HTML.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(vals.Challenge))
		return
	}

	// Twitch re-sends a message (same ID) until it gets a timely 2xx. Ack
	// repeats without doing the work twice.
	msgID := r.Header.Get("Twitch-Eventsub-Message-Id")
	if s.seenMessages.seenOrAdd(msgID) {
		log.Warnf("ignoring duplicate %s message %s", vals.Subscription.Type, msgID)
		w.Write([]byte("OK"))
		return
	}

	// Twitch revoked the subscription (user banned/deleted, or too many
	// failed deliveries): no more events will arrive for it.
	if r.Header.Get("Twitch-Eventsub-Message-Type") == "revocation" {
		log.Errorf("EventSub subscription revoked: type=%s broadcaster=%s status=%s id=%s",
			vals.Subscription.Type, vals.Subscription.Condition.BroadcasterUserID, vals.Subscription.Status, vals.Subscription.ID)
		if s.callbackURL != "" {
			// Recreate it now rather than at the next daily sync.
			s.enqueue(statusJob{kind: jobSyncSubscriptions})
		}
		w.Write([]byte("OK"))
		return
	}

	job, ok := jobFromNotification(vals)
	if !ok {
		log.Errorf("error: event type %s has not been implemented -- pull requests welcome!", vals.Subscription.Type)
		w.Write([]byte("OK"))
		return
	}
	if !s.enqueue(job) {
		// Let Twitch redeliver once the queue has drained.
		s.seenMessages.forget(msgID)
		log.Errorf("event queue full, asking Twitch to retry %s for %s", job.kind, job.userName)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	log.Printf("queued %s event for: %s (%s)", job.kind, job.userName, job.userID)
	w.Write([]byte("OK"))
}

// jobFromNotification converts a verified EventSub notification into a
// worker job. It reports false for subscription types we don't handle.
func jobFromNotification(vals eventSubNotification) (statusJob, bool) {
	switch vals.Subscription.Type {
	case "stream.offline":
		var e helix.EventSubStreamOfflineEvent
		_ = json.Unmarshal(vals.Event, &e)
		return statusJob{kind: jobOffline, userID: e.BroadcasterUserID, userName: loginOr(e.BroadcasterUserLogin, e.BroadcasterUserName),
			userLogin: e.BroadcasterUserLogin, displayName: e.BroadcasterUserName}, true
	case "stream.online":
		var e helix.EventSubStreamOnlineEvent
		_ = json.Unmarshal(vals.Event, &e)
		return statusJob{kind: jobOnline, userID: e.BroadcasterUserID, userName: loginOr(e.BroadcasterUserLogin, e.BroadcasterUserName),
			userLogin: e.BroadcasterUserLogin, displayName: e.BroadcasterUserName}, true
	case "channel.update":
		var e helix.EventSubChannelUpdateEvent
		_ = json.Unmarshal(vals.Event, &e)
		return statusJob{
			kind:        jobChannelUpdate,
			userID:      e.BroadcasterUserID,
			userName:    loginOr(e.BroadcasterUserLogin, e.BroadcasterUserName),
			game:        e.CategoryName,
			language:    e.Language,
			title:       e.Title,
			userLogin:   e.BroadcasterUserLogin,
			displayName: e.BroadcasterUserName,
		}, true
	}
	return statusJob{}, false
}

// loginOr returns the Twitch login, or the display name if an event lacks
// one. Rows are matched by login: display names can be localised (e.g.
// CJK) and then never match the site.
func loginOr(login, name string) string {
	if login != "" {
		return login
	}
	return name
}

// applyStatus writes one streamer's status through the normal
// update/commit/push path. Callers must hold s.mutex (the worker does).
func (s *StreamersRepo) applyStatus(streamer string, online bool, game, language string, tags []string) bool {
	s.prelive, s.onlyIfPrelive = false, false
	s.streamer = streamer
	s.online = online
	s.game = game
	s.language = strings.ToUpper(language)
	s.tags = tags
	if !online {
		s.game = ""
		s.language = ""
		s.tags = nil
	}
	return s.commitAndPush()
}

// commitAndPush applies the prepared update and reports whether a change
// was pushed. "No change needed" is not an error.
func (s *StreamersRepo) commitAndPush() bool {
	if s.statusCommitsOff && !s.mayReactivate() {
		log.Debugf("status commits off, not committing %s", s.streamer)
		return false
	}
	if err := updateMarkdown(s); err != nil {
		var noChange *NoChangeNeededError
		if errors.As(err, &noChange) {
			log.Infof("no change needed for %s", s.streamer)
		}
		return false
	}
	if err := updateRepo(s); err != nil {
		return false
	}
	return pushRepo(s)
}

// fetchStreamInfo returns the live stream for user_id. live is false when
// Helix lists no stream for the user, which right after stream.online often
// just means Helix hasn't caught up yet -- it does not mean offline.
func (s *StreamersRepo) fetchStreamInfo(user_id string) (stream helix.Stream, live bool, err error) {
	log.Infof("trying to get stream info for uid %s", user_id)
	streams, err := s.client.GetStreams(&helix.StreamsParams{UserIDs: []string{user_id}})
	if err != nil {
		return helix.Stream{}, false, err
	}
	if err := helixError(streams.ResponseCommon); err != nil {
		return helix.Stream{}, false, err
	}
	if len(streams.Data.Streams) == 0 {
		return helix.Stream{}, false, nil
	}
	return streams.Data.Streams[0], true, nil
}

// helixError reports a failed Helix response. It goes by the final HTTP
// status: after helix renews an expired token and retries, the response
// still carries the first attempt's 401 in Error/ErrorStatus/ErrorMessage
// even though the retry succeeded.
func helixError(rc helix.ResponseCommon) error {
	if rc.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("helix status=%d %s: %s", rc.StatusCode, rc.Error, rc.ErrorMessage)
}

// applyPrelive marks an active, offline streamer 🟡 "getting ready".
// Callers must hold s.mutex.
func (s *StreamersRepo) applyPrelive(streamer, game, language string, since time.Time) bool {
	s.livePreliveLogin(streamer, game, language, since)
	s.streamer = streamer
	s.online = false
	s.prelive, s.onlyIfPrelive = true, false
	s.preliveSince = since
	s.game = game
	s.language = strings.ToUpper(language)
	s.tags = nil
	defer func() { s.prelive = false }()
	return s.commitAndPush()
}

// mayReactivate reports whether the prepared update could move an inactive
// streamer back to the active list: they are being shown live and are in
// inactive_streamers.csv (as of the last sync).
func (s *StreamersRepo) mayReactivate() bool {
	if !s.online || s.prelive || s.onlyIfPrelive || s.inactiveCsvPath == "" {
		return false
	}
	names, err := csvNames(s.inactiveCsvPath)
	if err != nil {
		// Can't tell; let the normal path (which syncs first) decide.
		return true
	}
	return contains(names, s.streamer)
}

// clearPrelive turns a 🟡 row back to offline; other rows are untouched.
// Callers must hold s.mutex.
func (s *StreamersRepo) clearPrelive(streamer string) bool {
	s.streamer = streamer
	s.online, s.prelive, s.onlyIfPrelive = false, false, true
	s.game, s.language, s.tags = "", "", nil
	defer func() { s.onlyIfPrelive = false }()
	return s.commitAndPush()
}
