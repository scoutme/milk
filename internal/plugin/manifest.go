package plugin

import (
	"fmt"
	"strings"
)

// Manifest is the root plugin.json (§5.2). The schema is closed: only these
// fields exist; unknown top-level fields are reported and ignored (§5.2) and
// every other schema violation is fatal.
type Manifest struct {
	Schema      string
	Name        string
	Version     string
	Description string
	Author      Author
	Homepage    string
	Repository  string
	License     string
	Keywords    []string
	// Extensions holds raw client-extension objects keyed by reverse-domain
	// namespace (§8.1). Members are not validated beyond being JSON objects.
	Extensions map[string]map[string]any
}

// Author is the manifest author object (§5.4): only name, email and url.
type Author struct {
	Name  string
	Email string
	URL   string
}

// ParseManifest validates a plugin.json payload (§5). Warnings are the
// non-fatal report-and-ignore cases (unknown fields); any other error rejects
// the plugin.
func ParseManifest(raw []byte) (Manifest, []string, error) {
	var m Manifest
	top, err := decodeObject(raw, "plugin.json")
	if err != nil {
		return m, nil, err
	}
	var warnings []string

	// $schema selects the supported manifest ruleset (§5.2). Unrecognized
	// version → reject, reporting the unsupported version.
	sch, err := requireString(top, "$schema")
	if err != nil {
		return m, nil, err
	}
	if sch != SchemaPlugin {
		return m, nil, fmt.Errorf("unsupported $schema %q (this client supports Agent Plugins 1.0.0)", sch)
	}
	m.Schema = sch

	m.Name, err = requireString(top, "name")
	if err != nil {
		return m, nil, err
	}
	if err := validatePluginName(m.Name); err != nil {
		return m, nil, fmt.Errorf("name: %w", err)
	}

	if v, ok := top["version"]; ok {
		if m.Version, err = wantString(v, "version"); err != nil {
			return m, nil, err
		}
	}
	if v, ok := top["description"]; ok {
		if m.Description, err = wantString(v, "description"); err != nil {
			return m, nil, err
		}
	}
	if v, ok := top["homepage"]; ok {
		if m.Homepage, err = wantString(v, "homepage"); err != nil {
			return m, nil, err
		}
	}
	if v, ok := top["repository"]; ok {
		if m.Repository, err = wantString(v, "repository"); err != nil {
			return m, nil, err
		}
	}
	if v, ok := top["license"]; ok {
		if m.License, err = wantString(v, "license"); err != nil {
			return m, nil, err
		}
	}
	if v, ok := top["keywords"]; ok {
		if err := decodeInto(v, &m.Keywords); err != nil {
			return m, nil, fmt.Errorf("keywords: %w", err)
		}
	}
	if v, ok := top["author"]; ok {
		if m.Author, err = parseAuthor(v); err != nil {
			return m, nil, err
		}
	}
	if v, ok := top["extensions"]; ok {
		ext, err := parseExtensions(v)
		if err == errExtensionsNotObject {
			// Non-object extensions: report and ignore, not fatal (§8.1).
			warnings = append(warnings, "extensions: not a JSON object — ignored")
		} else if err != nil {
			return m, nil, err
		} else {
			m.Extensions = ext
		}
	}

	// Unknown top-level fields: report and ignore (§5.2).
	for _, k := range sortedKeys(top) {
		switch k {
		case "$schema", "name", "version", "description", "author",
			"homepage", "repository", "license", "keywords", "extensions":
		default:
			warnings = append(warnings, fmt.Sprintf("unknown field %q ignored", k))
		}
	}
	return m, warnings, nil
}

// validatePluginName enforces §5.5: 1-64 chars, a-z0-9-. only, alphanumeric
// first and last, no "--" or "..".
func validatePluginName(name string) error {
	if len(name) < 1 || len(name) > 64 {
		return fmt.Errorf("must be 1-64 characters, got %d", len(name))
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !('a' <= c && c <= 'z') && !('0' <= c && c <= '9') && c != '-' && c != '.' {
			return fmt.Errorf("character %q not allowed (a-z, 0-9, -, . only)", c)
		}
	}
	isAlnum := func(c byte) bool { return ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') }
	if !isAlnum(name[0]) || !isAlnum(name[len(name)-1]) {
		return fmt.Errorf("first and last characters must be alphanumeric")
	}
	if strings.Contains(name, "--") || strings.Contains(name, "..") {
		return fmt.Errorf("consecutive hyphens or periods not allowed")
	}
	return nil
}

func parseAuthor(v []byte) (Author, error) {
	var a Author
	obj, err := decodeObject(v, "author")
	if err != nil {
		return a, fmt.Errorf("author: %w", err)
	}
	for _, k := range sortedKeys(obj) {
		switch k {
		case "name":
			if a.Name, err = wantString(obj[k], "author.name"); err != nil {
				return a, err
			}
		case "email":
			if a.Email, err = wantString(obj[k], "author.email"); err != nil {
				return a, err
			}
		case "url":
			if a.URL, err = wantString(obj[k], "author.url"); err != nil {
				return a, err
			}
		default:
			return a, fmt.Errorf("author: unknown field %q", k) // fatal (§5.4)
		}
	}
	return a, nil
}

var errExtensionsNotObject = fmt.Errorf("extensions is not an object")

func parseExtensions(v []byte) (map[string]map[string]any, error) {
	obj, err := decodeObject(v, "extensions")
	if err != nil {
		// decodeObject fails for both "not an object" and malformed JSON;
		// only the plain non-object case is the §8.1 report-and-ignore one.
		if isJSONNull(v) || len(v) > 0 && (v[0] == '[' || v[0] == '"' || (v[0] >= '0' && v[0] <= '9') || v[0] == 't' || v[0] == 'f') {
			return nil, errExtensionsNotObject
		}
		return nil, fmt.Errorf("extensions: %w", err)
	}
	out := make(map[string]map[string]any, len(obj))
	for k, raw := range obj {
		var member map[string]any
		if err := decodeInto(raw, &member); err != nil {
			return nil, fmt.Errorf("extensions.%s: member values must be objects", k) // fatal (§5.2)
		}
		out[k] = member
	}
	return out, nil
}
