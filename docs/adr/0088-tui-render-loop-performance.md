# 88. TUI render-loop performance: viewport fork + render caches

- **Status:** accepted
- **Date:** 2026-10-27

## Context

Input lag reported from real use: holding arrow keys in the input area made
the cursor stutter and keep moving after release — the classic Update-loop
backlog. A capable workstation idle-still felt heavy, which pointed at the
render loop rather than the machine.

Benchmarks (cmd/milk/perf_bench_test.go) pinned the per-keystroke cost of
the Update path (`updateTA` + `syncLayout` + full viewport rebuild) against
transcript size:

| transcript rows | per keystroke |
|---|---|
| 200 | 0.37 ms |
| 2,000 | 2.50 ms |
| 10,000 | **12.4 ms** |

O(transcript bytes) per key event. Terminal auto-repeat fires 30–50 keys/s,
so a long session spent more time rebuilding the screen than draining its
input queue — the backlog the user felt. Profiling attributed 84% to
`viewport.SetContentLines`, which (upstream bubbles v2.2.1, unchanged on
master) runs `maxLineWidth` — grapheme-level `ansi.StringWidth` over **every
content line** — on every content set, purely for horizontal scrolling milk
never uses. The rest was milk's own rebuild: concatenating the whole
transcript into one content string, `strings.Split`-ing it back, and
re-tinting every row (`tintFullRows`'s per-row checks) on each keystroke.

Per-frame `View()` cost (~0.8 ms, panel rendering + lipgloss width passes)
is constant in transcript size and was left alone: it only runs on repaints,
and the spinner/poll ticks already stop when idle.

## Decision

Three complementary changes, all semantics-preserving:

1. **Fork the viewport into `internal/vp`** (bubbles v2.2.1, MIT,
   attribution in the file headers). Changes: `maxLineWidth` is deferred
   (`widthsDirty`, computed on demand via `longestLine()` for the
   horizontal-scroll accessors milk never calls); `View`'s "does anything
   need cutting" check becomes per-visible-row (≤ height width scans per
   frame instead of one full-content scan per content set); new
   `SetLines([]string)` installs pre-split lines without the split or the
   line-ending scan, and `NormalizeLines` exposes that scan for callers to
   run once per cache rebuild. Upstream is unchanged and may catch up
   eventually; the fork is small and its API surface is what milk already
   used.

2. **Content-addressed memo around `wrappedTranscript`** (the expensive
   half of every rebuild: wrap + per-row tint + selection highlight over
   the whole transcript). Keyed on everything that can change the output —
   transcript length (append-only), width, colorize mode, `thinkGen` (a new
   generation counter `invalidateThinkDisplay` bumps), `colorizeForce`
   (plain re-colorize requests like agentDoneMsg), and the selection range.
   Cursor movement and typing change none of them, so they serve the memo.
   `appendThinkingStreamed`'s open-block branch bumps `thinkGen` too: it
   mutates the display without changing the transcript's length (the inner
   cache's prefix+fresh-tail fast path for streaming is untouched).

3. **Cached transcript rows with tail splice** in `setViewportContent`:
   the wrapped transcript + separator are kept as a normalized `[]string`
   (content-addressed, rebuilt only when the wrapped output changes), and
   each rebuild assembles cached rows + the small input-area tail, handed
   to `SetLines`. Typing and cursor movement no longer concatenate, split,
   or re-normalize the transcript.

`cmd/milk/perf_bench_test.go` pins all of it (keystroke Update, frame View,
keystroke+frame) across 200/2,000/10,000-row transcripts.

## Consequences

- Per-keystroke cost is flat in transcript size: 0.37 ms → 0.15 ms at 200
  rows, 2.5 ms → 0.16 ms at 2,000, **12.4 ms → 0.20 ms at 10,000 (62×)**.
  Held-arrow input no longer builds a backlog regardless of session length.
- `internal/vp` is a maintained fork (~960 lines). Its drift risk is low
  (viewport v2's API is small and milk used only 18 methods), but a future
  bubbles upgrade must be diffed against it — the fork header and this ADR
  say so explicitly.
- Render output is byte-identical by construction (the memo is
  content-addressed over the existing render chain; `SetLines` is the same
  normalization the old `SetContent` path ran, just moved to cache
  rebuilds). The full test suite — layout, panels, selection, think blocks,
  scroll behavior — passes unchanged.
- Deferred longest-line measurement means `HorizontalScrollPercent`/
  `maxXOffset` compute on demand; milk never calls them (content is
  pre-wrapped to the viewport width), so nothing pays for the deferral.

## Alternatives considered

- **Debounce/coalesce keystroke rebuilds**: masks the symptom, adds visible
  cursor lag of its own; the cost itself was the bug.
- **Move the input area out of the viewport content (render-time overlay)**:
  would make keystrokes O(input rows) even on rebuilds, but changes the
  "input scrolls with the transcript" semantics — too risky for a perf fix;
  revisit if the remaining ~0.2 ms ever matters.
- **Wait for upstream**: bubbles master still measures every line on every
  `SetContent`; no release path in sight.
