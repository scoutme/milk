package eval

// Tests for the filesystem-isolation backend (issue #355, ADR-0098):
// resolution/auto-degradation, bwrap argv construction, HOME seeding, and —
// when a working bwrap is on the machine — a live escape matrix proving the
// exact property the issue exists for: the agent can write the workdir and
// nothing else (not HOME, not other checkouts, not the rootfs).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// fakeBwrap writes an executable `bwrap` into a temp dir and points PATH at
// it only, so ResolveIsolate's probe runs the fake. exitCode controls the
// probe outcome (0 = working sandbox, 1 = probe failure).
func fakeBwrap(t *testing.T, exitCode int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake bwrap is a POSIX script; Windows gets the same probes-free build/vet gate as the rest of eval")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "bwrap"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// execRun runs a wrapped argv (a real sandboxed command) and returns its
// combined output.
func execRun(argv []string) (string, error) {
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	return string(out), err
}

func TestResolveIsolate_OffByDefault(t *testing.T) {
	for _, mode := range []string{"", IsolateOff} {
		r, err := ResolveIsolate(IsolateConfig{Mode: mode})
		if err != nil || r != nil {
			t.Errorf("mode %q: got (%v, %v), want (nil, nil)", mode, r, err)
		}
	}
}

func TestResolveIsolate_InvalidMode(t *testing.T) {
	if _, err := ResolveIsolate(IsolateConfig{Mode: "chroot"}); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
}

func TestResolveIsolate_AutoDegradesWithoutBwrap(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // nothing on PATH
	r, err := ResolveIsolate(IsolateConfig{Mode: IsolateAuto})
	if err != nil {
		t.Fatalf("auto must degrade, not fail: %v", err)
	}
	if r == nil || r.active() {
		t.Fatal("auto without bwrap must yield a degraded (inactive) runtime carrying the warning")
	}
	if !strings.Contains(r.warn, IsolateAuto) || !strings.Contains(r.warn, "WITHOUT filesystem isolation") {
		t.Errorf("warning must be loud and explicit, got %q", r.warn)
	}
}

func TestResolveIsolate_ExplicitBwrapErrorsWithoutIt(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := ResolveIsolate(IsolateConfig{Mode: IsolateBwrap}); err == nil {
		t.Fatal("an explicit bwrap request must never silently downgrade")
	}
}

func TestResolveIsolate_ProbeFailure(t *testing.T) {
	fakeBwrap(t, 1) // present but broken (e.g. user namespaces disabled)
	if _, err := ResolveIsolate(IsolateConfig{Mode: IsolateBwrap}); err == nil {
		t.Fatal("broken bwrap must fail an explicit request")
	}
	r, err := ResolveIsolate(IsolateConfig{Mode: IsolateAuto})
	if err != nil || r.active() {
		t.Fatalf("auto must degrade on probe failure, got (%v, %v)", r, err)
	}
	if !strings.Contains(r.warn, "probe failed") {
		t.Errorf("warning should carry the probe reason, got %q", r.warn)
	}
}

func TestResolveIsolate_ExtraRWExpandsHome(t *testing.T) {
	fakeBwrap(t, 0)
	r, err := ResolveIsolate(IsolateConfig{Mode: IsolateAuto, Rw: []string{"~/scratch"}})
	if err != nil || !r.active() {
		t.Fatalf("got (%v, %v)", r, err)
	}
	home, _ := os.UserHomeDir()
	if len(r.extraRW) != 1 || r.extraRW[0].dst != filepath.Join(home, "scratch") {
		t.Errorf("rw paths must be ~-expanded into same-path binds, got %v", r.extraRW)
	}
}

func TestIsolateWrap_OrderAndIdentity(t *testing.T) {
	binds := []isoBind{{"/seed/.claude", "/home/u/.claude"}, {"/dup", "/dup"}}

	// Off → identity.
	off, _ := ResolveIsolate(IsolateConfig{Mode: ""})
	argv := []string{"agent", "--x"}
	if got := off.wrap("/wd", binds, argv); len(got) != 2 || got[0] != "agent" {
		t.Fatalf("off must not touch argv, got %v", got)
	}

	fakeBwrap(t, 0)
	r, err := ResolveIsolate(IsolateConfig{Mode: IsolateAuto, Rw: []string{"/cfg/rw"}})
	if err != nil || !r.active() {
		t.Fatalf("got (%v, %v)", r, err)
	}
	got := r.wrap("/tmp/wd", append(binds, isoBind{"/tmp/wd", "/tmp/wd"}), []string{"agent", "arg"})
	joined := strings.Join(got, " ")

	if strings.Index(joined, "--ro-bind / /") > strings.Index(joined, "--tmpfs /tmp") {
		t.Error("root must be mounted read-only")
	}
	// The ordering rule that broke the first probe: tmpfs /tmp BEFORE any
	// bind under /tmp, or the workdir/seed is buried under the tmpfs.
	if i, j := strings.Index(joined, "--tmpfs /tmp"), strings.Index(joined, "--bind /tmp/wd"); i < 0 || j < 0 || i > j {
		t.Errorf("tmpfs /tmp must precede /tmp binds (the eval workdir lives there): %q", joined)
	}
	if !strings.Contains(joined, "--bind /tmp/wd /tmp/wd") || !strings.Contains(joined, "--chdir /tmp/wd") {
		t.Errorf("workdir must be bound rw and chdir'd to: %q", joined)
	}
	if strings.Contains(joined, "--unshare-net") {
		t.Error("the network must stay shared (localhost model servers — ADR-0098)")
	}
	for _, want := range []string{"--bind /cfg/rw /cfg/rw", "--bind /seed/.claude /home/u/.claude"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing bind %q in %q", want, joined)
		}
	}
	if strings.Count(joined, "--bind /tmp/wd") != 1 {
		t.Errorf("workdir must not be bound twice: %q", joined)
	}
	// agent argv must follow -- after the mounts
	if got[len(got)-3] != "--" || got[len(got)-2] != "agent" || got[len(got)-1] != "arg" {
		t.Errorf("agent argv must follow -- after the mounts, got tail %v", got[len(got)-4:])
	}
	if got[1] == "agent" {
		t.Error("prefix must be bwrap, not the agent")
	}
}

