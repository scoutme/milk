package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Marketplace is a parsed marketplace.json catalog — the ecosystem's
// de-facto interchange (Claude Code's format, ADR-0104 Part II): one repo,
// one machine-readable list of plugins with their distribution sources.
// Hosting stays outside milk; this is only the reader.
type Marketplace struct {
	Name    string
	Owner   string // best-effort ("name" of the owner object)
	Plugins []MarketPlugin
}

// MarketPlugin is one catalog entry. The source vocabulary is the
// ecosystem's (github / git-subdir / url / archive / npm / relative); the
// "command" kind is rejected at parse (never run).
type MarketPlugin struct {
	Name        string
	Description string
	Version     string
	Category    string
	Keywords    []string
	Source      Source
}

// ParseMarketplace validates a marketplace.json payload. A malformed entry
// is skipped with a warning — one bad row never kills the catalog (the
// narrowest-failure-boundary rule, applied at the catalog level).
func ParseMarketplace(raw []byte) (Marketplace, []string, error) {
	var mp Marketplace
	top, err := decodeObject(raw, "marketplace.json")
	if err != nil {
		return mp, nil, err
	}
	var warns []string
	mp.Name, err = requireString(top, "name")
	if err != nil {
		return mp, nil, err
	}
	if ownerRaw, ok := top["owner"]; ok {
		var owner struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(ownerRaw, &owner); err == nil {
			mp.Owner = owner.Name
		}
	}
	entriesRaw, ok := top["plugins"]
	if !ok {
		return mp, warns, fmt.Errorf("marketplace.json: plugins: required")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(entriesRaw, &entries); err != nil {
		return mp, nil, fmt.Errorf("marketplace.json: plugins: %w", err)
	}
	for _, eraw := range entries {
		obj, err := decodeObject(eraw, "marketplace plugin entry")
		if err != nil {
			warns = append(warns, fmt.Sprintf("marketplace entry skipped: %v", err))
			continue
		}
		name, err := requireString(obj, "name")
		if err != nil {
			warns = append(warns, fmt.Sprintf("marketplace entry skipped: %v", err))
			continue
		}
		srcRaw, ok := obj["source"]
		if !ok {
			warns = append(warns, fmt.Sprintf("marketplace entry %q skipped: source: required", name))
			continue
		}
		src, err := ParseMarketplaceSource(srcRaw)
		if err != nil {
			warns = append(warns, fmt.Sprintf("marketplace entry %q skipped: %v", name, err))
			continue
		}
		ep := MarketPlugin{Name: name, Source: src}
		opt := func(key string) string {
			var v string
			if raw, ok := obj[key]; ok {
				_ = json.Unmarshal(raw, &v)
			}
			return v
		}
		ep.Description = opt("description")
		ep.Version = opt("version")
		ep.Category = opt("category")
		if raw, ok := obj["keywords"]; ok {
			_ = json.Unmarshal(raw, &ep.Keywords)
		}
		mp.Plugins = append(mp.Plugins, ep)
	}
	return mp, warns, nil
}

// LoadMarketplaceDir reads a marketplace from an extracted repo dir: the
// canonical .claude-plugin/marketplace.json, with the root spelling accepted
// as a fallback.
func LoadMarketplaceDir(dir string) (Marketplace, []string, error) {
	for _, rel := range []string{filepath.Join(".claude-plugin", "marketplace.json"), "marketplace.json"} {
		p := filepath.Join(dir, rel)
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		mp, warns, err := ParseMarketplace(raw)
		return mp, warns, err
	}
	return Marketplace{}, nil, fmt.Errorf("no marketplace.json found in %s (looked in .claude-plugin/ and root)", dir)
}

// Entry returns the catalog row for a plugin name.
func (m Marketplace) Entry(name string) (MarketPlugin, bool) {
	for _, ep := range m.Plugins {
		if ep.Name == name {
			return ep, true
		}
	}
	return MarketPlugin{}, false
}

// String names the catalog for messages, e.g. "minicut (4 plugins)".
func (m Marketplace) String() string {
	return fmt.Sprintf("%s (%d plugins)", m.Name, len(m.Plugins))
}
