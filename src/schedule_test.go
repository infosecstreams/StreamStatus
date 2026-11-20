package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nicklaw5/helix/v2"
	log "github.com/sirupsen/logrus"
)

var schedNow = time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC)

// fakeSchedule is a scripted Helix for users, schedules and channels.
type fakeSchedule struct {
	mu        sync.Mutex
	users     map[string]helix.User // by lowercase login
	usersErr  error
	schedules map[string][]helix.GetScheduleResponse // pages by broadcaster ID
	errs      map[string]error                       // per broadcaster ID
	channels  map[string]helix.ChannelInformation
	calls     []helix.GetScheduleParams
}

func (f *fakeSchedule) GetUsers(p *helix.UsersParams) (*helix.UsersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.usersErr != nil {
		return nil, f.usersErr
	}
	r := &helix.UsersResponse{}
	r.StatusCode = 200
	for _, l := range p.Logins {
		if u, ok := f.users[strings.ToLower(l)]; ok {
			r.Data.Users = append(r.Data.Users, u)
		}
	}
	return r, nil
}

func (f *fakeSchedule) GetSchedule(p *helix.GetScheduleParams) (*helix.GetScheduleResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, *p)
	if err := f.errs[p.BroadcasterID]; err != nil {
		return nil, err
	}
	pages, ok := f.schedules[p.BroadcasterID]
	if !ok {
		r := &helix.GetScheduleResponse{}
		r.StatusCode = 404
		r.ErrorMessage = "segments were not found"
		return r, nil
	}
	i := 0
	if p.After != "" {
		fmt.Sscanf(p.After, "page%d", &i)
	}
	r := pages[i]
	return &r, nil
}

func (f *fakeSchedule) GetChannelInformation(p *helix.GetChannelInformationParams) (*helix.GetChannelInformationResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := &helix.GetChannelInformationResponse{}
	r.StatusCode = 200
	for _, id := range p.BroadcasterIDs {
		if ch, ok := f.channels[id]; ok {
			r.Data.Channels = append(r.Data.Channels, ch)
		}
	}
	return r, nil
}

func seg(id string, start time.Time, length time.Duration, title, category string) helix.GetScheduleSegment {
	g := helix.GetScheduleSegment{ID: id, StartTime: helix.Time{Time: start}, Title: title, Category: helix.GetScheduleSegmentCategory{Name: category}}
	if length > 0 {
		g.EndTime = helix.Time{Time: start.Add(length)}
	}
	return g
}

func schedPage(cursor string, segs ...helix.GetScheduleSegment) helix.GetScheduleResponse {
	r := helix.GetScheduleResponse{}
	r.StatusCode = 200
	r.Data.Schedule.Segments = segs
	r.Data.Pagination.Cursor = cursor
	return r
}

func direct(fn func()) bool { fn(); return true }

// levelHook records log entries at Warn and above.
type levelHook struct {
	mu      sync.Mutex
	entries []*log.Entry
}

func (h *levelHook) Levels() []log.Level { return []log.Level{log.ErrorLevel, log.WarnLevel} }
func (h *levelHook) Fire(e *log.Entry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, e)
	return nil
}
func (h *levelHook) count(l log.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, e := range h.entries {
		if e.Level == l {
			n++
		}
	}
	return n
}

func captureLogs(t *testing.T) *levelHook {
	h := &levelHook{}
	log.AddHook(h)
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(log.LevelHooks{}) })
	return h
}

func noSchedulePause(t *testing.T) {
	old := schedulePause
	schedulePause = 0
	t.Cleanup(func() { schedulePause = old })
}

