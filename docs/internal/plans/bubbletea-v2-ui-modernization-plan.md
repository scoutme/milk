# Bubble Tea v2 UI modernization plan

Branch: `claude/bubbletea-ui-modernization-kyrnje`

## Starting point

- `bubbletea v1.3.10`, `lipgloss v1.1.1`, `bubbles v1.0.0`.
- Used today: `textarea`, `viewport`, `tea.Tick`, `tea.Batch`, `tea.ExecProcess`, mouse, bracketed paste, hand-written OSC 52 (`transcript.go`), `lipgloss.HasDarkBackground()` (`theme.go`).
- Migration surface: `repl.go` (about 5000 lines), 96 non-test files in `cmd/milk`, `tea.KeyMsg` and `tea.MouseMsg` in about 20 files.

## Decision: what we use and what we skip

### Use

| Feature | Why | Phase |
|---|---|---|
| Bubble Tea v2, declarative `View` | Base for everything else. Replaces `WithAltScreen`, `EnterAltScreen`, `EnableBracketedPaste`. | 1 |
| New cell renderer + synchronized output (mode 2026) | Less flicker while streaming with side panels open. | 1 |
| `BackgroundColorMsg` + `lipgloss.LightDark` | Replaces the blocking `HasDarkBackground()`. Skins pick light or dark correctly. | 2 |
| `KeyPressMsg` + keyboard enhancements | Real `Ctrl+Enter` / `Shift+Enter`. Retires `zprobe_ctrlenter_test.go` workarounds. | 1 |
| `PasteMsg` | Native paste, simpler paste-taint handling. | 1 |
| `FocusMsg` / `BlurMsg` | Pause tips rotation and 3s git/project ticks when the window is in the background. | 3 |
| `SetClipboard` | Replaces the manual OSC 52 code. | 3 |
| `SetWindowTitle` | Tab title with agent and state. | 3 |
| Lip Gloss layers (`Layer`, `Canvas`) | Toasts, permission asks and a command palette as real overlays. | 4 |
| `key.Binding` + `bubbles/help` | One key registry. Help bar adapts to context. | 5 |
| `bubbles/progress` | Context usage bar, background jobs, workflows. | 6 |
| `bubbles/table` | `/usage` with aligned columns (tok/s, TTFT). | 6 |
| `harmonica` | Spring animation for panel open/close only. Small and subtle. | 6 |
| `teatest` | Golden-file TUI tests, replacing fragile probe tests over time. | all |

### Skip (for now)

| Feature | Reason |
|---|---|
| `huh` | The `/config init` and `/setup` wizards share a host-neutral core with ACP (`initwizard_core.go`). A TUI-only form library would fork that logic. |
| `bubbles/list` | Slash completion and session lists are shared with ACP and the CLI. Our own renderer stays. |
| `bubbles/filepicker` | The project panel already runs on `fileindex.go`. |
| Gradients | Cosmetic. Revisit after the skin settles. |
| `charmbracelet/log` | `slog` + OTel already cover logging. |
| Terminal progress bar (OSC 9;4) | Nice to have. Only if Windows Terminal testing shows value. |

### Spike, decide later

- `ultraviolet` for app-managed text selection (open backlog item: selection highlight sticks to screen coordinates while scrolling). Spike after phase 4. Adopt only if it removes the current workaround.

## Phases

Each phase builds, passes `go vet`, `go test ./cmd/milk/...` and ships on its own.

1. **v2 core migration (done).** Switch imports to `charm.land/*/v2`. `View()` returns `tea.View`. `KeyMsg` to `KeyPressMsg`, `MouseMsg` split by action, `PasteMsg`. Move alt screen and mouse mode into the view. Update `go.mod`, keep tests green. Files: `repl.go`, `layout.go`, `attach.go`, `commands.go`, `workflow_cmd.go`, `bg_agent_wizard.go`, `direct_bash.go`, panels.
2. **Theme (done).** `BackgroundColorMsg` plus `LightDark` tokens in `theme.go`. Remove the `HasDarkBackground()` calls.
3. **Terminal integration (done).** Focus/blur gating of ticks, `SetClipboard`, `SetWindowTitle`.
4. **Overlays (done).** Toasts and permission prompt on layers. Optional command palette behind a shortcut.
5. **Key registry (done).** Single `KeyMap`, `help` bar, derive `tips.go` shortcut tips from it.
6. **Widgets (done, trimmed).** `progress` for ctx and jobs, `table` for `/usage`, `harmonica` for panel transitions.
7. **Selection spike (closed, not adopted)** (`ultraviolet`).

