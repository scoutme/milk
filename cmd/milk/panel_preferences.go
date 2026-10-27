package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/scoutme/milk/internal/session"
)

// Side-panel state persistence (ADR-0078), generalized from the
// memory-only preference this started as: every panel's open/closed choice
// is a durable preference. On a manual toggle (/panel <name> or F1-F6) the
// state is written to the folder-bound file resolved by panelStatePathFor
// (~/.milk/panel-state/<name>-<hash>.json, one per project folder) and
// restored at startup — as *startup layout* only (ADR-0087 amending
// ADR-0078): the restored choice seeds panelPrefs, never the in-session
// auto-open veto (panelManualOverride). A remembered close therefore does
// not lock the panel out of its next content: new activity auto-opens it
// again, and "never move my layout" is /panel autoopen off's job (ADR-0087).
// The in-session veto itself follows ADR-0059: a close sticks exactly as
// long as the content it was dismissed over stays live (see
// resetManualOverrideIfFinalized).
//
// The whole mechanism is feature-gated by the "remember_panel_state"
// config key (default on). With it off nothing is read or written and
// panels are session-only, with ADR-0059's finalization expiry semantics.
// Load is skipped at the call site; save is guarded here so no code path
// can leak a write when the feature is off.

// panelStateFile is the on-disk shape of a panel-state file. Version
// guards future shape changes; Panels maps panelSubName → the panel's raw
// open flag at the time of the last manual toggle; Active names the tab
// in front of the shared panel slot on tabbed themes (ADR-0079) — which of
// the open panels the user last looked at. Active is empty in files written
// before it, and harmless if the named panel is not open after restore
// (panelActive falls back to the first open panel). Folder records the
// absolute folder the file belongs to, so a listing of
// ~/.milk/panel-state/ is self-describing; it is empty in files loaded by
// an explicit path (tests, ACP).
type panelStateFile struct {
	Version int             `json:"version"`
	Panels  map[string]bool `json:"panels"`
	Active  string          `json:"active,omitempty"`
	Folder  string          `json:"folder,omitempty"`
}

const panelStateFileVersion = 1

// panelStatePathFor resolves the folder-bound panel-state file for folder
// (ADR-0078 amendment): <configDir>/panel-state/<name>-<hash>.json — one
// file per folder, so panel preferences follow the project the TUI was
// launched in instead of leaking between projects, and two folders can
// never clobber each other's state. <hash> is session.CWDHash(folder) (12
// hex chars of the folder's sha256) so same-named folders stay distinct;
// <name> is a sanitized copy of the folder's base name so the file stays
// human-findable, and the full path is stored inside the file itself
// (panelStateFile.Folder). Empty configDir or folder resolves to "" —
// nothing to bind to: load then reads nothing and save is guarded on the
// empty path, i.e. no persistence, same as the feature being off.
func panelStatePathFor(configDir, folder string) string {
	if configDir == "" || folder == "" {
		return ""
	}
	return filepath.Join(configDir, "panel-state", panelStateFolderKey(folder)+".json")
}

// panelStateFolderKey builds the file stem for a folder: its sanitized,
// capped base name plus session.CWDHash(folder), e.g.
// "milk-3f9a1c2d4e5f". Non-ASCII and separator bytes collapse to '-', so
// the result is safe on every filesystem milk runs on.
func panelStateFolderKey(folder string) string {
	base := filepath.Base(filepath.Clean(folder))
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if len(name) > 32 {
		name = name[:32]
	}
	if name == "" {
		name = "folder"
	}
	return name + "-" + session.CWDHash(folder)
}

// adoptLegacyPanelState migrates the pre-folder-bound global
// <configDir>/panel-state.json (one file for every project) to the current
// folder's path: the first folder to load after the upgrade claims it via
// rename — the user's existing preferences survive without re-appearing in
// every other folder. When the folder's own file already exists it wins and
// the legacy file is left for the next state-less folder to claim; if the
// move itself cannot happen (path missing, permissions), the legacy path is
// returned so this load reads it instead of losing the state — it is only
// ever *moved*, never discarded.
func adoptLegacyPanelState(configDir, target string) string {
	if configDir == "" || target == "" {
		return target
	}
	legacy := filepath.Join(configDir, "panel-state.json")
	if _, err := os.Stat(target); err == nil {
		return target // this folder already has its own state
	}
	if _, err := os.Stat(legacy); err != nil {
		return target
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return legacy
	}
	if err := os.Rename(legacy, target); err != nil {
		return legacy
	}
	return target
}

// loadPanelState restores saved panel preferences from path. Entries
// become the panel's open state and a durable pref (panelPrefs) — startup
// layout only, never an auto-open veto (ADR-0087 amending ADR-0078).
// Panels without an entry keep the content-driven default (closed until
// their content appears). A missing or unreadable file is not an error — a
// fresh install simply has no preferences.
func (m *model) loadPanelState(path string) {
	m.panelStatePath = path
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var f panelStateFile
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	for name, open := range f.Panels {
		region := panelRegionForSubName(name)
		if region == regionNone {
			continue
		}
		if ptr := m.panelOpenPtr(region); ptr != nil {
			*ptr = open
		}
		if m.panelPrefs == nil {
			m.panelPrefs = map[panelRegion]bool{}
		}
		m.panelPrefs[region] = true
	}
	if f.Active != "" {
		if region := panelRegionForSubName(f.Active); region != regionNone {
			m.activatePanelTab(region)
		}
	}
}

// savePanelState writes the manually chosen panels' current state to the
// state file (atomic temp+rename, like every other milk state file). The
// written set is panelPrefs (restored plus this session's toggles) ∪ the
// current in-session override, so preferences accumulate across sessions
// without ever being mistaken for an auto-open veto.
// No-op when persistence is off or no path is configured (tests, ACP).
func (m *model) savePanelState() error {
	if m.panelStatePath == "" || !m.rememberPanelState {
		return nil
	}
	f := panelStateFile{Version: panelStateFileVersion, Panels: map[string]bool{}, Folder: m.panelStateFolder}
	for _, region := range panelRenderOrder {
		if !m.panelPrefs[region] && !m.panelManualOverride[region] {
			continue
		}
		if ptr := m.panelOpenPtr(region); ptr != nil {
			f.Panels[panelSubName(region)] = *ptr
		}
	}
	// The front tab of the shared slot (ADR-0079) is persisted only when
	// the user themselves chose that panel — the same rule as the open
	// flags above: auto-opened state is not a preference. An auto-opened
	// front panel is transient anyway (it will not be open after restore),
	// so nothing is lost by leaving the key out.
	if active := m.panelActive(); active != regionNone && (m.panelPrefs[active] || m.panelManualOverride[active]) {
		f.Active = panelSubName(active)
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.panelStatePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "panel-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), m.panelStatePath)
}

// savePanelStateReport saves the panel preference after a manual toggle
// and reports a persistence failure in the transcript — the toggle itself
// already happened; only the cross-session memory of it failed.
func (m *model) savePanelStateReport() {
	if err := m.savePanelState(); err != nil {
		m.appendTranscript(fmt.Sprintf("%s panel state could not be saved: %v\n", milkTag(), err))
	}
}

// panelRegionForSubName maps a persisted panelSubName back to its region
// (regionNone for unknown names — old/corrupt files are skipped, not fatal).
func panelRegionForSubName(name string) panelRegion {
	for _, region := range panelRenderOrder {
		if panelSubName(region) == name {
			return region
		}
	}
	return regionNone
}
