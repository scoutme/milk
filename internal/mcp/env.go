package mcp

import (
	"runtime"
	"strings"
)

// stdioEnv assembles a plugin stdio subprocess environment per the Agent
// Plugins spec §9.1: the base environment, overlaid by the configured env
// map (same-name entries replace base values), then PLUGIN_ROOT and
// PLUGIN_DATA set last so they can never be overridden by configuration
// (§9.2 forbids configured entries with those names entirely — the ordering
// here is belt and braces).
//
// Environment-name matching follows platform semantics: exact on Unix,
// case-insensitive on Windows.
func stdioEnv(base []string, overlay map[string]string, pluginRoot, pluginData string) []string {
	match := func(a, b string) bool {
		if runtime.GOOS == "windows" {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	out := append([]string(nil), base...)
	set := func(k, v string) {
		for i, kv := range out {
			bk, _, ok := strings.Cut(kv, "=")
			if ok && match(bk, k) {
				out[i] = k + "=" + v
				return
			}
		}
		out = append(out, k+"="+v)
	}
	// Configured env overlays the base (§9.1).
	for k, v := range overlay {
		set(k, v)
	}
	// Reserved variables last: client-provided, replacing same-name entries.
	set("PLUGIN_ROOT", pluginRoot)
	set("PLUGIN_DATA", pluginData)
	return out
}