func TestFetchSchedule(t *testing.T) {
	vac := schedPage("",
		seg("old", schedNow.Add(-3*time.Hour), time.Hour, "ended", ""),
		seg("now", schedNow.Add(-30*time.Minute), 2*time.Hour, "running", ""),
		seg("tomorrow", schedNow.Add(24*time.Hour), 2*time.Hour, "CTF night", "Science & Technology"),
		seg("vac", schedNow.Add(48*time.Hour), time.Hour, "", ""),
		seg("noend", schedNow.Add(72*time.Hour), 0, "open", ""),
	)
	vac.Data.Schedule.Segments[2].CanceledUntil = schedNow.Add(26 * time.Hour).Format(time.RFC3339)
	vac.Data.Schedule.Vacation = helix.GetScheduleVacation{StartTime: helix.Time{Time: schedNow.Add(40 * time.Hour)}, EndTime: helix.Time{Time: schedNow.Add(60 * time.Hour)}}
	f := &fakeSchedule{schedules: map[string][]helix.GetScheduleResponse{
		"1": {vac},
		"2": {
			schedPage("page1", seg("a", schedNow.Add(time.Hour), time.Hour, "", "")),
			schedPage("page2", seg("b", schedNow.Add(5*24*time.Hour), time.Hour, "", "")),
			schedPage("page3", seg("c", schedNow.Add(8*24*time.Hour), time.Hour, "", "")),
			schedPage("", seg("d", schedNow.Add(9*24*time.Hour), time.Hour, "", "")),
		},
	}}

	got, _, err := fetchSchedule(f, "1", schedNow)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, g := range got {
		ids = append(ids, fmt.Sprintf("%s canceled=%v vacation=%v end=%v", g.ID, g.Canceled, g.Vacation, !g.End.IsZero()))
	}
	want := []string{"now canceled=false vacation=false end=true", "tomorrow canceled=true vacation=false end=true", "vac canceled=false vacation=true end=true", "noend canceled=false vacation=false end=false"}
	if strings.Join(ids, "|") != strings.Join(want, "|") {
		t.Errorf("got  %v\nwant %v", ids, want)
	}
	if got[1].Title != "CTF night" || got[1].Category != "Science & Technology" || got[1].BroadcasterID != "1" {
		t.Errorf("fields: %+v", got[1])
	}

	got, _, err = fetchSchedule(f, "2", schedNow)
	if err != nil || len(got) != 2 {
		t.Errorf("paging: got %d segments (%v), want a and b only", len(got), err)
	}
	if n := len(f.calls); n != 4 { // 1 + 3 pages (stops at the page beyond the horizon)
		t.Errorf("made %d calls, want 4", n)
	}

	// No schedule at all is an empty schedule, not an error.
	got, _, err = fetchSchedule(f, "404", schedNow)
	if err != nil || len(got) != 0 {
		t.Errorf("404: got %v, %v", got, err)
	}
}

func TestFetchScheduleError(t *testing.T) {
	f := &fakeSchedule{schedules: map[string][]helix.GetScheduleResponse{}}
	r := helix.GetScheduleResponse{}
	r.StatusCode = 500
	f.schedules["1"] = []helix.GetScheduleResponse{r}
	if _, _, err := fetchSchedule(f, "1", schedNow); err == nil {
		t.Error("a 500 must be an error")
	}
}

// newScheduleRepo returns a StreamersRepo with a roster of alpha (1), beta
// (2) and dormant (9, inactive) and schedules for alpha and dormant.
func newScheduleRepo(t *testing.T) (*StreamersRepo, *fakeSchedule) {
	t.Helper()
	freezeTime(t, schedNow)
	noSleep(t)
	noSchedulePause(t)
	s, _ := newGitFixture(t, []string{offlineLine("alpha"), offlineLine("beta")}, nil, "alpha,\nbeta,\nghost,\n", "dormant,\n")
	f := &fakeSchedule{
		users: map[string]helix.User{
			"alpha":   {ID: "1", Login: "alpha", DisplayName: "Alpha"},
			"beta":    {ID: "2", Login: "beta", DisplayName: "Beta"},
			"dormant": {ID: "9", Login: "dormant", DisplayName: "Dormant"},
		},
		schedules: map[string][]helix.GetScheduleResponse{
			"1": {schedPage("", seg("a2", schedNow.Add(26*time.Hour), 2*time.Hour, "Malware, RE; and \\ more", "Science & Technology"),
				seg("a1", schedNow.Add(2*time.Hour), 2*time.Hour, "CTF night", "Just Chatting"))},
			"9": {schedPage("", seg("d1", schedNow.Add(2*time.Hour), time.Hour, "", ""))},
		},
	}
	s.schedule = newScheduleState(f)
	return s, f
}

