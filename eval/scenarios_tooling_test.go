package eval

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestToolingScenarioFixture validates that the tooling-debug-and-extend
// scenario discriminates exactly as designed: the fixture compiles, the
// suite fails in precisely the two pinned service tests (while the store
// tests — which never cover the buggy path — stay green), and the
// canonical one-line root-cause fix makes everything pass. Without this,
// a fixture drift could quietly turn the scenario into noise.
func TestToolingScenarioFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("materializes and runs the scenario's Go fixture")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	scenarios, err := LoadScenarios("scenarios")
	if err != nil {
		t.Fatalf("LoadScenarios: %v", err)
	}
	var sc *Scenario
	for i := range scenarios {
		if scenarios[i].Name == "tooling-debug-and-extend" {
			sc = &scenarios[i]
			break
		}
	}
	if sc == nil {
		t.Fatal("scenario tooling-debug-and-extend not found")
	}
	if !sc.MultiTurn {
		t.Error("scenario must be multi_turn")
	}
	if len(sc.Turns) != 2 {
		t.Errorf("want 2 turns, got %d", len(sc.Turns))
	}
	if len(sc.Turns) > 0 && len(sc.Turns[0].Rubric) != 4 {
		t.Errorf("turn 1 rubric: want 4 criteria, got %d", len(sc.Turns[0].Rubric))
	}
	if len(sc.Turns) > 1 && len(sc.Turns[1].Rubric) != 3 {
		t.Errorf("turn 2 rubric: want 3 criteria, got %d", len(sc.Turns[1].Rubric))
	}

	dir := t.TempDir()
	for _, f := range sc.Setup.Files {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	goTest := func() (string, error) {
		cmd := exec.Command("go", "test", "./...")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// 1. The fixture must compile.
	build := exec.Command("go", "build", "./...")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("fixture does not build: %v\n%s", err, out)
	}

	// 2. It must fail in exactly the two designed service tests.
	out, err := goTest()
	if err == nil {
		t.Fatalf("fixture suite unexpectedly passes; the scenario would not discriminate:\n%s", out)
	}
	var failed []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "--- FAIL: ") {
			failed = append(failed, strings.Fields(line)[2])
		}
	}
	sort.Strings(failed)
	want := []string{"TestConfirmOrderClearsPendingReservation", "TestConfirmOrderKeepsOtherReservations"}
	sort.Strings(want)
	if strings.Join(failed, ",") != strings.Join(want, ",") {
		t.Errorf("failing tests = %v, want %v\n%s", failed, want, out)
	}
	if strings.Contains(out, "FAIL\texample.com/orderflow/internal/store") {
		t.Errorf("store tests must stay green (the suite's coverage gap is the point):\n%s", out)
	}

	// 3. The canonical root-cause fix must make the whole suite green.
	storePath := filepath.Join(dir, "internal", "store", "store.go")
	src, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	fixed := strings.Replace(string(src),
		"delete(s.reserved, ref[:len(ref)-1])",
		"delete(s.reserved, ref)", 1)
	if fixed == string(src) {
		t.Fatalf("canonical fix target not found in fixture store.go")
	}
	if err := os.WriteFile(storePath, []byte(fixed), 0o644); err != nil {
		t.Fatalf("write fixed store.go: %v", err)
	}
	if out, err := goTest(); err != nil {
		t.Errorf("fixture does not pass after the canonical fix: %v\n%s", err, out)
	}
}
