package main

import (
	"testing"
)

func TestStripCompletionPlaceholders(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/memory show <pat|#id>", "/memory show"},
		{"/agent switch <name> [as primary|escalation]", "/agent switch"},
		{"/escalate <msg>", "/escalate"},
		{"/agent add name=… url=… model=… [provider=…]", "/agent add name=… url=… model=…"},
		{"/primary", "/primary"},
		{"/export <path>", "/export"},
		// No placeholders — unchanged.
		{"/memory global", "/memory global"},
		// Nested not supported but shouldn't crash.
		{"<a <b> c>", "c>"},
		// Brackets in normal text (not a placeholder) — only strips when matched.
		{"hello [world]", "hello"},
		// Empty.
		{"", ""},
		// Only placeholders — collapses to empty.
		{"<param> [opt]", ""},
		// Multiline input must preserve newlines.
		{"line one\nline two\nline three", "line one\nline two\nline three"},
		// Multiline with placeholder on one line only.
		{"/memory show <pat|#id>\nsome other line", "/memory show\nsome other line"},
	}
	for _, c := range cases {
		got := stripCompletionPlaceholders(c.in)
		if got != c.want {
			t.Errorf("stripCompletionPlaceholders(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTabInputPrefix(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"/mem", "/mem"},
		{"/memory show", ""}, // last word is "show", not a slash token
		{"@src/", "@src/"},
		{"hello /esc", "/esc"},
		{"", ""},
		{"hello world", ""},
		{"/memory ", ""}, // trailing space → no prefix
	}
	for _, c := range cases {
		got := tabInputPrefix(c.input)
		if got != c.want {
			t.Errorf("tabInputPrefix(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestBuildTabMatches_SlashCmd(t *testing.T) {
	matches, idx := buildTabMatches("/mem", ".")
	if len(matches) == 0 {
		t.Fatal("expected matches for /mem")
	}
	if idx != 0 {
		t.Errorf("expected initial idx 0, got %d", idx)
	}
	for _, m := range matches {
		if len(m) < 4 || m[:4] != "/mem" {
			t.Errorf("match %q does not start with /mem", m)
		}
	}
}

func TestBuildTabMatches_NoMatch(t *testing.T) {
	matches, _ := buildTabMatches("/zzznomatch", ".")
	if len(matches) != 0 {
		t.Errorf("expected no matches, got %v", matches)
	}
}

func TestBuildTabMatches_TrailingSpace(t *testing.T) {
	matches, _ := buildTabMatches("/memory ", ".")
	if len(matches) != 0 {
		t.Errorf("expected no matches with trailing space, got %v", matches)
	}
}