func TestRefreshKeepsLastGoodDataAndAlertsAfterRepeatedFailures(t *testing.T) {
	s, f := newScheduleRepo(t)
	logs := captureLogs(t)

	s.refreshSchedules(direct)
	if at, ok := s.nextStreamAt("1"); !ok || !at.Equal(schedNow.Add(2*time.Hour)) {
		t.Fatalf("nextStreamAt(1) = %v %v", at, ok)
	}
	if _, ok := s.nextStreamAt("2"); ok {
		t.Error("beta has no schedule (404)")
	}
	if logs.count(log.ErrorLevel)+logs.count(log.WarnLevel) != 0 {
		t.Errorf("a clean refresh logged warnings: %v", logs.entries)
	}

	f.errs = map[string]error{"1": errors.New("boom")}
	for i := 1; i <= scheduleFailureAlert; i++ {
		s.refreshSchedules(direct)
		if _, ok := s.nextStreamAt("1"); !ok {
			t.Fatalf("refresh %d dropped alpha's last good schedule", i)
		}
		wantErr := 0
		if i >= scheduleFailureAlert {
			wantErr = 1
		}
		if got := logs.count(log.ErrorLevel); got != wantErr {
			t.Fatalf("after %d failures: %d Error lines, want %d", i, got, wantErr)
		}
	}

	f.errs = nil
	f.usersErr = errors.New("users down")
	for i := 1; i <= scheduleFailureAlert; i++ {
		s.refreshSchedules(direct)
	}
	if got := logs.count(log.ErrorLevel); got != 2 {
		t.Errorf("roster failures: %d Error lines in total, want 2", got)
	}
	if _, ok := s.nextStreamAt("1"); !ok {
		t.Error("a failed roster lookup dropped the schedule")
	}
}

func TestRefreshDropsStreamersLeavingTheRoster(t *testing.T) {
	s, f := newScheduleRepo(t)
	s.refreshSchedules(direct)
	delete(f.users, "alpha")
	s.refreshSchedules(direct)
	if _, ok := s.nextStreamAt("1"); ok {
		t.Error("alpha left the roster but is still scheduled")
	}
}

func TestRefreshPausesOnLowRateLimit(t *testing.T) {
	s, f := newScheduleRepo(t)
	slept := noSleep(t)
	page := f.schedules["1"][0]
	page.Header = http.Header{}
	page.Header.Set("RateLimit-Remaining", "3")
	page.Header.Set("RateLimit-Reset", fmt.Sprint(schedNow.Add(20*time.Second).Unix()))
	f.schedules["1"][0] = page
	f.schedules["2"] = []helix.GetScheduleResponse{page}
	f.schedules["9"] = []helix.GetScheduleResponse{page}
	s.refreshSchedules(direct)
	if len(*slept) != 1 || (*slept)[0] != 21*time.Second {
		t.Errorf("slept %v, want [21s]", *slept)
	}
}

func TestRateLimitWait(t *testing.T) {
	rc := func(status int, remaining string, reset time.Time) helix.ResponseCommon {
		h := http.Header{}
		if remaining != "" {
			h.Set("RateLimit-Remaining", remaining)
			h.Set("RateLimit-Reset", fmt.Sprint(reset.Unix()))
		}
		return helix.ResponseCommon{StatusCode: status, Header: h}
	}
	for _, c := range []struct {
		rc   helix.ResponseCommon
		want time.Duration
	}{
		{helix.ResponseCommon{}, 0},
		{rc(200, "", time.Time{}), 0},
		{rc(200, "700", schedNow.Add(time.Minute)), 0},
		{rc(200, "5", schedNow.Add(10*time.Second)), 11 * time.Second},
		{rc(429, "500", schedNow.Add(10*time.Minute)), time.Minute},
		{rc(200, "0", schedNow.Add(-time.Minute)), time.Second},
	} {
		if got := rateLimitWait(c.rc, schedNow); got != c.want {
			t.Errorf("%d/%s: got %s, want %s", c.rc.StatusCode, c.rc.Header.Get("RateLimit-Remaining"), got, c.want)
		}
	}
}

func TestNextStreamAtSkipsCanceledAndVacation(t *testing.T) {
	freezeTime(t, schedNow)
	s := &StreamersRepo{schedule: newScheduleState(nil)}
	if _, ok := s.nextStreamAt("1"); ok {
		t.Error("empty state")
	}
	s.schedule.byID["1"] = &broadcasterSchedule{segments: []scheduledSegment{
		{ID: "past", Start: schedNow.Add(-time.Hour), End: schedNow.Add(time.Hour)},
		{ID: "c", Start: schedNow.Add(time.Hour), Canceled: true},
		{ID: "v", Start: schedNow.Add(2 * time.Hour), Vacation: true},
		{ID: "late", Start: schedNow.Add(5 * time.Hour)},
		{ID: "next", Start: schedNow.Add(3 * time.Hour)},
	}}
	if at, ok := s.nextStreamAt("1"); !ok || !at.Equal(schedNow.Add(3*time.Hour)) {
		t.Errorf("got %v %v, want +3h", at, ok)
	}
	if _, ok := (&StreamersRepo{}).nextStreamAt("1"); ok {
		t.Error("nil schedule must report nothing")
	}
}

