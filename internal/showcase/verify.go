package showcase

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/tmux"
)

// The verifier — the second runner of ADR-0099 (issue #387, second half):
// same manifest, same tmux driver, same waits — asserted instead of
// measured. Every wait/wait_absent/extract is a claim about what milk must
// show; the run passes only if every claim holds within the timeout, and the
// outcome trail is the report a CI job (or a human) reads.
//
// Deliberately NOT a batch run, correcting ADR-0099's first sketch: the
// beats' actions are TUI slash commands (/learn, /task done, /escalate) that
// single-prompt milk never executes, and some waits are inherently UI claims
// (the picker's "↑↓ choose", a panel's rendered header) with no state-file
// twin to assert against. Pane state is the state both runners share — which
// is exactly what makes the waits translate 1:1 *between the runners*, the
// convergence the ADR chose.

// BeatOutcome is one beat's verdict: nil Err means every assertion in the
// beat held (Skipped means skip_if matched before any step ran — nothing was
// proven, nothing failed).
type BeatOutcome struct {
	ID      string
	Skipped bool
	Elapsed time.Duration
	Err     error
}

// Report is a full verification pass — readable via Summary, pass/fail via
// OK. Snippets carries the final pane per completed beat (a CI artifact for
// diagnosing a failure without rerunning), Vars the extracted values.
type Report struct {
	Name     string
	Outcomes []BeatOutcome
	Vars     map[string]string
	Snippets map[string]string
	// Err is the failure that ended the run (fail-fast: the first broken
	// beat), nil when everything held. Also set on that beat's outcome.
	Err error
}

// OK reports that every asserted beat held.
func (r *Report) OK() bool { return r.Err == nil }

// Summary is the human/CI-readable report: one line per beat, and a footer
// naming the first failure. The failure line is one line by design — the
// full error (with the pane dump) stays in Err for the caller to print.
func (r *Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "showcase: %s — %d beat(s) asserted\n", r.Name, len(r.Outcomes))
	for _, o := range r.Outcomes {
		switch {
		case o.Err != nil:
			first, _, _ := strings.Cut(o.Err.Error(), "\n")
			fmt.Fprintf(&b, "  %-16s FAIL  %s\n", o.ID, first)
		case o.Skipped:
			fmt.Fprintf(&b, "  %-16s skip  (skip_if matched — nothing proven)\n", o.ID)
		default:
			fmt.Fprintf(&b, "  %-16s ok    %.1fs\n", o.ID, o.Elapsed.Seconds())
		}
	}
	if r.Err != nil {
		fmt.Fprintf(&b, "showcase: FAILED at %s — %d of %d asserted beats passed before it\n",
			r.failedID(), r.passedBeforeFailure(), len(r.Outcomes))
	}
	return b.String()
}

// failedID is the outcome whose Err is the run-ending failure.
func (r *Report) failedID() string {
	for _, o := range r.Outcomes {
		if o.Err != nil {
			return o.ID
		}
	}
	return "?"
}

// passedBeforeFailure counts clean beats ahead of the failure.
func (r *Report) passedBeforeFailure() int {
	n := 0
	for _, o := range r.Outcomes {
		if o.Err != nil {
			break
		}
		if !o.Skipped {
			n++
		}
	}
	return n
}

// Verify runs the manifest's beats against a live TUI as assertions and
// returns the report of what held — the run stops at the first broken beat
// (its Err set, its outcome recorded); the report is meaningful either way,
// so callers print Summary first and then act on the error (exit non-zero).
func Verify(ctx context.Context, m *Manifest, opts Options) (*Report, error) {
	res := prepare(&opts, m)
	if err := tmux.NewSession(opts.Session, m.Terminal.Cols, m.Terminal.Rows, opts.Workdir); err != nil {
		return nil, fmt.Errorf("tmux new-session (is tmux installed?): %w", err)
	}
	if !opts.KeepSession {
		defer tmux.KillSession(opts.Session)
	}
	if err := openLive(ctx, m, &opts, &res); err != nil {
		return nil, err
	}
	failedID, err := driveBeats(ctx, m, &opts, &res)

	rep := &Report{Name: m.Name, Vars: res.Vars, Snippets: res.Snippets, Err: err}
	for _, t := range res.Timings {
		rep.Outcomes = append(rep.Outcomes, BeatOutcome{ID: t.ID, Skipped: t.Skipped, Elapsed: t.Elapsed})
	}
	if err != nil {
		// The failing beat never got a timing — record it as the failure
		// it is, so the trail shows where the run stopped.
		rep.Outcomes = append(rep.Outcomes, BeatOutcome{ID: failedID, Err: err})
	}
	return rep, err
}
