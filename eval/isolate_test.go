package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIsolateWrap_MountOrderAndShape pins the exact wrapper argv. The mount
// order is load-bearing (bubblewrap applies mounts in argv order and eval
// workdirs live under /tmp): --tmpfs /tmp must come before the workdir bind
// or the bind is shadowed and every write dies with "Directory nonexistent"
// — hit while prototyping the probe in the issue.
func TestIsolateWrap_MountOrderAndShape(t *testing.T) {
	got := isolateWrap([]string{"/bin/true", "--flag", "arg"}, "/tmp/workdir", "/state", "/state", "")

	want := []string{
		"bwrap",
		"--unshare-user",
		"--ro-bind", "/", "/",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
		"--bind", "/tmp/workdir", "/tmp/workdir",
		"--bind", "/state", "/state", // duplicate collapsed, empty skipped
		"--chdir", "/tmp/workdir",
		"--",
		"/bin/true", "--flag", "arg",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("isolateWrap =\n%q\nwant\n%q", got, want)
	}

	// The tmpfs must precede every /tmp bind — the load-bearing order.
	var tmpfs, bind int
	for i, a := range got {
		if a == "--tmpfs" {
			tmpfs = i
		}
		if a == "--bind" && bind == 0 {
			bind = i
		}
	}
	if tmpfs > bind {
		t.Errorf("--tmpfs /tmp (%d) must come before the first --bind (%d)", tmpfs, bind)
	}
}

// TestAbsArgv: a relative *path* must become absolute (the wrapper
// --chdir's into the workdir, so "./bin" would resolve against the wrong
// directory), while a bare name is PATH lookup and must stay untouched.
func TestAbsArgv(t *testing.T) {
	if got := absArgv([]string{"claude", "x"}); got[0] != "claude" {
		t.Errorf("bare name = %q, want claude (PATH lookup)", got[0])
	}
	got := absArgv([]string{"./bin/milk", "x"})
	if !filepath.IsAbs(got[0]) {
		t.Errorf("relative path = %q, want absolute", got[0])
	}
	if absArgv([]string{"/already/abs"})[0] != "/already/abs" {
		t.Error("absolute path must pass through unchanged")
	}
	if len(absArgv(nil)) != 0 {
		t.Error("empty argv must pass through unchanged")
	}
}

// TestShellJoin pins the tmux seam's quoting: every argument single-quoted,
// embedded single quotes escaped — prompt-shaped extraArgs can contain
// anything.
func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"claude", "--append", "it's a 'test'", "a b"})
	want := `'claude' '--append' 'it'\''s a '\''test'\''' 'a b'`
	if got != want {
		t.Fatalf("shellJoin = %s\nwant       %s", got, want)
	}
}

// TestIsolationResolve pins the degradation contract (issue #355 constraint
// 4): "auto" without bubblewrap degrades loudly to unsandboxed, "bwrap"
// fails instead of silently running uncontained, "off" is silent.
func TestIsolationResolve(t *testing.T) {
	for _, tc := range []struct {
		mode, wantEffective string
		wantWarn, wantErr   bool
	}{
		{"", IsolateOff, false, false},
		{IsolateOff, IsolateOff, false, false},
		{"nope", "", false, true},
	} {
		eff, warn, err := (IsolationConfig{Mode: tc.mode}).Resolve()
		if (err != nil) != tc.wantErr || eff != tc.wantEffective || (warn != "") != tc.wantWarn {
			t.Errorf("Resolve(%q) = (%q, %q, %v), want (%q, warn=%v, err=%v)", tc.mode, eff, warn, err, tc.wantEffective, tc.wantWarn, tc.wantErr)
		}
	}

	// No bubblewrap on PATH: auto degrades with a loud warning, bwrap fails.
	t.Setenv("PATH", "")
	eff, warn, err := (IsolationConfig{Mode: IsolateAuto}).Resolve()
	if err != nil || eff != IsolateOff || !strings.Contains(warn, "UNSANDBOXED") {
		t.Errorf("auto without bwrap = (%q, %q, %v), want off + UNSANDBOXED warning", eff, warn, err)
	}
	if _, _, err := (IsolationConfig{Mode: IsolateBwrap}).Resolve(); err == nil {
		t.Error("bwrap without bwrap must fail, not degrade")
	}
}

