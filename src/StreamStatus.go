package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/nikoksr/notify"

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
	auth               *httpauth.BasicAuth
	activeCsvPath      string
	inactiveCsvPath    string
	inactiveFilePath   string
	inactiveMdText     string
	indexFilePath      string
	indexMdText        string
	online             bool
	reactivated        bool
	repo               *git.Repository
	repoPath           string
	streamer           string
	url                string
	language           string
	game               string
	tags               []string
	notificationClient *notify.Notify
	client             streamsClient
	mutex              *sync.Mutex

	jobs         chan statusJob
	pending      sync.WaitGroup
	seenMessages *messageIDCache
	// reconcileInactive lets reconciliation reactivate live streamers that
	// are on the inactive list (SS_RECONCILE_INACTIVE).
	reconcileInactive bool
}

// streamsClient is the slice of the Helix API the bot uses; *helix.Client
// satisfies it and tests substitute a fake.
type streamsClient interface {
	GetStreams(params *helix.StreamsParams) (*helix.StreamsResponse, error)
}

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
	err := s.repo.Push(&git.PushOptions{
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
	if s.online {
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
	newXML, err := updateSitemapXML(sitemap)
	if err != nil {
		log.Errorf("Error updating sitemap.xml: %s", err)
	}
	err = os.WriteFile(sitemapPath, newXML, 0644)
	if err != nil {
		log.Errorf("Error writing sitemap.xml: %s", err)
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
	repo, err := git.PlainClone(directory, false, &git.CloneOptions{
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
	err = repo.Fetch(&git.FetchOptions{
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
			otherInfo := strings.Split(v, "|")[2]
			newLine := s.generateStreamerLine(otherInfo)
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

	if !streamerFound {
		log.Warnf("streamer not found in index.md, checking inactive.md for %s", s.streamer)
		for i, v := range inactiveMdLines {
			if strings.Contains(strings.ToLower(v), streamerFormatted) {
				// Remove the streamer from the inactive list.
				inactiveMdLines = append(inactiveMdLines[:i], inactiveMdLines[i+1:]...)
				// Append to indexMdLines after the last streamer, not last line.
				otherInfo := strings.Split(v, "|")[1]
				newLine := s.generateStreamerLine(otherInfo)
				// Find the line before 'Credits' in the indexMdLines.
				end := lineIndex(indexMdLines, "Credits") - 1
				// Insert newLine before 'end' in the indexMdLines.
				indexMdLines = append(indexMdLines[:end], append([]string{newLine}, indexMdLines[end:]...)...)
				s.reactivated = true
				break
			}
		}
	}
	s.indexMdText = strings.Join(indexMdLines, "\n")
	s.inactiveMdText = strings.Join(inactiveMdLines, "\n")

	return nil
}

func (s *StreamersRepo) generateStreamerLine(otherInfo string) string {
	tw := fmt.Sprintf("[<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/%s", s.streamer)
	yt := strings.Split(otherInfo, "&nbsp;")[1]
	tags := strings.Join(s.tags, ", ")
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
	err := repo.getRepo()
	if err != nil {
		log.Printf("error during repo clone: %s", err)
	}

	err = repo.readFile()
	if err != nil {
		log.Errorf("error reading file: %+s", err)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		repo.notificationClient.Send(ctx, "error reading file", err.Error())
		return err
	}

	err = repo.updateStreamStatus()
	if err != nil {
		if fmt.Sprintf("%T", err) == "*main.NoChangeNeededError" {
			return err
		}
		log.Printf("error updating status: %s", err)
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
func updateRepo(repo *StreamersRepo) {
	err := repo.gitAdd()
	if err != nil {
		log.Printf("error git adding file: error: %s", err)
	}

	err = repo.gitCommit()
	if err != nil {
		log.Printf("error making commit: %s", err)
	}
}

// pushRepo pushes the committed changes to GitHub. If the push fails (e.g.
// non-fast-forward because a PR merged upstream between our sync and push),
// it resyncs the clone with origin and replays the update once so the event
// isn't lost.
func pushRepo(repo *StreamersRepo) {
	err := repo.gitPush()
	if err == nil {
		return
	}
	log.Printf("error pushing repo to GitHub: %s -- resyncing with origin and retrying once", err)

	retryErr := updateMarkdown(repo)
	if retryErr != nil {
		if _, ok := retryErr.(*NoChangeNeededError); ok {
			// After resyncing, origin already reflects the desired state.
			log.Printf("no push needed after resync for %s", repo.streamer)
			return
		}
		notifyPushError(repo, retryErr)
		return
	}
	updateRepo(repo)
	if err := repo.gitPush(); err != nil {
		notifyPushError(repo, err)
	}
}

// notifyPushError reports a push failure via the notification client and log.
func notifyPushError(repo *StreamersRepo, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repo.notificationClient.Send(
		ctx,
		"error pushing repo to GitHub",
		fmt.Sprintf("%s", err),
	)
	log.Printf("error pushing repo to GitHub: %s", err)
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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Verify that the notification came from twitch using the secret.
	if !helix.VerifyEventSubNotification(os.Getenv("SS_SECRETKEY"), r.Header, string(body)) {
		log.Println("invalid signature on message")
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
		return statusJob{kind: jobOffline, userID: e.BroadcasterUserID, userName: e.BroadcasterUserName}, true
	case "stream.online":
		var e helix.EventSubStreamOnlineEvent
		_ = json.Unmarshal(vals.Event, &e)
		return statusJob{kind: jobOnline, userID: e.BroadcasterUserID, userName: e.BroadcasterUserName}, true
	case "channel.update":
		var e helix.EventSubChannelUpdateEvent
		_ = json.Unmarshal(vals.Event, &e)
		return statusJob{
			kind:     jobChannelUpdate,
			userID:   e.BroadcasterUserID,
			userName: e.BroadcasterUserName,
			game:     e.CategoryName,
			language: e.Language,
		}, true
	}
	return statusJob{}, false
}

// applyStatus writes one streamer's status through the normal
// update/commit/push path. Callers must hold s.mutex (the worker does).
func (s *StreamersRepo) applyStatus(streamer string, online bool, game, language string, tags []string) {
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
	if err := updateMarkdown(s); err != nil {
		log.Warnf("Repository doesn't need to be changed for %s", s.streamer)
		return
	}
	updateRepo(s)
	pushRepo(s)
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
	if streams.ErrorStatus != 0 {
		return helix.Stream{}, false, fmt.Errorf("error fetching stream info status=%d %s error=%s", streams.ErrorStatus, streams.Error, streams.ErrorMessage)
	}
	if len(streams.Data.Streams) == 0 {
		return helix.Stream{}, false, nil
	}
	return streams.Data.Streams[0], true, nil
}