## Compatibility rules

- No change to slash command names, config keys or session files.
- ACP and CLI hosts are untouched: only `cmd/milk` TUI files change.
- Update `CLAUDE.md` tech stack and ADRs where behavior changes (renderer, theme detection, toasts).
- Manual check per phase on Windows Terminal (PowerShell), plus one Linux terminal.

## Status

- Phase 1 done: imports moved to `charm.land/*/v2`, `View()` returns `tea.View` with the alt screen declared, `KeyPressMsg` and `PasteMsg` handled, `mouse.go` adapts the v2 mouse messages, `AdaptiveColor` replaced by `adaptiveColor()`, textarea and viewport on the v2 API. `go vet` and `go test ./...` pass, and a pty smoke run starts, renders and answers keys.
- Phase 2 done: no blocking terminal query at startup any more. `Init` sends `tea.RequestBackgroundColor`, `BackgroundColorMsg` flips `darkBackground` and calls `refreshThemeColors()` (styles, think surface, pulse) and invalidates the colorize cache. Chrome styles are now filled only by `rebuildStyles()`.
- Phase 3 done: the view declares `ReportFocus` and `WindowTitle` (`milk · <agent> · working|needs input`, `window_title.go`). `FocusMsg`/`BlurMsg` set `model.blurred`; while blurred the tips rotation and the project/git refresh pause (timers stay armed), and focus triggers one immediate panel refresh. `copyToClipboard` keeps the OS clipboard and now sets the terminal clipboard through `tea.SetClipboard` instead of writing OSC 52 to stderr.
- Phase 4 done: `overlay.go` composes layers with the Lip Gloss compositor (`composeOverlays`, row widths restored because the canvas trims trailing blanks). Toasts paint through it, and the new command palette (`palette.go`, Ctrl+O) uses it for a floating box. The permission ask deliberately stays a transcript prompt: it is shared with ACP and Telegram and its y/n/a/d flow and queue are covered by many tests, so a modal copy would only duplicate it.
- Phase 5 done: `keymap.go` is the one registry of global shortcuts (`key.Binding`). Update matches F1-F6, Alt+Up/Down, Ctrl+G and Ctrl+O through it, and the new TUI-only `/keys` renders it with `bubbles/help`. Input-editing keys inside `handleKey` (Ctrl+R/S/Z/Y/T, newline) stay string cases and are listed in the registry for documentation only.
- Phase 6 done: the `ctx:` fragment gets a `bubbles/progress` meter from 130 columns (`contextMeter`, same 75/90 thresholds). `table` and a `harmonica` panel animation were dropped, see ADR-0076.
- Phase 7 closed: `ultraviolet` has no selection primitive and selection is already content-anchored (ADR-0024), so the backlog item was stale and is removed from CLAUDE.md.
- Left on purpose: mouse modes are still written as raw escapes (`enableMouseTracking`), `glamour` still pulls Lip Gloss v1 transitively.

- Follow-up: `harmonica` (panel slide, wheel inertia) and `charm.land/log/v2` (`log_format: "pretty"`, `agent response` log line) were added after all, see ADR-0076.

## Skins and visual redesign

The v2 stack made a live theme switch possible (`BackgroundColorMsg`), so the
TUI now ships two named skins, selected with `/theme` and persisted as `theme`
in `config.json`. Full rationale: ADR-0077.

- **`off` (default).** The original look, redesigned to a neutral palette:
  graphite chrome on dark terminals, warm cream chrome on light ones, no
  diamond or pulse in the wordmark, and muted sage (primary) / steel
  (escalation) agent tags. `agentTag` and `userEcho` render both the live
  transcript and seeded session turns, so a resumed session looks like the one
  that produced it.
- **`supermilk` (opt-in).** Milk-white ground (`#f8f5ee`), warm ink, hairline
  separators, one muted olive accent, slim scrollbars, quiet tool rows and
  italic compact reasoning on a light surface. It carries its own tagline
  (`Got models? Got milk.`) and, while active, sets the terminal's default
  colours and 16-colour ANSI ramp via OSC; exit and `/theme off` restore them.

Rules: a skin restyles chrome only (header, status bar, turn labels, panel
tabs, scrollbars, staleness ramp, tool rows, reasoning surface) and never
changes slash commands, config keys or session files. `rebuildStyles()` is the
only builder of chrome styles, so `refreshThemeColors()` remains the single
entry point for a skin or background change. The memory panel also remembers
whether it was open (`~/.milk/memory-panel.state`, independent of the skin),
and a double-click expands a brick or percept in place instead of dumping it
into the transcript.