// TestExperiment_IsolateValidation: garbage isolate: values are rejected
// before any agent is launched.
func TestExperiment_IsolateValidation(t *testing.T) {
	for _, ok := range []string{"", IsolateOff, IsolateAuto, IsolateBwrap} {
		e := Experiment{Isolate: ok, Scenarios: "eval/scenarios", Repetitions: 1}
		if err := e.Validate(); err != nil {
			t.Errorf("isolate=%q: %v", ok, err)
		}
	}
	e := Experiment{Isolate: "docker", Repetitions: 1}
	if err := e.Validate(); err == nil {
		t.Error("isolate=docker must be rejected (only auto/bwrap/off)")
	}
}

// TestIsolation_SandboxMatrix is the live probe from issue #355 run as a
// test — the adapter-agnostic contract, no LLM involved: the scenario
// workdir is writable, /tmp is a fresh writable scratch, everything else —
// $HOME (the incident: writes into the live checkout under ~), the repo the
// test runs from, the rootfs — is visible but read-only. Skipped where
// bubblewrap is unusable (macOS, Windows, userns-disabled kernels): there
// the Resolve-level degradation tests are the coverage.
func TestIsolation_SandboxMatrix(t *testing.T) {
	if err := probeBwrap(); err != nil {
		t.Skipf("bubblewrap unusable here: %v", err)
	}
	workdir := t.TempDir()
	outside, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	script := strings.Join([]string{
		`printf ok > ./w || echo NO-WORKDIR-WRITE`,
		`printf x > "$HOME/.milk-eval-isolate-probe" 2>/dev/null && echo HOME-WRITABLE || echo HOME-BLOCKED`,
		`printf x > /etc/milk-eval-isolate-probe 2>/dev/null && echo ROOTFS-WRITABLE || echo ROOTFS-BLOCKED`,
		`printf x > "$OUTSIDE/.milk-eval-isolate-probe" 2>/dev/null && echo OUTSIDE-WRITABLE || echo OUTSIDE-BLOCKED`,
		`mkdir -p /tmp/milk-eval-isolate-probe 2>/dev/null && echo TMP-WRITABLE || echo TMP-BLOCKED`,
		`[ -d "$HOME" ] && echo HOME-VISIBLE`,
		`[ -d "$OUTSIDE" ] && echo OUTSIDE-VISIBLE`,
	}, "; ")

	argv := isolateWrap([]string{"/bin/sh", "-c", script}, workdir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "OUTSIDE="+outside)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed probe failed: %v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{"HOME-BLOCKED", "ROOTFS-BLOCKED", "OUTSIDE-BLOCKED", "TMP-WRITABLE", "HOME-VISIBLE", "OUTSIDE-VISIBLE"} {
		if !strings.Contains(got, want) {
			t.Errorf("probe output missing %s:\n%s", want, got)
		}
	}
	for _, leak := range []string{"NO-WORKDIR-WRITE", "HOME-WRITABLE", "ROOTFS-WRITABLE", "OUTSIDE-WRITABLE"} {
		if strings.Contains(got, leak) {
			t.Errorf("probe output shows %s:\n%s", leak, got)
		}
	}
	if _, err := os.Stat(filepath.Join(workdir, "w")); err != nil {
		t.Errorf("workdir write did not land on the real workdir: %v", err)
	}
	// Nothing escaped: the blocked write targets must not exist host-side.
	for _, p := range []string{
		filepath.Join(os.Getenv("HOME"), ".milk-eval-isolate-probe"),
		filepath.Join(outside, ".milk-eval-isolate-probe"),
		"/etc/milk-eval-isolate-probe",
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("escaped write landed on the host: %s", p)
			_ = os.Remove(p)
		}
	}
}

