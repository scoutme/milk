# ADR-0024: TUI Selection, Copy/Paste, and Welcome Screen

Date: 2026-05-23

## Status

Accepted — partially supersedes [ADR-0014](0014-full-tui-bubbletea.md) (input area design)

## Context

After the persistent TUI was established (ADR-0014) and the memory panel added (ADR-0021), the TUI lacked three capabilities that together make the transcript and input area practical for daily use:

1. **Transcript selection and copy** — users could read output but not extract it without leaving the TUI or relying on terminal-native selection (which breaks under alt-screen + fixed-layout panels).
2. **Input area selection** — keyboard selection (shift+arrows) is the standard text editing model; without it every edit required backspacing to the target position.
3. **Empty-state guidance** — a blank viewport on first launch provided no orientation.

The existing mouse mode (`\x1b[?1000h` basic click) was already wired; the textarea cursor infrastructure was already in place. The primary challenge was ANSI-safe highlighting — both the transcript and input use colored text, and a naive `\x1b[7m` reverse-video overlay conflicted with the cursor (also reverse-video), causing flicker.

## Decision

### Transcript (chat view) selection

Mouse drag on the viewport selects by line+column. Implementation:
- `tea.MouseButtonLeft` press → sets `(selAnchorLine, selAnchorCol)`
- Mouse motion (`\x1b[?1002h` button-motion) → updates `(selEndLine, selEndCol)` live
- Release at same point as press → clears selection
- `wrappedTranscript()` re-renders the selected range with `\x1b[48;5;240m` background (not reverse-video) so it doesn't interact with colored text

**Copy triggers:**
- Right-click → copies `selText` to clipboard (via `atotto/clipboard` + OSC 52)
- Ctrl+C with an active selection → copies instead of interrupting

### Input area (textarea) keyboard selection

Shift+Arrow keys extend a rune-offset-based selection:
- On first shift press, `taSelAnchor = taCursorOffset()` is recorded
- The bare direction key is forwarded to the textarea (moves cursor)
- `taSelEnd = taCursorOffset()` is recorded after the move
- Any non-shift key clears the selection
- Esc clears the selection without moving the cursor
- History navigation (Up/Down) clears the selection

**Highlight rendering:** `applyInputHighlight()` strips ANSI from the colorized display line, applies `\x1b[48;5;240m` background to the selected rune range, and reconstructs the line — preserving the colorized prefix (prompt label) and the cursor escape (`\x1b[7m`) which is re-injected after highlighting.

**Copy triggers:** same as transcript selection — Ctrl+C and right-click both copy `taSelText()`.

### Copy/paste via right-click

Right-click is dual-purpose:
- Active selection (either textarea or transcript) → copy to clipboard
- No selection → paste clipboard content into textarea

### Welcome screen

When the transcript is empty, `setViewportContent()` renders a centered placeholder (`welcomeScreen()`) instead of a blank viewport. The placeholder is a pure computed string — never written to `m.transcript` — so it disappears automatically on the first `appendTranscript()` call. It shows the logo, tagline, and `/help` hint.

## Alternatives considered

**Use `\x1b[7m` (reverse-video) for selection highlight** — rejected because the textarea cursor also uses `\x1b[7m`. When the cursor lands on a selected character, double reverse-video cancels out (normal appearance), and the cursor's blink rate causes the selection boundary character to flicker.

**Native terminal selection only** — insufficient under alt-screen with a two-panel layout. Terminal selection follows screen coordinates and does not scroll with the viewport; it also captures the panel border characters and ANSI codes as raw bytes.

**Line-granular selection only** — simpler but loses the ability to select a partial line (e.g. copy one argument from a command output). Column-aware selection added relatively little complexity on top of the line tracking already needed.

## Consequences

- Mouse mode upgraded from 1000 to 1002 (button-motion). This disables terminal-native drag selection. Since the TUI provides app-managed selection, this is acceptable.
- `wrapLineIntoRows()` — a package-level port of the textarea's internal `wrap()` logic — must be kept in sync with upstream bubbles/textarea. Divergence would cause highlight placement to shift after wrapping boundaries.
- `taCursorOffset()` uses `m.ta.Line()` + `m.ta.LineInfo().ColumnOffset`. These are public APIs but their semantics (logical line / rune column) should be verified when upgrading bubbletea.
