package main

// GitHub App authentication for pushes to the site repo.
//
// With SS_GH_APP_ID, SS_GH_APP_INSTALLATION_ID and SS_GH_APP_PRIVATE_KEY_PATH
// set, the bot signs a short-lived JWT (RS256, stdlib only) with the App's
// private key, exchanges it for an installation access token scoped to the
// site repository with contents:write, and uses that token as the go-git
// basic-auth password (username "x-access-token"). Installation tokens live
// one hour; keepGitTokenFresh swaps in a new one well before that, on the
// worker goroutine, so no git operation ever reads a half-updated password.
//
// Without the App variables the bot falls back to SS_USERNAME/SS_TOKEN
// (a personal access token), as before.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	httpauth "github.com/go-git/go-git/v5/plumbing/transport/http"
	log "github.com/sirupsen/logrus"
)

// githubAPIURL is the GitHub REST API base (a var so tests can fake it).
var githubAPIURL = "https://api.github.com"

const (
	// appTokenUser is the basic-auth username GitHub expects with an
	// installation access token.
	appTokenUser = "x-access-token"
)

const (
	// gitTokenRefreshBefore is how long before expiry a token is replaced.
	gitTokenRefreshBefore = 15 * time.Minute
	// gitTokenRetry is the wait before retrying a failed refresh.
	gitTokenRetry = time.Minute
)

// githubApp mints installation access tokens for one App installation.
type githubApp struct {
	appID          string // numeric App ID or the App's client ID
	installationID string
	key            *rsa.PrivateKey
	repo           string // site repository name the token is limited to
	apiURL         string
	client         *http.Client
	now            func() time.Time
	refreshBefore  time.Duration // replace tokens this long before expiry
	retry          time.Duration // wait after a failed refresh
}

// gitAuthFromEnv builds the credentials for the site repo. repoName is the
// site repository's name (without owner). It returns the App when App auth
// is configured (nil for a PAT) and the expiry of the current token.
func gitAuthFromEnv(getenv func(string) string, repoName string) (*httpauth.BasicAuth, *githubApp, time.Time, error) {
	appID := strings.TrimSpace(getenv("SS_GH_APP_ID"))
	instID := strings.TrimSpace(getenv("SS_GH_APP_INSTALLATION_ID"))
	keyPath := strings.TrimSpace(getenv("SS_GH_APP_PRIVATE_KEY_PATH"))

	if appID == "" && instID == "" && keyPath == "" {
		user, token := getenv("SS_USERNAME"), getenv("SS_TOKEN")
		if user == "" || token == "" {
			return nil, nil, time.Time{}, errors.New("no GitHub credentials: set SS_GH_APP_ID, SS_GH_APP_INSTALLATION_ID and SS_GH_APP_PRIVATE_KEY_PATH (GitHub App), or SS_USERNAME and SS_TOKEN (personal access token)")
		}
		return &httpauth.BasicAuth{Username: user, Password: token}, nil, time.Time{}, nil
	}
	if appID == "" || instID == "" || keyPath == "" {
		return nil, nil, time.Time{}, errors.New("GitHub App auth needs all of SS_GH_APP_ID, SS_GH_APP_INSTALLATION_ID and SS_GH_APP_PRIVATE_KEY_PATH")
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("reading SS_GH_APP_PRIVATE_KEY_PATH: %w", err)
	}
	app, err := newGitHubApp(appID, instID, keyPEM, repoName)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token, expires, err := app.installationToken(ctx)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("minting GitHub App installation token: %w", err)
	}
	return &httpauth.BasicAuth{Username: appTokenUser, Password: token}, app, expires, nil
}

