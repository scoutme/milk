package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestThemeTextInksMarkdown: the text token is the body ink — the glamour
// document colour follows it (off declares the classic ink, so its prose is
// painted with the palette, not left to the terminal default). A theme that
// declares no text keeps glamour's own standard document ink.
func TestThemeTextInksMarkdown(t *testing.T) {
	old := th
	t.Cleanup(func() { applyThemeDef(old) })
	applyThemeDef(&themeDef{name: "inked", text: themeColor{dark: "#ff0000", light: "#00ff00"}})
	if out := colorizeGlamour("hello"); !strings.Contains(out, "38;2;255;0;0") {
		t.Errorf("body ink must follow the text token, got %q", out)
	}
	applyThemeDef(&themeDef{name: "plain"})
	if out := colorizeGlamour("hello"); strings.Contains(out, "38;2;255;0;0") {
		t.Errorf("a text-less theme must keep glamour's own ink, got %q", out)
	}
}

// TestThemeTerminalOSCs: the terminal palette is the theme's ground and
// ink — off pushes the standard black background (#000000) and its body
// ink, a theme with an explicit terminal block keeps it, and a theme with
// neither pushes nothing (the terminal's own colours stand).
func TestThemeTerminalOSCs(t *testing.T) {
	old := th
	t.Cleanup(func() { applyThemeDef(old) })

	applyThemeDef(themeOff)
	if got, want := themeTerminalOSCs(), []string{"11;#000000", "10;#d0d0d0", "12;#d0d0d0"}; !slices.Equal(got, want) {
		t.Errorf("off palette = %v, want %v", got, want)
	}
	applyThemeDef(&themeDef{name: "bare"})
	if got := themeTerminalOSCs(); len(got) != 0 {
		t.Errorf("a palette-less theme must push nothing, got %v", got)
	}
	applyThemeDef(&themeDef{name: "term",
		base:       themeColor{dark: "#111111", light: "#eeeeee"},
		text:       themeColor{dark: "#cccccc", light: "#222222"},
		termBg:     "#f8f5ee", termFg: "#3f2a26", termCursor: "#3f2a26",
	})
	got := themeTerminalOSCs()
	if len(got) < 3 || got[0] != "11;#f8f5ee" || got[1] != "10;#3f2a26" {
		t.Errorf("an explicit terminal block must win, got %v", got)
	}
}

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
