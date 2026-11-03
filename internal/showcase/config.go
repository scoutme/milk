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
func WriteCleanConfig(srcConfigPath, dstDir string, strip []string) (string, error) {
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
