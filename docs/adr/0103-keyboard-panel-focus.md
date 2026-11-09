# 0103. Keyboard focus for side panels

Date: 2026-11-08
Status: accepted
Implemented: cmd/milk/panel_focus.go (+ repl.go key/mouse routing, layout.go input gate, panel_common.go row background, theme.go focus token), issue #412

## Context

The six side panels were mouse-only creatures: the wheel and drag scrolled them,
clicks operated them, but every keyboard scroll key (`pgup`/`pgdown` and the
`ctrl+u`/`ctrl+f` aliases) belonged to the transcript viewport no matter where
the user was looking. On a multi-display setup — or anywhere the user drives
milk from the keyboard — the panels could be on screen and unreadable without
letting go of the home row. There was also no answer to "which pane owns my
keys": the only focus-like signal was the window-level blur (#367), which is
about the terminal window, not panes inside it.

The raw material was already there. `session/persisted panel state` tracks
which panels are open; ADR-0089's width budget decides which are *visible*;
`panelOffsetPtr` gives every panel its own scroll offset (the wheel and drag
already share those offsets); #367 built an input-area "inactive" render;
ADR-0098 made the layout theme-independent. What was missing was one focus
state and the rules around it.

## Decision

**Focus is one model field — `panelFocus panelRegion` — where `regionNone`
(zero) means the main area (transcript + input).** Everything else follows
from rules on that field:

1. **Rotation** — `shift+pgup` / `shift+pgdown` walk `[main, visible panels…]`
   in `panelRenderOrder`, wrapping. Main is deliberately inside the cycle:
   rotation is the only keyboard path *back* to the input area (and shift+pgup
   the way back out). Panels the width budget hides are not in the sequence —
   only what is actually rendered may be focused (ADR-0089). **`tab`/`shift+tab`
   are aliases whenever the main area holds focus and the token under the cursor has nothing to complete** — completion always wins on the input (the cycling key doubles as rotation only when cycling has no answer, `handleTabKey`), and while inline hints or an `@`-token sit under the cursor a Tab with no buildable candidates is a **no-op**, never a focus steal (user report, 2026-11-08: parameter-value and @-index-cold positions were rotating focus mid-completion). **While a panel holds focus, `tab`/`shift+tab` rotate outright** — completion context belongs to the input, and typing would have handed focus back anyway (user report, second round: "if focus is on panels, tab must be read for panels navigation"). Bare Tab is free at
  the terminal level (terminal tab-switching lives on Ctrl+Tab and friends),
  so the alias costs no chord.
2. **Paging follows focus** — `pgup`/`pgdown` (and `ctrl+u`/`ctrl+f`) page the
   focused panel's own offset (half the panel height, clamped exactly like the
   wheel: `[0, panelMaxOffset]`) or the transcript when main is focused — the
   original behavior, byte-for-byte, in the default state.
3. **Click sets focus** — a left *click* inside a visible panel focuses it,
   deferred behind the same 400 ms window the cadence uses: press and release
   must be close together for it to count as a click at all (holding the button
   down is not one), a double click runs the action instead, and a drag
   selection never moves focus (movement is not a click) — see Consequences.
   A press in the main area (transcript or input) returns focus there
   immediately. **Wheel scrolling never moves focus** — scrolling is not a
   change of intent.
4. **One accessor, self-healing** — `focusedPanel()` returns the focused panel
   *only while it is visible* (`panelVisible`), else `regionNone`. Closing the
   focused panel, a width-budget re-fit, `/new`, a theme switch — every
   consumer (scroll routing, tint, input dim) falls back to main with no
   per-site reset logic. Focus is transient view state: never persisted, never
   toasts, never moves layout.
