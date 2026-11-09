package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/plugin"
)

// resetPluginState gives a test the distribution layer's state root empty
// and the process-wide registry consistent with it (the state root is
// package-shared under the TestMain sandbox — TestInitPlugins_EndToEnd
// leaves a demo install behind). Cleanup wipes everything this suite
// creates so no sibling test inherits it.
func resetPluginState(t *testing.T) {
	t.Helper()
	state, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	wipe := func() {
		os.RemoveAll(filepath.Join(state, "plugins"))
		os.RemoveAll(filepath.Join(state, "marketplaces"))
		os.Remove(filepath.Join(state, "plugin-installs.json"))
		initPlugins(config.Config{})
	}
	wipe()
	t.Cleanup(wipe)
}

// writeStateFile writes a file under the shared state root.
func writeStateFile(t *testing.T, rel, content string) string {
	t.Helper()
	state, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(state, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// seedSuiteMarketplace lays down a registered marketplace exactly as
// `plugins marketplace add` leaves it: catalog + a relative-source plugin
// with a skill. Zero network — the install path exercised below is the
// marketplace-relative one (InstallFromDir).
func seedSuiteMarketplace(t *testing.T) {
	t.Helper()
	writeStateFile(t, "marketplaces/mp1/marketplace.json",
		`{"name":"mp1","owner":{"name":"t"},"plugins":[
		  {"name":"demo","description":"demo plugin","source":"./demo"}]}`)
	writeStateFile(t, "marketplaces/mp1/demo/plugin.json",
		`{"$schema":"`+plugin.SchemaPlugin+`","name":"demo","version":"0.1.0","description":"demo plugin"}`)
	writeStateFile(t, "marketplaces/mp1/demo/skills/ship/SKILL.md",
		"---\nname: ship\ndescription: Ship safely.\n---\n# Ship\n")
}

// TestPluginsActionRun_UsageErrors pins the host-neutral usage contract:
// every malformed invocation fails loud with a "usage:" lead both hosts
// render verbatim.
func TestPluginsActionRun_UsageErrors(t *testing.T) {
	resetPluginState(t)
	cases := [][]string{
		nil,
		{"bogus"},
		{"add"},
		{"marketplace"},
		{"marketplace", "bogus"},
		{"marketplace", "add"},
		{"install", "no-at-sign"},
		{"details"},
		{"remove"},
		{"shim"},
	}
	for _, tokens := range cases {
		text, mutated, err := pluginsActionRun(tokens, pluginsActionOpts{})
		if err == nil || !strings.HasPrefix(err.Error(), "usage:") {
			t.Errorf("pluginsActionRun(%v): err = %v, want usage: error", tokens, err)
		}
		if text != "" || mutated {
			t.Errorf("pluginsActionRun(%v): text=%q mutated=%v — usage errors carry neither", tokens, text, mutated)
		}
	}
	// A non-usage failure (unknown marketplace) is NOT dressed as usage.
	if _, _, err := pluginsActionRun([]string{"install", "nope@missing-mkt"}, pluginsActionOpts{}); err == nil || strings.HasPrefix(err.Error(), "usage:") {
		t.Errorf("install from a missing marketplace: err = %v, want a real (non-usage) error", err)
	}
}

// TestPluginsActionRun_EmptyStateReport keeps the empty-state messages
// stable (they are what both hosts print on a fresh install).
func TestPluginsActionRun_EmptyStateReport(t *testing.T) {
	resetPluginState(t)
	if text, mutated, err := pluginsActionRun([]string{"marketplace", "list"}, pluginsActionOpts{}); err != nil || mutated || text != "no marketplaces registered" {
		t.Errorf("marketplace list (empty) = %q mutated=%v err=%v", text, mutated, err)
	}
	if text, mutated, err := pluginsActionRun([]string{"list"}, pluginsActionOpts{}); err != nil || mutated || !strings.Contains(text, "nothing installed") {
		t.Errorf("list (empty) = %q mutated=%v err=%v", text, mutated, err)
	}
	if text, mutated, err := pluginsActionRun([]string{"update"}, pluginsActionOpts{}); err != nil || mutated || !strings.Contains(text, "nothing to update") {
		t.Errorf("update (empty) = %q mutated=%v err=%v", text, mutated, err)
	}
}

// TestPluginsActionRun_TUIEndToEnd drives the REAL TUI path — dispatch →
// action goroutine → pluginsActionMsg → handler — through install, view,
// details and remove, asserting the mutation hot-reloads the plugin set
// (the suite's whole point: no restart) with zero network involved.
func TestPluginsActionRun_TUIEndToEnd(t *testing.T) {
	// The model fixture's SandboxHome re-points HOME — take it first so
	// every state path below derives from the final sandbox root.
	m := newTestModelForPanels(t)
	resetPluginState(t)
	seedSuiteMarketplace(t)
	state, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	setTestPlugins(t, nil) // isolate the registry view; restored after

	// --- install via the full TUI path.
	mm, cmd := m.handlePluginsCmd("install demo@mp1")
	if cmd == nil {
		t.Fatal("dispatch must return an action command")
	}
	if !mm.(model).pluginsBusy {
		t.Error("pluginsBusy must be set while an action runs")
	}
	raw := cmd()
	got, ok := raw.(pluginsActionMsg)
	if !ok {
		t.Fatalf("action command yielded %T, want pluginsActionMsg", raw)
	}
	if got.err != nil {
		t.Fatalf("install: %v", got.err)
	}
	if !got.mutated || !strings.Contains(got.text, "installed demo") || !strings.Contains(got.text, "skill ship") {
		t.Fatalf("install msg: mutated=%v text=%q", got.mutated, got.text)
	}
	mm2, _ := mm.(model).handlePluginsActionMsg(got)
	if mm2.(model).pluginsBusy {
		t.Error("pluginsBusy must clear when the action lands")
	}
	out := mm2.(model).transcript.String()
	if !strings.Contains(out, "installed demo") || !strings.Contains(out, "reloaded") {
		t.Errorf("transcript after install:\n%s", out)
	}

	// The reload happened inside the handler: the loader view lists it,
	// skill included.
	view := renderPluginsView()
	if !strings.Contains(view, "demo") || !strings.Contains(view, "skills: ship") {
		t.Errorf("loader view after install reload:\n%s", view)
	}

	// --- details + marketplace list through the runner (shared layer).
	if text, _, err := pluginsActionRun([]string{"details", "demo"}, pluginsActionOpts{}); err != nil || !strings.Contains(text, "skills:") || !strings.Contains(text, "origin:") {
		t.Errorf("details demo = %q err=%v", text, err)
	}
	if text, _, err := pluginsActionRun([]string{"marketplace", "list"}, pluginsActionOpts{}); err != nil || !strings.Contains(text, "mp1") {
		t.Errorf("marketplace list = %q err=%v", text, err)
	}
	// Installed record exists (bookkeeping written by the relative install).
	if text, _, err := pluginsActionRun([]string{"list"}, pluginsActionOpts{}); err != nil || !strings.Contains(text, "demo") {
		t.Errorf("list = %q err=%v", text, err)
	}

	// --- remove via the full TUI path; the reload must empty the view.
	mm3, cmd := mm2.(model).handlePluginsCmd("remove demo")
	if cmd == nil {
		t.Fatal("remove dispatch must return an action command")
	}
	raw = cmd()
	got, ok = raw.(pluginsActionMsg)
	if !ok || got.err != nil || !got.mutated {
		t.Fatalf("remove msg: ok=%v err=%v mutated=%v", ok, got.err, got.mutated)
	}
	mm4, _ := mm3.(model).handlePluginsActionMsg(got)
	if out := mm4.(model).transcript.String(); !strings.Contains(out, "removed demo") || !strings.Contains(out, "reloaded") {
		t.Errorf("transcript after remove:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(state, "plugins", "demo")); !os.IsNotExist(err) {
		t.Errorf("install dir must be gone after remove (stat err=%v)", err)
	}
	if view := renderPluginsView(); !strings.Contains(view, "none installed") {
		t.Errorf("loader view after remove reload:\n%s", view)
	}
}

// TestHandlePluginsCmd_SuiteDispatch pins the dispatcher's own rules:
// busy refusal, parse-time feedback (no goroutine spawned), and the
// network announcement for fetch actions.
func TestHandlePluginsCmd_SuiteDispatch(t *testing.T) {
	m := newTestModelForPanels(t)

	// One action at a time.
	mm, cmd := m.handlePluginsCmd("list")
	if cmd == nil {
		t.Fatal("a valid subcommand must dispatch")
	}
	busyModel := mm.(model)
	busyModel.pluginsBusy = true
	if _, cmd := busyModel.handlePluginsCmd("list"); cmd != nil {
		t.Error("a second action while busy must be refused synchronously")
	}
	if out := busyModel.transcript.String(); !strings.Contains(out, "another operation") {
		t.Errorf("busy refusal not announced:\n%s", out)
	}
	_ = cmd // first dispatch's command intentionally never runs (no state mutation: list)

	// Unknown flag: rejected at parse time, no command, no busy latch.
	if _, cmd := m.handlePluginsCmd("add --nope"); cmd != nil {
		t.Error("an unknown flag must fail at parse time (no goroutine)")
	}
	if out := m.transcript.String(); !strings.Contains(out, "unknown flag") {
		t.Errorf("unknown flag not reported:\n%s", out)
	}

	// Network actions announce themselves; the command is real (never run
	// here — this test must not fetch).
	mm, cmd = m.handlePluginsCmd("add owner/repo")
	if cmd == nil {
		t.Fatal("add must dispatch")
	}
	if !mm.(model).pluginsBusy {
		t.Error("add must latch the busy flag")
	}
	if out := mm.(model).transcript.String(); !strings.Contains(out, "working") {
		t.Errorf("network action not announced:\n%s", out)
	}
}

// TestPluginsTUIArgParsing covers the TUI flag grammar (the CLI's
// --ref/--sha as inline tokens).
func TestPluginsTUIArgParsing(t *testing.T) {
	tokens, opts, err := parsePluginsTUIArgs("add owner/repo --ref v1 --sha deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 || tokens[0] != "add" || tokens[1] != "owner/repo" {
		t.Errorf("tokens = %v", tokens)
	}
	if opts.ref != "v1" || opts.sha != "deadbeef" {
		t.Errorf("opts = %+v", opts)
	}
	if _, _, err := parsePluginsTUIArgs("add --ref"); err == nil || !strings.Contains(err.Error(), "needs a value") {
		t.Errorf("dangling --ref: err = %v", err)
	}
	if _, _, err := parsePluginsTUIArgs(""); err == nil || !strings.HasPrefix(err.Error(), "usage:") {
		t.Errorf("empty args: err = %v", err)
	}
}
