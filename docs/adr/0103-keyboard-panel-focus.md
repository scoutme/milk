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
   only what is actually rendered may be focused (ADR-0089).
2. **Paging follows focus** — `pgup`/`pgdown` (and `ctrl+u`/`ctrl+f`) page the
   focused panel's own offset (half the panel height, clamped exactly like the
   wheel: `[0, panelMaxOffset]`) or the transcript when main is focused — the
   original behavior, byte-for-byte, in the default state.
3. **Click sets focus** — a left press inside a visible panel focuses it; a
   press in the main area (transcript or input) returns focus there. **Wheel
   scrolling never moves focus** — scrolling is not a change of intent.
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

## Consequences

- Keyboard parity is complete without any new mode: `shift+` rotation,
  focused paging, click-to-focus, F1–F6 still toggle (bring-to-front for
  grouped panels per issue #373 — toggling a *visible* panel closes it, which
  also drops a focused panel and heals via rule 4).
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
