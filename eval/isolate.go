package eval

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Filesystem isolation of agent runs (issue #355). `milk eval` gives every
// run a throwaway scenario workdir (setupWorkdir + defer os.RemoveAll) and
// that is the *only* boundary: nothing stopped an agent from writing
// anywhere else on the machine, and nothing detected it afterwards — the
// routing-reps incident (scoring agents escaping into the live milk
// checkout across repetitions, one escape chain committed as PR #352)
// proved it matters. The cheap fixes were considered and rejected in the
// issue itself (prompt guardrails contaminate the evaluation, worktree
// git-status guards are too narrow and false-positive on the operator,
// tool-trail write tripwires block legitimate surfaces) — the only real
// fix is OS-level sandboxing of the run process, which this file provides
// on top of bubblewrap.
//
// The shape is "read-only by default, enumerate the writes":
//
//	bwrap --unshare-user --ro-bind / / --tmpfs /tmp --bind <workdir> … -- <agent>
//
// Everything stays *visible* (agent binaries, git identity, $HOME config,
// other repos — no read-allowlist to maintain, constraint 3 of the issue
// falls out free) but only the scenario workdir, a fresh per-run /tmp
// scratch and the adapter's own state root are writable. The risk therefore
// inverts toward the detectable direction: a surface missing from the write
// list fails loudly in testing (tool errors), while an accidental rw bind
// would fail silently — so the allowlist only ever grows from observed,
// reproducible needs. The network namespace is shared on purpose: localhost
// model servers must keep working.
//
// Modes (experiment `isolate:` / `--isolate`): "off" (default), "auto"
// (probe bubblewrap, degrade loudly to unsandboxed when absent — macOS and
// Windows today), "bwrap" (require it, fail closed). The mode is resolved
// once per experiment and handed to every adapter alike, so a comparison is
// never half-sandboxed (comparability — the issue's constraint 5); the
// judge/scoring side runs outside the sandbox entirely.

// Effective isolation backends (what adapters receive after resolution).
const (
	// IsolateOff runs the agent unsandboxed.
	IsolateOff = "off"
	// IsolateBwrap wraps the agent in a bubblewrap filesystem sandbox.
	IsolateBwrap = "bwrap"
)

// Requested isolation modes (what the experiment/flag asks for).
const (
	// IsolateAuto uses bubblewrap when it works here, else warns loudly and
	// runs unsandboxed.
	IsolateAuto = "auto"
)

// IsolationConfig is the experiment's `isolate:` instruction (issue #355).
type IsolationConfig struct {
	// Mode is "off" (or ""), "auto" or "bwrap" — validated by
	// Experiment.Validate.
	Mode string
}

// Isolatable is implemented by adapters that can wrap their agent process
// in a filesystem sandbox (issue #355). The harness resolves the isolation
// request once — probing bubblewrap a single time per experiment — and
// hands the *effective* mode ("off" or IsolateBwrap) to every adapter
// through this interface, so one comparison is never run half-sandboxed;
// an adapter that doesn't implement it runs unsandboxed and the harness
// says so. Mirrors Sandboxable (adapter.go), which isolates state instead
// of the filesystem.
type Isolatable interface {
	SetIsolation(effective string)
}

// Resolve probes for a usable backend and returns the effective mode
// (IsolateOff or IsolateBwrap). The warning is non-empty exactly when the
// run continues unsandboxed despite being asked for isolation ("auto"
// degradation, issue #355 constraint 4); "bwrap" fails instead of silently
// running uncontained.
func (c IsolationConfig) Resolve() (effective, warning string, err error) {
	switch c.Mode {
	case "", IsolateOff:
		return IsolateOff, "", nil
	case IsolateAuto, IsolateBwrap:
	default:
		return "", "", fmt.Errorf("isolate mode must be %q, %q or %q, got %q", IsolateAuto, IsolateBwrap, IsolateOff, c.Mode)
	}
	if err := probeBwrap(); err != nil {
		if c.Mode == IsolateBwrap {
			return "", "", fmt.Errorf("isolate=bwrap but bubblewrap is unusable: %w (install bubblewrap, or run with --isolate=auto/off)", err)
		}
		return IsolateOff, fmt.Sprintf("warning: filesystem isolation unavailable (%v) — running UNSANDBOXED; writes outside the scenario workdir are NOT contained", err), nil
	}
	return IsolateBwrap, "", nil
}

// probeBwrap runs a throwaway sandbox to prove bubblewrap actually works on
// this host — a bare LookPath is not enough: kernels with user namespaces
// disabled (some distro defaults, some container runners) ship a bwrap
// binary that fails every run. The probe exercises the same mount shape
// isolateWrap builds, so a mount-order regression fails here rather than
// mid-experiment. No agent involved; milliseconds.
func probeBwrap() error {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return fmt.Errorf("bwrap not found in PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bwrap,
		"--unshare-user", "--ro-bind", "/", "/",
		"--tmpfs", "/tmp", "--proc", "/proc", "--dev", "/dev",
		"--", "/bin/sh", "-c", "")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isolateWrap wraps argv in a bubblewrap filesystem sandbox (issue #355):
// the whole filesystem read-only, /tmp a fresh per-run scratch tmpfs, and
// workdir plus any extra dirs the only writable locations. The child's
// working directory is workdir (--chdir).
//
// Mount order is load-bearing and pinned by test: bubblewrap applies its
// mounts in argv order and eval workdirs live under /tmp (os.MkdirTemp), so
// --tmpfs /tmp must come *before* the workdir bind — the other order
// shadows the workdir and the run dies on its first write ("cannot create
// …: Directory nonexistent", hit while prototyping). Duplicate binds are
// collapsed so a state root equal to the workdir cannot shadow itself.
func isolateWrap(argv []string, workdir string, extraRW ...string) []string {
	wrap := []string{
		"bwrap",
		"--unshare-user",
		"--ro-bind", "/", "/",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
	}
	seen := map[string]bool{}
	for _, d := range append([]string{workdir}, extraRW...) {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		wrap = append(wrap, "--bind", d, d)
	}
	wrap = append(wrap, "--chdir", workdir, "--")
	return append(wrap, absArgv(argv)...)
}

// absArgv makes argv[0] absolute when it is a *path* (contains a separator):
// --chdir lands the child in the workdir, so a relative program path would
// resolve against the wrong directory inside the sandbox. A bare name
// ("claude", "mimo") is left alone — that is PATH lookup, and making it
// absolute would break it.
func absArgv(argv []string) []string {
	if len(argv) == 0 || filepath.IsAbs(argv[0]) || !strings.ContainsRune(argv[0], os.PathSeparator) {
		return argv
	}
	abs, err := filepath.Abs(argv[0])
	if err != nil {
		return argv
	}
	out := append([]string(nil), argv...)
	out[0] = abs
	return out
}

// isolatedExec builds the command for argv's program — wrapped in the
// sandbox when effective is IsolateBwrap, plain otherwise — so an adapter's
// exec seam is a one-line change either way. dir is applied as cmd.Dir
// (harmless under the wrapper: --chdir already lands the child there).
func isolatedExec(ctx context.Context, effective string, argv []string, workdir string, extraRW ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if effective == IsolateBwrap {
		w := isolateWrap(argv, workdir, extraRW...)
		cmd = exec.CommandContext(ctx, w[0], w[1:]...)
	} else {
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
	}
	cmd.Dir = workdir
	return cmd
}

// shellJoin renders argv as one POSIX shell command line (the tmux seam
// sends a single command): every argument single-quoted, embedded quotes
// escaped — prompt-shaped arguments (extraArgs) can contain anything.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, s := range argv {
		parts[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}

// ensureRWDir makes sure a writable bind source exists: bubblewrap refuses
// to bind a missing path, and an adapter's state root is legitimately
// created on first use anyway (mkdir -p is what the agent's own first run
// would do). Returns "" when it cannot be made usable — the caller then
// drops it from the bind list and the write fails loudly *inside* the
// sandbox instead of failing the wrap.
func ensureRWDir(dir string) string {
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

// ensureRWFile is ensureRWDir for a file bind source (e.g. claude-code's
// ~/.claude.json): created empty when missing (again: what the agent's own
// first run would do), "" when unusable.
func ensureRWFile(path string) string {
	if path == "" {
		return ""
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return ""
	}
	_ = f.Close()
	return path
}
