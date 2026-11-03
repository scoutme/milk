// Command showcase records milk's showcase GIF from the one manifest in
// demo/showcase.yaml (issue #387, ADR-0099):
//
//  1. calibrate — drive a live TUI in tmux, wait on pane state (never on
//     guessed sleeps), and measure each beat;
//  2. generate — write demo/demo.tape from those measurements;
//  3. record — hand the tape to vhs for the visual artifact.
//
// Steps 1+2 are deterministic given a working agent config; step 3 needs
// vhs. `-tape-only` stops after the tape (CI-friendly: needs tmux + milk +
// configured agents, but no vhs/display).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/showcase"
)

func main() {
	var (
		manifestPath = flag.String("manifest", "demo/showcase.yaml", "showcase manifest (one source of truth for beats and waits)")
		milkBin      = flag.String("milk", "", "milk binary to record (default: $MILK_BIN, then PATH)")
		vhsBin       = flag.String("vhs", "vhs", "vhs binary for the recording pass")
		configPath   = flag.String("config", "", "source config.json (default: <milk state root>/config.json)")
		tapeOnly     = flag.Bool("tape-only", false, "calibrate and generate the tape, skip the vhs recording")
		margin       = flag.Float64("margin", 0, "override the manifest's sleep margin (>= 1)")
		timeout      = flag.Duration("timeout", 0, "override the per-wait timeout")
		session      = flag.String("session", "", "tmux session name (default: showcase-<pid>)")
		keep         = flag.Bool("keep", false, "keep temp config roots and the tmux session (debugging)")
	)
	flag.Parse()

	if err := run(*manifestPath, *milkBin, *vhsBin, *configPath, *tapeOnly, *margin, *timeout, *session, *keep); err != nil {
		fmt.Fprintln(os.Stderr, "showcase: "+err.Error())
		os.Exit(1)
	}
}

func run(manifestPath, milkBin, vhsBin, srcConfig string, tapeOnly bool, marginOverride float64, timeoutOverride time.Duration, session string, keep bool) error {
	manifest, err := showcase.Load(manifestPath)
	if err != nil {
		return err
	}
	if marginOverride > 0 {
		if marginOverride < 1 {
			return fmt.Errorf("margin %.2f must be >= 1", marginOverride)
		}
		manifest.Margin = marginOverride
	}
	if timeoutOverride > 0 {
		manifest.Timeout = timeoutOverride
	}

	// Preflight: the toolchain decides what this run can produce, loudly.
	tmuxOK, vhsOK := showcase.Available()
	if !tmuxOK {
		return fmt.Errorf("tmux not found — calibration drives a real terminal (install tmux)")
	}
	if !tapeOnly && !vhsOK {
		return fmt.Errorf("vhs not found — install it (gem install vhs) or pass -tape-only")
	}
	milkPath, err := resolveMilk(milkBin)
	if err != nil {
		return err
	}
	if srcConfig == "" {
		dir, derr := config.Dir()
		if derr != nil || dir == "" {
			return fmt.Errorf("no milk state root resolves — pass -config")
		}
		srcConfig = filepath.Join(dir, "config.json")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	workdir := cwd
	if manifest.Workdir != "" && manifest.Workdir != "." {
		if !filepath.IsAbs(manifest.Workdir) {
			workdir = filepath.Join(cwd, manifest.Workdir)
		} else {
			workdir = manifest.Workdir
		}
	}
	if session == "" {
		session = fmt.Sprintf("showcase-%d", os.Getpid())
	}

	// ---- pass 1: calibrate against a clean root --------------------------
	calRoot, err := os.MkdirTemp("", "milk-showcase-cal-*")
	if err != nil {
		return err
	}
	if !keep {
		defer os.RemoveAll(calRoot)
	}
	if _, err := showcase.WriteCleanConfig(srcConfig, calRoot, manifest.StripConfigKeys); err != nil {
		return err
	}
	launch := fmt.Sprintf("MILK_CONFIG_ROOT=%s %s", shellQuote(calRoot), shellQuote(milkPath)) +
		" " + strings.TrimSpace(manifest.Launch)

	fmt.Fprintf(os.Stderr, "showcase: calibrating %d beat(s) in tmux...\n", len(manifest.Beats))
	opts := showcase.Options{
		Session:     session,
		Workdir:     workdir,
		Launch:      launch,
		ConfigRoot:  calRoot,
		Timeout:     manifest.Timeout,
		KeepSession: keep,
	}
	res, err := showcase.Calibrate(context.Background(), manifest, opts)
	if err != nil {
		hint := fmt.Sprintf("(tmux session %q kept for inspection)", session)
		if !keep {
			hint = fmt.Sprintf("(tmux session %q killed; rerun with -keep to inspect)", session)
		}
		return fmt.Errorf("%w\n%s", err, hint)
	}
	for _, t := range res.Timings {
		state := "ok"
		if t.Skipped {
			state = "skipped"
		}
		fmt.Fprintf(os.Stderr, "  %-16s %6.1fs %s\n", t.ID, t.Elapsed.Seconds(), state)
	}

	// ---- pass 2: generate the tape (its own clean root for the record) ---
	recRoot, err := os.MkdirTemp("", "milk-showcase-rec-*")
	if err != nil {
		return err
	}
	if !keep {
		defer os.RemoveAll(recRoot)
	}
	if _, err := showcase.WriteCleanConfig(srcConfig, recRoot, manifest.StripConfigKeys); err != nil {
		return err
	}
	tape := manifest.TapePath(manifestPath)
	rendered := showcase.RenderTape(manifest, res, showcase.RenderOpts{
		MilkPath:   milkPath,
		ConfigRoot: recRoot,
		Workdir:    workdir,
	})
	if err := os.WriteFile(tape, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("writing tape %s: %w", tape, err)
	}
	fmt.Fprintf(os.Stderr, "showcase: wrote %s\n", tape)
	if keep {
		fmt.Fprintf(os.Stderr, "showcase: kept config roots cal=%s rec=%s session=%s\n", calRoot, recRoot, session)
	}

	// ---- pass 3: record ---------------------------------------------------
	if tapeOnly {
		fmt.Fprintf(os.Stderr, "showcase: -tape-only — run `vhs %s` (or `task demo`) to record %s\n", tape, manifest.Output)
		return nil
	}
	abs, err := filepath.Abs(tape)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "showcase: recording %s with vhs...\n", manifest.Output)
	cmd := exec.Command(vhsBin, abs)
	cmd.Dir = cwd // Output is relative to the invocation directory, as before
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("vhs: %w", err)
	}
	fmt.Fprintf(os.Stderr, "showcase: recorded %s\n", manifest.Output)
	return nil
}

// resolveMilk picks the binary the showcase records: -milk, then MILK_BIN,
// then PATH — with a build hint, because an installed-but-stale binary
// records yesterday's UI.
func resolveMilk(flagVal string) (string, error) {
	for _, cand := range []string{flagVal, os.Getenv("MILK_BIN")} {
		if cand != "" {
			abs, err := filepath.Abs(cand)
			if err != nil {
				return "", err
			}
			if _, err := os.Stat(abs); err != nil {
				return "", fmt.Errorf("milk binary %s: %w", abs, err)
			}
			return abs, nil
		}
	}
	if p, err := exec.LookPath("milk"); err == nil {
		abs, _ := filepath.Abs(p)
		return abs, nil
	}
	return "", fmt.Errorf("milk not on PATH — run `task build` first or pass -milk /path/to/milk")
}

func shellQuote(p string) string {
	return "'" + strings.ReplaceAll(p, `'`, `'\''`) + "'"
}
