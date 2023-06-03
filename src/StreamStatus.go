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
	inactiveFilePath   string
	inactiveMdText     string
	indexFilePath      string
	indexMdText        string
	online             bool
	repo               *git.Repository
	repoPath           string
	streamer           string
	url                string
	language           string
	game               string
	tags               []string
	notificationClient *notify.Notify
	client             *helix.Client
	mutex              *sync.Mutex
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
	s.mutex.Lock()
	defer s.mutex.Unlock()
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
// The Below code has a race condition. Suggest a fix:
func (s *StreamersRepo) gitCommit() error {
	s.mutex.Lock()
	w, err := s.repo.Worktree()
	s.mutex.Unlock()
	if err != nil {
		return err
	}
	commitMessage := ""
	if s.online {
		commitMessage = fmt.Sprintf("🟢 %s has gone online! [no ci]", s.streamer)
	} else {
		commitMessage = fmt.Sprintf("☠️  %s has gone offline! [no ci]", s.streamer)
	}
	s.mutex.Lock()
	_, err = w.Commit(commitMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "🤖 STATUSS (Seriously Totally Automated Twitch Updating StreamStatus)",
			Email: "goproslowyo+statuss@users.noreply.github.com",
			When:  time.Now(),
		},
	})
	s.mutex.Unlock()
	if err != nil {
		return err
	}
	s.mutex.Lock()
	commit, err := s.getHeadCommit()
	s.mutex.Unlock()
	if err != nil {
		return err
	}
	log.Printf("Current HEAD commit: %s\n", commit)
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

	s.mutex.Lock()
	defer s.mutex.Unlock()
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
		s.mutex.Lock()
		s.repo = repo
		s.mutex.Unlock()
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
	log.Warn("Doing git pull")
	w, err := repo.Worktree()
	if err != nil {
		return err
	}
	w.Pull(&git.PullOptions{
		Force:         true,
		ReferenceName: "HEAD",
		RemoteName:    "origin",
	})
	s.mutex.Lock()
	s.repo = repo
	s.mutex.Unlock()
	return nil
}

// writeFile writes given text and returns an error.
func (s *StreamersRepo) writefile(activeText, inactiveText string) error {
	bytesToWrite := []byte(activeText)
	s.mutex.Lock()
	err := os.WriteFile(s.indexFilePath, bytesToWrite, 0644)
	s.mutex.Unlock()
	if err != nil {
		return err
	}

	bytesToWrite = []byte(inactiveText)
	s.mutex.Lock()
	err = os.WriteFile(s.inactiveFilePath, bytesToWrite, 0644)
	s.mutex.Unlock()
	if err != nil {
		return err
	}
	return nil
}

// updateStreamStatus toggles the streamers status online/offline based on the boolean online.
// this function returns the strings in text replaced or an error.
func (s *StreamersRepo) updateStreamStatus() error {
	streamerFormatted := fmt.Sprintf("`%s`", strings.ToLower(s.streamer))

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
			}
		}
	}
	s.mutex.Lock()
	s.indexMdText = strings.Join(indexMdLines, "\n")
	s.inactiveMdText = strings.Join(inactiveMdLines, "\n")
	s.mutex.Unlock()

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
	s.mutex.Lock()
	markdownText, err := os.ReadFile(s.indexFilePath)
	s.mutex.Unlock()
	if err != nil {
		return err
	}
	s.indexMdText = string(markdownText)

	// Read inactive.md
	s.mutex.Lock()
	iMarkdownText, err := os.ReadFile(s.inactiveFilePath)
	s.mutex.Unlock()
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
		log.Printf("error during repo clone: %s\n", err)
	}

	err = repo.readFile()
	if err != nil {
		log.Fatalf("error reading file: %+s\n", err)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		repo.notificationClient.Send(ctx, "error reading file", err.Error())
	}

	err = repo.updateStreamStatus()
	if err != nil {
		if fmt.Sprintf("%T", err) == "*main.NoChangeNeededError" {
			return err
		}
		log.Printf("error updating status: %s\n", err)
	}
	err = repo.writefile(repo.indexMdText, repo.inactiveMdText)
	if err != nil {
		log.Printf("error writing file: %s\n", err)
	}
	return nil
}

// updateRepo adds and commits the chanages to the repository.
func updateRepo(repo *StreamersRepo) {
	err := repo.gitAdd()
	if err != nil {
		log.Printf("error git adding file: error: %s\n", err)
	}

	err = repo.gitCommit()
	if err != nil {
		log.Printf("error making commit: %s\n", err)
	}
}

// pushRepo pushes the committed changes to GitHub.
func pushRepo(repo *StreamersRepo) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := repo.gitPush()
	if err != nil {
		repo.notificationClient.Send(
			ctx,
			"error pushing repo to GitHub",
			fmt.Sprintf("%s", err),
		)
		log.Printf("error pushing repo to GitHub: %s\n", err)
	}
}

