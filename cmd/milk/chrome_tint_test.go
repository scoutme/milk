package main

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// A bar whose spans close with their own reset must not fall back to the
// terminal's colors midway: the bar's background is re-applied after every
// reset.
func TestTintChrome_ReappliesBackgroundAfterResets(t *testing.T) {
	oldTTY := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = oldTTY })
	oldTheme := th
	th = themes["off"]
	t.Cleanup(func() { th = oldTheme })

	bg := chromeHeaderBg.sgrBg()
	bar := "one " + dim("two") + " three " + dim("four")
	out := tintChrome(bar, chromeHeaderFg.sgrFg(), bg)
	resets := strings.Count(bar, ansiReset)
	if got := strings.Count(out, bg); got < resets+1 {
		t.Errorf("background applied %d times, want at least %d (start + after each of %d resets)", got, resets+1, resets)
	}
}

func TestTintChrome_LeavesTheSkinAlone(t *testing.T) {
	oldTTY, oldName := isTTY, th.name
	isTTY = true
	setTheme("supermilk")
	t.Cleanup(func() {
		isTTY = oldTTY
		setTheme(oldName)
	})
	if got := tintChrome("a "+dim("b"), chromeHeaderBg.sgrBg()); got != "a "+dim("b") {
		t.Errorf("the skin paints its own ground, got %q", got)
	}
}

// Switching theme asks the terminal for its background again, so "off" after
// the cream skin does not keep the skin's light verdict on a dark terminal.
func TestThemeSwitch_RequestsBackgroundColor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	oldTheme := th
	t.Cleanup(func() { setTheme(oldTheme.name) })

	m := layoutTestModel(t, 80, 24)
	_, cmd := m.handleThemeCmd("off")
	if cmd == nil {
		t.Fatal("switching theme must return a command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected a batch of commands")
	}
	want := reflect.TypeOf(tea.RequestBackgroundColor())
	for _, c := range batch {
		if c != nil && reflect.TypeOf(c()) == want {
			return
		}
	}
	t.Error("the batch must include tea.RequestBackgroundColor")
}

func TestTagline_FollowsTheTheme(t *testing.T) {
	oldName := th.name
	t.Cleanup(func() { setTheme(oldName) })

	setTheme("off")
	if got := taglineText(); got != "switch models, not context." {
		t.Errorf("default tagline = %q", got)
	}
	setTheme("supermilk")
	if got := taglineText(); got != "Got models? Got milk." {
		t.Errorf("supermilk tagline = %q", got)
	}
}
