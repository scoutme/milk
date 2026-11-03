package showcase

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/tmux"
)

// Options configures one calibration run.
type Options struct {
	// Session is the tmux session name (unique per run).
	Session string
	// Workdir is the absolute directory the TUI runs in (the repo — the
	// git-panel beat needs a real repository).
	Workdir string
	// Launch is the full shell line to start the TUI, e.g.
	// "MILK_CONFIG_ROOT=/tmp/x /home/u/.local/bin/milk --new".
	Launch string
	// ConfigRoot, when set, exports MILK_CONFIG_ROOT for the launch.
	ConfigRoot string
	// Timeout bounds each individual wait (0 = manifest default).
	Timeout time.Duration
	// Interval is the capture poll interval (0 = PollInterval).
	Interval time.Duration
	// KeepSession leaves the tmux session running after calibration
	// (debugging a failing beat in the live TUI).
	KeepSession bool
}

// BeatTiming is one beat's measured wall time (0 when skipped).
type BeatTiming struct {
	ID      string
	Elapsed time.Duration
	Skipped bool
	// Steps is each step's own measured duration, in manifest order —
	// which for a wait step IS the wait. The tape emits these as inline
	// Sleeps at the step where they happened instead of one lump at the
	// beat's end: keypresses must replay in the gaps they had in
	// calibration (type /tasks 0ms after Enter that starts a model turn
	// — or the flyover-accept Enter before the picker opened — replays
	// narration that never happened in reality).
	Steps []time.Duration
}

// Result is a full calibration pass.
type Result struct {
	// LaunchElapsed is time from starting the TUI until the header is
	// painted (the tape's first sleep).
	LaunchElapsed time.Duration
	// Timings are per-beat durations, in manifest order.
	Timings []BeatTiming
	// Vars are the extracted values ({{task_id}} ...).
	Vars map[string]string
	// Snippets is the final capture per beat — the recorder prints it in
	// failures so a broken wait is diagnosable without rerunning.
	Snippets map[string]string
}

// Calibrate runs the manifest against a live TUI in tmux and measures each
// beat: send exactly what the tape will send, wait exactly for what the
// tape's viewer would see, and record how long that took. Everything after
// this point (tape, sleeps, GIF) is derived from these numbers.
//
// Session, launch and the beat loop are shared with Verify — one driver
// behind both runners, so the waits can't drift between them (ADR-0099).
func Calibrate(ctx context.Context, m *Manifest, opts Options) (Result, error) {
	res := prepare(&opts, m)
	if err := tmux.NewSession(opts.Session, m.Terminal.Cols, m.Terminal.Rows, opts.Workdir); err != nil {
		return res, fmt.Errorf("tmux new-session (is tmux installed?): %w", err)
	}
	if !opts.KeepSession {
		defer tmux.KillSession(opts.Session)
	}
	if err := openLive(ctx, m, &opts, &res); err != nil {
		return res, err
	}
	_, err := driveBeats(ctx, m, &opts, &res)
	return res, err
}

// prepare fills option defaults from the manifest (timeout, poll interval,
// session name) in place and returns a fresh Result — both runners need the
// same resolution rules.
func prepare(opts *Options, m *Manifest) Result {
	if opts.Timeout == 0 {
		opts.Timeout = m.Timeout
	}
	if opts.Interval == 0 {
		opts.Interval = PollInterval
	}
	if opts.Session == "" {
		opts.Session = fmt.Sprintf("milk-showcase-%d", time.Now().UnixNano()%1e9)
	}
	return Result{Vars: map[string]string{}, Snippets: map[string]string{}}
}

// openLive launches the TUI in the session and waits until the header is
// painted and the status line renders — the moment a human would start
// typing. Sets res.LaunchElapsed.
func openLive(ctx context.Context, m *Manifest, opts *Options, res *Result) error {
	launchStart := time.Now()
	if err := tmux.SendKeys(opts.Session, opts.Launch); err != nil {
		return fmt.Errorf("sending launch line: %w", err)
	}
	if err := tmux.SendEnter(opts.Session); err != nil {
		return fmt.Errorf("launching: %w", err)
	}
	launchCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	err := tmux.PollPaneContains(launchCtx, opts.Session, StartupWaitText, opts.Interval)
	if err == nil {
		err = tmux.PollPaneContains(launchCtx, opts.Session, StatusWaitText, opts.Interval)
	}
	cancel()
	if err != nil {
		return fmt.Errorf("TUI did not become ready (header + status) within %s (launch: %s): %w\n%s",
			opts.Timeout, opts.Launch, err, paneExcerpt(opts.Session))
	}
	// The header paints on the first frame, before input handling has
	// necessarily settled (first-boot init runs concurrently); a beat that
	// submits within that window can lose its Enter — observed once in a
	// live record, never after this. Give startup its moment, and count it
	// in the launch sleep so the recording waits like the calibration did.
	time.Sleep(300 * time.Millisecond)
	res.LaunchElapsed = time.Since(launchStart)
	return nil
}

