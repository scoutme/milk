package eval

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion_LabelDefaults(t *testing.T) {
	if got := (Version{Ref: "v0.8.0"}).label(); got != "v0.8.0" {
		t.Errorf("ref label = %q, want the ref", got)
	}
	if got := (Version{Label: "old", Ref: "v0.8.0"}).label(); got != "old" {
		t.Errorf("explicit label = %q", got)
	}
	if got := (Version{MilkBinary: "/opt/bin/milk-x"}).label(); got != "milk-x" {
		t.Errorf("binary label = %q, want basename", got)
	}
}

func TestExperiment_ResolveAdapterSpecs_Binaries(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "milk-v1")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	exp := &Experiment{
		Agents:   []string{"claude-code[--cache-cooldown,1m]"},
		Versions: []Version{{Label: "v1", MilkBinary: bin}, {Ref: "HEAD", MilkBinary: bin}},
	}
	specs, err := exp.ResolveAdapterSpecs()
	if err != nil {
		t.Fatalf("ResolveAdapterSpecs: %v", err)
	}
	if len(specs) != 3 {
		t.Fatalf("specs = %v, want 3", specs)
	}
	if specs[0] != "claude-code[--cache-cooldown,1m]" {
		t.Errorf("agents spec lost: %q", specs[0])
	}
	if !strings.HasPrefix(specs[1], "milk-tui[--label,v1,--milk-bin,") {
		t.Errorf("version spec = %q", specs[1])
	}
	// milk_binary wins over ref; label defaults to the ref here.
	if !strings.HasPrefix(specs[2], "milk-tui[--label,HEAD,--milk-bin,") {
		t.Errorf("version spec = %q", specs[2])
	}
}

func TestExperiment_ResolveAdapterSpecs_DuplicateLabel(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "milk")
	if err := os.WriteFile(bin, []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}
	exp := &Experiment{Versions: []Version{
		{Label: "same", MilkBinary: bin},
		{Label: "same", MilkBinary: bin},
	}}
	if _, err := exp.ResolveAdapterSpecs(); err == nil || !strings.Contains(err.Error(), "duplicate version label") {
		t.Errorf("want duplicate-label error, got %v", err)
	}
}

func TestLoadExperiment_VersionsValidation(t *testing.T) {
	path := writeExperiment(t, "versions:\n  - label: v1\n")
	if _, err := LoadExperiment(path); err == nil || !strings.Contains(err.Error(), "needs ref or milk_binary") {
		t.Errorf("want missing-source error, got %v", err)
	}

	path = writeExperiment(t, "versions:\n  - ref: v0.8.0\n  - label: v0.8.0\n    ref: v0.9.0\n")
	if _, err := LoadExperiment(path); err == nil || !strings.Contains(err.Error(), "duplicate version label") {
		t.Errorf("want duplicate-label error, got %v", err)
	}
}

func TestMilkAdapter_Label(t *testing.T) {
	a := &milkAdapter{}
	if a.Name() != "milk-tui" {
		t.Errorf("default Name() = %q", a.Name())
	}
	a.SetArgs([]string{"--label", "v0.8.0", "--milk-bin", "/bin/true"})
	if a.Name() != "v0.8.0" {
		t.Errorf("labeled Name() = %q", a.Name())
	}
	if len(a.extraArgs) != 0 {
		t.Errorf("pseudo-flags leaked into forwarded args: %v", a.extraArgs)
	}
}

// TestBuildMilkFromRef builds cmd/milk from HEAD through the version-matrix
// build path (throwaway git worktree + cached binary) and checks the cache
// short-circuits the second time.
func TestBuildMilkFromRef(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the module")
	}
	if _, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err != nil {
		t.Skip("not inside a git checkout")
	}
	bin, err := buildMilkFromRef("HEAD")
	if err != nil {
		t.Fatalf("buildMilkFromRef: %v", err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("built binary missing: %v", err)
	}
	again, err := buildMilkFromRef("HEAD")
	if err != nil {
		t.Fatalf("second buildMilkFromRef: %v", err)
	}
	if again != bin {
		t.Errorf("cache miss: %q != %q", again, bin)
	}
	if _, err := buildMilkFromRef("no-such-ref-anywhere"); err == nil {
		t.Error("unknown ref must error")
	}
}
