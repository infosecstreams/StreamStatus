package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	httpauth "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// fakeGitHubApps serves POST /app/installations/<id>/access_tokens. It
// checks the JWT against the App's public key and hands out token-1,
// token-2, ... each valid for ttl.
type fakeGitHubApps struct {
	t       *testing.T
	pub     *rsa.PublicKey
	appID   string
	instID  string
	ttl     time.Duration
	status  int // non-zero: fail with this status
	mu      sync.Mutex
	minted  int
	failed  int
	lastReq map[string]any
}

func (f *fakeGitHubApps) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/app/installations/"+f.instID+"/access_tokens" {
		http.NotFound(w, r)
		return
	}
	if err := f.checkJWT(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); err != nil {
		f.t.Errorf("bad JWT: %s", err)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"A JSON web token could not be decoded"}`))
		return
	}
	if f.status != 0 {
		f.failed++
		w.WriteHeader(f.status)
		w.Write([]byte(`{"message":"Bad credentials"}`))
		return
	}
	f.lastReq = nil
	json.NewDecoder(r.Body).Decode(&f.lastReq)
	f.minted++
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"token":"token-%d","expires_at":%q}`, f.minted, time.Now().Add(f.ttl).UTC().Format(time.RFC3339Nano))
}

func (f *fakeGitHubApps) checkJWT(tok string) error {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return fmt.Errorf("want 3 parts, got %d", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, sum[:], sig); err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	var header map[string]string
	if b, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil || json.Unmarshal(b, &header) != nil || header["alg"] != "RS256" {
		return fmt.Errorf("header %v", header)
	}
	var claims struct {
		Iat int64           `json:"iat"`
		Exp int64           `json:"exp"`
		Iss json.RawMessage `json:"iss"`
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &claims); err != nil {
		return err
	}
	now := time.Now().Unix()
	if claims.Iat > now || claims.Exp <= now || claims.Exp-claims.Iat > 600 {
		return fmt.Errorf("bad iat/exp %d/%d (now %d)", claims.Iat, claims.Exp, now)
	}
	if strings.Trim(string(claims.Iss), `"`) != f.appID {
		return fmt.Errorf("iss %s, want %s", claims.Iss, f.appID)
	}
	return nil
}

func (f *fakeGitHubApps) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.minted
}

func testAppKey(t *testing.T, pkcs8 bool) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return key, path
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// withAPI points newly created Apps at srv for the test's duration.
func withAPI(t *testing.T, url string) {
	t.Helper()
	old := githubAPIURL
	githubAPIURL = url
	t.Cleanup(func() { githubAPIURL = old })
}

func TestGitAuthFromEnvFallsBackToPAT(t *testing.T) {
	auth, app, _, err := gitAuthFromEnv(envMap(map[string]string{"SS_USERNAME": "bob", "SS_TOKEN": "pat"}), "site")
	if err != nil {
		t.Fatal(err)
	}
	if app != nil || auth.Username != "bob" || auth.Password != "pat" {
		t.Fatalf("got %+v, app %v", auth, app)
	}
	if _, _, _, err := gitAuthFromEnv(envMap(nil), "site"); err == nil {
		t.Fatal("no credentials at all must be an error")
	}
}

func TestGitAuthFromEnvRejectsPartialAppConfig(t *testing.T) {
	_, _, _, err := gitAuthFromEnv(envMap(map[string]string{
		"SS_GH_APP_ID": "123", "SS_USERNAME": "bob", "SS_TOKEN": "pat",
	}), "site")
	if err == nil || !strings.Contains(err.Error(), "needs all of") {
		t.Fatalf("got %v, want a partial-config error (not a silent PAT fallback)", err)
	}
}

func TestGitAuthFromEnvMintsInstallationToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		appID string
		pkcs8 bool
	}{{"numeric app id, pkcs1", "123456", false}, {"client id, pkcs8", "Iv23liAbCdEf", true}} {
		t.Run(tc.name, func(t *testing.T) {
			key, keyPath := testAppKey(t, tc.pkcs8)
			fake := &fakeGitHubApps{t: t, pub: &key.PublicKey, appID: tc.appID, instID: "987", ttl: time.Hour}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			withAPI(t, srv.URL)

			auth, app, expires, err := gitAuthFromEnv(envMap(map[string]string{
				"SS_GH_APP_ID":               tc.appID,
				"SS_GH_APP_INSTALLATION_ID":  "987",
				"SS_GH_APP_PRIVATE_KEY_PATH": keyPath,
				"SS_USERNAME":                "ignored", "SS_TOKEN": "ignored",
			}), "infosecstreams.github.io")
			if err != nil {
				t.Fatal(err)
			}
			if app == nil || auth.Username != "x-access-token" || auth.Password != "token-1" {
				t.Fatalf("got %+v", auth)
			}
			if d := time.Until(expires); d < 59*time.Minute || d > time.Hour {
				t.Fatalf("expiry %s from now, want ~1h", d)
			}
			repos, _ := fake.lastReq["repositories"].([]any)
			perms, _ := fake.lastReq["permissions"].(map[string]any)
			if len(repos) != 1 || repos[0] != "infosecstreams.github.io" || perms["contents"] != "write" || len(perms) != 1 {
				t.Fatalf("token request not scoped to the site repo/contents:write: %v", fake.lastReq)
			}
		})
	}
}

