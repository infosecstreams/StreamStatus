package main

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nicklaw5/helix/v2"
	log "github.com/sirupsen/logrus"
)

// App access tokens come from the client-credentials flow: Twitch never
// issues a refresh token for them, you just request a new token whenever you
// like. They last ~60 days. Two layers keep ours valid:
//
//   - helix itself requests a new app token and retries once when a call
//     gets a 401, but only if an app token was set to begin with, which is
//     why startup fails hard if the first request doesn't produce one.
//   - keepAppTokenFresh renews it proactively before it expires, so a
//     renewal failure shows up (and gets retried) before it breaks events.

// twitchHTTPClient is the HTTP client for Twitch; tests point it at a fake.
var twitchHTTPClient helix.HTTPClient = &http.Client{Timeout: 30 * time.Second}

// newTwitchClient builds a Helix client holding a valid app access token.
// It returns the token's lifetime.
func newTwitchClient(clientID, clientSecret string) (*helix.Client, time.Duration, error) {
	client, err := helix.NewClient(&helix.Options{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		HTTPClient:   twitchHTTPClient,
	})
	if err != nil {
		return nil, 0, err
	}
	ttl, err := renewAppToken(client)
	if err != nil {
		return nil, 0, err
	}
	return client, ttl, nil
}

// renewAppToken requests a new app access token, installs it on client and
// returns its lifetime.
func renewAppToken(client *helix.Client) (time.Duration, error) {
	resp, err := client.RequestAppAccessToken([]string{})
	if err != nil {
		return 0, fmt.Errorf("requesting app access token: %w", err)
	}
	if resp.StatusCode != http.StatusOK || resp.Data.AccessToken == "" {
		return 0, fmt.Errorf("requesting app access token: status=%d %s %s", resp.StatusCode, resp.Error, resp.ErrorMessage)
	}
	client.SetAppAccessToken(resp.Data.AccessToken)
	ttl := time.Duration(resp.Data.ExpiresIn) * time.Second
	if ttl <= 0 {
		return 0, errors.New("requesting app access token: no expires_in in response")
	}
	return ttl, nil
}

// tokenRenewRetry is the wait before retrying a failed proactive renewal.
var tokenRenewRetry = 5 * time.Minute

// nextTokenRenewal returns how long to wait before renewing a token with
// the given lifetime: at 90% of it, but at least a minute.
func nextTokenRenewal(ttl time.Duration) time.Duration {
	d := ttl * 9 / 10
	if d < time.Minute {
		d = time.Minute
	}
	return d
}

// keepAppTokenFresh renews the app token before it expires, forever. run
// executes the renewal where it can't race other Helix calls (the worker);
// it reports false if it couldn't. A failure is logged at Error level,
// which also alerts Discord.
func keepAppTokenFresh(client *helix.Client, ttl time.Duration, run func(func()) bool) {
	wait := nextTokenRenewal(ttl)
	for {
		log.Infof("next Twitch app token renewal in %s", wait.Round(time.Minute))
		time.Sleep(wait)
		var newTTL time.Duration
		var err error
		if !run(func() { newTTL, err = renewAppToken(client) }) {
			err = errors.New("worker queue full")
		}
		if err != nil {
			log.Errorf("renewing Twitch app token failed, retrying in %s: %s", tokenRenewRetry, err)
			wait = tokenRenewRetry
			continue
		}
		log.Info("renewed Twitch app token")
		wait = nextTokenRenewal(newTTL)
	}
}
