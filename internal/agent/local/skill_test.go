package local

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSkillTool_CatalogSchemaAndDispatch(t *testing.T) {
	t.Cleanup(func() { SetSkillCatalog(nil) })
	SetSkillCatalog([]SkillEntry{
		{Name: "deploy", Description: "Ship it safely.", Load: func() (string, error) { return "# Deploy\n\nsteps", nil }},
	})

	// Schema present with the progressive-disclosure listing.
	sc := skillToolSchema()
	if sc == nil {
		t.Fatal("schema must exist with a non-empty catalog")
	}
	if toolName(sc) != "skill" {
		t.Fatalf("tool name: %v", toolName(sc))
	}
	fn := sc["function"].(map[string]any)
	if !strings.Contains(fn["description"].(string), "- deploy: Ship it safely.") {
		t.Fatalf("listing missing: %v", fn["description"])
	}

	// Dispatch: known skill returns its body.
	out, _ := runSkill(map[string]any{"name": "deploy"})
	if !strings.Contains(out, "# Deploy") {
		t.Fatalf("body: %q", out)
	}

	// Unknown skill: error naming the available list.
	out, _ = runSkill(map[string]any{"name": "nope"})
	if !isToolError(out) || !strings.Contains(out, "deploy") {
		t.Fatalf("unknown-skill error: %q", out)
	}

	// Missing name: error listing names.
	out, _ = runSkill(map[string]any{})
	if !isToolError(out) {
		t.Fatalf("missing-name error: %q", out)
	}

	// Truncation cap.
	SetSkillCatalog([]SkillEntry{{Name: "big", Description: "d", Load: func() (string, error) {
		return strings.Repeat("x", skillBodyMax+100), nil
	}}})
	out, _ = runSkill(map[string]any{"name": "big"})
	if len(out) >= skillBodyMax+200 || !strings.Contains(out, "truncated") {
		t.Fatalf("cap not applied: %d bytes", len(out))
	}
}

func TestSkillTool_OmittedWhenNoCatalog(t *testing.T) {
	t.Cleanup(func() { SetSkillCatalog(nil) })
	SetSkillCatalog(nil)
	if sc := skillToolSchema(); sc != nil {
		t.Fatal("no skills → no tool schema (zero token cost)")
	}
	// And the built tool list carries no skill entry.
	tools := schemas(nil, "", nil, nil, nil, nil)
	for _, s := range tools {
		if toolName(s) == "skill" {
			var dump, _ = json.Marshal(s)
			t.Fatalf("skill tool must be absent: %s", dump)
		}
	}
}
