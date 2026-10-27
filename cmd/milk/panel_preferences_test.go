package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/memory"
)

// Panel state persistence (ADR-0078 as amended by ADR-0087): every
// panel's manually chosen open/closed state is a durable preference —
// saved on toggle, restored at startup as *startup layout*. Restored state
// never vetoes content-driven auto-open; the in-session veto while a
// dismissal covers live content is the manual override (ADR-0059's
// expiry). The whole mechanism is feature-gated by "remember_panel_state"
// (default on); off means session-only panels.

// TestPanelState_RoundTripAcrossSessions: a manual toggle in one session
// comes back as the panel's state in the next — startup layout, without
// an auto-open veto (ADR-0087).
func TestPanelState_RoundTripAcrossSessions(t *testing.T) {
	m := newTestModelForPanels(t)
	m.panelStatePath = filepath.Join(t.TempDir(), "panel-state.json")

	// Session 1: memory opened-then-closed (a close preference), tasks
	// opened (an open preference).
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyF1})
	m1 := updated.(model)
	updated, _ = m1.Update(tea.KeyPressMsg{Code: tea.KeyF1})
	m1 = updated.(model)
	if m1.panelMemory {
		t.Fatal("expected the second F1 to close the memory panel")
	}
	updated, _ = m1.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	m1 = updated.(model)
	if !m1.panelTasks {
		t.Fatal("expected F2 to open the tasks panel")
	}
	if err := m1.savePanelState(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Session 2: a fresh model restores both as startup layout.
	m2 := newTestModelForPanels(t)
	m2.panelStatePath = m1.panelStatePath
	m2.loadPanelState(m2.panelStatePath)
	if m2.panelMemory {
		t.Error("expected the remembered memory close to restore")
	}
	if !m2.panelTasks {
		t.Error("expected the remembered tasks open to restore")
	}
	if !m2.panelPrefs[regionMemory] || !m2.panelPrefs[regionTasks] {
		t.Error("restored preferences must seed the durable pref")
	}
	if len(m2.panelManualOverride) != 0 {
		t.Error("restored preferences are startup layout — they must not seed an auto-open veto")
	}

	// New content re-opens a remembered-closed panel: the saved state is
	// startup layout, not an eternal veto (ADR-0087).
	m2.autoOpenPanel(regionMemory)
	if !m2.panelMemory {
		t.Error("expected new content to re-open the remembered-closed panel")
	}
}

