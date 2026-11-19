package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTwitch serves /oauth2/token and /helix/streams. Only the newest token
// it issued is accepted; older ones get a 401, as after expiry.
type fakeTwitch struct {
	mu          sync.Mutex
	issued      int
	tokenStatus int
	streamCalls int
}

func (f *fakeTwitch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/token"):
		if f.tokenStatus != 0 && f.tokenStatus != 200 {
			w.WriteHeader(f.tokenStatus)
			w.Write([]byte(`{"status":400,"message":"invalid client secret"}`))
			return
		}
		f.issued++
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok" + string(rune('0'+f.issued)),
			"expires_in":   5000000,
			"token_type":   "bearer",
		})
	case strings.HasSuffix(r.URL.Path, "/streams"):
		f.streamCalls++
		if r.Header.Get("Authorization") != "Bearer tok"+string(rune('0'+f.issued)) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"Unauthorized","status":401,"message":"Invalid OAuth token"}`))
			return
		}
		w.Write([]byte(`{"data":[{"user_login":"alpha","user_name":"alpha","game_name":"Just Chatting"}],"pagination":{}}`))
	default:
		http.NotFound(w, r)
	}
}

// rewriteTransport sends every request to the test server.
type rewriteTransport struct{ target *url.URL }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

func withFakeTwitch(t *testing.T) *fakeTwitch {
	t.Helper()
	f := &fakeTwitch{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	old := twitchHTTPClient
	twitchHTTPClient = &http.Client{Transport: rewriteTransport{u}}
	t.Cleanup(func() { twitchHTTPClient = old })
	return f
}

func TestNewTwitchClientGetsToken(t *testing.T) {
	f := withFakeTwitch(t)
	client, ttl, err := newTwitchClient("id", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if ttl != 5000000*time.Second || client.GetAppAccessToken() != "tok1" || f.issued != 1 {
		t.Errorf("got ttl=%s token=%q issued=%d", ttl, client.GetAppAccessToken(), f.issued)
	}
}

// Without a token at startup helix can never renew one, so startup must fail.
func TestNewTwitchClientFailsWithoutToken(t *testing.T) {
	f := withFakeTwitch(t)
	f.tokenStatus = 400
	if _, _, err := newTwitchClient("id", "bad"); err == nil {
		t.Fatal("want an error when Twitch refuses the token request")
	}
}

// An expired token must not break lookups: helix gets a new app token on
// the 401 and retries.
func TestExpiredTokenIsRenewedOn401(t *testing.T) {
	f := withFakeTwitch(t)
	client, _, err := newTwitchClient("id", "secret")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.issued++ // Twitch has moved on; tok1 is now expired.
	f.mu.Unlock()

	s := &StreamersRepo{client: client}
	live, err := s.fetchLiveByLogin([]string{"alpha"})
	if err != nil {
		t.Fatalf("lookup after token expiry failed: %v", err)
	}
	if _, ok := live["alpha"]; !ok {
		t.Errorf("alpha missing from %v", live)
	}
	if client.GetAppAccessToken() != "tok3" || f.streamCalls != 2 {
		t.Errorf("got token %q after %d /streams calls, want tok3 after 2", client.GetAppAccessToken(), f.streamCalls)
	}
}

func TestRenewAppTokenInstallsNewToken(t *testing.T) {
	withFakeTwitch(t)
	client, _, err := newTwitchClient("id", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := renewAppToken(client); err != nil {
		t.Fatal(err)
	}
	if client.GetAppAccessToken() != "tok2" {
		t.Errorf("token %q, want tok2", client.GetAppAccessToken())
	}
}

func TestNextTokenRenewal(t *testing.T) {
	if got := nextTokenRenewal(60 * 24 * time.Hour); got != 54*24*time.Hour {
		t.Errorf("60d token: renew after %s, want 54d", got)
	}
	if got := nextTokenRenewal(10 * time.Second); got != time.Minute {
		t.Errorf("tiny ttl: renew after %s, want the 1m floor", got)
	}
}
