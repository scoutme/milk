package eval

// Filesystem isolation for agent runs (issue #355).
//
// Until now the temporary scenario workdir was the *only* isolation: every
// adapter ran the agent with cmd.Dir = workdir and nothing else — an agent
// could write anywhere on the machine, and did (the routing-reps incident
// escaped into the live milk checkout and authored real work there across
// repetitions). This file is the bwrap backend: the whole filesystem mounted
// read-only with an explicit rw allowlist, so writes outside the allowlist
// are impossible by construction while nothing in the agent's context
// changes (evaluation purity — constraint 1 of the issue).
//
// Shape (ADR-0098 records the decisions):
//
//	bwrap --ro-bind / / --tmpfs /tmp --bind <workdir> <workdir> … -- <agent>
//
//   - No read-allowlist to maintain: binaries, HOME configs, git identity all
//     stay *visible* (read-only). A missing rw fails loudly (the agent's tool
//     errors, caught in first testing); an accidental rw would fail silently
//     — so the risk sits on the detectable side.
//   - HOME subtrees the agent legitimately reads AND writes (claude's
//     ~/.claude, mimo's ~/.mimocode) are per-run *copies* bound over the live
//     paths: the agent sees identical bytes — comparability with unsandboxed
//     baselines holds — and every write lands in the copy, discarded at Stop.
//   - The network stays shared (no --unshare-net): localhost model servers
//     and API access keep working. This issue is about writes, not traffic.
//   - `auto` degrades loudly instead of failing: no working bwrap (macOS,
//     Windows, WSL without user namespaces) → one explicit warning and a
//     normal unsandboxed run, so CI and other machines warn-and-run rather
//     than break.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Isolate modes (isolate.mode / --isolate).
const (
	IsolateOff   = "off"
	IsolateAuto  = "auto"
	IsolateBwrap = "bwrap"
)

// IsolateConfig is the experiment- and CLI-facing isolation request (yaml
// `isolate:` block, --isolate flag). Mode:
//
//   - "" / "off": no isolation (the default — behavior unchanged).
//   - "auto": use bwrap when present and its probe passes, otherwise warn
//     loudly and run unsandboxed.
//   - "bwrap": require bwrap — resolution errors if it is missing or the
//     probe fails. An explicit request must never silently downgrade.
type IsolateConfig struct {
	Mode string   `yaml:"mode"`
	Rw   []string `yaml:"rw,omitempty"` // extra rw paths ("~" expanded): agent binaries' state, scratch dirs…
}

// isoBind is one explicit rw surface inside the sandbox: src is mounted at
// dst. Plain rw paths bind a path onto itself; HOME seeds bind a per-run copy
// onto the live path (seedCopy), so the agent reads identical bytes while
// writes land in the copy.
type isoBind struct{ src, dst string }

// isolateRuntime is the resolved isolation config. The nil runtime (and a
// runtime with no bwrap) mean "off": every method is the identity, so
// adapters carry the pointer unconditionally and special-case nothing.
// Resolved once per command — the bwrap probe runs a single time.
type isolateRuntime struct {
	bwrapPath string    // "" = degraded/off
	warn      string    // degradation note (auto without working bwrap), surfaced once
	extraRW   []isoBind // from IsolateConfig.Rw (same-path binds)
}

// active reports whether runs are actually sandboxed (nil-safe).
func (r *isolateRuntime) active() bool { return r != nil && r.bwrapPath != "" }

// ResolveIsolate probes bwrap once per command and returns the runtime for
// the requested mode: nil for off, a runtime (possibly carrying a
// degradation warning) for auto, an error for explicit bwrap without a
// working bwrap or an unknown mode.
func ResolveIsolate(cfg IsolateConfig) (*isolateRuntime, error) {
	switch cfg.Mode {
	case "", IsolateOff:
		return nil, nil
	case IsolateAuto, IsolateBwrap:
	default:
		return nil, fmt.Errorf("isolate.mode must be %q, %q or %q, got %q", IsolateOff, IsolateAuto, IsolateBwrap, cfg.Mode)
	}
	r := &isolateRuntime{}
	for _, p := range cfg.Rw {
		if p = expandHome(p); p != "" {
			r.extraRW = append(r.extraRW, isoBind{p, p})
		}
	}
	path, err := probeBwrap()
	if err != nil {
		if cfg.Mode == IsolateBwrap {
			return nil, fmt.Errorf("isolate.mode=%s: %w", IsolateBwrap, err)
		}
		r.warn = fmt.Sprintf("isolate=%s: %v — running WITHOUT filesystem isolation (issue #355)", IsolateAuto, err)
		return r, nil
	}
	r.bwrapPath = path
	return r, nil
}

