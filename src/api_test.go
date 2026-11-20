package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicklaw5/helix/v2"
)

// newAPIRepo returns a repo whose live state has alpha online and beta
// offline, and a mux serving its API.
func newAPIRepo(t *testing.T) (*StreamersRepo, *http.ServeMux) {
	t.Helper()
	users := &fakeUsers{}
	users.set(userAlpha, userBeta)
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, ""), users: users}
	s.resolveRoster([]string{"alpha", "beta"}, t0)
	s.live.online("1", helix.Stream{UserID: "1", UserLogin: "alpha", UserName: "Alpha", GameName: "Just Chatting",
		Title: "Hi", Language: "en", ViewerCount: 0, StartedAt: t0.Add(-time.Hour)}, t0)
	s.live.rebuild(t0)
	mux := http.NewServeMux()
	s.registerAPI(mux)
	return s, mux
}

func apiGet(mux http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestStatusJSONShapeAndHeaders(t *testing.T) {
	old := nextStreamAt
	nextStreamAt = func(id string) (time.Time, bool) {
		if id == "2" {
			return time.Date(2026, 11, 20, 18, 0, 0, 0, time.FixedZone("x", 3600)), true
		}
		return time.Time{}, false
	}
	t.Cleanup(func() { nextStreamAt = old })
	s, mux := newAPIRepo(t)
	s.live.lastPayload = nil // force a rebuild with the schedule hook
	s.live.snap.Store(nil)
	s.live.rebuild(t0)

	rec := apiGet(mux, "GET", "/streamstatus/status.json", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	h := rec.Header()
	for k, want := range map[string]string{
		"Content-Type":                jsonContentType,
		"Access-Control-Allow-Origin": "*",
		"Cache-Control":               "public, max-age=15",
		"Vary":                        "Accept-Encoding",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if et := h.Get("ETag"); !strings.HasPrefix(et, `"`) || strings.HasPrefix(et, "W/") {
		t.Errorf("ETag %q is not strong", et)
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["generated_at"] != "2026-11-01T18:00:00Z" || raw["live_count"] != 1.0 {
		t.Errorf("header fields: %v", raw)
	}
	list := raw["streamers"].([]any)
	alpha, beta := list[0].(map[string]any), list[1].(map[string]any)
	if alpha["login"] != "alpha" || alpha["status"] != "online" || alpha["viewers"] != 0.0 ||
		alpha["started_at"] != "2026-11-01T17:00:00Z" || alpha["hours_30d"] != 1.0 || alpha["display_name"] != "Alpha" {
		t.Errorf("alpha = %v", alpha)
	}
	if _, ok := alpha["next_stream_at"]; ok {
		t.Errorf("alpha has no schedule, want next_stream_at omitted: %v", alpha)
	}
	want := map[string]any{"id": "2", "login": "beta", "display_name": "Beta", "status": "offline",
		"hours_30d": 0.0, "next_stream_at": "2026-11-20T17:00:00Z"}
	if len(beta) != len(want) {
		t.Errorf("beta = %v, want exactly %v", beta, want)
	}
	for k, v := range want {
		if beta[k] != v {
			t.Errorf("beta[%s] = %v, want %v", k, beta[k], v)
		}
	}
}

func TestStatusJSONGzipAndConditional(t *testing.T) {
	_, mux := newAPIRepo(t)
	plain := apiGet(mux, "GET", "/streamstatus/status.json", nil)
	zipped := apiGet(mux, "GET", "/streamstatus/status.json", map[string]string{"Accept-Encoding": "br, gzip;q=0.8"})
	if zipped.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("gzip not used")
	}
	zr, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if !bytes.Equal(body, plain.Body.Bytes()) {
		t.Error("gzipped body differs")
	}
	et, etgz := plain.Header().Get("ETag"), zipped.Header().Get("ETag")
	if et == etgz {
		t.Error("identity and gzip representations share a strong ETag")
	}

	for _, inm := range []string{et, etgz, "W/" + etgz, `"nope", ` + et, "*"} {
		rec := apiGet(mux, "GET", "/streamstatus/status.json", map[string]string{"If-None-Match": inm, "Accept-Encoding": "gzip"})
		if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("ETag") != etgz {
			t.Errorf("If-None-Match %s: code %d, etag %q", inm, rec.Code, rec.Header().Get("ETag"))
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Cache-Control") == "" {
			t.Errorf("304 lacks CORS/cache headers: %v", rec.Header())
		}
	}
	if rec := apiGet(mux, "GET", "/streamstatus/status.json", map[string]string{"If-None-Match": `"other"`}); rec.Code != 200 {
		t.Errorf("non-matching If-None-Match: %d", rec.Code)
	}
	if rec := apiGet(mux, "GET", "/streamstatus/status.json", map[string]string{"Accept-Encoding": "gzip;q=0"}); rec.Header().Get("Content-Encoding") != "" {
		t.Error("gzip used despite q=0")
	}

	head := apiGet(mux, "HEAD", "/streamstatus/status.json", nil)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != plain.Header().Get("Content-Length") {
		t.Errorf("HEAD: %d len %d", head.Code, head.Body.Len())
	}
	opt := apiGet(mux, "OPTIONS", "/streamstatus/status.json", nil)
	if opt.Code != http.StatusNoContent || opt.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("OPTIONS: %d %v", opt.Code, opt.Header())
	}
	if rec := apiGet(mux, "POST", "/streamstatus/status.json", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
	if rec := apiGet(mux, "GET", "/streamstatus/nope.json", nil); rec.Code != 404 || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("unknown path: %d", rec.Code)
	}
}

// The ETag changes only when the content does; generated_at alone doesn't
// make a new version.
func TestSnapshotOnlyChangesWithContent(t *testing.T) {
	s, mux := newAPIRepo(t)
	first := apiGet(mux, "GET", "/streamstatus/status.json", nil).Header().Get("ETag")
	s.live.rebuild(t0.Add(time.Second))
	if got := apiGet(mux, "GET", "/streamstatus/status.json", nil).Header().Get("ETag"); got != first {
		t.Error("ETag changed without a content change")
	}
	s.live.online("1", helix.Stream{UserID: "1", UserLogin: "alpha", GameName: "Just Chatting", ViewerCount: 9, StartedAt: t0.Add(-time.Hour)}, t0.Add(time.Minute))
	s.live.rebuild(t0.Add(time.Minute))
	rec := apiGet(mux, "GET", "/streamstatus/status.json", nil)
	if rec.Header().Get("ETag") == first || !strings.Contains(rec.Body.String(), `"viewers":9`) {
		t.Errorf("new viewer count not published: %s", rec.Body)
	}
}

// Serving must never wait for the worker.
func TestStatusHandlerDoesNotTakeWorkerMutex(t *testing.T) {
	s, mux := newAPIRepo(t)
	s.mutex.Lock()
	defer s.mutex.Unlock()
	done := make(chan int)
	go func() { done <- apiGet(mux, "GET", "/streamstatus/status.json", nil).Code }()
	select {
	case code := <-done:
		if code != 200 {
			t.Errorf("status %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler blocked on the worker mutex")
	}
}

func TestStatusBeforeFirstSnapshot(t *testing.T) {
	s := &StreamersRepo{mutex: &sync.Mutex{}, live: newTestLive(t, "")}
	mux := http.NewServeMux()
	s.registerAPI(mux)
	rec := apiGet(mux, "GET", "/streamstatus/status.json", nil)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("before roster: %d %v", rec.Code, rec.Header())
	}
	// Live state off entirely (no database).
	s.live = nil
	if rec := apiGet(mux, "GET", "/streamstatus/status.json", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("without live state: %d", rec.Code)
	}
}

func TestGzipAccepted(t *testing.T) {
	for h, want := range map[string]bool{
		"":                    false,
		"gzip":                true,
		"GZIP":                true,
		"deflate, gzip;q=0.5": true,
		"gzip;q=0":            false,
		"gzip;q=0.0, *":       false,
		"*":                   true,
		"br":                  false,
		"x-gzip":              true,
		"identity":            false,
	} {
		if got := gzipAccepted(h); got != want {
			t.Errorf("gzipAccepted(%q) = %v, want %v", h, got, want)
		}
	}
}
