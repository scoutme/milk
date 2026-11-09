package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/plugin"
)

// TestBarePlugins_OpensBrowseFlyover: bare /plugins keeps the installed view
// as the record and opens the flyover when registered catalogs hold entries
// — with the choice reduced to the install tokens (dir key, never ambiguous).
func TestBarePlugins_OpensBrowseFlyover(t *testing.T) {
	m := newTestModelForPanels(t)
	resetPluginState(t)
	seedAliasMarketplace(t)
	setTestPlugins(t, nil)

	mm, cmd := m.handlePluginsCmd("")
	if cmd != nil {
		t.Error("bare /plugins must not spawn an action — it opens the flyover")
	}
	w := mm.(model).pendingPluginBrowse
	if w == nil {
		t.Fatal("browse flyover must open over registered entries")
	}
	if len(w.choices) != 1 || w.choices[0].label != "beta" || w.choices[0].mktDir != "snap-alias" {
		t.Fatalf("choices = %+v, want beta from snap-alias", w.choices)
	}
	if got := strings.Join(w.choices[0].tokens(), " "); got != "install beta@snap-alias" {
		t.Errorf("tokens = %q", got)
	}
	out := mm.(model).transcript.String()
	if !strings.Contains(out, "1 available to install") || !strings.Contains(out, "none installed") {
		t.Errorf("transcript must carry the installed view AND the browse header:\n%s", out)
	}
	// The box renders with the browse title and the marketplace row.
	mbox := mm.(model)
	if box := mbox.renderAgentPicker(60); !strings.Contains(box, "install from marketplaces") ||
		!strings.Contains(box, "beta") || !strings.Contains(box, "snap-alias") {
		t.Errorf("flyover render:\n%s", box)
	}
}

// TestBarePlugins_NoCatalogs_HintsInsteadOfOpening: no registered entries →
// the old installed view plus a how-to line, no flyover state.
func TestBarePlugins_NoCatalogs_HintsInsteadOfOpening(t *testing.T) {
	m := newTestModelForPanels(t)
	resetPluginState(t)
	setTestPlugins(t, nil)

	mm, _ := m.handlePluginsCmd("")
	if mm.(model).pendingPluginBrowse != nil {
		t.Fatal("no entries → no flyover")
	}
	out := mm.(model).transcript.String()
	if !strings.Contains(out, "no marketplace entries to browse") ||
		!strings.Contains(out, "/plugins marketplace add") {
		t.Errorf("hint missing:\n%s", out)
	}
}

// TestPluginBrowse_EnterInstallsViaSuite: the whole accept path — flyover
// closes, the echoed record is a replayable command, and the install runs
// through the exact suite dispatcher (busy latch → report → hot reload).
func TestPluginBrowse_EnterInstallsViaSuite(t *testing.T) {
	m := newTestModelForPanels(t)
	resetPluginState(t)
	seedAliasMarketplace(t)
	state, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	setTestPlugins(t, nil)

	mm, _ := m.handlePluginsCmd("")
	km, cmd := mm.(model).handlePluginBrowseKey(enterKey())
	got := km.(model)
	if got.pendingPluginBrowse != nil {
		t.Fatal("flyover must close on accept")
	}
	if !got.pluginsBusy || cmd == nil {
		t.Fatal("accept must dispatch through the suite (busy latch + action cmd)")
	}
	raw := cmd()
	msg, ok := raw.(pluginsActionMsg)
	if !ok || msg.err != nil {
		t.Fatalf("action: %#v", raw)
	}
	if !msg.mutated || !strings.Contains(msg.text, "installed beta") {
		t.Fatalf("msg: mutated=%v text=%q", msg.mutated, msg.text)
	}
	final, _ := got.handlePluginsActionMsg(msg)
	out := final.(model).transcript.String()
	if !strings.Contains(out, "/plugins install beta@snap-alias") ||
		!strings.Contains(out, "installed beta") || !strings.Contains(out, "reloaded") {
		t.Errorf("transcript must record the replayable command + report + reload:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(state, "plugins", "beta")); err != nil {
		t.Errorf("install dir missing: %v", err)
	}
}

// TestPluginBrowse_EscCancels: cancel closes the flyover without touching
// the installed set — and clears any filter text with it.
func TestPluginBrowse_EscCancels(t *testing.T) {
	m := newTestModelForPanels(t)
	resetPluginState(t)
	seedAliasMarketplace(t)
	setTestPlugins(t, nil)

	mm, _ := m.handlePluginsCmd("")
	mopen := mm.(model)
	mopen.ta.SetValue("bet") // filter typed into the flyover
	km, cmd := mopen.handlePluginBrowseKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd != nil || km.(model).pendingPluginBrowse != nil {
		t.Error("esc must close the flyover without an action")
	}
	if v := km.(model).ta.Value(); v != "" {
		t.Errorf("filter text must be cleared, got %q", v)
	}
	if out := km.(model).transcript.String(); !strings.Contains(out, "browse cancelled") {
		t.Errorf("cancel notice missing:\n%s", out)
	}
}

// TestPluginBrowse_UnknownTypedKeepsFlyover: text matching no row is
// rejected loudly and the browse stays open — the direct-ref hint points at
// /plugins add instead of half-installing something guessed.
func TestPluginBrowse_UnknownTypedKeepsFlyover(t *testing.T) {
	m := newTestModelForPanels(t)
	resetPluginState(t)
	seedAliasMarketplace(t)
	setTestPlugins(t, nil)

	mm, _ := m.handlePluginsCmd("")
	munk := mm.(model)
	munk.ta.SetValue("zzz")
	km, cmd := munk.handlePluginBrowseKey(enterKey())
	if cmd != nil {
		t.Error("unknown answer must not dispatch an action")
	}
	if km.(model).pendingPluginBrowse == nil {
		t.Fatal("flyover must stay open after a rejected answer")
	}
	out := km.(model).transcript.String()
	if !strings.Contains(out, `unknown plugin "zzz"`) || !strings.Contains(out, "/plugins add") {
		t.Errorf("rejection must name the plugin and the direct-ref path:\n%s", out)
	}
}

// TestBarePlugins_DuplicateEntryNamesDisambiguate: the same entry name in
// two catalogs must yield two uniquely-labelled rows (exact-typed answers
// depend on label uniqueness), each installing from its own directory.
func TestBarePlugins_DuplicateEntryNamesDisambiguate(t *testing.T) {
	resetPluginState(t)
	writeStateFile(t, "marketplaces/a/marketplace.json",
		`{"name":"alpha","owner":{"name":"t"},"plugins":[{"name":"dupe","source":"./p"}]}`)
	writeStateFile(t, "marketplaces/a/p/plugin.json",
		`{"$schema":"`+plugin.SchemaPlugin+`","name":"dupe","version":"1.0.0","description":"d"}`)
	writeStateFile(t, "marketplaces/b/marketplace.json",
		`{"name":"beta","owner":{"name":"t"},"plugins":[{"name":"dupe","source":"./p"}]}`)
	writeStateFile(t, "marketplaces/b/p/plugin.json",
		`{"$schema":"`+plugin.SchemaPlugin+`","name":"dupe","version":"2.0.0","description":"d"}`)

	choices := browsePluginChoices()
	if len(choices) != 2 {
		t.Fatalf("choices = %+v", choices)
	}
	seen := map[string]bool{}
	for _, c := range choices {
		if seen[c.label] {
			t.Errorf("duplicate label %q", c.label)
		}
		seen[c.label] = true
		if !strings.Contains(c.label, c.mktDir) {
			t.Errorf("label %q must carry its directory", c.label)
		}
	}
}
