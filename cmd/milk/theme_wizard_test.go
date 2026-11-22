package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/testutil"
)

// The theme builder (plan PR 3): /theme new walks the steps, previews the
// candidate through the real switch path, and saves a materialized JSON
// theme with `derived_from` provenance. Esc restores the original theme.

func wizardTestModel(t *testing.T) model {
	t.Helper()
	testutil.SandboxHome(t)
	t.Setenv("USERPROFILE", t.TempDir())
	old := th.name
	t.Cleanup(func() { setTheme(old) })
	// The registry is process-global and earlier tests may have saved themes
	// into their own sandboxes — start each wizard test from builtins only.
	loadThemeRegistry("")
	m := layoutTestModel(t, 80, 24)
	out, _ := m.handleThemeCmd("new")
	m = out.(model)
	if m.pendingThemeWizard == nil {
		t.Fatal("/theme new must open the builder")
	}
	return m
}

func wizKey(m model, msg tea.KeyPressMsg) model {
	out, _ := m.handleThemeWizardKey(msg)
	return out.(model)
}

func wizDown(m model) model { return wizKey(m, tea.KeyPressMsg{Code: tea.KeyDown}) }

func wizType(m model, text string) model {
	m.ta.SetValue(text)
	return wizKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
}

func wizEnter(m model) model { return wizKey(m, tea.KeyPressMsg{Code: tea.KeyEnter}) }

func TestThemeWizardFromScratchSaves(t *testing.T) {
	m := wizardTestModel(t)

	// mode: down → "from scratch", enter.
	m = wizDown(m)
	m = wizEnter(m)
	// name
	m = wizType(m, "mytheme")
	// six core colours
	for range 6 {
		m = wizType(m, "#123456")
	}
	// more colors? no (sel 0) → terminal: derive (sel 0) → tagline
	m = wizEnter(m)
	m = wizEnter(m)
	m = wizType(m, "hand-built")
	// review: enter saves
	m = wizEnter(m)

	if m.pendingThemeWizard != nil {
		t.Fatalf("builder must close after save (err %q)", m.pendingThemeWizard.err)
	}
	if th.name != "mytheme" || !th.skin {
		t.Errorf("active theme = %q skin=%v, want the saved theme", th.name, th.skin)
	}
	if m.st.cfg.Theme != "mytheme" {
		t.Errorf("cfg.Theme = %q, want mytheme", m.st.cfg.Theme)
	}
	rt, ok := themeRegistry.Get("mytheme")
	if !ok {
		t.Fatal("the saved theme must be in the registry")
	}
	if rt.Tagline != "hand-built" {
		t.Errorf("tagline = %q", rt.Tagline)
	}
	if rt.Terminal == nil {
		t.Error("derive mode must write a terminal block")
	}
	data, err := os.ReadFile(filepath.Join(mustThemesDir(t), "mytheme.json"))
	if err != nil {
		t.Fatalf("saved file: %v", err)
	}
	if !strings.Contains(string(data), `"base"`) || !strings.Contains(string(data), "#123456") {
		t.Errorf("saved JSON must carry the typed colours:\n%s", data)
	}
}

func TestThemeWizardVariantDerives(t *testing.T) {
	m := wizardTestModel(t)

	// mode: down ×2 → "darken/lighten variant", enter → base: down → supermilk.
	m = wizDown(m)
	m = wizDown(m)
	m = wizEnter(m)
	m = wizDown(m)
	m = wizEnter(m)
	// variant step: two lightness steps right.
	m = wizKey(m, tea.KeyPressMsg{Code: tea.KeyRight})
	m = wizKey(m, tea.KeyPressMsg{Code: tea.KeyRight})
	if dl := m.pendingThemeWizard.dl; dl != 0.10 {
		t.Fatalf("lightness = %v, want 0.10", dl)
	}
	m = wizEnter(m)
	m = wizType(m, "superlight")
	m = wizEnter(m) // review → save

	if m.pendingThemeWizard != nil {
		t.Fatalf("builder must close after save (err %q)", m.pendingThemeWizard.err)
	}
	if th.name != "superlight" {
		t.Errorf("active theme = %q, want superlight", th.name)
	}
	data, err := os.ReadFile(filepath.Join(mustThemesDir(t), "superlight.json"))
	if err != nil {
		t.Fatalf("saved file: %v", err)
	}
	if !strings.Contains(string(data), `"derived_from": "supermilk"`) {
		t.Errorf("variant JSON must record its provenance:\n%s", data)
	}
	rt, _ := themeRegistry.Get("superlight")
	if rt == nil {
		t.Fatal("variant must resolve")
	}
	base, _ := themeRegistry.Get("supermilk")
	if rt.Colors.Base == base.Colors.Base {
		t.Error("the variant palette must be transformed, not copied")
	}
}

