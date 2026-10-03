package streamjson

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	schemaPath  = "../../../docs/schema/stream-json.schema.json"
	goldenDir   = "testdata/events"
	expectedDir = "testdata/expected"
)

// TestGoldensValidateAgainstSchema is the in-repo check behind the reviewer
// rule "docs/schema/stream-json.schema.json validates the goldens": every
// line of every golden run validates against the locked schema (the same
// guarantee `check-jsonschema --schemafile docs/schema/stream-json.schema.json`
// gives externally, one line at a time).
func TestGoldensValidateAgainstSchema(t *testing.T) {
	root := loadSchema(t)
	for _, line := range readGoldenLines(t) {
		var value any
		if err := json.Unmarshal([]byte(line.raw), &value); err != nil {
			t.Errorf("%s:%d: invalid JSON: %v", line.file, line.num, err)
			continue
		}
		if err := validateSchema(value, root, root, fmt.Sprintf("%s:%d", line.file, line.num)); err != nil {
			t.Errorf("schema validation failed: %v", err)
		}
	}
}

// TestGoldenContractInvariants pins the framing rules the schema cannot
// express (ADR-0050 rules 1–2): init first, exactly one terminal result last,
// monotonic seq, and never ANSI.
func TestGoldenContractInvariants(t *testing.T) {
	byFile := map[string][]goldenLine{}
	for _, line := range readGoldenLines(t) {
		byFile[line.file] = append(byFile[line.file], line)
	}
	if len(byFile) == 0 {
		t.Fatal("no golden files found in testdata/events")
	}

	for file, lines := range byFile {
		// Companion expected/ parse results must stay in sync with the goldens.
		base := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		if _, err := os.Stat(filepath.Join(expectedDir, base+".json")); err != nil {
			t.Errorf("%s: missing companion %s/%s.json", file, expectedDir, base)
		}

		lastSeq := int64(0)
		for i, line := range lines {
			if strings.Contains(line.raw, "\x1b") {
				t.Errorf("%s:%d: machine transports never emit ANSI escapes", file, line.num)
			}
			var env struct {
				Type    string `json:"type"`
				Subtype string `json:"subtype"`
				Seq     int64  `json:"seq"`
			}
			if err := json.Unmarshal([]byte(line.raw), &env); err != nil {
				t.Errorf("%s:%d: %v", file, line.num, err)
				continue
			}
			if i == 0 && !(env.Type == "system" && env.Subtype == "init") {
				t.Errorf("%s:%d: first line must be system/init, got %s/%s", file, line.num, env.Type, env.Subtype)
			}
			if i == len(lines)-1 && env.Type != "result" {
				t.Errorf("%s:%d: last line must be the terminal result, got %s", file, line.num, env.Type)
			}
			if env.Type == "result" && i != len(lines)-1 {
				t.Errorf("%s:%d: terminal result must be the last line", file, line.num)
			}
			if i > 0 && env.Seq <= lastSeq {
				t.Errorf("%s:%d: seq %d not monotonic after %d", file, line.num, env.Seq, lastSeq)
			}
			lastSeq = env.Seq
		}
	}
}

// ---------------------------------------------------------------------------
// golden loading
// ---------------------------------------------------------------------------

type goldenLine struct {
	file string
	num  int // 1-based line number
	raw  string
	seq  int64
}

func readGoldenLines(t *testing.T) []goldenLine {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(goldenDir, "*.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no goldens in %s: %v", goldenDir, err)
	}
	var out []goldenLine
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for i, raw := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if strings.TrimSpace(raw) == "" {
				continue
			}
			out = append(out, goldenLine{file: path, num: i + 1, raw: raw})
		}
	}
	return out
}

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("reading schema %s: %v", schemaPath, err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parsing schema: %v", err)
	}
	return root
}

// ---------------------------------------------------------------------------
// minimal JSON Schema (draft 2020-12 subset) validator
// ---------------------------------------------------------------------------
//
// Supports exactly the keywords docs/schema/stream-json.schema.json uses:
// $ref (local #/$defs/...), type, const, enum, required, properties,
// additionalProperties (boolean or subschema), items, allOf, oneOf, if/then.
// If the schema ever grows a keyword this validator doesn't implement, fail
// loudly rather than silently skipping it — that keeps "the schema validates
// the goldens" honest. (External cross-check: check-jsonschema, which is a
// full draft implementation.)

