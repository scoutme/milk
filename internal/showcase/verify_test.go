package showcase

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubTMUX puts a fake `tmux` first on PATH: capture-pane serves pane
// (everything else exits 0). Verify's assertions run against real subprocess
// plumbing minus a real terminal — the waits, timeouts, report and exit
// semantics are all exercised, no tmux needed.
func stubTMUX(t *testing.T, pane string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pane1"), []byte(pane), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$1" in
capture-pane)
  cat "$SHOWCASE_STUB_DIR/pane1"
  ;;
*)
  exit 0
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHOWCASE_STUB_DIR", dir)
}

func verifyManifest(t *testing.T, body string) *Manifest {
	t.Helper()
	m, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// verifyYAML builds a manifest body from beat specs; kept tiny and explicit
// per test so each scenario reads as its own contract.
func verifyYAML(beats string) string {
	return `
name: t
output: out.gif
launch: "--new"
terminal: {cols: 120, rows: 40, width: 1200, height: 700}
beats:
` + beats
}

// TestVerify_AllAssertionsHold: every wait/extract/skip holds → the report
// is OK, each beat an outcome, extracted vars flow, Summary reads clean.
func TestVerify_AllAssertionsHold(t *testing.T) {
	stubTMUX(t, "ctrl+o palette  role:PRIMARY\n[milk] learned: fact  id=42\n")
	m := verifyManifest(t, verifyYAML(`
  - id: alpha
    steps:
      - type: "/learn x"
      - key: Enter
        wait: "learned:"
        extract: {pattern: "id=([0-9]+)", name: num}
  - id: beta
    skip_if: "learned:"
    steps:
      - wait: "ctrl+o"
`))
	rep, err := Verify(context.Background(), m, Options{Timeout: 2 * time.Second, Interval: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("Verify: %v\n%s", err, rep.Summary())
	}
	if !rep.OK() {
		t.Errorf("OK() = false on a clean run")
	}
	if len(rep.Outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(rep.Outcomes))
	}
	if o := rep.Outcomes[0]; o.ID != "alpha" || o.Err != nil || o.Skipped {
		t.Errorf("alpha outcome = %+v, want clean pass", o)
	}
	if o := rep.Outcomes[1]; o.ID != "beta" || !o.Skipped || o.Err != nil {
		t.Errorf("beta outcome = %+v, want skipped", o)
	}
	if got := rep.Vars["num"]; got != "42" {
		t.Errorf("extracted num = %q, want 42", got)
	}
	sum := rep.Summary()
	for _, want := range []string{"alpha", "ok", "beta", "skip", "asserted"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary missing %q:\n%s", want, sum)
		}
	}
	if strings.Contains(sum, "FAIL") {
		t.Errorf("clean run summary carries FAIL:\n%s", sum)
	}
	if _, ok := rep.Snippets["alpha"]; !ok {
		t.Error("completed beat must keep its pane snippet (the CI artifact)")
	}
}

// TestVerify_FirstFailureFailsFastAndReports: a wait that never holds ends
// the run at its beat — outcome trail shows the passes, the failure (with
// the wait text), and no beats after it; Summary names the spot.
func TestVerify_FirstFailureFailsFastAndReports(t *testing.T) {
	stubTMUX(t, "ctrl+o palette  role:PRIMARY\n[milk] learned: fact\n")
	m := verifyManifest(t, verifyYAML(`
  - id: alpha
    steps:
      - type: "/learn x"
      - key: Enter
      - wait: "learned:"
  - id: beta
    steps:
      - type: "/tasks"
      - key: Enter
      - wait: "never-appears"
  - id: gamma
    steps:
      - wait: "ctrl+o"
`))
	rep, err := Verify(context.Background(), m, Options{Timeout: 400 * time.Millisecond, Interval: 20 * time.Millisecond})
	if err == nil {
		t.Fatal("a broken wait must fail the run")
	}
	if rep.OK() {
		t.Error("OK() = true despite the failure")
	}
	if len(rep.Outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2 (alpha ok, beta failed; gamma never ran)", len(rep.Outcomes))
	}
	if o := rep.Outcomes[0]; o.ID != "alpha" || o.Err != nil {
		t.Errorf("alpha outcome = %+v, want clean pass", o)
	}
	bad := rep.Outcomes[1]
	if bad.ID != "beta" || bad.Err == nil {
		t.Fatalf("outcome[1] = %+v, want beta carrying the failure", bad)
	}
	first, _, _ := strings.Cut(bad.Err.Error(), "\n")
	if !strings.Contains(first, `wait "never-appears"`) {
		t.Errorf("failure first line = %q, want the unsatisfied wait", first)
	}
	sum := rep.Summary()
	for _, want := range []string{"FAIL", "never-appears", "FAILED at beta", "1 of 2"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary missing %q:\n%s", want, sum)
		}
	}
	if strings.Contains(sum, "gamma") {
		t.Errorf("summary reports a beat that never ran:\n%s", sum)
	}
}

// TestVerify_NoTmuxFailsLoudly: no driver on PATH is an error, not a silent
// pass — the verifier's whole value is its verdicts coming from a real run.
func TestVerify_NoTmuxFailsLoudly(t *testing.T) {
	empty := t.TempDir() // PATH without tmux
	t.Setenv("PATH", empty)
	m := verifyManifest(t, verifyYAML(`
  - id: alpha
    steps:
      - wait: "ctrl+o"
`))
	rep, err := Verify(context.Background(), m, Options{Timeout: time.Second})
	if err == nil {
		t.Fatal("missing tmux must fail")
	}
	if rep != nil {
		t.Error("no report without a run")
	}
	if !strings.Contains(err.Error(), "tmux") {
		t.Errorf("error must name tmux: %v", err)
	}
}

// TestFilterBeats: subsets keep manifest order; an unknown id is an error
// naming both sides (a typo'd CI subset must not pass as \"ran nothing\" or,
// worse, silently widen); empty input is a no-op.
func TestFilterBeats(t *testing.T) {
	m := verifyManifest(t, verifyYAML(`
  - id: a
    steps:
      - wait: "x"
  - id: b
    steps:
      - wait: "x"
  - id: c
    steps:
      - wait: "x"
`))
	if err := m.FilterBeats(""); err != nil || len(m.Beats) != 3 {
		t.Fatalf("empty filter: err=%v beats=%d", err, len(m.Beats))
	}
	if err := m.FilterBeats("c, a"); err != nil {
		t.Fatal(err)
	}
	if len(m.Beats) != 2 || m.Beats[0].ID != "a" || m.Beats[1].ID != "c" {
		t.Fatalf("subset order = %v, want [a c] (manifest order)", m.beatIDs())
	}
	err := m.FilterBeats("a,zzz")
	if err == nil {
		t.Fatal("unknown beat id must be rejected")
	}
	if !strings.Contains(err.Error(), "zzz") || !strings.Contains(err.Error(), "b") {
		t.Errorf("error must name the unknown id and the manifest's ids: %v", err)
	}
}