// eventSubNotification is a struct to hold the eventSub webhook request from Twitch.
type eventSubNotification struct {
	Challenge    string                     `json:"challenge"`
	Event        json.RawMessage            `json:"event"`
	Subscription helix.EventSubSubscription `json:"subscription"`
}

// eventsubStatus takes and http Request and ResponseWriter to handle the incoming webhook request.
func (s *StreamersRepo) eventsubStatus(w http.ResponseWriter, r *http.Request) {
	// Read the request body.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Println(err)
		return
	}
	defer r.Body.Close()

	// Verify that the notification came from twitch using the secret.
	if !helix.VerifyEventSubNotification(os.Getenv("SS_SECRETKEY"), r.Header, string(body)) {
		log.Println("invalid signature on message")
		return
	} else {
		log.Println("verified signature on message")
	}

	// Read the request into eventSubNotification struct.
	var vals eventSubNotification
	err = json.NewDecoder(bytes.NewReader(body)).Decode(&vals)
	if err != nil {
		log.Println(err)
		return
	}

	// If there's a challenge in the request respond with only the challenge to verify the eventsubscription.
	if vals.Challenge != "" {
		w.Write([]byte(vals.Challenge))
		return
	}

	if vals.Subscription.Type == "stream.offline" {
		var offlineEvent helix.EventSubStreamOfflineEvent
		_ = json.NewDecoder(bytes.NewReader(vals.Event)).Decode(&offlineEvent)
		// Check the requests headers for Twitch-Eventsub-Message-Type and return 200, OK if it's != 0 and return.
		if r.Header.Get("Twitch-Eventsub-Message-Retry") != "0" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
			log.Warnf("ignoring duplicate event from Twitch for %s", offlineEvent.BroadcasterUserName)
			return
		}
		log.Printf("got offline event for: %s\n", offlineEvent.BroadcasterUserName)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))

		s.streamer = offlineEvent.BroadcasterUserName
		s.online = false
		s.language = ""
		s.game = ""
		err := updateMarkdown(s)
		if err == nil {
			updateRepo(s)
			pushRepo(s)
		} else {
			log.Warnf("Repository doesn't need to be changed for %s", s.streamer)
		}
	} else if vals.Subscription.Type == "stream.online" {
		var onlineEvent helix.EventSubStreamOnlineEvent
		_ = json.NewDecoder(bytes.NewReader(vals.Event)).Decode(&onlineEvent)
		// Check the requests headers for Twitch-Eventsub-Message-Type and return 200, OK if it's != 0 and return.
		if r.Header.Get("Twitch-Eventsub-Message-Retry") != "0" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
			log.Warnf("ignoring duplicate event from Twitch for %s", onlineEvent.BroadcasterUserName)
			return
		}
		log.Printf("got online event for: %s\n", onlineEvent.BroadcasterUserName)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))

		stream, err := s.fetchStreamInfo(onlineEvent.BroadcasterUserID)
		if err != nil {
			errorString := fmt.Sprintf("Error fetching stream info for %s (uid: %s)", onlineEvent.BroadcasterUserName, onlineEvent.BroadcasterUserID)
			log.Error(errorString)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.notificationClient.Send(ctx, errorString, err.Error())
			return
		}

		s.game = stream.GameName
		s.tags = stream.Tags
		s.streamer = onlineEvent.BroadcasterUserName
		// Show streamer as offline if they're not "doing infosec"
		s.online = contains(VALID_GAMES, s.game) || containsTags(OPTIONAL_TAGS, s.tags)
		s.language = strings.ToUpper(stream.Language)

		err = updateMarkdown(s)
		if err == nil {
			updateRepo(s)
			pushRepo(s)
		} else {
			log.Warnf("Repository doesn't need to be changed for %s", s.streamer)
		}
	} else {
		log.Errorf("error: event type %s has not been implemented -- pull requests welcome!", r.Header.Get("Twitch-Eventsub-Subscription-Type"))
	}
}

func (s *StreamersRepo) fetchStreamInfo(user_id string) (*helix.Stream, error) {
	var streams *helix.StreamsResponse
	var err error
	for i := 1; i <= 3; i++ {
		log.Infof("[%d] trying to get stream info for %s", i, user_id)
		streams, err = s.client.GetStreams(
			&helix.StreamsParams{
				UserIDs: []string{user_id},
			})
		if err == nil {
			break
		}
		time.Sleep(time.Duration(i) * time.Millisecond * 1500)
	}
	if streams.ErrorStatus != 0 {
		return nil, fmt.Errorf("error fetching stream info status=%d %s error=%s", streams.ErrorStatus, streams.Error, streams.ErrorMessage)
	}
	if len(streams.Data.Streams) > 0 {
		return &streams.Data.Streams[0], nil
	}

	return nil, fmt.Errorf("no stream returned for uid: %s", user_id)
}
