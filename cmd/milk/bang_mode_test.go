package main

import "testing"

func TestStripBangPrefix(t *testing.T) {
	tests := []struct {
		input   string
		wantCmd string
		wantOK  bool
	}{
		{"!git pull", "git pull", true},
		{"!  ls -la  ", "ls -la", true},
		{"!", "", true},
		{"!   ", "", true},
		{"git pull", "", false},
		{"", "", false},
		{"/help", "", false},
	}

	for _, tt := range tests {
		gotCmd, gotOK := stripBangPrefix(tt.input)
		if gotOK != tt.wantOK || gotCmd != tt.wantCmd {
			t.Errorf("stripBangPrefix(%q) = (%q, %v), want (%q, %v)", tt.input, gotCmd, gotOK, tt.wantCmd, tt.wantOK)
		}
	}
}
