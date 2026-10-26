package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProjectMapDeriveBudget pins the window → default-caps formula:
// growth-only (floored at the historical 200/6000), capped at the hard caps,
// zero window = unwired.
func TestProjectMapDeriveBudget(t *testing.T) {
	cases := []struct {
		window      int
		wantFiles   int
		wantChars   int
		unwiredNote string
	}{
		{window: 0, wantFiles: 0, wantChars: 0, unwiredNote: "unknown window stays unwired"},
		{window: -1, wantFiles: 0, wantChars: 0, unwiredNote: "negative window stays unwired"},
		{window: 8_000, wantFiles: 200, wantChars: 6000, unwiredNote: "small window floored at legacy defaults"},
		{window: 32_000, wantFiles: 200, wantChars: 6000, unwiredNote: "32k window: 5333 chars and 50 files both floor"},
		{window: 64_000, wantFiles: 200, wantChars: 10666},
		{window: 128_000, wantFiles: 200, wantChars: 21333},
		{window: 256_000, wantFiles: 400, wantChars: 42666},
		{window: 400_000, wantFiles: 625, wantChars: 50000, unwiredNote: "chars hit the 50000 cap first"},
		{window: 1_000_000, wantFiles: 1000, wantChars: 50000, unwiredNote: "both caps"},
	}
	for _, c := range cases {
		got := projectMapDeriveBudget(c.window)
		if got.files != c.wantFiles || got.chars != c.wantChars {
			t.Errorf("projectMapDeriveBudget(%d) = {%d %d}, want {%d %d} (%s)",
				c.window, got.files, got.chars, c.wantFiles, c.wantChars, c.unwiredNote)
		}
	}
}

// TestProjectMapBudgetOnContext pins the ctx carrier: absent → historical
// constants, present → carried values, zero budget → carrier is a no-op.
func TestProjectMapBudgetOnContext(t *testing.T) {
	files, chars := projectMapDefaultsFrom(context.Background())
	if files != projectMapDefaultMaxFiles || chars != projectMapDefaultMaxChars {
		t.Fatalf("unwired ctx defaults = {%d %d}, want constants {%d %d}",
			files, chars, projectMapDefaultMaxFiles, projectMapDefaultMaxChars)
	}

	ctx := withProjectMapBudget(context.Background(), projectMapBudget{files: 50, chars: 4000})
	files, chars = projectMapDefaultsFrom(ctx)
	if files != 50 || chars != 4000 {
		t.Fatalf("wired ctx defaults = {%d %d}, want {50 4000}", files, chars)
	}

	// A zero budget must not set the ctx value at all.
	zero := withProjectMapBudget(ctx, projectMapBudget{})
	if _, ok := projectMapBudgetFrom(zero); !ok {
		t.Fatal("zero budget over a wired ctx must not erase the existing budget")
	}
	fresh := withProjectMapBudget(context.Background(), projectMapBudget{})
	if _, ok := projectMapBudgetFrom(fresh); ok {
		t.Fatal("zero budget on a fresh ctx must stay unwired")
	}
}

// TestWithProjectMapBudget_DerivesFromWindow pins the builder: derives from
// the window, does not mutate the receiver, and unwired agents stay unwired.
func TestWithProjectMapBudget_DerivesFromWindow(t *testing.T) {
	base := &Agent{}
	wired := base.WithProjectMapBudget(256_000)
	if wired.projectMapBudgetFiles != 400 || wired.projectMapBudgetChars != 42666 {
		t.Fatalf("WithProjectMapBudget(256000) = {%d %d}, want {400 42666}",
			wired.projectMapBudgetFiles, wired.projectMapBudgetChars)
	}
	if base.projectMapBudgetFiles != 0 || base.projectMapBudgetChars != 0 {
		t.Fatal("WithProjectMapBudget must not mutate the receiver")
	}
	if off := base.WithProjectMapBudget(0); off.projectMapBudgetFiles != 0 || off.projectMapBudgetChars != 0 {
		t.Fatalf("window 0 must stay unwired, got {%d %d}",
			off.projectMapBudgetFiles, off.projectMapBudgetChars)
	}
}

