# ADR-0076: Bubble Tea v2 UI modernization

Date: 2026-10-23

Status: Accepted

## Context

The TUI ran on Bubble Tea v1.3, Bubbles v1.0 and Lip Gloss v1.1. Three recurring problems traced back to that stack: Ctrl+Enter needed raw-CSI sniffing (`isCtrlEnterCSI`), the terminal background was a blocking query that ran at package init, and every overlay (toasts) was hand-written escape surgery on rendered rows. The v2 line fixes these at the source. Plan and per-phase notes: `docs/internal/plans/bubbletea-v2-ui-modernization-plan.md`.

## Decision

Move to `charm.land/bubbletea/v2`, `charm.land/bubbles/v2` and `charm.land/lipgloss/v2`, in seven phases, each shippable on its own:

1. **Core.** `View()` returns `tea.View` and declares the alt screen. Keys are `tea.KeyPressMsg`, pastes are `tea.PasteMsg` (routed by `routePaste`), mouse messages are flattened once into `mouseEvent` (`mouse.go`) so the selection and panel handlers stay unchanged.
2. **Theme.** `Init` sends `tea.RequestBackgroundColor`; `BackgroundColorMsg` sets `darkBackground` and `refreshThemeColors()` rebuilds the chrome styles, which are now built only by `rebuildStyles()`.
3. **Terminal integration.** The view declares focus reporting and a window title (`milk <glyph> <agent> · idle|working|needs input` — the state is always present; the glyph *replaces the separator after the app name*, sitting right before the agent like the status bar's spinner so a long agent name can hide the state word but never the glyph, and the working glyph is a braille spinner frame rotating on the existing 80ms busy tick; issue #367). While blurred, the input area rebuilds dimmed without its reverse-video cursor (nothing blinks in a backgrounded tab; restored instantly on focus) — focus affects presentation only: tips, polling and panel events keep running so an unfocused window in a multi-display setup stays a live view (the pause carried from the v2 cut is superseded, issue #389). Copy sets the terminal clipboard through `tea.SetClipboard`.
4. **Overlays.** `overlay.go` composes layers with the Lip Gloss compositor; toasts use it and the command palette (`palette.go`, Ctrl+O) floats over the main area.
5. **Key registry.** `keymap.go` holds the global shortcuts as `key.Binding`; Update matches through it and `/keys` renders it with `bubbles/help`.
6. **Widgets.** The `ctx:` status fragment gets a `bubbles/progress` meter on wide terminals.
7. **Selection spike.** Closed without adoption, see below.

### Deliberately not done

- **Permission ask as a modal layer.** It is shared with ACP and Telegram (ADR-0052) and its y/n/a/d flow, queue and timers are covered by many tests. A modal copy would duplicate the prompt for no gain.
- **`bubbles/table` for `/usage`.** `obs.FormatTokenUsage` already prints an aligned table and is shared with plain-text hosts.
- **`huh`, `bubbles/list`, `filepicker`.** Their jobs have host-neutral cores shared with ACP (`initwizard_core.go`, completion, `fileindex.go`).
- **`ultraviolet` for selection.** It has no selection primitive. The backlog item was stale: selection is already app-managed and anchored to content lines, not screen rows (ADR-0024), with drag autoscroll.
- **Declarative mouse mode.** Mouse tracking is still written as raw escapes because v2's declarative modes cannot express "1000 at rest, 1002 only while dragging" (see `enableMouseTracking`).

## Consequences

- Lip Gloss v2 always emits full color; downsampling moved to the output writer. Tests that forced a color profile no longer need to.
- `glamour` still depends on Lip Gloss v1 transitively; the two coexist.
- Ctrl+Enter arrives as `ctrl+enter` on terminals with extended key protocols; the CSI sniffing stays as a safety net.

## Follow-up: theme "off" header and tables

- **Header and status bar kept their colors only at the ends.** Spans inside a bar close with a full reset, which dropped the bar to the terminal colors midway, so a navy bar showed navy only around the logo and in the trailing pad. `tintChrome` (`theme.go`) re-applies the bar's own foreground and background after every reset through `sgr.WithBackground`, as the panels already do. The skin ("modern") paints its own ground and is left alone.
- **A skin switch left a wrong "light" verdict.** The detected background reflects what is painted, and the cream skin paints a light one. `/theme` now asks the terminal again (`tea.RequestBackgroundColor`) after changing the colors, so `/theme off` on a dark terminal goes back to the dark variants.
- **Tables wider than the viewport broke through their pipes.** `renderTableBlock` takes the viewport width (threaded from `wrappedTranscript` through `colorizeSingleWidth`): it narrows the widest columns (minimum 6 cells) and wraps the cell text inside them, so each row stays aligned. Below that minimum the table keeps its natural width and the plain wrap applies. The per-turn colorize cache is dropped when the width changes.
- **Models writing bad tables.** `escalation.TableGuidance` is injected in the local system prompt (standard and full tiers) and in the cached Claude-CLI prefix: at most 4 columns, short cells, one value per cell, header and separator rows, equal cell counts, escaped pipes, and a bullet list when it does not fit.

## Follow-up: harmonica and Charm log

- **Spring animations** (`animations.go`, config `animations`, default on). Animating the panel width was rejected because every frame would re-wrap the transcript. Instead the layout changes at once and only the panel's content moves: `Update` compares open state and columns before and after every message (`trackAnimations`), so F-keys, `/panel` and auto-open all animate without touching their call sites. An opening panel slides in inside its own slot; a closing panel leaves a ghost of its last frame that shrinks out over the freed columns wherever it sits — not only the right-most one — and because the layout is right-anchored, a toggle shifts the panels *left* of it one column (right on close, left on open), which they ride as full-width ghosts while their static slot renders blank — no direction teleports a neighbour. Leftward glides measure their offset from the destination with the same rounding shape the opening slot's slide-in uses, so the seam against the opening panel always lands on one column. The mouse wheel moves a scroll target and a critically damped spring glides the viewport to it, standing down if anything else scrolls meanwhile. One 60 fps tick runs only while something moves, and everything is instant without a TTY or with `animations: false`. Springs retuned after live feedback: frequency 24 with settle 0.04 / snap 0.12 — an exit lands in ~300 ms (pinned by `TestPanelSlide_ExitSettlesQuickly`); the original 9 / 0.01 / 0.01 combination crawled through its tail and read as sluggish.
- **Charm log** (`internal/obs/prettylog.go`). `log_format: "pretty"` writes colored structured lines through `charm.land/log/v2` as a `slog` handler, for `tail -f` / `less -R`. `milk otel debug enable` switches to it and `disable` restores the previous format. Each finished agent turn logs one `agent response` line (`obs.LogResponse`).

## Follow-up: the v2 virtual cursor (the missing blink)

The virtual cursor is a styled cell inside `ta.View()`, and its blink loop was healthy from day one (`Init` issues `textarea.Blink`, the chain re-arms through the default Update path — pinned by `TestTextareaCursor_BlinkChainReArms`). What broke was downstream: **v1 rendered the cell as `\x1b[7m<char>\x1b[m`, v2 as an SGR run with extra attributes (`\x1b[7;37m<char>\x1b[m`)**, and `colorizeInput` extracted the cursor by exact-v1 match before rewriting input line 0 from stripped text. With no match the cell was flattened by the rewrite, so the cursor never reached the screen at all — reported as "no blinking cursor in the input area".

- `findReverseCell` now accepts any SGR sequence whose attribute list contains the standalone reverse attribute `7`, followed by one character and a closing SGR — the shape both versions use. The re-injection was factored into `injectReverseCell` and is also applied on the promptless early return (`promptEnd < 0`), which previously dropped an extracted cursor.
- Tests: `TestColorizeInput_*` (v2 form, v1 form, reverse-after-fg, cursor-on-space, promptless, blink-hidden) and the chain test; verified live in a tmux pane (the reverse cell toggles on the 530 ms cadence).