// TestPanelState_OnlyManualTogglesPersisted: auto-opened panels are not a
// preference — only panels the user has touched are written, so a
// workflow panel that happened to be open never reopens empty next session.
func TestPanelState_OnlyManualTogglesPersisted(t *testing.T) {
	m := newTestModelForPanels(t)
	m.panelStatePath = filepath.Join(t.TempDir(), "panel-state.json")

	m.autoOpenPanel(regionBackground)                        // content-driven, not manual
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyF2}) // manual
	m1 := updated.(model)
	if err := m1.savePanelState(); err != nil {
		t.Fatalf("save: %v", err)
	}

	data, err := os.ReadFile(m.panelStatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"tasks":true`) {
		t.Errorf("expected the manually toggled tasks panel in the state file, got %s", data)
	}
	if strings.Contains(string(data), "background") {
		t.Errorf("auto-opened panels must not be persisted, got %s", data)
	}
}

// TestPanelState_FeatureOffWritesNoFile: "remember_panel_state": false
// keeps panels session-only — nothing is read or written.
func TestPanelState_FeatureOffWritesNoFile(t *testing.T) {
	m := newTestModelForPanels(t)
	m.rememberPanelState = false
	m.panelStatePath = filepath.Join(t.TempDir(), "panel-state.json")

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	m1 := updated.(model)
	if err := m1.savePanelState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(m.panelStatePath); !os.IsNotExist(err) {
		t.Error("expected no state file with remember_panel_state off")
	}
}

// TestPanelState_CorruptOrUnknownFileIgnored: a damaged state file or an
// unknown panel name is skipped, never fatal and never mis-applied.
func TestPanelState_CorruptOrUnknownFileIgnored(t *testing.T) {
	dir := t.TempDir()

	m := newTestModelForPanels(t)
	m.panelStatePath = filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(m.panelStatePath, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	m.loadPanelState(m.panelStatePath)
	if m.panelMemory || len(m.panelPrefs) != 0 {
		t.Error("a corrupt state file must change nothing")
	}

	m2 := newTestModelForPanels(t)
	m2.panelStatePath = filepath.Join(dir, "unknown.json")
	if err := os.WriteFile(m2.panelStatePath, []byte(`{"version":1,"panels":{"nope":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	m2.loadPanelState(m2.panelStatePath)
	for _, region := range panelRenderOrder {
		if m2.panelPrefs[region] {
			t.Errorf("unknown panel name must not seed a pref for %s", panelSubName(region))
		}
	}
}

// TestMemoryPanel_DefaultClosedOpensOnContent: the memory panel is closed
// by default and content presence is what opens it (like other panels) —
// an in-session manual close wins while it is active; a restored
// preference does not (it is startup layout only, ADR-0087).
func TestMemoryPanel_DefaultClosedOpensOnContent(t *testing.T) {
	store, err := memory.NewStoreParams(t.TempDir(), "sess", memory.Params{})
	if err != nil {
		t.Fatal(err)
	}

	m := newTestModelForPanels(t)
	m.mem = store
	if m.panelMemory {
		t.Fatal("expected the memory panel closed by default")
	}
	m.applyMemoryContentDefault()
	if m.panelMemory {
		t.Error("an empty store must not open the memory panel")
	}

	if _, err := store.Record(context.Background(), "a fact", memory.ProducerUser, memory.ConsumerAll, memory.Roles{}, false); err != nil {
		t.Fatal(err)
	}
	m.applyMemoryContentDefault()
	if !m.panelMemory {
		t.Error("expected content presence to open the memory panel")
	}

	// An in-session manual close (the sticky override) beats the content
	// default while it is active.
	m2 := newTestModelForPanels(t)
	m2.mem = store
	m2.panelManualOverride[regionMemory] = true
	m2.applyMemoryContentDefault()
	if m2.panelMemory {
		t.Error("a manual close must beat the content default")
	}

	// A restored preference (panelPrefs) is startup layout only — the
	// content default still opens the panel (ADR-0087).
	m3 := newTestModelForPanels(t)
	m3.mem = store
	m3.panelPrefs[regionMemory] = true
	m3.applyMemoryContentDefault()
	if !m3.panelMemory {
		t.Error("a restored close must not veto the content default")
	}
}

// TestMemoryRefreshMsg_OpensOnContentAndRearms: the memory poll is the
// content-arrival detector — it auto-opens the panel when percepts appear
// (unless the user has managed it) and always re-arms itself.
func TestMemoryRefreshMsg_OpensOnContentAndRearms(t *testing.T) {
	store, err := memory.NewStoreParams(t.TempDir(), "sess", memory.Params{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Record(context.Background(), "a fact", memory.ProducerUser, memory.ConsumerAll, memory.Roles{}, false); err != nil {
		t.Fatal(err)
	}

	m := newTestModelForPanels(t)
	m.mem = store
	updated, cmd := m.Update(memoryRefreshMsg{})
	m1 := updated.(model)
	if !m1.panelMemory {
		t.Error("expected the poll to open the memory panel on content")
	}
	if cmd == nil {
		t.Error("expected the poll to re-arm regardless of panel state")
	}

	// With a manual override the poll must not fight it, but still re-arm.
	m2 := newTestModelForPanels(t)
	m2.mem = store
	m2.panelManualOverride[regionMemory] = true
	updated, cmd = m2.Update(memoryRefreshMsg{})
	m3 := updated.(model)
	if m3.panelMemory {
		t.Error("expected the manual override to suppress the poll's auto-open")
	}
	if cmd == nil {
		t.Error("expected the poll to re-arm even while suppressed")
	}
}

// TestPanelStatePath_BoundToFolder: panel state resolves to one file per
// folder — different folders get different paths, the same folder always
// the same path, the stem stays human-findable (sanitized basename plus
// session.CWDHash), same-named folders in different parents stay distinct,
// and no folder means no persistence at all.
func TestPanelStatePath_BoundToFolder(t *testing.T) {
	dir := t.TempDir()
	a := panelStatePathFor(dir, "/home/u/repo-alpha")
	b := panelStatePathFor(dir, "/home/u/repo-beta")
	if a == b {
		t.Fatalf("two folders must resolve to different files, both got %s", a)
	}
	if a != panelStatePathFor(dir, "/home/u/repo-alpha") {
		t.Error("the same folder must always resolve to the same file")
	}
	if filepath.Base(filepath.Dir(a)) != "panel-state" {
		t.Errorf("state files live under panel-state/, got %s", a)
	}
	key := strings.TrimSuffix(filepath.Base(a), ".json")
	if !strings.HasPrefix(key, "repo-alpha-") {
		t.Errorf("file stem should keep the folder's basename, got %s", key)
	}
	if hash := strings.TrimPrefix(key, "repo-alpha-"); len(hash) != 12 {
		t.Errorf("hash suffix = %q, want 12 hex chars (session.CWDHash)", hash)
	}
	if x, y := panelStatePathFor(dir, "/a/build"), panelStatePathFor(dir, "/b/build"); x == y {
		t.Error("same-named folders in different parents must not collide (hash disambiguates)")
	}
	if got := panelStatePathFor(dir, ""); got != "" {
		t.Errorf("no folder must resolve to no persistence, got %q", got)
	}
	if got := panelStatePathFor("", "/x/y"); got != "" {
		t.Errorf("no config dir must resolve to no persistence, got %q", got)
	}
}

// TestPanelState_LegacyFileAdoptedOnce: the pre-folder-bound global file is
// claimed (moved) by the first folder that loads — its preferences survive
// the migration — and the next folder starts from its own empty state
// instead of inheriting the same global file again. An existing folder file
// always wins over a leftover legacy file.
func TestPanelState_LegacyFileAdoptedOnce(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "panel-state.json")
	if err := os.WriteFile(legacy, []byte(`{"version":1,"panels":{"memory":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Folder A claims the legacy file.
	pathA := adoptLegacyPanelState(dir, panelStatePathFor(dir, "/projects/alpha"))
	if pathA == legacy {
		t.Fatal("expected the legacy file to be adopted by the first folder")
	}
	mA := newTestModelForPanels(t)
	mA.loadPanelState(pathA)
	if !mA.panelMemory {
		t.Error("legacy preferences must survive into the adopting folder")
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("the legacy global file must be gone after adoption")
	}

	// A re-adopt attempt for folder A (its file exists) must not resurrect
	// or touch anything: the folder file wins.
	if err := os.WriteFile(legacy, []byte(`{"version":1,"panels":{"git":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := adoptLegacyPanelState(dir, pathA); got != pathA {
		t.Errorf("existing folder file must win, got %q", got)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Error("leftover legacy file must be left alone when the folder has state")
	}
	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}

	// Folder B: no legacy left — it must not inherit folder A's state.
	pathB := adoptLegacyPanelState(dir, panelStatePathFor(dir, "/projects/beta"))
	mB := newTestModelForPanels(t)
	mB.loadPanelState(pathB)
	if mB.panelMemory || mB.panelGit {
		t.Error("a second folder must not inherit another folder's (or the legacy global) state")
	}
}

// TestPanelState_FolderTaggedInFile: the folder a state file belongs to is
// stored inside it, so browsing ~/.milk/panel-state/ shows which project
// each file is for; tests/ACP that set the path directly leave it empty.
func TestPanelState_FolderTaggedInFile(t *testing.T) {
	m := newTestModelForPanels(t)
	m.panelStatePath = filepath.Join(t.TempDir(), "x.json")
	m.panelStateFolder = "/projects/alpha"
	m.panelManualOverride = map[panelRegion]bool{regionTasks: true}
	m.panelTasks = true
	if err := m.savePanelState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(m.panelStatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"folder":"/projects/alpha"`) {
		t.Errorf("state file must record its folder, got %s", data)
	}

	// A model without a bound folder writes no folder tag (omitempty keeps
	// such files byte-compatible with the pre-folder format).
	m2 := newTestModelForPanels(t)
	m2.panelStatePath = filepath.Join(t.TempDir(), "y.json")
	m2.panelManualOverride = map[panelRegion]bool{regionTasks: true}
	if err := m2.savePanelState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	data2, err := os.ReadFile(m2.panelStatePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data2), `"folder"`) {
		t.Errorf("no folder bound must mean no folder tag, got %s", data2)
	}
}

// TestRestoredCloseDoesNotVetoNewContent is the user-visible contract of
// the ADR-0087 amendment: a restored closed preference is startup layout —
// the first new content auto-opens the panel again. (Regression: a restored
// state file used to seed panelManualOverride, which with the default
// remember_panel_state silently killed content-driven auto-open for every
// panel ever toggled.)
func TestRestoredCloseDoesNotVetoNewContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel-state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"panels":{"background":false,"workflow":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newTestModelForPanels(t)
	m.panelStatePath = path
	m.loadPanelState(path)
	if m.panelBackground || m.workflowPanelOpen {
		t.Fatal("restored closed state must start closed")
	}

	updated, _ := m.Update(backgroundJobStartedMsg{})
	m2 := updated.(model)
	if !m2.panelBackground {
		t.Error("a new background job must auto-open the panel despite the restored close")
	}
	m2.autoOpenPanel(regionWorkflow)
	if !m2.workflowPanelOpen {
		t.Error("new workflow content must auto-open the panel despite the restored close")
	}
}

// TestPanelState_PrefsAccumulateWithoutVeto: toggles in different sessions
// accumulate in the state file (the durable pref set survives round trips),
// and the accumulation never turns into an auto-open veto.
func TestPanelState_PrefsAccumulateWithoutVeto(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel-state.json")

	// Session 1: toggle tasks.
	m1 := newTestModelForPanels(t)
	m1.panelStatePath = path
	updated, _ := m1.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	m1 = updated.(model)
	if err := m1.savePanelState(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Session 2: restore, toggle git, save — both prefs must survive.
	m2 := newTestModelForPanels(t)
	m2.panelStatePath = path
	m2.loadPanelState(path)
	updated, _ = m2.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	m2 = updated.(model)
	if err := m2.savePanelState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"tasks":`) {
		t.Errorf("the earlier tasks pref must not be dropped, got %s", data)
	}
	if !strings.Contains(string(data), `"git":`) {
		t.Errorf("the new git pref must be recorded, got %s", data)
	}

	// Session 3: restore — layout comes back, and nothing vetoes auto-open.
	m3 := newTestModelForPanels(t)
	m3.panelStatePath = path
	m3.loadPanelState(path)
	if !m3.panelTasks {
		t.Error("expected the tasks pref to restore as open layout")
	}
	if len(m3.panelManualOverride) != 0 {
		t.Error("accumulated prefs must never seed an auto-open veto")
	}
	m3.autoOpenPanel(regionBackground)
	if !m3.panelBackground {
		t.Error("auto-open must still work after prefs accumulated")
	}
}