func TestGitAuthFromEnvErrors(t *testing.T) {
	key, keyPath := testAppKey(t, false)
	fake := &fakeGitHubApps{t: t, pub: &key.PublicKey, appID: "1", instID: "2", ttl: time.Hour, status: http.StatusUnauthorized}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	withAPI(t, srv.URL)

	env := map[string]string{"SS_GH_APP_ID": "1", "SS_GH_APP_INSTALLATION_ID": "2", "SS_GH_APP_PRIVATE_KEY_PATH": keyPath}
	if _, _, _, err := gitAuthFromEnv(envMap(env), "site"); err == nil || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("got %v, want the API error", err)
	}
	env["SS_GH_APP_INSTALLATION_ID"] = "2/../../user"
	if _, _, _, err := gitAuthFromEnv(envMap(env), "site"); err == nil || !strings.Contains(err.Error(), "INSTALLATION_ID") {
		t.Fatalf("got %v, want an installation ID error", err)
	}
	env["SS_GH_APP_INSTALLATION_ID"] = "2"
	env["SS_GH_APP_PRIVATE_KEY_PATH"] = filepath.Join(t.TempDir(), "missing.pem")
	if _, _, _, err := gitAuthFromEnv(envMap(env), "site"); err == nil {
		t.Fatal("missing key file must be an error")
	}
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	os.WriteFile(garbage, []byte("not a key"), 0o600)
	env["SS_GH_APP_PRIVATE_KEY_PATH"] = garbage
	if _, _, _, err := gitAuthFromEnv(envMap(env), "site"); err == nil || !strings.Contains(err.Error(), "PEM") {
		t.Fatalf("got %v, want a PEM error", err)
	}
}

func TestNextGitTokenRefresh(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	app := &githubApp{now: func() time.Time { return now }, refreshBefore: gitTokenRefreshBefore, retry: gitTokenRetry}
	if got := app.nextRefresh(now.Add(time.Hour)); got != 45*time.Minute {
		t.Fatalf("1h token: refresh in %s, want 45m", got)
	}
	if got := app.nextRefresh(now.Add(5 * time.Minute)); got != gitTokenRetry {
		t.Fatalf("nearly expired token: refresh in %s, want %s", got, gitTokenRetry)
	}
}

func TestKeepGitTokenFreshRefreshesBeforeExpiry(t *testing.T) {
	key, keyPath := testAppKey(t, false)
	// Tokens live 2s and are replaced 1.9s early: a refresh every ~100ms.
	fake := &fakeGitHubApps{t: t, pub: &key.PublicKey, appID: "1", instID: "2", ttl: 2 * time.Second}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	withAPI(t, srv.URL)

	auth, app, expires, err := gitAuthFromEnv(envMap(map[string]string{
		"SS_GH_APP_ID": "1", "SS_GH_APP_INSTALLATION_ID": "2", "SS_GH_APP_PRIVATE_KEY_PATH": keyPath,
	}), "site")
	if err != nil {
		t.Fatal(err)
	}
	app.refreshBefore, app.retry = 1900*time.Millisecond, 10*time.Millisecond

	// Stand-in for the worker: swaps happen under mu; after the third
	// swap the test stops the loop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	swaps := 0
	run := func(fn func()) bool {
		mu.Lock()
		defer mu.Unlock()
		fn()
		if swaps++; swaps == 3 {
			cancel()
		}
		return true
	}
	stopped := make(chan struct{})
	go func() { keepGitTokenFresh(ctx, auth, app, expires, run); close(stopped) }()

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		cancel()
		<-stopped
		t.Fatalf("only %d tokens minted in 10s", fake.count())
	}
	if auth.Username != "x-access-token" || auth.Password != "token-4" {
		t.Fatalf("after 3 refreshes got %+v, want token-4", auth)
	}
}

func TestKeepGitTokenFreshRetriesFailures(t *testing.T) {
	key, _ := testAppKey(t, false)
	fake := &fakeGitHubApps{t: t, pub: &key.PublicKey, appID: "1", instID: "2", ttl: time.Hour, status: http.StatusBadGateway}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	app := &githubApp{appID: "1", installationID: "2", key: key, apiURL: srv.URL, client: srv.Client(), now: time.Now,
		refreshBefore: time.Hour, retry: 10 * time.Millisecond}

	auth := &httpauth.BasicAuth{Username: appTokenUser, Password: "token-old"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := func(fn func()) bool { fn(); cancel(); return true }
	stopped := make(chan struct{})
	go func() { keepGitTokenFresh(ctx, auth, app, time.Now(), run); close(stopped) }()

	// Let a few attempts fail (one every 10ms), then let the API recover.
	time.Sleep(100 * time.Millisecond)
	fake.mu.Lock()
	failed := fake.failed
	fake.status = 0
	fake.mu.Unlock()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		cancel()
		<-stopped
		t.Fatal("token was never refreshed after the API recovered")
	}
	if failed < 2 {
		t.Fatalf("only %d failed attempts before recovery, want retries", failed)
	}
	if auth.Password != "token-1" {
		t.Fatalf("got %q, want token-1", auth.Password)
	}
}
