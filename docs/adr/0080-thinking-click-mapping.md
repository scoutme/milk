# 80. Thinking-Block Click Mapping: One Measured Layout, One Deterministic Render

- **Status:** accepted
- **Date:** 2026-10-25

## Context

The report: *the thinking block click mapping deterministically acts on the
wrong block (exactly 2 blocks after, if any)* — clicking a fold header (ADR-0067)
toggled a different block than the one whose header was clicked, consistently
off by a fixed number of blocks, and clicks near the end of the session did
nothing. The click path is a two-step mapping — screen row → transcript row
(`contentLine = vp.YOffset + (ev.Y - vpRowStart)`), then transcript row →
block index (`thinkBlockAtLine`, the ordinal of fold headers before the row) —
and *both* steps were lying, in ways that each shift the target by a
content-determined, therefore "deterministic", amount:

1. **The screen-row step hardcoded the chrome.** `vpRowStart = 2` and
   `vpRowEnd = height - 2` (in `handleMouse`, `handleAttachMouse`,
   `handlePanelMouse`, `dragEdgeDir`) assumed exactly two rows of chrome above
   and below the viewport. The real layout measures them (`viewportHeight`
   already did: header bar rows + hint region + status bar), and the header
   bar *wraps* on narrow terminals or long session labels — every extra
   chrome row mapped every click one transcript row low, i.e. at the *next*
   fold header down. Clicks past the last header mapped to `-1` — the "if
   any" no-op.
2. **The row space itself was not deterministic.** `wrappedTranscript` has a
   fast path: the cached colorized prefix plus a plain-wrapped raw suffix,
   taken while little text has arrived. For reflowing colorization
   (`ColorizeFull`, glamour) that render has *different rows* than a full
   rebuild of the same content — glamour re-indents paragraphs and owns blank
   lines — and the very act of clicking (press → `setViewportContent`, fold
   toggle → `invalidateThinkDisplay`) flips which path runs. The hit-test
   recomputes the render, so it can see rows the screen never showed, and the
   target row lands wherever the row-count delta puts it.
3. **The row → block step was fragile at the edges.** `colorizeGlamour`
   trims trailing newlines from its output, so a fold container glued onto
   the neighbouring line ("intro text▸ thinking · 2 lines") — the glued
   header then failed the line-start prefix match (skewing every ordinal
   after it) and could split across wrapped rows. A transcript line merely
   *quoting* "▸ thinking · " counted as a header and shifted the ordinal the
   other way.

## Decision

- **One measured layout.** `viewportRows()` returns the viewport band (first
  row, first row below) from the same measurement `viewportHeight`/`View`
  build the layout from (header bar rows, hint region rows, status bar rows).
  Every mouse row→content-row mapping — transcript clicks, attach-view
  clicks, panel clicks, tab-strip clicks, drag outside/edge detection —
  derives from it. Nothing in the mouse path may hardcode chrome heights.
- **One deterministic render.** The cached-prefix fast path is taken only for
  row-preserving colorization (`ColorizeMode.reflows()` is false: off,
  fenced, balanced); reflowing modes always rebuild through the per-turn
  cache. `glamourChunk` restores the chunk's trailing line breaks, so a
  preserved styled span (a fold container) can never glue onto the
  neighbouring line. The render a click recomputes is now byte-identical to
  the one on screen.
- **Structural hit-testing.** `thinkHeaderCount` counts *complete* headers
  ("▸/▾ thinking · N lines", streaming variant) anywhere in a row — glued
  headers included — and only on rows carrying **the header's own paint**
  (`foldHeaderStyleRow`: the surface *plus* the signature — dim-classic,
  accent-or-muted italic-modern; bodies share the surface but not the
  signature, so header-shaped text quoted inside an expanded container never
  counts — the tool-side "click block 3 → block 5 toggled" regression;
  pattern-only off-TTY where the paint is a no-op and nothing is clickable).
  `thinkBlockAtLine` is the
  ordinal of those counts. The line-start prefix helper (`isThinkHeaderLine`)
  remains only as a plain-text predicate for tests.

## Consequences

- Clicking a fold header toggles exactly the block under it at any terminal
  width, chrome height, scroll position and colorize mode; clicks past the
  last header are honest no-ops. Selection/copy share the same coordinate
  space and get the same guarantee.
- Full-mode streaming re-colorizes the last turn on each viewport rebuild
  (the per-turn cache bounds it to O(last turn)) instead of appending a raw
  suffix — the price of rows that never disagree. The default (fenced) keeps
  the fast path.
- Test helpers convert content rows through the measured layout
  (`panelRowScreen`, `attachRowScreen`, `attachClick(m, x, contentRow)`)
  instead of baking in "screen row 2 is content row 0".
- Residual, accepted (as before): reasoning text is model output, not
  adversarial input — a quoted *complete* header that also carries the
  header's exact SGR signature (raw escapes in output) could still shift the
  ordinal; the worst case is a
  mis-targeted fold toggle.

## Verification

- `TestThinkHeaderClickTogglesOwnBlock` clicks every rendered fold header and
  asserts the toggled block is the header's own — across themes (off,
  supermilk) × widths (160/100/72/48) × colorize modes (off, fenced,
  balanced, full) × scroll positions. The pre-fix failures at wrapped-chrome
  widths mapped every click low; full-mode cases missed glued/split headers;
  both are pinned gone.
- `TestQuotedHeadersDoNotSkewClickMapping` pins the quoted-header ordinal
  hole.