func get(t *testing.T, s *StreamersRepo, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p := path
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	s.scheduleHandlers()[p](rec, req)
	return rec
}

func TestScheduleJSON(t *testing.T) {
	s, _ := newScheduleRepo(t)
	if rec := get(t, s, "/streamstatus/schedule.json", nil); rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("before the first refresh: %d, want 503 with Retry-After", rec.Code)
	}
	s.refreshSchedules(direct)

	rec := get(t, s, "/streamstatus/schedule.json", nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	h := rec.Header()
	if h.Get("Content-Type") != "application/json; charset=utf-8" || h.Get("Access-Control-Allow-Origin") != "*" ||
		h.Get("Cache-Control") != "public, max-age=300" || !strings.HasPrefix(h.Get("ETag"), `"`) {
		t.Errorf("headers: %v", h)
	}
	var got struct {
		GeneratedAt string           `json:"generated_at"`
		Segments    []map[string]any `json:"segments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.GeneratedAt != "2026-11-19T20:00:00Z" || len(got.Segments) != 3 {
		t.Fatalf("got %s", rec.Body)
	}
	first := got.Segments[0]
	want := map[string]any{"id": "1", "login": "alpha", "display_name": "Alpha", "segment_id": "a1",
		"start": "2026-11-19T22:00:00Z", "end": "2026-11-20T00:00:00Z", "title": "CTF night", "category": "Just Chatting", "canceled": false}
	for k, v := range want {
		if first[k] != v {
			t.Errorf("%s = %v, want %v", k, first[k], v)
		}
	}
	if _, ok := first["vacation"]; ok {
		t.Error("vacation=false should be omitted")
	}
	// Sorted by start, then login.
	if got.Segments[1]["segment_id"] != "d1" || got.Segments[2]["segment_id"] != "a2" {
		t.Errorf("order: %v", got.Segments)
	}
	if _, ok := got.Segments[1]["title"]; ok {
		t.Error("empty title should be omitted")
	}

	// ETag round trip.
	if rec2 := get(t, s, "/streamstatus/schedule.json", map[string]string{"If-None-Match": h.Get("ETag")}); rec2.Code != 304 || rec2.Body.Len() != 0 {
		t.Errorf("If-None-Match: %d", rec2.Code)
	}
	// gzip.
	rec3 := get(t, s, "/streamstatus/schedule.json", map[string]string{"Accept-Encoding": "br, gzip"})
	if rec3.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("no gzip: %v", rec3.Header())
	}
	zr, err := gzip.NewReader(rec3.Body)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := io.ReadAll(zr); !bytes.Equal(body, rec.Body.Bytes()) {
		t.Error("gzip body differs")
	}
	if rec3.Header().Get("ETag") != h.Get("ETag") {
		t.Error("ETag must identify the representation before compression")
	}

	// ?login=
	rec4 := get(t, s, "/streamstatus/schedule.json?login=DORMANT", nil)
	if !strings.Contains(rec4.Body.String(), `"segment_id":"d1"`) || strings.Contains(rec4.Body.String(), `"a1"`) {
		t.Errorf("login filter: %s", rec4.Body)
	}
	if rec5 := get(t, s, "/streamstatus/schedule.json?login=beta", nil); rec5.Code != 200 || !strings.Contains(rec5.Body.String(), `"segments":[]`) {
		t.Errorf("roster streamer without schedule: %d %s", rec5.Code, rec5.Body)
	}
	if rec6 := get(t, s, "/streamstatus/schedule.json?login=nobody", nil); rec6.Code != 404 {
		t.Errorf("unknown login: %d", rec6.Code)
	}
}

func TestScheduleMethods(t *testing.T) {
	s, _ := newScheduleRepo(t)
	s.refreshSchedules(direct)
	h := s.scheduleHandlers()["/streamstatus/schedule.ics"]
	for _, c := range []struct {
		method string
		code   int
		body   bool
	}{{"HEAD", 200, false}, {"OPTIONS", 204, false}, {"POST", 405, true}} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(c.method, "/streamstatus/schedule.ics", nil))
		if rec.Code != c.code || (rec.Body.Len() > 0) != c.body || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: %d body=%d", c.method, rec.Code, rec.Body.Len())
		}
	}
}

// unfoldICS undoes RFC 5545 line folding, checking every physical line.
func unfoldICS(t *testing.T, ics string) []string {
	t.Helper()
	if !strings.HasSuffix(ics, "\r\n") {
		t.Error("calendar must end with CRLF")
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSuffix(ics, "\r\n"), "\r\n") {
		if len(l) > 75 {
			t.Errorf("line over 75 octets: %q", l)
		}
		if !utf8.ValidString(l) {
			t.Errorf("fold split a UTF-8 sequence: %q", l)
		}
		if strings.ContainsAny(l, "\r\n") {
			t.Errorf("bare CR/LF in %q", l)
		}
		if strings.HasPrefix(l, " ") {
			out[len(out)-1] += l[1:]
			continue
		}
		out = append(out, l)
	}
	return out
}

func TestScheduleICS(t *testing.T) {
	s, f := newScheduleRepo(t)
	f.users["alpha"] = helix.User{ID: "1", Login: "alpha", DisplayName: "Ålphà_有一个非常长的名字的主播"}
	page := f.schedules["1"][0]
	page.Data.Schedule.Segments[0].Title = strings.Repeat("Reversing ünïcödé malware, live; ", 5) + "line\nbreak"
	page.Data.Schedule.Segments[0].CanceledUntil = "2026-11-21T00:00:00Z"
	s.refreshSchedules(direct)

	rec := get(t, s, "/streamstatus/schedule.ics", nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/calendar; charset=utf-8" || rec.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	lines := unfoldICS(t, rec.Body.String())
	if lines[0] != "BEGIN:VCALENDAR" || lines[len(lines)-1] != "END:VCALENDAR" {
		t.Errorf("not a calendar: %q ... %q", lines[0], lines[len(lines)-1])
	}
	for _, want := range []string{"VERSION:2.0", "METHOD:PUBLISH", "X-WR-CALNAME:InfoSec Streams schedule"} {
		if !containsLine(lines, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.HasPrefix(lines[1], "VERSION:") && !containsPrefix(lines, "PRODID:") {
		t.Error("missing PRODID")
	}
	events := strings.Count(rec.Body.String(), "BEGIN:VEVENT")
	if events != 3 {
		t.Fatalf("%d events, want 3", events)
	}
	// The first event is alpha's a1.
	ev := eventLines(lines, "UID:a1")
	for _, want := range []string{
		"DTSTAMP:20261119T200000Z",
		"DTSTART:20261119T220000Z",
		"DTEND:20261120T000000Z",
		"SUMMARY:Ålphà_有一个非常长的名字的主播: CTF night",
		"URL:https://www.twitch.tv/alpha",
		"STATUS:CONFIRMED",
		`DESCRIPTION:Category: Just Chatting\nhttps://www.twitch.tv/alpha`,
	} {
		if !containsLine(ev, want) {
			t.Errorf("a1 missing %q in %q", want, ev)
		}
	}
	ev = eventLines(lines, "UID:a2")
	wantSummary := `SUMMARY:Ålphà_有一个非常长的名字的主播: ` + strings.Repeat(`Reversing ünïcödé malware\, live\; `, 5) + `line\nbreak`
	if !containsLine(ev, wantSummary) || !containsLine(ev, "STATUS:CANCELLED") {
		t.Errorf("a2 escaping/cancel: %q", ev)
	}
	// Dormant's segment has no title.
	if ev := eventLines(lines, "UID:d1"); !containsLine(ev, "SUMMARY:Dormant: Live on Twitch") {
		t.Errorf("d1: %q", ev)
	}

	// One streamer's calendar.
	rec = get(t, s, "/streamstatus/schedule.ics?login=dormant", nil)
	lines = unfoldICS(t, rec.Body.String())
	if strings.Count(rec.Body.String(), "BEGIN:VEVENT") != 1 || !containsLine(lines, "X-WR-CALNAME:Dormant on Twitch (InfoSec Streams)") {
		t.Errorf("login filter:\n%s", rec.Body)
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func containsPrefix(lines []string, p string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// eventLines returns the lines of the VEVENT holding line uid.
func eventLines(lines []string, uid string) []string {
	start := -1
	for i, l := range lines {
		switch {
		case l == "BEGIN:VEVENT":
			start = i
		case l == uid && start >= 0:
			for j := i; j < len(lines); j++ {
				if lines[j] == "END:VEVENT" {
					return lines[start : j+1]
				}
			}
		}
	}
	return nil
}

func TestFoldICSLine(t *testing.T) {
	for _, in := range []string{"", "short", strings.Repeat("a", 75), strings.Repeat("a", 76), strings.Repeat("é", 100), strings.Repeat("有", 60) + "x"} {
		out := foldICSLine(in)
		var joined strings.Builder
		for i, l := range strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n") {
			if len(l) > 75 || !utf8.ValidString(l) {
				t.Errorf("bad physical line %q", l)
			}
			if i > 0 {
				l = strings.TrimPrefix(l, " ")
			}
			joined.WriteString(l)
		}
		if joined.String() != in {
			t.Errorf("round trip of %q gave %q", in, joined.String())
		}
	}
}

func TestICSEscape(t *testing.T) {
	if got, want := icsEscape("a\\b;c,d\ne\r\nf\tg"), `a\\b\;c\,d\ne\nfg`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAcceptsGzip(t *testing.T) {
	for h, want := range map[string]bool{"": false, "gzip": true, "br, gzip;q=0.5": true, "gzip;q=0": false, "GZIP": true, "deflate": false} {
		if acceptsGzip(h) != want {
			t.Errorf("%q: want %v", h, want)
		}
	}
}

// --- pre-live from schedules ---

// newSchedulePreliveRepo: alpha offline, beta 🟢, gamma 🟡 from a
// channel.update, delta offline; dormant inactive. Each has a segment.
func newSchedulePreliveRepo(t *testing.T, segs map[string]scheduledSegment) (*StreamersRepo, string) {
	t.Helper()
	freezeTime(t, schedNow)
	noSleep(t)
	s, origin := newGitFixture(t,
		[]string{offlineLine("alpha"), onlineLine("beta"), preliveLine("gamma", "2026-11-19T19:50Z"), offlineLine("delta")},
		[]string{"`dormant` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/dormant) &nbsp; |"},
		"alpha,\nbeta,\ngamma,\ndelta,\n", "dormant,\n")
	s.client = notLive()
	st := newScheduleState(&fakeSchedule{channels: map[string]helix.ChannelInformation{
		"1": {BroadcasterID: "1", BroadcasterLanguage: "de", GameName: "Minecraft"},
	}})
	ids := map[string]string{"alpha": "1", "beta": "2", "gamma": "3", "delta": "4", "dormant": "9"}
	for login, id := range ids {
		st.roster[id] = rosterEntry{id: id, login: login, displayName: login}
		if g, ok := segs[login]; ok {
			g.BroadcasterID = id
			if g.ID == "" {
				g.ID = "seg-" + login
			}
			st.byID[id] = &broadcasterSchedule{segments: []scheduledSegment{g}, fetched: true}
		}
	}
	st.refreshedAt = schedNow
	s.schedule = st
	return s, origin
}

func soon(d time.Duration, category string) scheduledSegment {
	return scheduledSegment{Start: schedNow.Add(d), End: schedNow.Add(d + 2*time.Hour), Category: category}
}

func TestSchedulePreliveMarksStreamerStartingSoon(t *testing.T) {
	s, origin := newSchedulePreliveRepo(t, map[string]scheduledSegment{
		"alpha":   soon(10*time.Minute, ""), // no category: falls back, channel is Minecraft
		"beta":    soon(5*time.Minute, "Just Chatting"),
		"gamma":   soon(5*time.Minute, "Just Chatting"),
		"delta":   soon(20*time.Minute, "Just Chatting"),
		"dormant": soon(5*time.Minute, "Just Chatting"),
	})
	before := gitOutput(t, origin, "rev-list", "--count", "main")

	s.process(statusJob{kind: jobFunc, fn: s.checkSchedulePrelive})

	idx := gitOutput(t, origin, "show", "main:index.md")
	if !strings.Contains(idx, "🟡 | `alpha`") || !strings.Contains(idx, `"Getting ready: scheduled stream (since 2026-11-19T20:00Z)"`) || !strings.Contains(idx, "| DE") {
		t.Errorf("alpha should be 🟡 with language DE:\n%s", idx)
	}
	if !strings.Contains(idx, "🟢 | `beta`") {
		t.Errorf("schedule pre-live must never override 🟢:\n%s", idx)
	}
	if !strings.Contains(idx, preliveLine("gamma", "2026-11-19T19:50Z")) {
		t.Errorf("an existing 🟡 keeps its own time:\n%s", idx)
	}
	if !strings.Contains(idx, "&nbsp; | `delta`") {
		t.Errorf("delta starts in 20 min, outside the window:\n%s", idx)
	}
	if strings.Contains(idx, "dormant") {
		t.Errorf("inactive streamer reactivated:\n%s", idx)
	}
	if n := gitOutput(t, origin, "rev-list", "--count", "main"); n == before {
		t.Fatal("no commit")
	}
	after := gitOutput(t, origin, "rev-parse", "main")

	// The next minute's check doesn't touch the site again.
	s.process(statusJob{kind: jobFunc, fn: s.checkSchedulePrelive})
	if gitOutput(t, origin, "rev-parse", "main") != after {
		t.Error("a second check committed again")
	}

	// It expires like any other 🟡, and isn't re-applied.
	freezeTime(t, schedNow.Add(61*time.Minute))
	s.reconcile()
	s.process(statusJob{kind: jobFunc, fn: s.checkSchedulePrelive})
	if idx := gitOutput(t, origin, "show", "main:index.md"); !strings.Contains(idx, "&nbsp; | `alpha`") {
		t.Errorf("schedule 🟡 didn't expire:\n%s", idx)
	}
}

func TestSchedulePreliveSkips(t *testing.T) {
	for name, g := range map[string]scheduledSegment{
		"canceled":       func() scheduledSegment { g := soon(5*time.Minute, ""); g.Canceled = true; return g }(),
		"vacation":       func() scheduledSegment { g := soon(5*time.Minute, ""); g.Vacation = true; return g }(),
		"other category": soon(5*time.Minute, "Minecraft"),
		"already began":  soon(-time.Minute, ""),
	} {
		t.Run(name, func(t *testing.T) {
			s, origin := newSchedulePreliveRepo(t, map[string]scheduledSegment{"alpha": g})
			before := gitOutput(t, origin, "rev-parse", "main")
			s.process(statusJob{kind: jobFunc, fn: s.checkSchedulePrelive})
			if gitOutput(t, origin, "rev-parse", "main") != before {
				t.Errorf("site changed:\n%s", gitOutput(t, origin, "show", "main:index.md"))
			}
		})
	}
}

func TestSchedulePreliveSkipsLiveStreamer(t *testing.T) {
	s, origin := newSchedulePreliveRepo(t, map[string]scheduledSegment{"alpha": soon(5*time.Minute, "")})
	// Live, but outside the infosec categories, so the row is offline.
	s.client = &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(helix.Stream{UserID: "1", UserLogin: "alpha", GameName: "Minecraft"}), nil
	}}
	before := gitOutput(t, origin, "rev-parse", "main")
	s.process(statusJob{kind: jobFunc, fn: s.checkSchedulePrelive})
	if gitOutput(t, origin, "rev-parse", "main") != before {
		t.Error("a live streamer was marked 🟡")
	}
}

func TestSchedulePreliveRetriesWhenHelixFails(t *testing.T) {
	s, origin := newSchedulePreliveRepo(t, map[string]scheduledSegment{"alpha": soon(5*time.Minute, "")})
	s.client = &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return nil, errors.New("down")
	}}
	s.process(statusJob{kind: jobFunc, fn: s.checkSchedulePrelive})
	s.client = notLive()
	s.process(statusJob{kind: jobFunc, fn: s.checkSchedulePrelive})
	if idx := gitOutput(t, origin, "show", "main:index.md"); !strings.Contains(idx, "🟡 | `alpha`") {
		t.Errorf("not retried after a Helix failure:\n%s", idx)
	}
}

// The refresh runs off the worker while handlers and nextStreamAt read the
// state; run with -race.
func TestScheduleConcurrentRefreshAndReads(t *testing.T) {
	s, _ := newScheduleRepo(t)
	s.client = notLive()
	s.startWorker()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			s.refreshSchedules(s.runInWorker)
		}
	}()
	for i := 0; i < 50; i++ {
		get(t, s, "/streamstatus/schedule.json", nil)
		get(t, s, "/streamstatus/schedule.ics?login=alpha", nil)
		s.nextStreamAt("1")
		s.enqueue(statusJob{kind: jobFunc, fn: s.checkSchedulePrelive})
	}
	wg.Wait()
	s.waitIdle()
}
