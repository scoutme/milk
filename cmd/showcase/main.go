// Command showcase runs milk's showcase from the one manifest in
// demo/showcase.yaml (issue #387, ADR-0099) through its two runners:
//
//  1. calibrate — drive a live TUI in tmux, wait on pane state (never on
//     guessed sleeps), and measure each beat;
//  2. generate — write demo/demo.tape from those measurements;
//  3. record — hand the tape to vhs for the visual artifact;
//  4. verify (-verify) — the same beats, asserted: every wait must hold
//     within the timeout, a PASS/FAIL report to stdout, exit non-zero on
//     the first broken beat (-beats narrows to named beats).
//
// Steps 1+2 are deterministic given a working agent config; step 3 needs
// vhs; step 4 needs only tmux. `-tape-only` stops after the tape
// (CI-friendly: needs tmux + milk + configured agents, but no vhs/display).
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
		verify       = flag.Bool("verify", false, "assert the beats instead of recording: same manifest, PASS/FAIL report on stdout, non-zero exit on the first broken wait (runner B, issue #387)")
		beats        = flag.String("beats", "", "comma-separated beat ids to run (default: all; unknown ids are an error)")
		margin       = flag.Float64("margin", 0, "override the manifest's sleep margin (>= 1)")
		timeout      = flag.Duration("timeout", 0, "override the per-wait timeout")
		session      = flag.String("session", "", "tmux session name (default: showcase-<pid>)")
		keep         = flag.Bool("keep", false, "keep temp config roots and the tmux session (debugging)")
		seedOnly     = flag.Bool("seed-only", false, "materialize the record roots (seed master + active, agents included) and exit — re-render without recalibrating or a live model")
	)
	flag.Parse()

	if err := run(runOpts{
		manifestPath: *manifestPath, milkBin: *milkBin, vhsBin: *vhsBin, srcConfig: *configPath,
		tapeOnly: *tapeOnly, verify: *verify, beats: *beats,
		marginOverride: *margin, timeoutOverride: *timeout, session: *session, keep: *keep, seedOnly: *seedOnly,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "showcase: "+err.Error())
		os.Exit(1)
	}
}

// runOpts is one showcase invocation's flags — a struct because flat
// parameters past ~8 invite argument-order swaps between two booleans and
// two strings that both parse fine and mean completely different runs.
type runOpts struct {
	manifestPath    string
	milkBin         string
	vhsBin          string
	srcConfig       string
	tapeOnly        bool
	verify          bool
	beats           string
	marginOverride  float64
	timeoutOverride time.Duration
	session         string
	keep            bool
	seedOnly        bool
}