// TestIsolatedExec_Seam pins the adapters' one-line exec seam: un-wrapped
// without isolation, wrapped (bwrap first arg) with it, same working
// directory either way.
func TestIsolatedExec_Seam(t *testing.T) {
	ctx := context.Background()
	off := isolatedExec(ctx, IsolateOff, []string{"/bin/true"}, "/tmp/w")
	if off.Args[0] != "/bin/true" {
		t.Errorf("unisolated argv[0] = %q, want /bin/true", off.Args[0])
	}
	wrapped := isolatedExec(ctx, IsolateBwrap, []string{"/bin/true"}, "/tmp/w")
	if wrapped.Args[0] != "bwrap" {
		t.Errorf("isolated argv[0] = %q, want bwrap", wrapped.Args[0])
	}
	if wrapped.Dir != "/tmp/w" || off.Dir != "/tmp/w" {
		t.Errorf("cmd.Dir = %q/%q, want /tmp/w", wrapped.Dir, off.Dir)
	}
}

// isoProbeAdapter is the minimal AgentAdapter + Isolatable for the
// applyIsolation contract test.
type isoProbeAdapter struct{ got string }

func (a *isoProbeAdapter) Name() string                        { return "iso-probe" }
func (a *isoProbeAdapter) SetArgs([]string)                    {}
func (a *isoProbeAdapter) Start(context.Context, string) error { return nil }
func (a *isoProbeAdapter) Stop() error                         { return nil }
func (a *isoProbeAdapter) SetIsolation(effective string)       { a.got = effective }
func (a *isoProbeAdapter) RunPrompt(context.Context, string) (RunResult, error) {
	return RunResult{}, nil
}

// TestApplyIsolation_AllAdaptersAlike pins the comparability contract
// (issue #355 constraint 5): one resolved mode reaches every adapter —
// never half-sandboxed — and "bwrap" fails the run instead of degrading.
func TestApplyIsolation_AllAdaptersAlike(t *testing.T) {
	t.Setenv("PATH", "") // no bubblewrap: exercise the degradation paths
	a1, a2 := &isoProbeAdapter{}, &isoProbeAdapter{}
	h := &Harness{Adapters: []AgentAdapter{a1, a2}, progress: newProgressReporter(os.Stderr)}
	if err := h.applyIsolation(IsolationConfig{Mode: IsolateAuto}); err != nil {
		t.Fatal(err)
	}
	if a1.got != IsolateOff || a2.got != IsolateOff {
		t.Errorf("degraded auto = %q/%q, want off on every adapter", a1.got, a2.got)
	}
	if err := h.applyIsolation(IsolationConfig{Mode: IsolateBwrap}); err == nil {
		t.Error("bwrap without bubblewrap must fail the run, not degrade")
	}
}

// TestEnsureRW: bind sources are created on demand (an agent's state root
// is legitimately born on first use — and bubblewrap cannot bind a missing
// path), and unusable paths degrade to "" so the caller drops them instead
// of failing the wrap.
func TestEnsureRW(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "new", "state")
	if got := ensureRWDir(dir); got != dir {
		t.Errorf("ensureRWDir = %q, want %q", got, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("state dir not created: %v", err)
	}
	file := filepath.Join(base, "state.json")
	if got := ensureRWFile(file); got != file {
		t.Errorf("ensureRWFile = %q, want %q", got, file)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("state file not created: %v", err)
	}
	// A file where a directory is expected: MkdirAll fails → dropped.
	blocker := filepath.Join(base, "file-blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ensureRWDir(blocker); got != "" {
		t.Errorf("ensureRWDir over a file = %q, want \"\"", got)
	}
	if got := ensureRWDir(""); got != "" {
		t.Errorf("ensureRWDir(\"\") = %q, want \"\"", got)
	}
}