// probeBwrap finds the binary and proves the kernel side works with a
// trivial sandboxed exec — a bare PATH hit would claim isolation on a host
// where bwrap then fails at launch (e.g. user namespaces disabled).
func probeBwrap() (string, error) {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return "", fmt.Errorf("bwrap not found on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path,
		"--unshare-user", "--ro-bind", "/", "/", "/bin/true").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("bwrap probe failed: %s", msg)
	}
	return path, nil
}

// wrap prefixes argv with the bwrap sandbox — every path read-only except
// the workdir and the explicit rw binds (config Rw, adapter seeds, state
// roots). nil/degraded runtime → argv unchanged.
//
// Argument order matters: --tmpfs /tmp must be mounted *before* the binds,
// or a workdir/seed under /tmp (eval's MkdirTemp puts them there) would be
// buried under the empty tmpfs — a mistake the first probe of this design
// hit immediately. --chdir pins the agent's cwd to the workdir instead of
// trusting bwrap's inherited cwd.
func (r *isolateRuntime) wrap(workdir string, binds []isoBind, argv []string) []string {
	if !r.active() || len(argv) == 0 {
		return argv
	}
	out := []string{
		r.bwrapPath,
		"--unshare-user", "--unshare-pid",
		"--ro-bind", "/", "/",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
	}
	if workdir != "" {
		out = append(out, "--bind", workdir, workdir, "--chdir", workdir)
	}
	seen := map[string]bool{workdir: true}
	for _, b := range append(r.extraRW, binds...) {
		if b.src == "" || b.dst == "" || seen[b.dst] {
			continue
		}
		seen[b.dst] = true
		out = append(out, "--bind", b.src, b.dst)
	}
	out = append(out, "--")
	return append(out, argv...)
}

// isoView resolves a live path through the bind table: when the path is a
// bind destination (a HOME seed), returns the copy the sandbox actually
// serves — so harness-side reads and pre-writes (claude's transcript,
// project settings) hit the same files the agent sees through the mount.
// Unbound paths pass through unchanged.
func isoView(binds []isoBind, path string) string {
	for _, b := range binds {
		if b.dst == path {
			return b.src
		}
	}
	return path
}

// seedCopy copies src (file or directory) into a fresh temp location and
// returns the copy; the sandbox binds it over src's live path. "src does not
// exist" → ("", nil): nothing to seed, the live path simply stays read-only.
// Copies are per adapter Start (per scenario run) and removed at Stop, so no
// run ever sees another run's writes — the same freshness rule the state
// sandbox (SandboxConfig) applies to milk's state root.
func seedCopy(src string) (string, error) {
	st, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat seed %s: %w", src, err)
	}
	if st.IsDir() {
		dir, err := os.MkdirTemp("", "milk-eval-seed-*")
		if err != nil {
			return "", fmt.Errorf("creating seed dir: %w", err)
		}
		if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
			os.RemoveAll(dir)
			return "", fmt.Errorf("seeding %s: %w", src, err)
		}
		return dir, nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("reading seed file %s: %w", src, err)
	}
	f, err := os.CreateTemp("", "milk-eval-seed-*")
	if err != nil {
		return "", fmt.Errorf("creating seed file: %w", err)
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", fmt.Errorf("copying seed file %s: %w", src, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// seedBinds creates binds for each existing path, copying it first. Used by
// the claude and mimo adapters for their HOME subtrees; milk needs none (its
// state root is the existing sandbox, bound rw as-is).
func seedBinds(paths ...string) ([]isoBind, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("getting home dir: %w", err)
	}
	var binds []isoBind
	for _, rel := range paths {
		live := filepath.Join(home, rel)
		cp, err := seedCopy(live)
		if err != nil {
			cleanupSeeds(binds)
			return nil, err
		}
		if cp != "" {
			binds = append(binds, isoBind{cp, live})
		}
	}
	return binds, nil
}

// cleanupSeeds removes every bind's copy (identity binds — src == dst, like
// a state root — are left alone: they are not copies we created).
func cleanupSeeds(binds []isoBind) {
	for _, b := range binds {
		if b.src != b.dst {
			os.RemoveAll(b.src)
		}
	}
}

// shq quotes one token as a POSIX shell word — tmux send-keys types the
// launch line into the pane's shell, so wrapped argv must survive it.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shqJoin quotes each argv token and joins with spaces.
func shqJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shq(a)
	}
	return strings.Join(parts, " ")
}

// warnOnce writes a degradation/warning line to w once — adapters that run
// per scenario would otherwise repeat it for every run.
func warnOnce(w io.Writer, msg string, said *bool) {
	if *said || msg == "" {
		return
	}
	*said = true
	fmt.Fprintln(w, msg)
}
