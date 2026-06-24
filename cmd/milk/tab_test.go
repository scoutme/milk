package main

import (
	"strings"
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
		// Nested not supported but shouldn't crash — no slash cmd, left untouched.
		{"<a <b> c>", "<a <b> c>"},
		// Brackets in normal text without slash cmd — untouched.
		{"hello [world]", "hello [world]"},
		// Empty.
		{"", ""},
		// Only placeholders without slash cmd — untouched.
		{"<param> [opt]", "<param> [opt]"},
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
	matches, idx, base := buildTabMatches("/mem", ".")
	if len(matches) == 0 {
		t.Fatal("expected matches for /mem")
	}
	if idx != 0 {
		t.Errorf("expected initial idx 0, got %d", idx)
	}
	if base != "" {
		t.Errorf("expected empty replaceBase for top-level completion, got %q", base)
	}
	for _, m := range matches {
		if len(m) < 4 || m[:4] != "/mem" {
			t.Errorf("match %q does not start with /mem", m)
		}
	}
}

func TestBuildTabMatches_NoMatch(t *testing.T) {
	matches, _, _ := buildTabMatches("/zzznomatch", ".")
	if len(matches) != 0 {
		t.Errorf("expected no matches, got %v", matches)
	}
}

func TestBuildTabMatches_TrailingSpace(t *testing.T) {
	// "/memory " → subcommand listing: all /memory variant sigs.
	matches, _, base := buildTabMatches("/memory ", ".")
	if len(matches) == 0 {
		t.Fatal("expected subcommand matches for '/memory '")
	}
	if base != "/memory" {
		t.Errorf("expected replaceBase \"/memory\", got %q", base)
	}
	for _, m := range matches {
		if !strings.HasPrefix(m, "/memory") {
			t.Errorf("subcommand match %q does not start with /memory", m)
		}
	}
}

func TestBuildTabMatches_SubcommandPartial(t *testing.T) {
	// "/memory sh" → only variants whose subcommand starts with "sh".
	matches, _, base := buildTabMatches("/memory sh", ".")
	if len(matches) == 0 {
		t.Fatal("expected subcommand matches for '/memory sh'")
	}
	if base != "/memory" {
		t.Errorf("expected replaceBase \"/memory\", got %q", base)
	}
	for _, m := range matches {
		words := strings.Fields(m)
		if len(words) < 2 {
			t.Errorf("match %q has no subcommand word", m)
			continue
		}
		if !strings.HasPrefix(strings.ToLower(words[1]), "sh") {
			t.Errorf("match %q subcommand does not start with 'sh'", m)
		}
	}
}

func TestBuildTabMatches_SubcommandNoMatch(t *testing.T) {
	// "/memory zzz" → no variants match.
	matches, _, _ := buildTabMatches("/memory zzz", ".")
	if len(matches) != 0 {
		t.Errorf("expected no matches for '/memory zzz', got %v", matches)
	}
}

func TestBuildTabMatches_UnknownCmdTrailingSpace(t *testing.T) {
	// "/zzz " → unknown command, no variants → no matches.
	matches, _, _ := buildTabMatches("/zzz ", ".")
	if len(matches) != 0 {
		t.Errorf("expected no matches for '/zzz ', got %v", matches)
	}
}
