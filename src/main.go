package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"sync"
	"time"

	httpauth "github.com/go-git/go-git/v5/plumbing/transport/http"
	log "github.com/sirupsen/logrus"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "unknown"

// main do the work.
func main() {
	// Alerts first, so even startup failures reach Discord.
	var alerts alertSender
	if hook := os.Getenv("SS_DISCORD_WEBHOOK"); hook != "" {
		alerts = newDiscordWebhook(hook)
		log.AddHook(newAlertHook(alerts, 30*time.Minute))
	} else {
		log.Warn("no SS_DISCORD_WEBHOOK specified in environment, notifications disabled (errors are only logged)")
	}

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

	// Create StreamersRepo object
	var repo = StreamersRepo{
		auth:              auth,
		activeCsvPath:     activeCsvPath,
		inactiveCsvPath:   inactiveCsvPath,
		inactiveFilePath:  iFilePath,
		indexFilePath:     filePath,
		repoPath:          repoPath,
		url:               repoUrl,
		client:            client,
		alerts:            alerts,
		subs:              client,
		callbackURL:       os.Getenv("SS_CALLBACK_URL"),
		secret:            os.Getenv("SS_SECRETKEY"),
		mutex:             &sync.Mutex{},
		reconcileInactive: os.Getenv("SS_RECONCILE_INACTIVE") == "true",
	}

	reconcileInterval := defaultReconcileInterval
	if v := os.Getenv("SS_RECONCILE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			log.Fatalf("error: invalid SS_RECONCILE_INTERVAL %q (want a duration >= 1m, e.g. 5m)", v)
		}
		reconcileInterval = d
	}
	repo.alert("StreamStatus started", fmt.Sprintf("version %s, reconciling every %s", version, reconcileInterval))
	repo.startWorker()
	go keepAppTokenFresh(client, tokenTTL, repo.runInWorker)
	repo.startReconciler(reconcileInterval)
	log.Printf("reconciling live status every %s (inactive streamers: %v)", reconcileInterval, repo.reconcileInactive)
	// --- bot-schedule: schedule polling and /api/schedule.{json,ics}. At
	// merge, mount scheduleHandlers() on the shared /api/ mux instead of
	// http.DefaultServeMux. ---
	repo.schedule = newScheduleState(client)
	if err := repo.schedule.adherence.loadHistory(scheduleHistoryPath()); err != nil {
		log.Warnf("schedule track record: %s", err)
	}
	if err := repo.schedule.history.load(streamHistoryPath()); err != nil {
		log.Warnf("stream history: %s", err)
	}
	repo.startStreamHistory()
	repo.startScheduleRefresher(scheduleRefreshInterval)
	for path, h := range repo.scheduleHandlers() {
		http.HandleFunc(path, h)
	}
	// --- end bot-schedule ---
	port := ":8080"
	// Google Cloud Run defaults to 8080. Their platform
	// sets the $PORT ENV var if you override it with, e.g.:
	// `gcloud run services update <service-name> --port <port>`.
	if os.Getenv("PORT") != "" {
		port = ":" + os.Getenv("PORT")
	} else if os.Getenv("SS_PORT") != "" {
		port = ":" + os.Getenv("SS_PORT")
	}

	// Listen before creating subscriptions: Twitch verifies each new one
	// by calling the callback straight away.
	http.HandleFunc("/webhook/callbacks", repo.eventsubStatus)
	ln, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("error: listening on %s: %s", port, err)
	}
	log.Printf("server starting on %s", port)
	if repo.callbackURL != "" {
		log.Printf("managing EventSub subscriptions for %s", repo.callbackURL)
		repo.startSubscriptionSync()
	} else {
		log.Warn("no SS_CALLBACK_URL specified in environment, not managing EventSub subscriptions")
	}
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Fatal(srv.Serve(ln))
}
