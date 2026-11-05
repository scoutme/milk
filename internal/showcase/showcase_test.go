package showcase

import (
	"encoding/json"
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
		SeedRoot:   "/home/u/repo-seed",
		Workdir:    "/home/u/repo",
	})

	wantLines := []string{
		"Output docs/x.mp4", // the tape records the raw mp4 — the gif is derived two-pass (GIFTwoPass)
		`Set Width 1200`,
		`Set Theme "Dracula"`,
		`Set PlaybackSpeed 0.9`,
		"Hide", // setup frames (shell + launch + boot) are not content
		`Type@0ms "rm -rf '/tmp/rec-root' && cp -r '/home/u/repo-seed' '/tmp/rec-root' && cd '/home/u/repo' && MILK_CONFIG_ROOT='/tmp/rec-root' '/home/u/.local/bin/milk' --new"`,
		"Enter",
		"Sleep 4.5s", // 2s × 2 + 0.5
		"Show",       // milk is up — the recording starts here
		"# ── learn: learn a fact ──",
		`Type "/learn milk's showcase"`,
		"Enter",
		"Sleep 6.5s", // 3s × 2 + 0.5
		"# wait: learned:",
		"# ── task: close the task ──",
		"# skipped in calibration (skip_if matched)",
		"# hold the final view — the last screen stays a few seconds before the loop",
		"Sleep 5s", // the finished composition holds before the loop
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
	// Setup order: Hide wraps the whole launch (through the boot sleep) and
	// Show lands before the first beat — milk owns frame one of the recording.
	launchIdx := strings.Index(tape, `Type@0ms "rm -rf`)
	showIdx := strings.Index(tape, "Show")
	if !(strings.Index(tape, "Hide") < launchIdx && launchIdx < showIdx) {
		t.Errorf("Hide/Show must wrap the launch line:\n%s", tape)
	}
	if showIdx > strings.Index(tape, "# ── learn") {
		t.Error("Show must come before the first beat")
	}
	// The tape ends holding the finished view for a few seconds.
	if !strings.HasSuffix(tape, "Sleep 5s\n") {
		t.Errorf("tape must end with the final-view hold, got tail %q", tape[max(0, len(tape)-40):])
	}
}

