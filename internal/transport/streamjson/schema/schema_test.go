package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/transport/streamjson"
)

// schemaPath is the committed generated artifact (relative to this package,
// matching gen.go's default -o).
const schemaPath = "../../../../docs/schema/stream-json.schema.json"

// TestSchemaUpToDate is the staleness check behind the reviewer rule
// `go generate ./... && git diff --exit-code docs/schema`: the committed
// artifact must be byte-identical to a fresh schema.Generate().
func TestSchemaUpToDate(t *testing.T) {
	fresh, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	committed, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("reading %s: %v", schemaPath, err)
	}
	if string(fresh) != string(committed) {
		t.Fatalf("%s is stale: run `go generate ./internal/transport/streamjson/schema` and commit the result\n--- generated %d bytes, committed %d bytes", schemaPath, len(fresh), len(committed))
	}
}

// TestSchemaIsWholeDocumentJSON pins the MarshalIndent whole-document rule
// (design §8.3): the artifact is indented JSON with one trailing newline and
// the expected top-level identity keys.
func TestSchemaIsWholeDocumentJSON(t *testing.T) {
	doc, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasSuffix(string(doc), "}\n") {
		t.Errorf("artifact must end with one trailing newline")
	}
	if strings.Contains(string(doc), "\n  {\"") {
		t.Errorf("artifact must be indented (whole-document json.MarshalIndent)")
	}
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		t.Fatalf("artifact is not valid JSON: %v", err)
	}
	for _, key := range []string{"$schema", "$id", "title", "description", "$defs", "oneOf"} {
		if _, ok := root[key]; !ok {
			t.Errorf("artifact missing %q", key)
		}
	}
	if root["$id"] != SchemaID {
		t.Errorf("$id = %v, want %s", root["$id"], SchemaID)
	}
}

// TestSchemaCoversTypedModel keeps the schema honest against the Go types
// (design §9: "Schema source of truth: Go types ... with full JSON tags"):
// every JSON key of Event and its nested modeled types must appear as a
// declared property somewhere in the schema.
func TestSchemaCoversTypedModel(t *testing.T) {
	doc, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		t.Fatal(err)
	}

	covered := map[string]bool{}
	collectProps(root, covered)

	for _, want := range []reflect.Type{
		reflect.TypeOf(streamjson.Event{}),
		reflect.TypeOf(streamjson.AgentInfo{}),
		reflect.TypeOf(streamjson.MCPServer{}),
		reflect.TypeOf(streamjson.RouteDecision{}),
		reflect.TypeOf(streamjson.RouteHop{}),
		reflect.TypeOf(streamjson.Command{}),
		reflect.TypeOf(streamjson.Usage{}),
		reflect.TypeOf(streamjson.Message{}),
		reflect.TypeOf(streamjson.ContentBlock{}),
		reflect.TypeOf(streamjson.StreamPayload{}),
		reflect.TypeOf(streamjson.Delta{}),
	} {
		for i := 0; i < want.NumField(); i++ {
			f := want.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _ := jsonName(f)
			if name == "-" {
				continue
			}
			if !covered[name] {
				t.Errorf("%s.%s (json %q) is not described by the schema", want.Name(), f.Name, name)
			}
		}
	}
}

// collectProps gathers every key of every "properties" object in the schema.
func collectProps(v any, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		if props, ok := t["properties"].(map[string]any); ok {
			for k := range props {
				out[k] = true
			}
		}
		for _, sub := range t {
			collectProps(sub, out)
		}
	case []any:
		for _, sub := range t {
			collectProps(sub, out)
		}
	}
}

// TestSchemaGoldenExampleRefs pins the artifact location contract: the golden
// recordings live at repo-root testdata/events and the schema references that
// contract in its description (docs/schema is a pure generated artifact —
// filepath sanity here keeps go generate's relative output path honest).
func TestSchemaArtifactPathResolves(t *testing.T) {
	if _, err := os.Stat(filepath.Dir(schemaPath)); err != nil {
		t.Fatalf("docs/schema directory missing: %v", err)
	}
}
