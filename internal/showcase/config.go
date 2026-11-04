package showcase

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteCleanConfig builds a recording/calibration state root: a copy of the
// user's config.json minus the given top-level keys (remote_oversight and
// friends — a showcase must not depend on, or announce, live infrastructure;
// the telegram ping warning that would otherwise open every transcript is
// stripped, not special-cased), and nothing else. Sessions, memory and panel
// state start empty under the root, so every recording opens on the same
// defaults — the manifest's beats then *show* the panels appearing, which is
// the showcase.
//
// Agents and model settings are kept: the showcase runs the user's own
// configured agents, exactly like a normal session.
//
// Returns the config.json path written inside dstDir.
func WriteCleanConfig(srcConfigPath, dstDir string, strip []string, allowTools bool) (string, error) {
	data, err := os.ReadFile(srcConfigPath)
	if err != nil {
		return "", fmt.Errorf("reading config %s (a configured milk is required): %w", srcConfigPath, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("parsing config %s: %w", srcConfigPath, err)
	}
	for _, key := range strip {
		delete(doc, key)
	}
	// allow_tools: auto-approve tools on every seeded agent entry (the
	// documented per-agent meaning — skipPermissionsFor reads it directly),
	// so a tool-mutating beat can't hang on an unanswered permission ask.
	if allowTools {
		if agents, ok := doc["agents"].([]any); ok {
			for _, entry := range agents {
				if agent, ok := entry.(map[string]any); ok {
					agent["dangerously_skip_permissions"] = true
				}
			}
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("re-encoding config: %w", err)
	}
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return "", fmt.Errorf("creating clean root: %w", err)
	}
	dst := filepath.Join(dstDir, "config.json")
	if err := os.WriteFile(dst, append(out, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("writing clean config: %w", err)
	}
	return dst, nil
}

// SeedRootFor and ActiveRootFor are the record root's two halves, beside the
// scenario workdir (both under demo/.workdirs/, gitignored):
//
//	<workdir>-seed — pristine master: clean config (agents included) + seeds
//	<workdir>-root — the active root milk launches with; every render starts
//	                 by re-copying the seed (the tape's launch line)
//
// They persist across record runs on purpose: the tape embeds their absolute
// paths, and the old temp root died with the record pass — every later
// render then booted a config-less milk and the recording showed it
// complaining instead of working.
func SeedRootFor(workdir string) string { return workdir + "-seed" }

// ActiveRootFor is the mutable root the tape launches milk with.
func ActiveRootFor(workdir string) string { return workdir + "-root" }

// MaterializeSeed rewrites both roots from scratch: the seed master gets the
// clean config (WriteCleanConfig — agents and model settings kept) and the
// manifest's seed commands; the active root is removed so the next render
// re-copies the seed instead of inheriting mutations from a previous one.
func MaterializeSeed(srcConfig string, m *Manifest, workdir string) (seed, root string, err error) {
	seed, root = SeedRootFor(workdir), ActiveRootFor(workdir)
	if err := os.RemoveAll(seed); err != nil {
		return "", "", fmt.Errorf("clearing seed root %s: %w", seed, err)
	}
	if _, err := WriteCleanConfig(srcConfig, seed, m.StripConfigKeys, m.AllowTools); err != nil {
		return "", "", err
	}
	if err := RunSeeds(m, workdir, seed); err != nil {
		return "", "", err
	}
	if err := os.RemoveAll(root); err != nil {
		return "", "", fmt.Errorf("clearing active root %s: %w", root, err)
	}
	return seed, root, nil
}