func validateSchema(value any, schema, root map[string]any, path string) error {
	if ref, ok := schema["$ref"].(string); ok {
		resolved, err := resolveRef(ref, root)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := validateSchema(value, resolved, root, path); err != nil {
			return err
		}
	}
	known := map[string]bool{
		"$ref": true, "type": true, "const": true, "enum": true,
		"required": true, "properties": true, "additionalProperties": true,
		"items": true, "allOf": true, "oneOf": true, "if": true, "then": true,
		// annotations / extension markers — never assertions
		"$schema": true, "$id": true, "$defs": true, "title": true,
		"description": true, "x-open-set": true,
	}
	for keyword := range schema {
		if !known[keyword] {
			return fmt.Errorf("%s: schema keyword %q not supported by this validator — extend it", path, keyword)
		}
	}

	if want, ok := schema["type"].(string); ok {
		if !matchesType(value, want) {
			return fmt.Errorf("%s: expected type %s, got %T (%v)", path, want, value, value)
		}
	}
	if want, ok := schema["const"]; ok {
		if !reflect.DeepEqual(value, want) {
			return fmt.Errorf("%s: expected const %v, got %v", path, want, value)
		}
	}
	if members, ok := schema["enum"].([]any); ok {
		found := false
		for _, m := range members {
			if reflect.DeepEqual(value, m) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: %v not in enum %v", path, value, members)
		}
	}
	if req, ok := schema["required"].([]any); ok {
		obj, _ := value.(map[string]any)
		for _, key := range req {
			ks, _ := key.(string)
			if _, present := obj[ks]; !present {
				return fmt.Errorf("%s: missing required property %q", path, ks)
			}
		}
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		obj, _ := value.(map[string]any)
		for key, rawSub := range props {
			if v, present := obj[key]; present {
				sub, _ := rawSub.(map[string]any)
				if err := validateSchema(v, sub, root, path+"."+key); err != nil {
					return err
				}
			}
		}
	}
	if addl, ok := schema["additionalProperties"].(bool); ok {
		obj, _ := value.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if !addl {
			for key := range obj {
				if _, declared := props[key]; !declared {
					return fmt.Errorf("%s: additional property %q not allowed", path, key)
				}
			}
		}
	} else if addl, ok := schema["additionalProperties"].(map[string]any); ok {
		obj, _ := value.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for key, v := range obj {
			if _, declared := props[key]; !declared {
				if err := validateSchema(v, addl, root, path+"."+key); err != nil {
					return err
				}
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		arr, _ := value.([]any)
		for i, elem := range arr {
			if err := validateSchema(elem, items, root, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	if branches, ok := schema["allOf"].([]any); ok {
		for _, branch := range branches {
			b, _ := branch.(map[string]any)
			if err := validateSchema(value, b, root, path); err != nil {
				return err
			}
		}
	}
	if branches, ok := schema["oneOf"].([]any); ok {
		matches := 0
		var lastErr error
		for _, branch := range branches {
			b, _ := branch.(map[string]any)
			if err := validateSchema(value, b, root, path); err == nil {
				matches++
			} else {
				lastErr = err
			}
		}
		if matches != 1 {
			return fmt.Errorf("%s: oneOf matched %d branches (want exactly 1, last mismatch: %v)", path, matches, lastErr)
		}
	}
	if iff, ok := schema["if"].(map[string]any); ok {
		if validateSchema(value, iff, root, path) == nil {
			then, _ := schema["then"].(map[string]any)
			if err := validateSchema(value, then, root, path); err != nil {
				return fmt.Errorf("%s: if/then: %v", path, err)
			}
		}
	}
	return nil
}

func resolveRef(ref string, root map[string]any) (map[string]any, error) {
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("only local $defs refs are supported, got %q", ref)
	}
	defs, _ := root["$defs"].(map[string]any)
	def, ok := defs[strings.TrimPrefix(ref, prefix)].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unresolved $ref %q", ref)
	}
	return def, nil
}

func matchesType(value any, want string) bool {
	switch want {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == float64(int64(f))
	default:
		return false
	}
}
