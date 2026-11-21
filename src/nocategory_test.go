package main

import (
	"testing"

	"github.com/nicklaw5/helix/v2"
)

func TestIsInfosecCategory(t *testing.T) {
	for cat, want := range map[string]bool{
		"Science & Technology":          true,
		"just chatting":                 true,
		"Software and Game Development": true,
		"Talk Shows & Podcasts":         true,
		"":                              true, // no category set
		"  ":                            true,
		"Unlisted on Twitch":            true,
		"VALORANT":                      false,
		"World of Warcraft":             false,
		"IRL":                           false,
	} {
		if got := isInfosecCategory(cat); got != want {
			t.Errorf("isInfosecCategory(%q) = %v, want %v", cat, got, want)
		}
	}
	if !countsAsInfosec(backfillCategory) {
		t.Error("backfilled time with no known category should still count")
	}
}

func TestReconcileShowsStreamWithNoCategoryOnline(t *testing.T) {
	live := map[string]helix.Stream{"brakesec": {UserLogin: "brakesec", GameName: ""}}
	changes := diffLiveState([]string{"BrakeSec"}, nil, map[string]bool{}, live)
	if len(changes) != 1 || !changes[0].online {
		t.Fatalf("a live stream with no category should be shown online, got %+v", changes)
	}
}
