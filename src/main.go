package main

import (
	"net/http"
	"os"
	"strings"
	"sync"

	httpauth "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/nicklaw5/helix/v2"
	"github.com/nikoksr/notify"
	"github.com/nikoksr/notify/service/pushbullet"
	log "github.com/sirupsen/logrus"
)

var version string = "unknown"

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
	iFilePath := repoPath + "/inactive.md"
	var awkPath string
	var found bool
	if awkPath, found = os.LookupEnv("SS_AWK_PATH"); !found {
		awkPath = "/gawk"
	}

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
		ClientID:     os.Getenv("TW_CLIENT_ID"),
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

	// Setup notifications
	if len(os.Getenv("SS_PUSHBULLET_APIKEY")) == 0 || len(os.Getenv("SS_PUSHBULLET_DEVICES")) == 0 {
		log.Fatalln("error: no SS_PUSHBULLET_APIKEY and/or SS_PUSHBULLET_DEVICES specified in environment! https://www.pushbullet.com/#settings/account")
	}
	notifier := notify.New()
	pushbullet := pushbullet.New(os.Getenv("SS_PUSHBULLET_APIKEY"))
	for _, device := range strings.Split(os.Getenv("SS_PUSHBULLET_DEVICES"), ",") {
		pushbullet.AddReceivers(device)
	}
	notifier.UseServices(pushbullet)

	// Create StreamersRepo object
	var repo = StreamersRepo{
		auth:               auth,
		awkPath:            awkPath,
		inactiveFilePath:   iFilePath,
		indexFilePath:      filePath,
		repoPath:           repoPath,
		url:                repoUrl,
		client:             client,
		notificationClient: notifier,
		mutex:              &sync.Mutex{},
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
	log.Printf("streamstatus v%s\n", version)
	log.Printf("server starting on %s\n", port)
	http.HandleFunc("/webhook/callbacks", repo.eventsubStatus)
	log.Fatal(http.ListenAndServe(port, nil))
}
