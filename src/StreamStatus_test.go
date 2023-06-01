package main

import (
	"testing"
)

func TestGenerateStreamerLine(t *testing.T) {
	s := StreamersRepo{
		streamer: "B7H30",
		tags:     []string{"CoLearning", "CoWorking", "Linux", "CyberSecurity", "BackSeatingAllowed", "TryHackMe", "TextToSpeech", "English"},
		game:     "Software and Game Development",
		online:   true,
		language: "EN",
	}
	tests := []struct {
		name       string
		game       string
		otherInfo  string
		wantResult string
		online     bool
	}{
		{
			name:       "online with game",
			otherInfo:  "&nbsp; ",
			game:       "Software and Game Development",
			wantResult: "🟢 | `B7H30` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/B7H30 \"Software and Game Development, Tags: CoLearning, CoWorking, Linux, CyberSecurity, BackSeatingAllowed, TryHackMe, TextToSpeech, English\") &nbsp; | EN",
			online:     true,
		},
		{
			name:       "online without game",
			game:       "",
			otherInfo:  "&nbsp; [<i class=\"fab fa-youtube\" style=\"color:#C00\"></i>](https://www.youtube.com/@theo6580) ",
			wantResult: "🟢 | `B7H30` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/B7H30 \"Tags: CoLearning, CoWorking, Linux, CyberSecurity, BackSeatingAllowed, TryHackMe, TextToSpeech, English\") &nbsp; [<i class=\"fab fa-youtube\" style=\"color:#C00\"></i>](https://www.youtube.com/@theo6580) | EN",
			online:     true,
		},
		{
			name:       "offline",
			game:       "",
			otherInfo:  "&nbsp; [<i class=\"fab fa-youtube\" style=\"color:#C00\"></i>](https://www.youtube.com/@theo6580)",
			wantResult: "&nbsp; | `B7H30` | [<i class=\"fab fa-twitch\" style=\"color:#9146FF\"></i>](https://www.twitch.tv/B7H30) &nbsp; [<i class=\"fab fa-youtube\" style=\"color:#C00\"></i>](https://www.youtube.com/@theo6580)",
			online:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.online {
				s.online = false
			}
			s.game = tt.game
			if tt.online && tt.game == "" {
				s.online = true
			}
			gotResult := s.generateStreamerLine(tt.otherInfo)
			if gotResult != tt.wantResult {
				t.Errorf("\nGot:    %v\nWanted: %v\n\n", gotResult, tt.wantResult)
			}
		})
	}
}