// driveBeats runs every beat of the manifest against the live pane in order
// and reports each beat's outcome through res (timings, snippets) — the
// shared runner loop behind Calibrate and Verify. Returns the id of the beat
// that failed ("" when every beat completed) and its error, annotated with
// beat/step and the last pane capture.
func driveBeats(ctx context.Context, m *Manifest, opts *Options, res *Result) (failed string, err error) {
	for bi := range m.Beats {
		beat := &m.Beats[bi]
		timing := BeatTiming{ID: beat.ID}

		// skip_if: does the pane already show what this beat would prove?
		if beat.SkipIf != "" {
			if pane, cerr := tmux.CapturePane(opts.Session); cerr == nil && strings.Contains(pane, beat.SkipIf) {
				timing.Skipped = true
				res.Timings = append(res.Timings, timing)
				continue
			}
		}

		start := time.Now()
		timing.Steps = make([]time.Duration, len(beat.Steps))
		for si := range beat.Steps {
			stepStart := time.Now()
			if err := runStep(ctx, opts, beat, &beat.Steps[si], res.Vars); err != nil {
				return beat.ID, fmt.Errorf("beat %q step %d: %w\npane:\n%s", beat.ID, si, err, paneExcerpt(opts.Session))
			}
			timing.Steps[si] = time.Since(stepStart)
		}
		// A human never types the next beat on the same frame the previous
		// state landed; the settle keeps the recording from looking like a
		// robot AND keeps the next step's capture race-free.
		time.Sleep(150 * time.Millisecond)
		timing.Elapsed = time.Since(start)
		res.Timings = append(res.Timings, timing)

		if pane, cerr := tmux.CapturePane(opts.Session); cerr == nil {
			res.Snippets[beat.ID] = pane
		}
	}
	return "", nil
}

// runStep executes one manifest step against the live pane.
func runStep(ctx context.Context, opts *Options, beat *Beat, step *Step, vars map[string]string) error {
	if step.Clear {
		// One tmux call, N backspaces — the textarea is empty already
		// (BSpace on an empty input is a no-op), so N is just an upper
		// bound on anything a previous step could have left behind.
		if err := tmux.SendBackspace(opts.Session, 120); err != nil {
			return fmt.Errorf("clear: %w", err)
		}
	}
	if step.Type != "" {
		text := Interpolate(step.Type, vars)
		if strings.Contains(text, "{{") {
			return fmt.Errorf("unresolved placeholder in %q (missing extract?)", text)
		}
		if err := tmux.SendKeys(opts.Session, text); err != nil {
			return fmt.Errorf("typing %q: %w", text, err)
		}
	}
	if step.Key != "" {
		if err := tmux.SendKey(opts.Session, step.Key); err != nil {
			return fmt.Errorf("key %s: %w", step.Key, err)
		}
	}
	wait := opts.Timeout
	if step.Wait != "" {
		wctx, cancel := context.WithTimeout(ctx, wait)
		err := tmux.PollPaneContains(wctx, opts.Session, step.Wait, opts.Interval)
		cancel()
		if err != nil {
			return fmt.Errorf("wait %q never appeared within %s: %w", step.Wait, wait, err)
		}
	}
	if step.WaitAbsent != "" {
		wctx, cancel := context.WithTimeout(ctx, wait)
		err := tmux.PollPaneNotContains(wctx, opts.Session, step.WaitAbsent, opts.Interval)
		cancel()
		if err != nil {
			return fmt.Errorf("%q never left the pane within %s: %w", step.WaitAbsent, wait, err)
		}
	}
	if step.Extract != nil {
		pane, err := tmux.CapturePane(opts.Session)
		if err != nil {
			return fmt.Errorf("extract capture: %w", err)
		}
		re, err := regexp.Compile(step.Extract.Pattern)
		if err != nil {
			return fmt.Errorf("extract pattern %q: %w", step.Extract.Pattern, err)
		}
		groups := re.FindStringSubmatch(pane)
		if len(groups) < 2 {
			return fmt.Errorf("extract %q matched nothing in the pane", step.Extract.Pattern)
		}
		vars[step.Extract.Name] = groups[1]
	}
	return nil
}

// paneExcerpt is the last screenful — enough to see what the pane showed
// when a wait failed, without dumping the whole scrollback.
func paneExcerpt(session string) string {
	pane, err := tmux.CapturePane(session)
	if err != nil {
		return "(pane capture failed: " + err.Error() + ")"
	}
	return pane
}

// Available reports whether the calibration toolchain exists — used by the
// recorder's preflight and by tests to skip cleanly.
func Available() (tmuxOK bool, vhsOK bool) {
	_, err := exec.LookPath("tmux")
	tmuxOK = err == nil
	_, err = exec.LookPath("vhs")
	vhsOK = err == nil
	return tmuxOK, vhsOK
}
