package showcase

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "showcase.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func minimalManifestYAML() string {
	return `
name: t
output: out.gif
launch: "--new"
terminal: {cols: 120, rows: 40, width: 1200, height: 700}
beats:
  - id: a
    steps:
      - type: "/learn x"
      - key: Enter
        wait: "learned:"
`
}

// TestLoad_DefaultsAndValidation: defaults land before validation, and the
// structural rules the recorder relies on actually reject.
func TestLoad_DefaultsAndValidation(t *testing.T) {
	m, err := Load(writeManifest(t, minimalManifestYAML()))
	if err != nil {
		t.Fatal(err)
	}
	if m.Margin != DefaultMargin || m.Base != DefaultBase || m.Timeout != DefaultTimeout {
		t.Errorf("defaults: margin=%v base=%v timeout=%v", m.Margin, m.Base, m.Timeout)
	}

	bad := map[string]string{
		"no beats": `
name: t
output: o.gif
launch: milk
terminal: {cols: 10, rows: 10}
beats: []
`,
		"margin below 1": `
name: t
output: o.gif
launch: milk
margin: 0.5
terminal: {cols: 10, rows: 10}
beats: [{id: a, steps: [{key: Enter}]}]
`,
		"duplicate beat id": `
name: t
output: o.gif
launch: milk
terminal: {cols: 10, rows: 10}
beats:
  - {id: a, steps: [{key: Enter}]}
  - {id: a, steps: [{key: Enter}]}
`,
		"empty step": `
name: t
output: o.gif
launch: milk
terminal: {cols: 10, rows: 10}
beats: [{id: a, steps: [{wait: ""}]}]
`,
		"typed text never submitted": `
name: t
output: o.gif
launch: "--new"
terminal: {cols: 10, rows: 10}
beats: [{id: a, steps: [{type: "/learn x", wait: "learned:"}]}]
`,
		"missing terminal size": `
name: t
output: o.gif
launch: milk
beats: [{id: a, steps: [{key: Enter}]}]
`,
	}
	for name, body := range bad {
		if _, err := Load(writeManifest(t, body)); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

// TestRenderTape_BeatForBeat: the tape must replay the calibration — same
// steps in the same order, one sleep per beat sized from measurement, and
// the launch line must carry the absolute binary, clean root and cd.
func TestRenderTape_BeatForBeat(t *testing.T) {
	m, err := Load(writeManifest(t, `
name: t
output: docs/x.gif
tape: demo.tape
launch: "--new"
margin: 2
base: 0.5
terminal: {cols: 120, rows: 40, width: 1200, height: 700, font_size: 14, theme: Dracula, typing_speed: 60ms, playback_speed: 0.9}
beats:
  - id: learn
    note: "learn a fact"
    steps:
      - type: "/learn {{fact}}"
      - key: Enter
        wait: "learned:"
  - id: task
    note: "close the task"
    steps:
      - type: "/task done {{task_id}}"
      - key: Enter
        wait: "marked done"
        extract: {pattern: "x", name: other}
`))
	if err != nil {
		t.Fatal(err)
	}
	res := Result{
		LaunchElapsed: 2 * time.Second,
		Timings: []BeatTiming{
			{ID: "learn", Elapsed: 3 * time.Second},
			{ID: "task", Elapsed: 4 * time.Second, Skipped: true},
		},
		Vars: map[string]string{"fact": "milk's showcase", "task_id": "abcd1234"},
	}
	tape := RenderTape(m, res, RenderOpts{
		MilkPath:   "/home/u/.local/bin/milk",
		ConfigRoot: "/tmp/rec-root",
		Workdir:    "/home/u/repo",
	})

	wantLines := []string{
		"Output docs/x.gif",
		`Set Width 1200`,
		`Set Theme "Dracula"`,
		`Set PlaybackSpeed 0.9`,
		`Type "cd '/home/u/repo' && MILK_CONFIG_ROOT='/tmp/rec-root' '/home/u/.local/bin/milk' --new"`,
		"Enter",
		"Sleep 4.5s", // 2s × 2 + 0.5
		"# ── learn: learn a fact ──",
		`Type "/learn milk's showcase"`,
		"Enter",
		"Sleep 6.5s", // 3s × 2 + 0.5
		"# wait: learned:",
		"# ── task: close the task ──",
		"# skipped in calibration (skip_if matched)",
	}
	for _, want := range wantLines {
		if !strings.Contains(tape, want) {
			t.Errorf("tape missing %q\n---\n%s", want, tape)
		}
	}
	// A skipped beat emits no steps after its comment.
	if idx := strings.Index(tape, "# skipped"); idx >= 0 {
		if strings.Contains(tape[idx:], "Type ") {
			t.Error("skipped beat still types in the tape")
		}
	}
}

// TestVhsQuote: the Type argument must survive a shell-ish tokenizer —
// backslashes and quotes in command text are real (paths, regexes).
func TestVhsQuote(t *testing.T) {
	if got, want := vhsQuote(`a"b\c`), `"a\"b\\c"`; got != want {
		t.Errorf("vhsQuote = %s, want %s", got, want)
	}
	if got, want := shellQuote(`/tmp/it's`), `'/tmp/it'\''s'`; got != want {
		t.Errorf("shellQuote = %s, want %s", got, want)
	}
}

// TestInterpolate: extracted values flow into later steps; unknown
// placeholders stay visible (the runner fails on them rather than sending
// a literal {{id}} to the TUI).
func TestInterpolate(t *testing.T) {
	vars := map[string]string{"task_id": "abcd1234"}
	if got := Interpolate("/task done {{task_id}}", vars); got != "/task done abcd1234" {
		t.Errorf("got %q", got)
	}
	if got := Interpolate("{{missing}}", vars); got != "{{missing}}" {
		t.Errorf("unknown placeholder must survive, got %q", got)
	}
}

// TestTapePath: relative to the manifest, absolute honored.
func TestTapePath(t *testing.T) {
	m := &Manifest{Tape: ""}
	if got := m.TapePath("/x/y/showcase.yaml"); got != "/x/y/demo.tape" {
		t.Errorf("default tape = %s", got)
	}
	m.Tape = "custom.tape"
	if got := m.TapePath("/x/y/showcase.yaml"); got != "/x/y/custom.tape" {
		t.Errorf("tape = %s", got)
	}
	m.Tape = "/abs/t.tape"
	if got := m.TapePath("/x/y/showcase.yaml"); got != "/abs/t.tape" {
		t.Errorf("absolute tape = %s", got)
	}
}

// TestWriteCleanConfig: strip keys vanish, everything else (the agents!)
// survives — the showcase runs the user's own configured agents, minus the
// live-infrastructure keys a recording must not announce.
func TestWriteCleanConfig(t *testing.T) {
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "config.json")
	body := `{"remote_oversight":{"backend":"telegram"},"agents":[{"name":"mimo-local"}],"theme":"paper"}`
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	out, err := WriteCleanConfig(src, dst, []string{"remote_oversight"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "remote_oversight") || strings.Contains(got, "telegram") {
		t.Errorf("stripped key survived: %s", got)
	}
	if !strings.Contains(got, "mimo-local") || !strings.Contains(got, "paper") {
		t.Errorf("agents/settings lost: %s", got)
	}
	if _, err := WriteCleanConfig(filepath.Join(srcDir, "nope.json"), dst, nil); err == nil {
		t.Error("missing source config must error (a configured milk is required)")
	}
}