func TestIsoView(t *testing.T) {
	binds := []isoBind{{"/copy/.claude", "/home/u/.claude"}}
	if got := isoView(binds, "/home/u/.claude"); got != "/copy/.claude" {
		t.Errorf("bound path must resolve to the copy, got %q", got)
	}
	if got := isoView(binds, "/home/u/.mimocode"); got != "/home/u/.mimocode" {
		t.Errorf("unbound path must pass through, got %q", got)
	}
}

func TestSeedCopy_AndCleanup(t *testing.T) {
	// Directory seed.
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp, err := seedCopy(src)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cp, "a.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("copy contents: %q %v", data, err)
	}

	// File seed.
	f := filepath.Join(t.TempDir(), "f.json")
	if err := os.WriteFile(f, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	fcp, err := seedCopy(f)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(fcp); err != nil || string(data) != "{}" {
		t.Fatalf("file copy: %q %v", data, err)
	}

	// Missing src seeds nothing (nothing to give the agent rw; the live
	// path just stays read-only).
	if cp2, err := seedCopy(filepath.Join(t.TempDir(), "nope")); err != nil || cp2 != "" {
		t.Fatalf("missing source: (%q, %v)", cp2, err)
	}

	// cleanupSeeds removes copies but leaves identity binds (state roots —
	// src == dst: paths we never created, not copies).
	identity := t.TempDir()
	binds := []isoBind{{cp, "/home/u/.claude"}, {fcp, "/home/u/.claude.json"}, {identity, identity}}
	cleanupSeeds(binds)
	if _, err := os.Stat(cp); !os.IsNotExist(err) {
		t.Error("copy must be removed at cleanup")
	}
	if _, err := os.Stat(identity); err != nil {
		t.Errorf("identity binds (state roots) must never be removed: %v", err)
	}
	// The live source is untouched by cleanup (only the copy dies).
	if _, err := os.Stat(filepath.Join(src, "a.txt")); err != nil {
		t.Errorf("live source must survive cleanup: %v", err)
	}
	if _, err := os.Stat(fcp); !os.IsNotExist(err) {
		t.Error("file copy must be removed at cleanup")
	}
}

