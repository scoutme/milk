package main

import (
	"reflect"
	"testing"

	"github.com/scoutme/milk/internal/theme"
)

// Golden literals: the pre-registry themeDef values (theme.go before the
// themes-as-data migration, plan PR 1). The registry-resolved builtins must
// convert to exactly these, so the render layer stays byte-identical across
// the migration and every future internal/theme change is pinned against the
// look users know.

var goldenOff = &themeDef{
	name:      "off",
	toolRows:  theme.ToolRowsPlain,
	scrollbar: theme.ScrollbarClassic,
	// The classic chrome as data (PR: classic chrome-as-data): off declares
	// its palette explicitly — every chrome constant the classic path reads
	// is a declared token. thumb/accentLo/think are the derivations over
	// those declarations (pinned so derivation changes are deliberate).
	// base is the standard black background (and its standard light
	// counterpart) — the look's ground — while the header bar's own #1b1b1f
	// lives in chrome.header_bg.
	base:     themeColor{dark: "#000000", light: "#ffffff"},
	raised:   themeColor{dark: "#25252a", light: "#e5e3dd"},
	toast:    themeColor{dark: "#1a263a", light: "#d9e4f5"},
	hairline: themeColor{dark: "#3a3a41", light: "#c4c0b6"},
	thumb:    themeColor{dark: "#7a7775", light: "#87837a"},
	text:     themeColor{dark: "#d0d0d0", light: "#080808"},
	muted:    themeColor{dark: "#b9b4a8", light: "#4a463e"},
	accent:   themeColor{dark: "#ffd03c", light: "#ffd03c"},
	accentLo: themeColor{dark: "#4d3e12", light: "#fff1c5"},
	think:    themeColor{dark: "#b9b4a8", light: "#4a463e"},
	focus:    themeColor{dark: "#161d2e", light: "#e2e7f3"},
	// The chrome block: every classic surface, declared (off.json) instead of
	// held as render constants. golden-pinned like the palette so chrome
	// changes are deliberate.
	chrome: theme.Chrome{
		HeaderBg:       theme.Color{Dark: "#1b1b1f", Light: "#e9e6df"},
		PanelTitle:     theme.Color{Dark: "#888888", Light: "#555555"},
		PanelSection:   theme.Color{Dark: "#666666", Light: "#777777"},
		Border:         theme.Color{Dark: "#555555", Light: "#aaaaaa"},
		PanelAlt:       theme.Color{Dark: "#18181c", Light: "#ececef"},
		ThinkSurface:   theme.Color{Dark: "#262626", Light: "#262626"},
		PermFg:         theme.Color{Dark: "#1a1a00", Light: "#1a1a00"},
		PermBg:         theme.Color{Dark: "#b8860b", Light: "#ffd700"},
		Wordmark:       theme.Color{Dark: "#ffd060", Light: "#ffd060"},
		AgentSage:      theme.Color{Dark: "#a8b58f", Light: "#5f6b3a"},
		AgentSteel:     theme.Color{Dark: "#93a9c7", Light: "#3f5a80"},
		StaleFresh:     theme.Color{Dark: "#dcdad7", Light: "#dcdad7"},
		StaleMid:       theme.Color{Dark: "#c38237", Light: "#c38237"},
		StaleWarm:      theme.Color{Dark: "#af4128", Light: "#af4128"},
		ChangeCreated:  theme.Color{Dark: "#6fbf82", Light: "#2f7d32"},
		ChangeModified: theme.Color{Dark: "#d0a93c", Light: "#8a6a00"},
		ChangeDeleted:  theme.Color{Dark: "#e07b70", Light: "#a5372b"},
		ChangeRenamed:  theme.Color{Dark: "#8ab0e8", Light: "#3a5fa8"},
		Selection:      theme.Color{Dark: "#585858", Light: "#585858"},
		Completion:     theme.Color{Dark: "#444444", Light: "#444444"},
		Pulse: []string{
			"#3c3200", "#433a01", "#514904", "#6e6509",
			"#9d870f", "#ccaa14", "#e8c519", "#fbd71c",
			"#ffdc1e", "#fbd71c", "#e8c519", "#ccaa14",
			"#9d870f", "#6e6509", "#514904", "#433a01",
		},
	},
}

var goldenSuperMilk = &themeDef{
	name: "supermilk", skin: true,
	toolRows: theme.ToolRowsAccent, scrollbar: theme.ScrollbarSlim,
	tagline: "Got models? Got milk.",
	base:    fixed("#f8f5ee"), raised: fixed("#efebe0"), toast: fixed("#e8e3d5"),
	hairline: fixed("#dcd7c9"), thumb: fixed("#c2bfb4"),
	text: fixed("#3f2a26"), muted: fixed("#7d7b74"), think: fixed("#5d6f4a"),
	accent: fixed("#6b6140"), accentLo: fixed("#c9c5b8"),
	focus:  fixed("#e7e9f5"),
	termBg: "#f8f5ee", termFg: "#3f2a26", termCursor: "#3f2a26",
	termAnsi: [16]string{
		"#f8f5ee", "#8a4b3c", "#5f6b3a", "#7a6a2e", "#4a6280", "#7a5a78", "#4a6f6a", "#3f2a26",
		"#7d7b74", "#a85f4d", "#7a8a4e", "#9a8640", "#6482a8", "#9a7498", "#6a948c", "#2b1a17",
	},
}

func TestThemeParityBuiltins(t *testing.T) {
	for _, tc := range []struct {
		name string
		want *themeDef
	}{
		{"off", goldenOff},
		{"supermilk", goldenSuperMilk},
	} {
		got, ok := themeDefFor(tc.name)
		if !ok {
			t.Fatalf("themeDefFor(%q) not found", tc.name)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("builtin %q drifted from the golden literal:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}

// TestThemeDropInTheme: a JSON file in a theme dir resolves through the same
// converter and switches like a builtin (registry-backed setTheme).
func TestThemeDropInTheme(t *testing.T) {
	user := t.TempDir()
	writeTestTheme(t, user, "oatmilk.json", `{
		"name": "oatmilk",
		"extends": "supermilk",
		"tagline": "oat milk, no sugar",
		"colors": {"base": "#f6f2ea"}
	}`)
	loadTestRegistry(t, user, "")

	if !setTheme("oatmilk") {
		t.Fatal("setTheme(oatmilk) must succeed")
	}
	t.Cleanup(func() { setTheme("off") })
	if th.name != "oatmilk" || th.tagline != "oat milk, no sugar" {
		t.Errorf("active theme = %q %q", th.name, th.tagline)
	}
	if th.base != fixed("#f6f2ea") || th.text != fixed("#3f2a26") {
		t.Errorf("override/inherit wrong: base=%+v text=%+v", th.base, th.text)
	}
	if !th.skin || th.scrollbar != theme.ScrollbarSlim {
		t.Errorf("shape defaults wrong: skin=%v scrollbar=%s", th.skin, th.scrollbar)
	}
	if tkBase != th.base {
		t.Error("setTheme must mirror tokens into tk*")
	}
}

// TestThemeInvalidFileSkipped: a broken drop-in file never reaches the
// registry and never breaks switching.
func TestThemeInvalidFileSkipped(t *testing.T) {
	user := t.TempDir()
	writeTestTheme(t, user, "bad.json", `{"name": "bad", "colors": {"base": "#nope"}}`)
	loadTestRegistry(t, user, "")
	if setTheme("bad") {
		t.Error("invalid theme must not switch")
	}
	if len(themeRegistry.Warnings()) == 0 {
		t.Error("invalid file must leave a warning")
	}
}