func TestThemeWizardCancelRestores(t *testing.T) {
	m := wizardTestModel(t)
	orig := m.pendingThemeWizard.orig

	// Pick a base so the preview switches the whole TUI to supermilk…
	m = wizEnter(m) // mode: from existing
	m = wizDown(m)
	m = wizEnter(m) // base: supermilk
	if th.name == orig {
		t.Fatal("picking a base must live-preview it")
	}
	m = wizKey(m, tea.KeyPressMsg{Code: tea.KeyEscape}) // esc
	if m.pendingThemeWizard != nil {
		t.Fatal("esc must close the builder")
	}
	if th.name != orig {
		t.Errorf("cancel left %q active, want %q restored", th.name, orig)
	}
}

// TestThemeChangeInvalidatesColorizedTurnCache pins the missing-live-preview
// bug: the per-turn colorized transcript cache is keyed by raw text alone,
// so a theme change that only set colorizeForce re-served the PREVIOUS
// theme's rows (the chrome changed, the transcript never did). Every theme
// activation must drop the caches.
func TestThemeChangeInvalidatesColorizedTurnCache(t *testing.T) {
	m := layoutTestModel(t, 80, 24)
	m.appendTranscript("turn one\n\nturn two\n\nturn three\n")
	_ = m.wrappedTranscript() // prime the per-turn colorized caches
	if len(m.turnColorCache) == 0 {
		t.Fatal("fixture needs completed turns in the colorized cache")
	}
	m.turnColorCache[0] = "STALE-MARKER" // same raw key, old-theme colours

	// The variant step's arrow presses run exactly this preview path.
	w := &themeWizardState{mode: twVariant, step: twStepVariant, base: "supermilk", orig: "off"}
	m.pendingThemeWizard = w
	m.themeWizardPreview(w)

	if strings.Contains(strings.Join(m.turnColorCache, "\n"), "STALE-MARKER") {
		t.Error("a theme change must re-colorize cached turns — the old theme's rows are being re-served")
	}
}

// TestThemeWizardVariantBaseListSkipsPaletteless pins the variant mode's
// "arrows do nothing" case: "off" is chrome-coded (no palette), so
// Transform has nothing to move and its preview can only no-op. The base
// step must not offer it.
func TestThemeWizardVariantBaseListSkipsPaletteless(t *testing.T) {
	m := wizardTestModel(t)
	m = wizDown(m)
	m = wizDown(m)
	m = wizEnter(m) // mode → variant → base step
	rows := m.pendingThemeWizard.choiceRows()
	if len(rows) == 0 {
		t.Fatal("variant base list must offer the palette themes")
	}
	for _, r := range rows {
		if r == "off" {
			t.Error("variant base list must skip palette-less themes (off) — arrows would preview nothing")
		}
	}
	if got := m.pendingThemeWizard.choiceLen(); got != len(rows) {
		t.Errorf("choiceLen = %d, want %d (the two must read the same list)", got, len(rows))
	}
}

func TestParseTypedColor(t *testing.T) {
	if c, ok := parseTypedColor("#a1b2c3"); !ok || c.Dark != "#a1b2c3" || c.Light != "#a1b2c3" {
		t.Errorf("fixed parse = %+v %v", c, ok)
	}
	if c, ok := parseTypedColor("#111111/#eeeeee"); !ok || c.Dark != "#111111" || c.Light != "#eeeeee" {
		t.Errorf("pair parse = %+v %v", c, ok)
	}
	if _, ok := parseTypedColor("nope"); ok {
		t.Error("garbage must be rejected")
	}
}

// mustThemesDir is the sandbox user theme dir the wizard saves into.
func mustThemesDir(t *testing.T) string {
	t.Helper()
	dir, err := config.Dir()
	if err != nil {
		t.Fatalf("state root: %v", err)
	}
	return filepath.Join(dir, "themes")
}