func TestShq(t *testing.T) {
	if got := shq("/tmp/w d"); got != "'/tmp/w d'" {
		t.Errorf("spaces: %q", got)
	}
	if got := shq("it's"); got != `'it'\''s'` {
		t.Errorf("quote escaping: %q", got)
	}
	joined := shqJoin([]string{"a", "b c"})
	if joined != "'a' 'b c'" {
		t.Errorf("join: %q", joined)
	}
}

func TestExperiment_ValidateIsolate(t *testing.T) {
	for _, mode := range []string{"", IsolateOff, IsolateAuto, IsolateBwrap} {
		e := &Experiment{Repetitions: 1, Isolate: IsolateConfig{Mode: mode}}
		if err := e.Validate(); err != nil {
			t.Errorf("mode %q must be valid: %v", mode, err)
		}
	}
	e := &Experiment{Repetitions: 1, Isolate: IsolateConfig{Mode: "firejail"}}
	if err := e.Validate(); err == nil {
		t.Error("unknown isolate.mode must be rejected before any agent launches")
	}
}

// --- live escape matrix (needs a working bwrap; skipped where absent) ---

// TestIsolateBwrap_EscapeMatrix is the issue's acceptance test, run without
// any LLM: a wrapped /bin/sh attempts every escape class from the incident —
// HOME (the live checkout lives there), other checkouts, rootfs — and each
// must fail, while the workdir, an explicit rw path and /tmp stay writable.
// The sandbox's /tmp is private, so scratch writes don't even leak out.
func TestIsolateBwrap_EscapeMatrix(t *testing.T) {
	r, err := ResolveIsolate(IsolateConfig{Mode: IsolateBwrap})
	if err != nil {
		t.Skipf("no working bwrap on this machine: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	rw := t.TempDir()
	pwned := filepath.Join(home, ".milk-iso-escape-test")
	t.Cleanup(func() { os.Remove(pwned) }) // belt: if the assertion below ever fails

	// Every expectation checked inside the sandbox; the script exits non-zero
	// naming any invariant that broke — one run proves the whole matrix.
	script := strings.Join([]string{
		"fail=0",
		"echo ok > " + workdir + "/in-workdir || { echo BROKEN:workdir-not-rw; fail=1; }",
		"echo ok > " + rw + "/in-rw || { echo BROKEN:rw-not-rw; fail=1; }",
		"echo ok > /tmp/in-tmp || { echo BROKEN:tmp-not-rw; fail=1; }",
		"if echo ESCAPED > " + pwned + " 2>/dev/null; then echo BROKEN:home-writable; fail=1; fi",
		"if echo ESCAPED > /etc/iso-escape 2>/dev/null; then echo BROKEN:rootfs-writable; fail=1; fi",
		"if echo ESCAPED > " + filepath.Join(home, "altworkspace", "milk", "iso-escape") + " 2>/dev/null; then echo BROKEN:checkout-writable; fail=1; fi",
		"test -r /bin/sh || { echo BROKEN:rootfs-invisible; fail=1; }",
		"test -r " + filepath.Join(home, ".bashrc") + " || echo note: bashrc not readable - harmless here",
		"exit $fail",
	}, "; ")
	argv := r.wrap(workdir, []isoBind{{rw, rw}}, []string{"/bin/sh", "-c", script})
	out, runErr := execRun(argv)
	if runErr != nil {
		t.Fatalf("escape matrix failed inside the sandbox: %v\n%s", runErr, out)
	}
	// Outside checks: nothing escaped into the real filesystem.
	for _, p := range []string{pwned, "/tmp/in-tmp"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s exists outside the sandbox — an escape succeeded", p)
			os.Remove(p)
		}
	}
	if _, err := os.Stat(filepath.Join(workdir, "in-workdir")); err != nil {
		t.Errorf("workdir write must land in the real workdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rw, "in-rw")); err != nil {
		t.Errorf("explicit rw write must land for real: %v", err)
	}
	if _, err := os.Stat("/etc/iso-escape"); err == nil {
		t.Error("rootfs was modified by a sandboxed run")
		os.Remove("/etc/iso-escape")
	}
}

// --- adapter seams ---

func TestClaudeLaunchLine_WrappedWhenIsolated(t *testing.T) {
	// Off: plain quoted launch (the legacy shape — cd, agent, flags).
	off := &claudeAdapter{sessionID: "sid", workdir: "/tmp/wd"}
	got := off.launchLine()
	want := "cd '/tmp/wd' && " + shqJoin([]string{
		"claude", "--session-id", "sid", "--dangerously-skip-permissions",
	})
	if got != want {
		t.Errorf("off launch line:\n got %q\nwant %q", got, want)
	}

	// Isolated: bwrap prefix, mounts before --, claude as the wrapped argv.
	fakeBwrap(t, 0)
	r, err := ResolveIsolate(IsolateConfig{Mode: IsolateAuto})
	if err != nil || !r.active() {
		t.Fatalf("got (%v, %v)", r, err)
	}
	a := &claudeAdapter{sessionID: "sid", workdir: "/tmp/wd", iso: r,
		isoBinds: []isoBind{{"/seed/.claude", "/home/u/.claude"}}}
	line := a.launchLine()
	if !strings.HasPrefix(line, "cd '/tmp/wd' && ") {
		t.Errorf("must cd into the workdir first: %q", line)
	}
	if !strings.Contains(line, "'--ro-bind' '/' '/'") || !strings.Contains(line, "'--bind' '/seed/.claude' '/home/u/.claude'") {
		t.Errorf("agent must launch inside the sandbox: %q", line)
	}
	shell := line[strings.Index(line, "&& ")+3:]
	if !strings.Contains(shell, "'--' 'claude' '--session-id' 'sid'") {
		t.Errorf("claude argv must be the wrapped tail, shell-quoted: %q", shell)
	}
}

// fakeIsoAdapter implements AgentAdapter + Isolatable to observe what
// applyIsolate hands adapters.
type fakeIsoAdapter struct {
	got  *isolateRuntime
	setC bool
}

func (f *fakeIsoAdapter) Name() string                              { return "fake" }
func (f *fakeIsoAdapter) SetArgs([]string)                          {}
func (f *fakeIsoAdapter) Start(ctx context.Context, s string) error { return nil }
func (f *fakeIsoAdapter) RunPrompt(ctx context.Context, s string) (RunResult, error) {
	return RunResult{}, nil
}
func (f *fakeIsoAdapter) Stop() error { return nil }
func (f *fakeIsoAdapter) SetIsolate(r *isolateRuntime) {
	f.got = r
	f.setC = true
}

func TestHarnessApplyIsolate_SetsRuntimeOnIsolatable(t *testing.T) {
	fakeBwrap(t, 0)
	h := &Harness{Adapters: []AgentAdapter{&fakeIsoAdapter{}}}
	if err := h.applyIsolate(IsolateConfig{Mode: IsolateAuto}); err != nil {
		t.Fatal(err)
	}
	fa := h.Adapters[0].(*fakeIsoAdapter)
	if !fa.setC || !fa.got.active() {
		t.Fatal("an Isolatable adapter must receive the resolved runtime")
	}

	// Invalid mode surfaces as an error before anything runs.
	if err := h.applyIsolate(IsolateConfig{Mode: "nope"}); err == nil {
		t.Error("invalid mode must fail applyIsolate")
	}
}