// TestCloneForBackground_CopiesProjectMapBudget guards the background-clone
// field list (the supportsVision lesson): a background job's project_map
// calls must inherit the parent's derived defaults, not fall back to the
// historical constants.
func TestCloneForBackground_CopiesProjectMapBudget(t *testing.T) {
	parent := &Agent{projectMapBudgetFiles: 400, projectMapBudgetChars: 42666}
	clone := parent.cloneForBackground()
	if clone.projectMapBudgetFiles != 400 || clone.projectMapBudgetChars != 42666 {
		t.Fatalf("clone budget = {%d %d}, want {400 42666}",
			clone.projectMapBudgetFiles, clone.projectMapBudgetChars)
	}
}

// TestApplyProjectMapSchemaBudget pins the schema stamping: a wired budget
// rewrites project_map's parameter descriptions to the actual defaults; an
// unwired budget leaves the static text (which documents the constants) in
// place.
func TestApplyProjectMapSchemaBudget(t *testing.T) {
	static := schemas(nil, "", nil, nil, nil, nil)
	staticFiles, staticChars := projectMapParamDescriptions(t, static)
	if !strings.Contains(staticFiles, "default 200") || !strings.Contains(staticChars, "default 6000") {
		t.Fatalf("static schema must document the constants, got files=%q chars=%q", staticFiles, staticChars)
	}

	wired := schemas(nil, "", nil, nil, nil, nil)
	applyProjectMapSchemaBudget(wired, projectMapBudget{files: 400, chars: 42666})
	gotFiles, gotChars := projectMapParamDescriptions(t, wired)
	if !strings.Contains(gotFiles, "default 400") || !strings.Contains(gotFiles, "hard cap 1000") {
		t.Fatalf("wired max_files description = %q, want default 400 + hard cap 1000", gotFiles)
	}
	if !strings.Contains(gotChars, "default 42666") || !strings.Contains(gotChars, "hard cap 50000") {
		t.Fatalf("wired max_chars description = %q, want default 42666 + hard cap 50000", gotChars)
	}
	if !strings.Contains(gotFiles, "scales with this model's context window") {
		t.Fatalf("wired max_files description must state the scaling: %q", gotFiles)
	}

	// Zero budget after a wired one is a no-op on the static schema.
	untouched := schemas(nil, "", nil, nil, nil, nil)
	applyProjectMapSchemaBudget(untouched, projectMapBudget{})
	uFiles, uChars := projectMapParamDescriptions(t, untouched)
	if uFiles != staticFiles || uChars != staticChars {
		t.Fatal("zero budget must leave the static descriptions untouched")
	}
}

// TestRunProjectMap_UsesBudgetFromContext is the end-to-end pin: the same
// directory maps through the wired default cap (a 50-file budget truncates a
// 60-file tree) and through the constants (no truncation), with no call-site
// args in play.
func TestRunProjectMap_UsesBudgetFromContext(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 60; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%02d.txt", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	wiredCtx := withProjectMapBudget(context.Background(), projectMapBudget{files: 50, chars: projectMapDefaultMaxChars})
	wired, _ := runProjectMap(wiredCtx, map[string]any{"path": dir}, "")
	if strings.Contains(wired, "no files under") || !strings.Contains(wired, "f00.txt") {
		t.Fatalf("wired run produced no map: %s", wired)
	}
	if !strings.Contains(wired, "10 more files not shown") {
		t.Fatalf("wired run must truncate to the 50-file budget, got:\n%s", wired)
	}

	plain, _ := runProjectMap(context.Background(), map[string]any{"path": dir}, "")
	if strings.Contains(plain, "no files under") || !strings.Contains(plain, "f59.txt") {
		t.Fatalf("plain run produced no map: %s", plain)
	}
	if strings.Contains(plain, "more files not shown") {
		t.Fatalf("unwired run must use the 200-file constant (60 files fit), got:\n%s", plain)
	}
}

// projectMapParamDescriptions extracts the max_files/max_chars descriptions
// of the project_map schema for assertions.
func projectMapParamDescriptions(t *testing.T, tools []map[string]any) (files, chars string) {
	t.Helper()
	for _, tool := range tools {
		fn, _ := tool["function"].(map[string]any)
		if name, _ := fn["name"].(string); name != "project_map" {
			continue
		}
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		pf, _ := props["max_files"].(map[string]any)
		pc, _ := props["max_chars"].(map[string]any)
		files, _ = pf["description"].(string)
		chars, _ = pc["description"].(string)
		return files, chars
	}
	t.Fatal("project_map schema not found")
	return "", ""
}
