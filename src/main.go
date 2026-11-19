package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	httpauth "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/nikoksr/notify"
	"github.com/nikoksr/notify/service/pushbullet"
	log "github.com/sirupsen/logrus"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "unknown"

// main do the work.
func main() {
	buildInfo, _ := debug.ReadBuildInfo()
	log.Printf("started streamstatus %s", version)
	fmt.Printf("%s\n", buildInfo.String())

	// Setup file and repo paths.
	repoUrl, repoPath, err := repoFromEnv(os.Getenv("SS_GH_REPO"))
	if err != nil {
		log.Fatalf("error: %s", err)
	}
	log.Infof("site repository: %s", repoUrl)
	filePath := repoPath + "/index.md"
	iFilePath := repoPath + "/inactive.md"
	activeCsvPath := repoPath + "/streamers.csv"
	inactiveCsvPath := repoPath + "/inactive_streamers.csv"

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

	client, tokenTTL, err := newTwitchClient(os.Getenv("TW_CLIENT_ID"), os.Getenv("TW_CLIENT_SECRET"))
	if err != nil {
		log.Fatalf("error: could not get a Twitch app access token: %s", err)
	}

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

	go keepAppTokenFresh(client, tokenTTL, func(err error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		notifier.Send(ctx, "Twitch app token renewal failed", err.Error())
	})

	// Create StreamersRepo object
	var repo = StreamersRepo{
		auth:               auth,
		activeCsvPath:      activeCsvPath,
		inactiveCsvPath:    inactiveCsvPath,
		inactiveFilePath:   iFilePath,
		indexFilePath:      filePath,
		repoPath:           repoPath,
		url:                repoUrl,
		client:             client,
		notificationClient: notifier,
		mutex:              &sync.Mutex{},
		reconcileInactive:  os.Getenv("SS_RECONCILE_INACTIVE") == "true",
	}

	reconcileInterval := defaultReconcileInterval
	if v := os.Getenv("SS_RECONCILE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			log.Fatalf("error: invalid SS_RECONCILE_INTERVAL %q (want a duration >= 1m, e.g. 5m)", v)
		}
		reconcileInterval = d
	}
	repo.startWorker()
	repo.startReconciler(reconcileInterval)
	log.Printf("reconciling live status every %s (inactive streamers: %v)", reconcileInterval, repo.reconcileInactive)
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
	log.Printf("server starting on %s", port)
	http.HandleFunc("/webhook/callbacks", repo.eventsubStatus)
	log.Fatal(http.ListenAndServe(port, nil))
}