// TestVhsQuote: the Type argument must survive vhs's tokenizer.
// No quote: double-quoted with tokenizer-significant escapes. A quote
// anywhere: backtick raw form — vhs has no \" escape inside double quotes
// (a quoted prompt parsed as garbage while recording the plots' task
// titles), and raw form passes backslashes through literally, which is
// what a TUI input line wants.
func TestVhsQuote(t *testing.T) {
	if got, want := vhsQuote(`a\b`), `"a\\b"`; got != want {
		t.Errorf("vhsQuote = %s, want %s", got, want)
	}
	if got, want := vhsQuote(`a"b\c`), "`a\"b\\c`"; got != want {
		t.Errorf("quoted vhsQuote = %s, want %s", got, want)
	}
	if got, want := vhsQuote("plain prompt"), `"plain prompt"`; got != want {
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
	out, err := WriteCleanConfig(src, dst, []string{"remote_oversight"}, false)
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
	if _, err := WriteCleanConfig(filepath.Join(srcDir, "nope.json"), dst, nil, false); err == nil {
		t.Error("missing source config must error (a configured milk is required)")
	}
}

// TestRenderTape_NoSeedRoot_PlainLaunch: without a seed root the launch is
// the plain cd+config line (callers that manage the root themselves).
func TestRenderTape_NoSeedRoot_PlainLaunch(t *testing.T) {
	m, err := Load(writeManifest(t, `
name: t
output: docs/x.gif
tape: demo.tape
launch: "--new"
margin: 1
base: 0.5
terminal: {cols: 80, rows: 24, width: 800, height: 400}
beats:
  - id: b
    steps:
      - type: "hi"
      - key: Enter
`))
	if err != nil {
		t.Fatal(err)
	}
	tape := RenderTape(m, Result{LaunchElapsed: time.Second}, RenderOpts{
		MilkPath: "/bin/milk", ConfigRoot: "/tmp/root", Workdir: "/w",
	})
	if strings.Contains(tape, "cp -r") || strings.Contains(tape, "rm -rf") {
		t.Errorf("no SeedRoot must not re-seed:\n%s", tape)
	}
	if !strings.Contains(tape, `Type@0ms "cd '/w' && MILK_CONFIG_ROOT='/tmp/root' '/bin/milk' --new"`) {
		t.Errorf("plain launch missing:\n%s", tape)
	}
}

// TestMaterializeSeed: the record roots are rebuilt pristine from the user's
// config every time — agents kept (the showcase runs the user's own agents),
// strip keys gone, seed commands applied to the seed half, and the active
// root cleared so the tape's re-copy starts from the master instead of
// inheriting a previous render's mutations (the failure mode that made
// re-renders show config-less milk).
func TestMaterializeSeed(t *testing.T) {
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "config.json")
	if err := os.WriteFile(src, []byte(`{"remote_oversight":{"backend":"telegram"},"agents":[{"name":"mimo-local"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(writeManifest(t, `
name: t
output: docs/x.gif
tape: demo.tape
launch: "--new"
terminal: {cols: 80, rows: 24, width: 800, height: 400}
strip_config_keys: [remote_oversight]
seed:
  - echo seeded > "$STATE_ROOT/marker.txt"
beats:
  - id: b
    steps:
      - type: "hi"
      - key: Enter
`))
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	root := ActiveRootFor(workdir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stale-session.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	seed, gotRoot, err := MaterializeSeed(src, m, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if seed != SeedRootFor(workdir) || gotRoot != root {
		t.Errorf("roots = %s/%s, want %s/%s", seed, gotRoot, SeedRootFor(workdir), root)
	}
	cfg, err := os.ReadFile(filepath.Join(seed, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "mimo-local") {
		t.Errorf("agents lost from the seed config: %s", cfg)
	}
	if strings.Contains(string(cfg), "remote_oversight") {
		t.Errorf("strip key survived: %s", cfg)
	}
	if _, err := os.Stat(filepath.Join(seed, "marker.txt")); err != nil {
		t.Errorf("seed command did not run against the seed root: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Error("active root must be cleared so the tape's re-copy starts clean")
	}

	// A second materialization resets mutations in the seed half too.
	if err := os.WriteFile(filepath.Join(seed, "stale.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := MaterializeSeed(src, m, workdir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(seed, "stale.txt")); !os.IsNotExist(err) {
		t.Error("stale seed mutation survived re-materialization")
	}
}

// TestRenderTape_BetweenBeats: the between pause lands after every beat but
// the last — the viewer reads the result before the next command types (the
// maintainer caught /tasks and /learn typing back-to-back in the dashboard
// recording: waits measure completion, not reading comfort).
func TestRenderTape_BetweenBeats(t *testing.T) {
	m, err := Load(writeManifest(t, `
name: t
output: docs/x.gif
tape: demo.tape
launch: "--new"
margin: 1
base: 0.4
between: 1.5s
terminal: {cols: 80, rows: 24, width: 800, height: 400}
beats:
  - id: one
    steps:
      - type: "/tasks"
      - key: Enter
      - wait: "tasks:"
  - id: two
    steps:
      - type: "/learn x"
      - key: Enter
  - id: three
    steps:
      - type: "bye"
      - key: Enter
`))
	if err != nil {
		t.Fatal(err)
	}
	tape := RenderTape(m, Result{Timings: []BeatTiming{
		{ID: "one", Elapsed: time.Second},
		{ID: "two", Elapsed: 500 * time.Millisecond},
		{ID: "three", Elapsed: 500 * time.Millisecond},
	}}, RenderOpts{MilkPath: "/bin/milk", ConfigRoot: "/tmp/root", Workdir: "/w"})
	if got := strings.Count(tape, "Sleep 1.5s"); got != 2 {
		t.Errorf("between pauses = %d, want 2 (every beat but the last)\n%s", got, tape)
	}
	// The last beat is followed by the hold, never a between pause.
	if !strings.HasSuffix(strings.TrimSpace(tape), "Sleep 5s") {
		t.Errorf("tape must end in the final hold:\n%q", tape[len(tape)-60:])
	}
	// A bad duration is rejected at load, not by vhs at record time.
	bad := writeManifest(t, `
name: t
output: docs/x.gif
tape: demo.tape
launch: "--new"
between: 40parsnips
terminal: {cols: 80, rows: 24, width: 800, height: 400}
beats:
  - id: one
    steps:
      - type: "hi"
      - key: Enter
`)
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "between") {
		t.Errorf("invalid between must fail Load, got %v", err)
	}
}

// TestRenderTape_WaitsSleepWhereTheyHappened pins the fix for the flaw the
// maintainer spotted in the recorded GIF: waits used to fold into ONE sleep
// at the beat's end, so every keypress replayed back-to-back at typing
// speed — /tasks typed 0ms after the Enter starting a model turn, the
// flyover-accept Enter landing before the picker rendered. Now each wait
// sleeps at its own step, and the trailing sleep covers only what wasn't
// emitted: the grand total is unchanged (margin*elapsed+base — relocated,
// not added), so the narration rhythm calibration measured is preserved.
func TestRenderTape_WaitsSleepWhereTheyHappened(t *testing.T) {
	m, err := Load(writeManifest(t, `
name: t
output: docs/x.gif
tape: demo.tape
launch: "--new"
margin: 1.5
base: 0.4
terminal: {cols: 120, rows: 40, width: 1200, height: 700}
beats:
  - id: task
    steps:
      - type: "/tasks"
      - key: Enter
      - wait: "pending"
      - type: "/task done {{task_id}}"
      - key: Enter
      - wait: "marked done"
`))
	if err != nil {
		t.Fatal(err)
	}
	// wait "pending" took 5s, wait "marked done" 2s, the keys ~ms each;
	// elapsed = steps + the loop's 150ms settle.
	res := Result{
		LaunchElapsed: time.Second,
		Timings: []BeatTiming{{
			ID:      "task",
			Elapsed: 7200 * time.Millisecond,
			Steps: []time.Duration{
				10 * time.Millisecond, 5 * time.Millisecond, 5 * time.Second,
				10 * time.Millisecond, 5 * time.Millisecond, 2 * time.Second,
			},
		}},
		Vars: map[string]string{"task_id": "abcd1234"},
	}
	tape := RenderTape(m, res, RenderOpts{
		MilkPath: "/bin/milk", ConfigRoot: "/tmp/r", Workdir: "/w",
	})

	// Each wait sleeps AT ITS STEP: 5s*1.5=7.5 after "# wait: pending",
	// 2s*1.5=3.0 after "# wait: marked done" — never both after the beat.
	waitA := strings.Index(tape, "# wait: pending")
	waitB := strings.Index(tape, "# wait: marked done")
	if waitA < 0 || waitB < 0 {
		t.Fatalf("wait comments missing:\n%s", tape)
	}
	if got := strings.Index(tape[waitA:], "Sleep 7.5s"); got <= 0 {
		t.Errorf("no inline Sleep 7.5s at the first wait:\n%s", tape)
	}
	if got := strings.Index(tape[waitB:], "Sleep 3.0s"); got <= 0 {
		t.Errorf("no inline Sleep 3.0s at the second wait:\n%s", tape)
	}
	// The second Type comes AFTER the first wait's sleep — the sub-wait gap
	// exists where calibration had it.
	if iType2 := strings.Index(tape, `Type "/task done abcd1234"`); iType2 < waitA {
		t.Errorf("second command before the first wait — narration not sequenced:\n%s", tape)
	} else if iSleep := strings.Index(tape, "Sleep 7.5s"); iSleep > iType2 {
		t.Errorf("first wait's sleep landed after the next command:\n%s", tape)
	}

	// Accounting identity: emitted sleeps + trailing sleep == the old
	// formula for the whole beat — relocated, not added.
	// trailing = (7.2s - 7.0s slow)*1.5 + 0.4 = 0.7s
	if !strings.Contains(tape, "Sleep 0.7s") {
		t.Errorf("trailing sleep must cover only unemitted time (want Sleep 0.7s):\n%s", tape)
	}
}

// TestRunSeeds: seeds run in order, in the workdir, with WORKDIR/STATE_ROOT
// in the environment; a failing seed fails loudly with its output; no seeds
// means no workdir is created (today's behavior, byte-for-byte).
func TestRunSeeds(t *testing.T) {
	root := t.TempDir()
	wd := filepath.Join(root, "wd")
	st := filepath.Join(root, "state")

	m := &Manifest{Seed: []string{
		`printf '%s' "$STATE_ROOT" > state.path`,
		`test -f state.path && printf '%s' "$WORKDIR" > wd.path`,
		`printf ok > out.txt`,
	}}
	if err := RunSeeds(m, wd, st); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(wd, "state.path")); string(got) != st {
		t.Errorf("STATE_ROOT = %q, want %q", got, st)
	}
	if got, _ := os.ReadFile(filepath.Join(wd, "wd.path")); string(got) != wd {
		t.Errorf("WORKDIR = %q, want %q", got, wd)
	}
	if _, err := os.Stat(filepath.Join(wd, "out.txt")); err != nil {
		t.Errorf("third seed did not run in the workdir: %v", err)
	}

	// Re-running resets (the calibration pass must start where seeds left
	// off, every pass): overwrite works, order holds.
	m.Seed = append(m.Seed, `rm out.txt`)
	if err := RunSeeds(m, wd, st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wd, "out.txt")); !os.IsNotExist(err) {
		t.Error("later seed did not run after earlier ones")
	}

	// A failing seed reports its index, command and output.
	m.Seed = []string{`echo first-ok`, `echo seed-failed; exit 3`}
	err := RunSeeds(m, wd, st)
	if err == nil {
		t.Fatal("failing seed must error")
	}
	if !strings.Contains(err.Error(), "seed 1") || !strings.Contains(err.Error(), "seed-failed") {
		t.Errorf("error must name the seed and its output, got: %v", err)
	}

	// Empty seeds: no-op, no workdir.
	empty := &Manifest{}
	wd2 := filepath.Join(root, "never")
	if err := RunSeeds(empty, wd2, st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wd2); !os.IsNotExist(err) {
		t.Error("empty seeds must not create the workdir")
	}
}

// TestWriteCleanConfig_AllowTools: allow_tools patches every seeded agent
// entry with dangerously_skip_permissions (the key skipPermissionsFor reads)
// — and only the seeded copy; the source config is never touched.
func TestWriteCleanConfig_AllowTools(t *testing.T) {
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "config.json")
	orig := `{"agents":[{"name":"a"},{"name":"b","dangerously_skip_permissions":false}]}`
	if err := os.WriteFile(src, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	out, err := WriteCleanConfig(src, dst, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Agents []map[string]any `json:"agents"`
	}
	raw, _ := os.ReadFile(out)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(doc.Agents))
	}
	for i, a := range doc.Agents {
		if v, ok := a["dangerously_skip_permissions"].(bool); !ok || !v {
			t.Errorf("agent %d: dangerously_skip_permissions = %v, want true", i, a["dangerously_skip_permissions"])
		}
	}
	if rawSrc, _ := os.ReadFile(src); string(rawSrc) != orig {
		t.Error("source config was modified — seeds may only touch the copy")
	}

	// allow_tools: false leaves flags alone (a config that prompts keeps prompting).
	dst2 := t.TempDir()
	out2, err := WriteCleanConfig(src, dst2, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var doc2 struct {
		Agents []map[string]any `json:"agents"`
	}
	raw2, _ := os.ReadFile(out2)
	if err := json.Unmarshal(raw2, &doc2); err != nil {
		t.Fatal(err)
	}
	if v, ok := doc2.Agents[0]["dangerously_skip_permissions"]; ok && v == true {
		t.Error("allow_tools: false must not inject the flag")
	}
}

// TestEnsureRCHomeForLaunch pins the #404 forensics guard: under $SHELL=zsh
// a fresh (rc-less) HOME gets a stub .zshrc before any tmux line is typed, a
// non-zsh shell stays untouched, and an existing rc is never modified.
func TestEnsureRCHomeForLaunch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/usr/bin/zsh")
	ensureRCHomeForLaunch()
	if _, err := os.Stat(filepath.Join(home, ".zshrc")); err != nil {
		t.Fatalf("zsh shell + rc-less home: .zshrc not created: %v", err)
	}

	home2 := t.TempDir()
	t.Setenv("HOME", home2)
	t.Setenv("SHELL", "/bin/bash")
	ensureRCHomeForLaunch()
	if _, err := os.Stat(filepath.Join(home2, ".zshrc")); err == nil {
		t.Error("bash shell should not get a .zshrc")
	}

	sentinel := []byte("# mine\ntypeset -U path\n")
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), sentinel, 0o644); err != nil {
		t.Fatal(err)
	}
	ensureRCHomeForLaunch()
	got, err := os.ReadFile(filepath.Join(home, ".zshrc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sentinel) {
		t.Errorf("existing .zshrc modified: %q", got)
	}
}
