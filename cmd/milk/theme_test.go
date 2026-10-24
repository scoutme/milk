package main

import (
	"strings"
	"testing"
)

func TestThemeOffKeepsOriginalLook(t *testing.T) {
	t.Cleanup(func() { setTheme("off") })
	if !setTheme("off") {
		t.Fatal("off must be a valid theme")
	}
	if th.modern {
		t.Fatal("off theme must not be modern")
	}
	if got := userEcho("❯ ", "hi"); got != "❯ hi\n" {
		t.Errorf("off userEcho = %q, want the original label+input line", got)
	}
	if pulseColors != originalPulse {
		t.Error("off must keep the original yellow pulse ramp")
	}
	if accentStyle() != "\033[1;38;2;255;208;60m" {
		t.Error("off must keep the original gold emphasis")
	}
}

func TestThemeSwitchAndReject(t *testing.T) {
	t.Cleanup(func() { setTheme("off") })
	for _, name := range []string{"supermilk"} {
		if !setTheme(name) || !th.modern || th.name != name {
			t.Fatalf("setTheme(%q) did not activate a modern theme", name)
		}
		if !strings.Contains(userEcho("❯ ", "hi"), "YOU") {
			t.Errorf("%s: user echo should carry the YOU label", name)
		}
	}
	if setTheme("nope") {
		t.Error("unknown theme must be rejected")
	}
	if th.name != "supermilk" {
		t.Errorf("a rejected switch must keep the active theme, got %q", th.name)
	}
	if setTheme("OFF"); th.modern {
		t.Error("theme names are case-insensitive; OFF should switch back")
	}
}
