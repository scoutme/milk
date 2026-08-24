package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

func withTempMilkHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	return home
}

func TestLoadRegistry_BuiltinDev(t *testing.T) {
	withTempMilkHome(t)
	reg, errs := LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry errors: %v", errs)
	}
	def, ok := reg.Lookup("dev")
	if !ok {
		t.Fatal("expected built-in \"dev\" definition to be registered")
	}
	if def.Name != "dev" {
		t.Errorf("Name = %q, want %q", def.Name, "dev")
	}
	if len(def.Stages) == 0 {
		t.Error("expected dev definition to have stages")
	}
}

func TestLoadRegistry_UserOverride(t *testing.T) {
	home := withTempMilkHome(t)
	dir := filepath.Join(home, ".milk", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	override := `
name: dev
roles: [designer]
stages:
  - id: designer
    kind: agent_turn
    role: designer
    prompt: "overridden"
`
	if err := os.WriteFile(filepath.Join(dir, "dev.yaml"), []byte(override), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reg, errs := LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry errors: %v", errs)
	}
	def, ok := reg.Lookup("dev")
	if !ok {
		t.Fatal("expected \"dev\" to still be registered")
	}
	if len(def.Stages) != 1 || def.Stages[0].Prompt != "overridden" {
		t.Errorf("expected user override to replace the built-in dev definition, got %+v", def)
	}
}

func TestLoadRegistry_UserNewDefinition(t *testing.T) {
	home := withTempMilkHome(t)
	dir := filepath.Join(home, ".milk", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	custom := `
name: custom
roles: [worker]
stages:
  - id: work
    kind: agent_turn
    role: worker
    prompt: "do the thing"
`
	if err := os.WriteFile(filepath.Join(dir, "custom.yaml"), []byte(custom), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reg, errs := LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry errors: %v", errs)
	}
	if _, ok := reg.Lookup("dev"); !ok {
		t.Error("expected built-in dev to remain registered alongside a new custom definition")
	}
	if _, ok := reg.Lookup("custom"); !ok {
		t.Error("expected user-defined \"custom\" workflow to be registered")
	}
	names := reg.Names()
	if len(names) < 2 {
		t.Errorf("Names() = %v, want at least dev and custom", names)
	}
}

func TestLoadRegistry_MalformedUserFileSkippedNotFatal(t *testing.T) {
	home := withTempMilkHome(t)
	dir := filepath.Join(home, ".milk", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("not: [valid"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reg, errs := LoadRegistry()
	if len(errs) == 0 {
		t.Error("expected an error reported for the malformed file")
	}
	if _, ok := reg.Lookup("dev"); !ok {
		t.Error("a malformed user file should not prevent built-ins from loading")
	}
}

func TestLoadRegistry_NoUserDirIsNotAnError(t *testing.T) {
	withTempMilkHome(t)
	reg, errs := LoadRegistry()
	if len(errs) != 0 {
		t.Fatalf("LoadRegistry errors: %v", errs)
	}
	if _, ok := reg.Lookup("dev"); !ok {
		t.Error("expected built-in dev definition when no ~/.milk/workflows exists")
	}
}
