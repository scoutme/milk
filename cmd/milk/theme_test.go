package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestThemeOffKeepsOriginalLook(t *testing.T) {
	t.Cleanup(func() { setTheme("off") })
	if !setTheme("off") {
		t.Fatal("off must be a valid theme")
	}
	if th.skin {
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
		if !setTheme(name) || !th.skin || th.name != name {
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
	if setTheme("OFF"); th.skin {
		t.Error("theme names are case-insensitive; OFF should switch back")
	}
}

// TestModernToolRows_UsesTheSharedToolMarker pins the tool-row marker across
// skins: the supermilk skin restyles tool rows (accent marker, bold-muted
// name, muted summary) but must keep the default theme's shared tool glyph —
// » (ADR-0075, the font-safe double closing brackets), accent-tinted — so a
// transcript reads the same whichever skin paints it. Rows seeded from
// pre-ADR-0075 sessions (⚙) land on the same marker.
func TestModernToolRows_UsesTheSharedToolMarker(t *testing.T) {
	// The accent assertion needs real SGR bytes (colorize no-ops off a TTY).
	oldTTY, oldName := isTTY, th.name
	isTTY = true
	setTheme("supermilk")
	t.Cleanup(func() {
		isTTY = oldTTY
		setTheme(oldName)
	})

	for _, in := range []string{"» Bash: go test ./...", "⚙ Bash: go test ./..."} {
		raw := modernToolRows(in)
		if !strings.HasPrefix(raw, colorize("»", tkAccent.sgrFg())+" ") {
			t.Errorf("modernToolRows(%q) = %q, want the shared » marker in the accent", in, raw)
		}
		if out := ansi.Strip(raw); strings.Contains(out, "▸") || strings.Contains(out, "⚙") {
			t.Errorf("modernToolRows(%q) = %q, must not introduce another marker", in, out)
		}
	}
}