5. **Two render consequences**:
   - The focused panel's body and scrollbar rows carry the **focus tint** via
     `panelRowBg` (which supersedes the alternating parity while focused — a
     focused panel must read as focused wherever it landed). The tint is a
     **theme token** (`themeDef.focus`, mirrored as `tkFocus`): supermilk gets
     a pale cool tint on its warm ground; theme "off" — which otherwise keeps
     its colours in code by design — declares focus as its one token so the
     render path carries no escapes for either theme (ADR-0098's "the theme
     picks colours" extended to focus). Panels in this layout have no border
     glyphs; the tinted column (body + scrollbar) *is* the indicator.
   - The **input area renders inactive while focus is elsewhere** — the same
     `dimBlurredInput` path as an unfocused window (#367), byte-identical
     output (pinned by test). Typing still lands in the input; the dim says
     where paging will go.

6. **Esc returns focus to main, one layer per press** — after any active
   selection clears (textarea, transcript, panel — in routing order), a
   focused panel's Esc sets focus back to `regionNone`. The attach view
   peels identically: attach selection → focused panel's focus → detach, so
   Esc gets the user out of the *surface* they are in before it leaves it.
   Busy and idle share the branch (`handleBusyKey` tails into `handleKey`),
   and every real modal (prompts, wizards, palette) still routes ahead with
   its own Esc contract.

## Consequences

- Keyboard parity is complete without any new mode: `shift+` rotation,
  focused paging, click-to-focus — uniformly deferred behind the 400 ms
  double-click window on *every* side panel (single = focus, double = the
  panel's action, which must never move focus — ADR-0066's cadence, one
  machine in `panelClickAction`; only a press+release close together counts
  as a click — holding the button down is not one — and a drag selection
  never moves focus; the main area stays immediate at press), plus the row
  cursor: `↑`/`↓` walk the focused panel's rows (the cursor row wears the
  selection highlight, scrolling follows), `Enter` runs the cursor row's
  action — the exact double-click action, rows with items only, never
  submitting input (`panelActivateRow`), **in both states**: while a turn
  runs the busy queue disposition (ADR-0083) yields Enter to the focused
  panel exactly as it yields completion-accept, consuming the gesture
  (the armed queue confirm clears with it); the queue keeps Enter whenever
  focus is on the input (every pending prompt/wizard is still routed
  ahead of the busy handler, so a real modal always keeps Enter) — and
  typing any text, or pressing a text-editing key (backspace, delete,
  ctrl+h), hands focus
  straight back to the input, keystroke included (type-to-focus — editing
  keys mean editing intent everywhere, attach view included),
  F1–F6 still toggle (bring-to-front for
  grouped panels per issue #373 — toggling a *visible* panel closes it, which
  also drops a focused panel and heals via rule 4). The same navigation
  stays live while the attach view (ADR-0047) owns the main area — a
  preview must not freeze the panels the mouse already reaches:
  `handleAttachKey` mirrors `handleKey` for rotation, the row cursor, the
  row's action and paging (with main focus, pgup/pgdn page the attached
  content itself), and Esc peels one layer per press — the attach selection,
  then a focused panel's focus, and only then the detach (same order as the
  main path).
- The `/keys` help gains a column for the three bindings; the raw switch cases
  in `handleKey` stay the implementation (registry entries are display-only,
  per the keymap's own convention).
- Mouse users see one new thing (the tint under their cursor when they click a
  panel) and lose nothing: wheel, drag, selection and the strip all behave as
  before.
- Deliberately *not* done: focus does not suppress keystrokes (the input keeps
  receiving text — this is navigation focus, not input focus), arrows do not
  scroll panels (pgup/pgdn only, as specified), and the attach/PTY views keep
  their own key routing untouched.

## Alternatives considered

- **Reusing the window blur machinery for pane focus** — conflates two
  distinct signals (window vs pane); #367's dim would then double as focus
  indicator with no way to express "window focused, panel focused".
- **A focus ring in the status bar only** — visible, but adds a persistent
  chrome line for a transient state; the tint + input dim already answer
  "where are my keys" at the point of use.
- **Rotating through hidden panels** (open but budget-cycled out) — focus
  would point at pixels that do not exist; bringing it visible on focus would
  move layout under the user (violates ADR-0089's "content never moves the
  user").
