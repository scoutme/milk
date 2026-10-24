# ADR-0076: TUI skins — the neutral original look and the opt-in supermilk theme

*Date: 2026-10-23*

## Status

Accepted

## Context

The TUI had one look, hard-coded per call site: a navy header on dark
terminals, a yellow pulse on the wordmark, and raw ANSI green/blue for the
agent labels. Three problems followed:

- the chrome was only tuned for dark terminals, so light terminals got
  low-contrast bars and an invisible `dim`;
- the pulsing diamond and the saturated green/blue labels competed with the
  transcript, which is the only content that should carry attention;
- there was no way to opt into a different look without patching the code.

Phase 2 of the Bubble Tea v2 migration (ADR-0076) replaced the blocking
`HasDarkBackground()` query with `BackgroundColorMsg`, which makes a live
light/dark switch possible for the first time.

## Decision

Two named skins, selected with `/theme` and persisted as `theme` in
`config.json`:

- **`off` (default)** — the original look, redesigned to a neutral palette:
  graphite chrome on dark terminals and warm cream chrome on light ones, no
  diamond and no pulse in the wordmark, and muted sage (primary) / steel
  (escalation) agent tags. The same `agentTag`/`userEcho` renderers paint the
  live transcript and seeded session turns, so a resumed session looks like
  the one that produced it.
- **`supermilk` (opt-in)** — a semi-skimmed milk skin: fixed milk-white ground
  (`#f8f5ee`), warm ink text, hairline separators, one muted olive accent, a
  slim scrollbar and quiet tool rows. It carries its own tagline
  (`Got models? Got milk.`). While active it also sets the terminal's default
  background/foreground/cursor and the 16-colour ANSI ramp via OSC, so
  everything milk does not paint itself (edge columns, scrollbar gutter,
  ANSI-coloured text) matches; `exit` and `/theme off` hand the terminal's own
  colours back.

Rules that keep the two skins from forking behaviour:

- A skin restyles chrome only — header, status bar, turn labels, panel tabs,
  scrollbars, staleness ramp, tool rows, reasoning surface. Transcript
  content, slash commands, config keys and session files are unchanged.
- `rebuildStyles()` is the only place that builds chrome styles, so a
  background or skin change has one entry point (`refreshThemeColors()`).
- `/theme` re-requests the terminal background after switching, because the
  detected light/dark reflects what the skin paints, not the terminal's own
  setting.

## Consequences

- `off` keeps its layout and behaviour; only colours and the logo glyph
  change. Output is still byte-stable when `isTTY` is false.
- The supermilk skin depends on OSC support for the terminal palette; on a
  terminal that ignores it the app-painted surfaces still render, only the
  default colours and the ANSI ramp stay the terminal's own.
- New chrome colours belong in the theme tokens (`theme.go`), not in call
  sites, or the two skins drift apart.
- `/theme` is TUI-only; ACP and the plain-text CLI hosts are untouched.
