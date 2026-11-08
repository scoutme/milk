package plugin

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// requireString reads a required top-level string field (§5.3): missing, wrong
// type or empty is fatal.
func requireString(top map[string]json.RawMessage, key string) (string, error) {
	raw, ok := top[key]
	if !ok {
		return "", fmt.Errorf("%s: required field missing", key)
	}
	return wantString(raw, key)
}

// wantString type-checks a permitted string field (§5.4: validated by JSON
// type only — no URL/email/semver scrutiny).
func wantString(raw []byte, key string) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s: must be a string", key)
	}
	return s, nil
}

// decodeInto unmarshals raw into dst with a plain type check.
func decodeInto(raw []byte, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return err
	}
	return nil
}

// decodeObject strictly decodes a JSON object into raw member values.
func decodeObject(raw []byte, what string) (map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if top == nil {
		return nil, fmt.Errorf("%s: not a JSON object", what)
	}
	return top, nil
}

func isJSONNull(raw []byte) bool { return strings.TrimSpace(string(raw)) == "null" }

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
