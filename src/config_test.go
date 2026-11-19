package main

import "testing"

func TestRepoFromEnv(t *testing.T) {
	tests := []struct {
		in, wantURL, wantDir string
		wantErr              bool
	}{
		{"", "https://github.com/infosecstreams/infosecstreams.github.io", "infosecstreams.github.io", false},
		{"  ", "https://github.com/infosecstreams/infosecstreams.github.io", "infosecstreams.github.io", false},
		{"https://github.com/me/fork", "https://github.com/me/fork", "fork", false},
		{"https://github.com/me/fork.git", "https://github.com/me/fork", "fork", false},
		{"https://github.com/me/fork/", "https://github.com/me/fork", "fork", false},
		{"github.com/me/fork", "", "", true},
		{"http://github.com/me/fork", "", "", true},
		{"https://github.com/me", "", "", true},
		{"https://github.com/me/fork/tree/main", "", "", true},
		{"https://github.com/me/.git", "", "", true},
	}
	for _, tt := range tests {
		gotURL, gotDir, err := repoFromEnv(tt.in)
		if (err != nil) != tt.wantErr || gotURL != tt.wantURL || gotDir != tt.wantDir {
			t.Errorf("repoFromEnv(%q) = %q, %q, %v; want %q, %q, err=%v", tt.in, gotURL, gotDir, err, tt.wantURL, tt.wantDir, tt.wantErr)
		}
	}
}
