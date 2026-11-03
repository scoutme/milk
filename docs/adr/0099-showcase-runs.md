# 99. Showcase Runs: one manifest, two runners — calibrated recording first

- **Status:** accepted
- **Date:** 2026-11-03
- **Closes:** #387 (analysis half; the headless verifier remains the planned second runner)

## Context

milk had no first-class way to produce a showcase of its feature surface
(tasks, jobs, memory, panels/events, routing). What existed rotted in place:
`task demo` ran a hand-written `demo/demo.tape` whose fixed `Sleep 8s` /
`Sleep 24s` waits were guesses against live model latency, hardcoded a home
path, and narrated agents (`copilot-ghe`, claude-cli) the config no longer
has. The eval harness *drives* agents but measures rather than demonstrates,
and never renders the TUI.

Two asks were filed together (#387) with an explicit open question: does a
**recorded-terminal session task** and an **agent-driven feature showcase**
converge, or are they separate paths?

## Decision

**Converge at the definition, separate at the runners — and ship the
visual artifact first** (recording runner now, headless verifier next).

1. **One manifest** — `demo/showcase.yaml`: an ordered beat list where each
   beat is *keys to send* plus *the pane state that must appear* (`wait`,
   `wait_absent`, `extract` for values like a generated task id). The waits
   are the contract; the manifest is the single source both runners consume,
   which is what kills the rot — the old tape had nothing tying it to the
   features milk claims.
2. **The recorder calibrates, it does not guess** — `task showcase:record`
   drives a live TUI in tmux (`internal/tmux`, the machinery the eval
   claude-code adapter already proved), measures every beat against real
   state (`role:PRIMARY` busy → gone, `[milk] learned:`, the picker's
   `↑↓ choose`, `completed status=ok`, …), emits a vhs tape whose sleeps are
   `measured × margin + base`, then lets vhs render the GIF. A beat that
   never satisfies its wait **fails the calibration loudly with the pane
   excerpt** — the failure mode of guessing is a bad video; the failure mode
   of state-sync is an error.
3. **A clean config root per pass** — the run gets a copy of the user's
   config minus `strip_config_keys` (`remote_oversight`: a recording must
   not depend on or announce live infrastructure — the telegram ping warning
   that would open every transcript is stripped, not special-cased), and
   empty sessions/memory/panel state, so every recording opens on the same
   defaults and the panels *appear* on cue. Agents and settings are the
   user's own: the showcase runs their configured agents.
4. **Not eval** — eval's schema is prompt+rubric+judge for model quality;
   showcase beats are UI/state assertions, and the visual runner cannot be
   headless while the verifier must be. Forcing one schema onto both
   distorts both; the definition is what they share.

## Consequences

- `task showcase:record` needs tmux + vhs + a configured milk + working
  agents (one calibration pass + one recording pass, both live);
  `task showcase:tape` stops after the tape; `task demo` still re-renders
  the committed tape with no agents at all. The generated `demo/demo.tape`
  is committed (docs-worthy and replayable as-is), header-marked as
  generated — edit the manifest, not the tape.
- Timing headroom is explicit: `margin: 1.5` + `base: 0.4` on measured
  durations covers typing speed and second-run latency variance; the next
  backend hiccup eats into headroom instead of desyncing the narration.
- The planned **verifier runner** (same manifest, asserts beats against a
  batch run, CI-runnable) is the remaining half of #387 — waits translate
  1:1 from pane substrings to state assertions, which is the whole point of
  the convergence choice.
- `internal/tmux` moved out of `eval/` (was unexported helpers): one
  state-synchronized terminal-driver, two consumers.