func run(o runOpts) error {
	manifestPath, milkBin, vhsBin, srcConfig := o.manifestPath, o.milkBin, o.vhsBin, o.srcConfig
	keep, session, tapeOnly, seedOnly := o.keep, o.session, o.tapeOnly, o.seedOnly
	manifest, err := showcase.Load(manifestPath)
	if err != nil {
		return err
	}
	if o.beats != "" {
		if err := manifest.FilterBeats(o.beats); err != nil {
			return err
		}
	}
	if o.marginOverride > 0 {
		if o.marginOverride < 1 {
			return fmt.Errorf("margin %.2f must be >= 1", o.marginOverride)
		}
		manifest.Margin = o.marginOverride
	}
	if o.timeoutOverride > 0 {
		manifest.Timeout = o.timeoutOverride
	}

	// Preflight: the toolchain decides what this run can produce, loudly.
	// Verify needs tmux only — it never records.
	tmuxOK, vhsOK := showcase.Available()
	if !tmuxOK {
		return fmt.Errorf("tmux not found — calibration drives a real terminal (install tmux)")
	}
	if !o.tapeOnly && !o.verify && !vhsOK {
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

	// ---- seed-only: roots for a re-render, nothing else ------------------
	// The tape's launch line re-copies <workdir>-seed into <workdir>-root on
	// every render; this materializes the seed without a live calibration
	// (fresh clone, or any time the roots are missing).
	if seedOnly {
		seed, root, serr := showcase.MaterializeSeed(srcConfig, manifest, workdir)
		if serr != nil {
			return serr
		}
		fmt.Fprintf(os.Stderr, "showcase: record roots ready seed=%s root=%s\nrun `vhs %s` to render\n", seed, root, manifest.TapePath(manifestPath))
		return nil
	}

	// ---- pass 1: calibrate against a clean root --------------------------
	calRoot, err := os.MkdirTemp("", "milk-showcase-cal-*")
	if err != nil {
		return err
	}
	if !keep {
		defer os.RemoveAll(calRoot)
	}
	if _, err := showcase.WriteCleanConfig(srcConfig, calRoot, manifest.StripConfigKeys, manifest.AllowTools); err != nil {
		return err
	}
	launch := fmt.Sprintf("MILK_CONFIG_ROOT=%s %s", shellQuote(calRoot), shellQuote(milkPath)) +
		" " + strings.TrimSpace(manifest.Launch)

	opts := showcase.Options{
		Session:     session,
		Workdir:     workdir,
		Launch:      launch,
		ConfigRoot:  calRoot,
		Timeout:     manifest.Timeout,
		KeepSession: keep,
	}

	// Seeds build the plot's starting state for this pass — before verify
	// AND calibrate (they share this root; the flags are exclusive), and
	// again for the record root below (calibration just mutated the
	// workdir, and the replay must start where the measurements did).
	if err := showcase.RunSeeds(manifest, workdir, calRoot); err != nil {
		return err
	}

	// ---- runner B: assert instead of record --------------------------------
	// (before the "calibrating" line: verifying must not claim to measure)
	if o.verify {
		fmt.Fprintf(os.Stderr, "showcase: verifying %d beat(s) in tmux...\n", len(manifest.Beats))
		rep, verr := showcase.Verify(context.Background(), manifest, opts)
		if rep != nil {
			fmt.Print(rep.Summary()) // the report is the product — stdout, before any error text
		}
		if verr != nil {
			return fmt.Errorf("%w\n%s", verr, sessionHint(keep, session))
		}
		return nil
	}

	fmt.Fprintf(os.Stderr, "showcase: calibrating %d beat(s) in tmux...\n", len(manifest.Beats))
	res, err := showcase.Calibrate(context.Background(), manifest, opts)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, sessionHint(keep, session))
	}
	for _, t := range res.Timings {
		state := "ok"
		if t.Skipped {
			state = "skipped"
		}
		fmt.Fprintf(os.Stderr, "  %-16s %6.1fs %s\n", t.ID, t.Elapsed.Seconds(), state)
	}

	// ---- pass 2: generate the tape (persistent seed/active record roots) --
	// The roots live beside the workdir — <plot>-seed is the pristine master
	// (clean config with the user's agents + the manifest's seeds), <plot>-root
	// what milk launches with, re-copied from the seed by the tape on every
	// render. They outlive the record run deliberately: the old temp root was
	// removed with the pass, so any later render booted a config-less milk —
	// recordings of it complaining about a missing primary agent.
	seedRoot, recRoot, err := showcase.MaterializeSeed(srcConfig, manifest, workdir)
	if err != nil {
		return err
	}
	tape := manifest.TapePath(manifestPath)
	rendered := showcase.RenderTape(manifest, res, showcase.RenderOpts{
		MilkPath:   milkPath,
		ConfigRoot: recRoot,
		SeedRoot:   seedRoot,
		Workdir:    workdir,
	})
	if err := os.WriteFile(tape, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("writing tape %s: %w", tape, err)
	}
	fmt.Fprintf(os.Stderr, "showcase: wrote %s\n", tape)
	if keep {
		fmt.Fprintf(os.Stderr, "showcase: kept cal root %s (record roots persist: %s, %s) session=%s\n", calRoot, seedRoot, recRoot, session)
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
	fmt.Fprintf(os.Stderr, "showcase: recording %s with vhs...\n", showcase.MP4Path(manifest.Output))
	cmd := exec.Command(vhsBin, abs)
	cmd.Dir = cwd // Output is relative to the invocation directory, as before
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("vhs: %w", err)
	}
	// vhs exits 0 even when its ffmpeg dies mid-encode (an OOM kill leaves a
	// 0-byte file and a "Host your GIF" footer) — the artifact itself is the
	// only honest success signal.
	raw := showcase.MP4Path(manifest.Output)
	if fi, serr := os.Stat(raw); serr != nil || fi.Size() == 0 {
		return fmt.Errorf("vhs produced no recording at %s (empty or missing — its ffmpeg may have been killed, e.g. out of memory; check `dmesg | grep -i oom`)", raw)
	}
	ff, ferr := exec.LookPath("ffmpeg")
	if ferr != nil {
		return fmt.Errorf("ffmpeg not found — needed to derive %s from %s", manifest.Output, raw)
	}
	fmt.Fprintf(os.Stderr, "showcase: deriving %s from %s (two-pass palette)...\n", manifest.Output, raw)
	if err := showcase.GIFTwoPass(context.Background(), ff, raw, manifest.Output); err != nil {
		return err
	}
	// Refresh the committed compressed variant too (gitignore: the tracked
	// asset is the compressed one — the site hero) so a record never ships
	// a stale homepage clip; without gifsicle the raw stands alone and the
	// note says so.
	comp, cerr := showcase.CompressGIF(mustLookPath("gifsicle"), manifest.Output, showcase.CompressedPath(manifest.Output))
	if cerr != nil {
		return cerr
	}
	if comp != "" {
		fmt.Fprintf(os.Stderr, "showcase: compressed %s\n", comp)
	} else {
		fmt.Fprintf(os.Stderr, "showcase: gifsicle not found — %s not refreshed\n", showcase.CompressedPath(manifest.Output))
	}
	fmt.Fprintf(os.Stderr, "showcase: recorded %s (+ raw %s)\n", manifest.Output, raw)
	return nil
}

// mustLookPath is LookPath with an empty-string fallback (soft-skip callers
// take "" as "feature disabled", not as an error).
func mustLookPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
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

// sessionHint tells a failed run where its tmux session went — kept when
// asked, killed otherwise (with the exact flag to keep it next time).
func sessionHint(keep bool, session string) string {
	if keep {
		return fmt.Sprintf("(tmux session %q kept for inspection)", session)
	}
	return fmt.Sprintf("(tmux session %q killed; rerun with -keep to inspect)", session)
}