// newGitHubApp validates the App settings and parses its PEM private key
// (PKCS#1 as downloaded from GitHub, or PKCS#8).
func newGitHubApp(appID, installationID string, keyPEM []byte, repoName string) (*githubApp, error) {
	if appID == "" || strings.ContainsAny(appID, " \t\r\n\"") {
		return nil, fmt.Errorf("invalid SS_GH_APP_ID %q", appID)
	}
	if _, err := strconv.ParseUint(installationID, 10, 64); err != nil {
		return nil, fmt.Errorf("invalid SS_GH_APP_INSTALLATION_ID %q: want the numeric installation ID", installationID)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("GitHub App private key: no PEM block found")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("GitHub App private key: %w", err)
		}
		key = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("GitHub App private key: %w", err)
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("GitHub App private key: not an RSA key")
		}
		key = rk
	default:
		return nil, fmt.Errorf("GitHub App private key: unexpected PEM type %q", block.Type)
	}
	return &githubApp{
		appID:          appID,
		installationID: installationID,
		key:            key,
		repo:           repoName,
		apiURL:         githubAPIURL,
		client:         &http.Client{Timeout: 30 * time.Second},
		now:            time.Now,
		refreshBefore:  gitTokenRefreshBefore,
		retry:          gitTokenRetry,
	}, nil
}

// jwt returns an App JWT valid for 9 minutes (GitHub allows at most 10),
// backdated a minute to absorb clock skew.
func (a *githubApp) jwt() (string, error) {
	now := a.now()
	// GitHub accepts the numeric App ID or the client ID as issuer.
	var iss any = a.appID
	if n, err := strconv.ParseUint(a.appID, 10, 64); err == nil {
		iss = n
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": iss,
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + enc.EncodeToString(sig), nil
}

// installationToken mints a new installation access token limited to the
// site repository and contents:write.
func (a *githubApp) installationToken(ctx context.Context) (string, time.Time, error) {
	jwt, err := a.jwt()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("signing App JWT: %w", err)
	}
	reqBody := map[string]any{"permissions": map[string]string{"contents": "write"}}
	if a.repo != "" {
		reqBody["repositories"] = []string{a.repo}
	}
	body, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", strings.TrimRight(a.apiURL, "/"), a.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "StreamStatus/"+version)
	resp, err := a.client.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, err
	}
	if resp.StatusCode != http.StatusCreated {
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &apiErr)
		return "", time.Time{}, fmt.Errorf("GitHub API %s: %s", resp.Status, apiErr.Message)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("decoding installation token: %w", err)
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return "", time.Time{}, errors.New("GitHub API returned no token or expiry")
	}
	return out.Token, out.ExpiresAt, nil
}

// nextRefresh returns how long to wait before replacing a token that
// expires at expires: refreshBefore ahead of expiry, but not sooner than
// retry so a short-lived token can't spin the loop.
func (a *githubApp) nextRefresh(expires time.Time) time.Duration {
	d := expires.Sub(a.now()) - a.refreshBefore
	if d < a.retry {
		d = a.retry
	}
	return d
}

// refreshGitToken mints a new token and, on the worker via run, swaps it
// into auth. It returns the new token's expiry.
func refreshGitToken(ctx context.Context, auth *httpauth.BasicAuth, app *githubApp, run func(func()) bool) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token, expires, err := app.installationToken(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if !run(func() { auth.Password = token }) {
		return time.Time{}, errors.New("worker queue full")
	}
	return expires, nil
}

// keepGitTokenFresh replaces the installation token before it expires,
// until ctx is done. run executes the swap where no git operation can be reading
// auth at the same time (the worker). Failures are logged at Error level,
// which also alerts Discord, and retried; the old token stays in use
// until it expires.
func keepGitTokenFresh(ctx context.Context, auth *httpauth.BasicAuth, app *githubApp, expires time.Time, run func(func()) bool) {
	for {
		wait := app.nextRefresh(expires)
		log.Infof("next GitHub App token refresh in %s", wait.Round(time.Second))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		newExpiry, err := refreshGitToken(ctx, auth, app, run)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Errorf("refreshing GitHub App installation token failed (current token expires %s): %s", expires.Format(time.RFC3339), err)
			// Pretend the deadline is refreshBefore away so the next
			// attempt comes after retry.
			expires = app.now().Add(app.refreshBefore)
			continue
		}
		expires = newExpiry
		log.Info("refreshed GitHub App installation token")
	}
}
