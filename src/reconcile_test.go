package main

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nicklaw5/helix/v2"
)

func TestOnlineStreamers(t *testing.T) {
	md := strings.Join([]string{
		"# Streamers",
		"| --: | --- | :-- | :-- |",
		onlineLine("TheLaluka"),
		offlineLine("Garr_7"),
		"🟢 | `antisyphon` | x",
		"some prose mentioning `code` 🟢 later",
	}, "\n")
	got := onlineStreamers(md)
	want := map[string]bool{"thelaluka": true, "antisyphon": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDiffLiveState(t *testing.T) {
	chatting := func(user string) helix.Stream {
		return helix.Stream{UserLogin: strings.ToLower(user), UserName: user, GameName: "Just Chatting", Language: "en"}
	}
	gaming := func(user string) helix.Stream {
		return helix.Stream{UserLogin: strings.ToLower(user), UserName: user, GameName: "Minecraft"}
	}
	tests := []struct {
		name     string
		active   []string
		inactive []string
		online   map[string]bool
		live     map[string]helix.Stream
		want     []statusChange
	}{
		{
			name:   "in sync",
			active: []string{"alpha", "beta"},
			online: map[string]bool{"alpha": true},
			live:   map[string]helix.Stream{"alpha": chatting("alpha")},
		},
		{
			// TheLaluka after the missed stream.offline on 2026-11-18.
			name:   "stuck online after missed offline",
			active: []string{"TheLaluka"},
			online: map[string]bool{"thelaluka": true},
			want:   []statusChange{{name: "TheLaluka", online: false}},
		},
		{
			name:   "missed online",
			active: []string{"antisyphon"},
			live:   map[string]helix.Stream{"antisyphon": chatting("antisyphon")},
			want:   []statusChange{{name: "antisyphon", online: true, stream: chatting("antisyphon")}},
		},
		{
			name:   "live outside infosec categories counts as offline",
			active: []string{"alpha", "beta"},
			online: map[string]bool{"alpha": true},
			live:   map[string]helix.Stream{"alpha": gaming("alpha"), "beta": gaming("beta")},
			want:   []statusChange{{name: "alpha", online: false}},
		},
		{
			name:   "csv casing differs from helix",
			active: []string{"thelaluka"},
			live:   map[string]helix.Stream{"thelaluka": chatting("TheLaluka")},
			want:   []statusChange{{name: "TheLaluka", online: true, stream: chatting("TheLaluka")}},
		},
		{
			// Garr_7 on 2026-11-05: live, but moved back to inactive.
			name:     "live inactive streamer is reactivated",
			inactive: []string{"Garr_7", "idle"},
			live:     map[string]helix.Stream{"garr_7": chatting("Garr_7"), "idle": gaming("idle")},
			want:     []statusChange{{name: "Garr_7", online: true, stream: chatting("Garr_7")}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			online := tt.online
			if online == nil {
				online = map[string]bool{}
			}
			got := diffLiveState(tt.active, tt.inactive, online, tt.live)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestFetchLiveByLoginBatches(t *testing.T) {
	logins := make([]string, 250)
	for i := range logins {
		logins[i] = fmt.Sprintf("User%d", i)
	}
	f := &fakeStreams{fn: func(_ int, p *helix.StreamsParams) (*helix.StreamsResponse, error) {
		// Everyone whose name ends in 7 is live.
		var out []helix.Stream
		for _, l := range p.UserLogins {
			if strings.HasSuffix(l, "7") {
				out = append(out, helix.Stream{UserLogin: strings.ToLower(l), UserName: l})
			}
		}
		return streamsResp(out...), nil
	}}
	s := &StreamersRepo{client: f}
	live, err := s.fetchLiveByLogin(logins)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("got %d calls, want 3", len(f.calls))
	}
	for _, c := range f.calls {
		if len(c.UserLogins) > 100 || c.First != 100 {
			t.Errorf("batch of %d logins with first=%d, want <=100 and first=100", len(c.UserLogins), c.First)
		}
	}
	if len(live) != 25 {
		t.Errorf("got %d live, want 25", len(live))
	}
	if _, ok := live["user247"]; !ok {
		t.Error("user247 missing from live map (keys must be lowercased)")
	}
}

func TestFetchLiveByLoginFailsOnAnyBatchError(t *testing.T) {
	logins := make([]string, 150)
	for i := range logins {
		logins[i] = fmt.Sprint(i)
	}
	f := &fakeStreams{fn: func(call int, _ *helix.StreamsParams) (*helix.StreamsResponse, error) {
		if call == 2 {
			r := streamsResp()
			r.StatusCode, r.ErrorStatus, r.Error, r.ErrorMessage = 401, 401, "Unauthorized", "Invalid OAuth token"
			return r, nil
		}
		return streamsResp(), nil
	}}
	if _, err := (&StreamersRepo{client: f}).fetchLiveByLogin(logins); err == nil {
		t.Fatal("want an error when a batch fails")
	}
}

// End to end against a real git origin: TheLaluka is stuck 🟢 but not live,
// antisyphon is live but shown offline, Garr_7 is live but inactive.
func TestReconcileCorrectsSite(t *testing.T) {
	s, origin := newGitFixture(t,
		[]string{onlineLine("TheLaluka"), offlineLine("antisyphon"), onlineLine("steady")},
		[]string{"`Garr_7` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/Garr_7) &nbsp; |"},
		"TheLaluka,\nantisyphon,\nsteady,\n", "Garr_7,\n")
	s.reconcileInactive = true
	s.client = &fakeStreams{fn: func(_ int, p *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(
			helix.Stream{UserLogin: "antisyphon", UserName: "antisyphon", GameName: "Science & Technology", Language: "en"},
			helix.Stream{UserLogin: "steady", UserName: "steady", GameName: "Just Chatting", Language: "en"},
			helix.Stream{UserLogin: "garr_7", UserName: "Garr_7", GameName: "Just Chatting", Language: "en"},
		), nil
	}}

	s.reconcile()

	idx := gitOutput(t, origin, "show", "main:index.md")
	for _, want := range []string{"&nbsp; | `TheLaluka`", "🟢 | `antisyphon`", "🟢 | `steady`", "🟢 | `Garr_7`"} {
		if !strings.Contains(idx, want) {
			t.Errorf("origin/main index.md missing %q:\n%s", want, idx)
		}
	}
	if csv := gitOutput(t, origin, "show", "main:streamers.csv"); !strings.Contains(csv, "Garr_7") {
		t.Errorf("Garr_7 not moved to streamers.csv:\n%s", csv)
	}
	// One commit per correction, none for the streamer already in sync.
	if n := strings.Count(gitOutput(t, origin, "log", "--format=%s", "main"), "\n"); n != 4 {
		t.Errorf("got %d commits, want 4 (seed + 3 corrections):\n%s", n, gitOutput(t, origin, "log", "--oneline", "main"))
	}
}

func TestReconcileSkipsInactiveUnlessEnabled(t *testing.T) {
	s, origin := newGitFixture(t, []string{offlineLine("alpha")},
		[]string{"`Garr_7` | x |"}, "alpha,\n", "Garr_7,\n")
	f := &fakeStreams{fn: func(_ int, p *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return streamsResp(helix.Stream{UserLogin: "garr_7", UserName: "Garr_7", GameName: "Just Chatting"}), nil
	}}
	s.client = f
	before := gitOutput(t, origin, "rev-parse", "main")

	s.reconcile()

	if gitOutput(t, origin, "rev-parse", "main") != before {
		t.Error("reconcile changed the site with reconcileInactive=false")
	}
	if got := f.calls[0].UserLogins; !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Errorf("queried %v, want only active streamers", got)
	}
}

// A Helix failure must never be read as "nobody is live".
func TestReconcileDoesNothingWhenHelixFails(t *testing.T) {
	s, origin := newGitFixture(t, []string{onlineLine("alpha")}, nil, "alpha,\n", "")
	s.client = &fakeStreams{fn: func(int, *helix.StreamsParams) (*helix.StreamsResponse, error) {
		return nil, errors.New("connection reset")
	}}
	before := gitOutput(t, origin, "rev-parse", "main")
	s.reconcile()
	if gitOutput(t, origin, "rev-parse", "main") != before {
		t.Error("reconcile changed the site although Helix failed")
	}
}
