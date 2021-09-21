package main

import (
	"bytes"
	"encoding/json"
  "errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	httpauth "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/nicklaw5/helix"
)

var VALID_GAMES = []string{ "Science \u0026 Technology", "Software and Game Development", "TryHackMe", "Hack the Box", "Just Chatting" }

// StreamersRepo struct represents fields to hold various data while updating status.
type StreamersRepo struct {
	auth          *httpauth.BasicAuth
	indexFilePath string
	indexMdText   string
	online        bool
	repo          *git.Repository
	repoPath      string
	streamer      string
	url           string
  language      string
  game          string
  client        *helix.Client
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
		commitMessage = fmt.Sprintf("🟢 %s has gone online! [no ci]", s.streamer)
	} else {
		commitMessage = fmt.Sprintf("☠️  %s has gone offline! [no ci]", s.streamer)
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
	log.Println(commit)
	return nil
}

// gitAdd adds the index file to the repository and returns an error.
func (s *StreamersRepo) gitAdd() error {
	w, err := s.repo.Worktree()
	if err != nil {
		return err
	}
	_, err = w.Add(strings.Split(s.indexFilePath, "/")[1])
	if err != nil {
		return err
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
		// The intended use of a GitHub personal access token is in replace of your password
		// because access tokens can easily be revoked.
		// https://help.github.com/articles/creating-a-personal-access-token-for-the-command-line/
		Auth: s.auth,
		URL:  s.url,
		// We're discarding the stdout out here. If you'd like to see it toggle
		// `Progress` to something like os.Stdout.
		Progress: ioutil.Discard,
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
	s.repo = repo
	return nil
}

// writeFile writes given text and returns an error.
func (s *StreamersRepo) writefile(text string) error {
	bytesToWrite := []byte(text)
	return ioutil.WriteFile(s.indexFilePath, bytesToWrite, 0644)
}

// updateStreamStatus toggles the streamers status online/offline based on the boolean online.
// this function returns the strings in text replaced or an error.
func (s *StreamersRepo) updateStreamStatus() error {
	streamerFormatted := fmt.Sprintf("`%s`", s.streamer)

  indexMdLines := strings.Split(s.indexMdText, "\n")
  for i, v := range indexMdLines {
    if strings.Contains(v, streamerFormatted) {
      otherInfo := strings.Split(v, "|")[3]
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

  s.indexMdText = strings.Join(indexMdLines, "\n")
 
	return nil
}

func (s *StreamersRepo) generateStreamerLine(otherInfo string) string {
  var onlineString string
  if s.online {
    onlineString = "🟢"
  } else {
    onlineString = "&nbsp;"
  }
  return fmt.Sprintf("%s | `%s` | [%s](%s) |%s| %s | %s |",
    onlineString,
    s.streamer,
    s.streamer,
    s.url,
    otherInfo,
    s.language,
    s.game,
  )
}

// readFile reads in a slice of bytes from the provided path and returns a string or an error.
func (s *StreamersRepo) readFile() error {
	markdownText, err := os.ReadFile(s.indexFilePath)
	if err != nil {
		return err
	} else {
		s.indexMdText = string(markdownText)
		return nil
	}
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
		log.Printf("error reading file: %+s\n", err)
		os.Exit(-1)
	}

	err = repo.updateStreamStatus()
	if err != nil {
		if fmt.Sprintf("%T", err) == "*main.NoChangeNeededError" {
			return err
		}
		log.Printf("error updating status: %s\n", err)
	}
	err = repo.writefile(repo.indexMdText)
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
	err := repo.gitPush()
	if err != nil {
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
	body, err := ioutil.ReadAll(r.Body)
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
		log.Printf("got offline event for: %s\n", offlineEvent.BroadcasterUserName)
		w.WriteHeader(200)
		w.Write([]byte("ok"))

		s.streamer = offlineEvent.BroadcasterUserName
		s.online = false
    s.language = ""
    s.game = ""
		err := updateMarkdown(s)
		if err == nil {
			updateRepo(s)
			pushRepo(s)
		} else {
			log.Warnf("index.md doesn't need to be changed for %s", s.streamer)
		}
	} else if vals.Subscription.Type == "stream.online" {
		var onlineEvent helix.EventSubStreamOnlineEvent
		_ = json.NewDecoder(bytes.NewReader(vals.Event)).Decode(&onlineEvent)
		log.Printf("got online event for: %s\n", onlineEvent.BroadcasterUserName)
		w.WriteHeader(200)
		w.Write([]byte("ok"))
    
    stream, err := s.fetchStreamInfo(onlineEvent.BroadcasterUserID)
    if err != nil {
      log.Warnf("Error fetching stream info for %s", onlineEvent.BroadcasterUserName)
      return
    }

    s.game = stream.GameName
		s.streamer = onlineEvent.BroadcasterUserName
    // Show streamer as offline if they're not doing infosec
		s.online = contains(VALID_GAMES, s.game)
    s.language = stream.Language

		err = updateMarkdown(s)
		if err == nil {
			updateRepo(s)
			pushRepo(s)
		} else {
			log.Warnf("index.md doesn't need to be changed for %s", s.streamer)
		}
	} else {
		log.Errorf("error: event type %s has not been implemented -- pull requests welcome!", r.Header.Get("Twitch-Eventsub-Subscription-Type"))
	}
}

func (s *StreamersRepo) fetchStreamInfo(user_id string) (*helix.Stream, error) {
  streams, err := s.client.GetStreams(&helix.StreamsParams{
    UserIDs: []string{user_id},
  })
  if err != nil {
    return nil, err
  }
  if streams.ErrorStatus != 0 {
    return nil, errors.New(fmt.Sprintf("Error fetching stream info status=%d %s error=%s", streams.ErrorStatus, streams.Error, streams.ErrorMessage))
  }

  if len(streams.Data.Streams) > 0 {
    return &streams.Data.Streams[0], nil
  }

  return nil, errors.New("No streams returned")
}

func contains(arr []string, item string) bool {
  for _, v := range arr {
    if v == item {
      return true
    }
  }
  return false
}

// main do the work.
func main() {
	// Setup file and repo paths.
	var repoUrl string
	if len(os.Getenv("SS_GH_REPO")) == 0 {
		log.Warn("warning: no SS_GH_REPO specified in environment, defaulting to: https://github.com/infosecstreams/infosecstreams.github.io")
		repoUrl = "https://github.com/infosecstreams/infosecstreams.github.io"
	}
	repoPath := strings.Split(repoUrl, "/")[4]
	filePath := repoPath + "/index.md"

	// Setup auth.
	if len(os.Getenv("SS_USERNAME")) == 0 || len(os.Getenv("SS_TOKEN")) == 0 || len(os.Getenv("SS_SECRETKEY")) == 0 {
		log.Fatalln("error: no SS_USERNAME and/or SS_TOKEN and/or SS_SECRETKEY specified in environment!")
	}
	auth := &httpauth.BasicAuth{
		Username: os.Getenv("SS_USERNAME"),
		Password: os.Getenv("SS_TOKEN"),
	}

  if len(os.Getenv("TW_CLIENT_ID")) == 0 || len(os.Getenv("TW_CLIENT_SECRET")) == 0 {
    log.Fatalln("error: no TW_CLIENT_ID and/or TW_CLIENT_SECRET specified in environment! https://dev.twitch.tv/console/app")
  }

  client, err := helix.NewClient(&helix.Options{
    ClientID: os.Getenv("TW_CLIENT_ID"),
    ClientSecret: os.Getenv("TW_CLIENT_SECRET"),
  })
  if err != nil {
    log.Fatalln(err)
    return
  }

  access_token, err := client.RequestAppAccessToken([]string{})
  if err != nil {
    log.Fatalln(err)
    return
  }
  client.SetAppAccessToken(access_token.Data.AccessToken)

	// Create StreamersRepo object
	var repo = StreamersRepo{
		auth:          auth,
		indexFilePath: filePath,
		repoPath:      repoPath,
		url:           repoUrl,
    client:         client,
	}
	port := ":8080"
	// Google Cloud Run defaults to 8080. Their platform
	// sets the $PORT ENV var if you override it with, e.g.:
	// `gcloud run services update <service-name> --port <port>`.
	if os.Getenv("PORT") != "" {
		port = ":" + os.Getenv("PORT")
	} else if os.Getenv("SS_PORT") != "" {
		port = ":" + os.Getenv("SS_PORT")
	}

	// Listen and serve.
	log.Printf("server starting on %s\n", port)
	http.HandleFunc("/webhook/callbacks", repo.eventsubStatus)
	log.Fatal(http.ListenAndServe(port, nil))
}
