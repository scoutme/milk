package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// NeedsShim reports whether dir lacks the portable root manifest — i.e. it is
// a per-client package (e.g. a .claude-plugin/ layout) that predates the
// Agent Plugins format. Shimming bridges it (ADR-0104 Part II).
func NeedsShim(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "plugin.json"))
	return err != nil
}

// WriteShim synthesizes root plugin.json (and mcp.json when the package
// declares servers) from a .claude-plugin/ layout. Create-only: existing
// files are never overwritten — the shim is a compatibility bridge, never a
// rewrite of a portable package. Returns the files created and warnings.
func WriteShim(dir string) (created []string, warns []string, err error) {
	claudePath := filepath.Join(dir, ".claude-plugin", "plugin.json")
	raw, err := os.ReadFile(claudePath)
	if err != nil {
		return nil, nil, fmt.Errorf("shim: no .claude-plugin/plugin.json to shim from: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, nil, fmt.Errorf("shim: .claude-plugin/plugin.json: %w", err)
	}
	get := func(key string) string {
		var v string
		if r, ok := top[key]; ok {
			_ = json.Unmarshal(r, &v)
		}
		return v
	}

	// Root plugin.json (create-only).
	if _, statErr := os.Stat(filepath.Join(dir, "plugin.json")); statErr == nil {
		return nil, nil, nil // portable already — nothing to do
	}
	name := get("name")
	if err := validatePluginName(name); err != nil {
		return nil, nil, fmt.Errorf("shim: name: %w", err)
	}
	man := map[string]any{
		"$schema": SchemaPlugin,
		"name":    name,
	}
	for _, k := range []string{"version", "description", "homepage", "repository", "license"} {
		if v := get(k); v != "" {
			man[k] = v
		}
	}
	if authorRaw, ok := top["author"]; ok {
		// Claude's author is a string or an object; the spec's is an object
		// with name/email/url. Normalize both.
		var asString string
		if json.Unmarshal(authorRaw, &asString) == nil && asString != "" {
			man["author"] = map[string]any{"name": asString}
		} else {
			man["author"] = json.RawMessage(authorRaw)
		}
	}
	if kwsRaw, ok := top["keywords"]; ok {
		var kws []string
		if json.Unmarshal(kwsRaw, &kws) == nil && len(kws) > 0 {
			man["keywords"] = kws
		}
	}
	manJSON, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), append(manJSON, '\n'), 0o644); err != nil {
		return nil, nil, err
	}
	created = append(created, "plugin.json")

	// mcp.json: prefer servers declared in the claude manifest, else the
	// claude mcp.json — both mapped into the spec's mcpServers shape.
	if _, statErr := os.Stat(filepath.Join(dir, "mcp.json")); statErr == nil {
		return created, warns, nil
	}
	serversRaw, hasServers := top["mcpServers"]
	if !hasServers {
		if b, readErr := os.ReadFile(filepath.Join(dir, ".claude-plugin", "mcp.json")); readErr == nil {
			var mt map[string]json.RawMessage
			if json.Unmarshal(b, &mt) == nil {
				serversRaw, hasServers = mt["mcpServers"]
				if !hasServers {
					serversRaw, hasServers = mt["servers"]
				}
			}
		}
	}
	if !hasServers {
		return created, warns, nil
	}
	var cl map[string]json.RawMessage
	if err := json.Unmarshal(serversRaw, &cl); err != nil {
		warns = append(warns, fmt.Sprintf("shim: mcpServers not an object — no mcp.json written: %v", err))
		return created, warns, nil
	}
	specServers := map[string]any{}
	for srvName, srvRaw := range cl {
		var e map[string]json.RawMessage
		if err := json.Unmarshal(srvRaw, &e); err != nil {
			warns = append(warns, fmt.Sprintf("shim: mcp server %q skipped: %v", srvName, err))
			continue
		}
		getE := func(key string) string {
			var v string
			if r, ok := e[key]; ok {
				_ = json.Unmarshal(r, &v)
			}
			return v
		}
		switch {
		case getE("command") != "":
			entry := map[string]any{"type": "stdio", "command": getE("command")}
			if argsRaw, ok := e["args"]; ok {
				var args []string
				if json.Unmarshal(argsRaw, &args) == nil {
					entry["args"] = args
				}
			}
			if envRaw, ok := e["env"]; ok {
				var env map[string]string
				if json.Unmarshal(envRaw, &env) == nil && len(env) > 0 {
					entry["env"] = env
				}
			}
			specServers[srvName] = entry
		case getE("url") != "":
			entry := map[string]any{"type": "streamable-http", "url": getE("url")}
			if hRaw, ok := e["headers"]; ok {
				entry["headers"] = json.RawMessage(hRaw)
			}
			specServers[srvName] = entry
		default:
			warns = append(warns, fmt.Sprintf("shim: mcp server %q has neither command nor url — skipped", srvName))
		}
	}
	if len(specServers) == 0 {
		return created, warns, nil
	}
	mcpJSON, err := json.MarshalIndent(map[string]any{
		"$schema":    SchemaMCP,
		"mcpServers": specServers,
	}, "", "  ")
	if err != nil {
		return created, warns, err
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), append(mcpJSON, '\n'), 0o644); err != nil {
		return created, warns, err
	}
	created = append(created, "mcp.json")
	return created, warns, nil
}
