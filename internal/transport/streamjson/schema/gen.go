//go:build ignore

// Command gen regenerates the published JSON Schema for milk's batch
// `--output-format stream-json` line format from the typed wire model.
//
// Run via `go generate ./internal/transport/streamjson/schema` (the directive
// lives in schema.go), or directly: `go run gen.go [-o out.json]`.
//
// The committed artifact is docs/schema/stream-json.schema.json;
// schema_test.go fails when it drifts from schema.Generate().
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/scoutme/milk/internal/transport/streamjson/schema"
)

func main() {
	out := flag.String("o", "../../../../docs/schema/stream-json.schema.json",
		"path of the schema artifact to write")
	flag.Parse()

	doc, err := schema.Generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, doc, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}
