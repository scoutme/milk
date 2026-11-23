# ADR-0107: Themes as data — JSON theme files over `internal/theme`

- **Status:** Accepted 2026-11-23
- **Context:** a theme was a Go `themeDef` literal plus a `themes` map entry and a recompile; worse, behaviour forked on a single `th.modern` boolean at ~30 call sites, so a third theme could only inherit one of the two behavioural profiles
- **Supersedes/amends:** supersedes the code-defined-skins half of [ADR-0077](0077-tui-skins.md) (how themes are *defined*); the skin concept survives as the `look` enum. [ADR-0098](0098-one-panel-layout-every-theme.md) is unaffected

## Context

Themes before this work were code in three ways at once:

1. **Colours** were Go literals (`themeDef` in `cmd/milk/theme.go`), one literal per theme, compiled in. Adding a theme meant a release.
2. **Behaviour** forked on `th.modern` (renamed `th.skin`) at ~30 call sites — chrome styles, pulse ramp, scrollbar glyphs, think surface, staleness ramp, status-bar grouping, label layouts. A theme was *either* the classic profile *or* the skin profile; mixing them meant yet another branch per site.
3. **Some "colours" were constants of two roles at once** (off's panel titles are grey while its accent is gold), so no flat palette could describe the classic look.

The themes-as-data plan (`docs/internal/plans/config-themes-carousel-wizard-plan.md`) was implemented across four PRs: `internal/theme` + migration, the `/theme` carousel + `/theme view`, the `/theme new` builder (darken/lighten variants via `Transform`), and this ADR. A fifth pass ("close all off-theme colour gaps") completed the palette with a `chrome` block and — the lesson this ADR is built on — fixed four successive bugs where a **declared** colour was never **rendered** (`text` showed in swatches while glamour rendered its own document ink; `base` existed as data while the ground stayed the terminal's own).

## Decision

### 1. A theme is a JSON overlay on a common basis (`internal/theme`)

A theme file (`Theme`) is: `name`, optional `extends` (chain, cycle-detected), `derived_from` (provenance metadata written by the builder), shape fields, and colour data:

- **`colors`** — the 11-token palette over a common basis: `base` (the ground), `raised`, `toast`, `hairline`, `thumb`, `text` (body ink), `muted`, `accent`, `accent_lo`, `think`, `focus`. A token is `"#rrggbb"` (fixed) or `{"dark": …, "light": …}` (picked by the detected terminal ground); empty means absent.
- **`chrome`** — the surfaces no palette slot can serve (one slot cannot hold two classic roles): `header_bg`, `panel_title`, `panel_section`, `border`, `panel_alt`, `think_surface`, `perm_fg`/`perm_bg`, `wordmark`, `agent_sage`/`agent_steel`, `stale_fresh`/`stale_mid`/`stale_warm`, `change_created`/`modified`/`deleted`/`renamed`, `selection`, `completion`, and the ≤16-entry `pulse` ramp.
- **`terminal`** — the emulator palette pushed while active (OSC 10/11/12 + the 16-colour ramp); omit the block to leave the terminal's own colours alone, empty `ansi16` entries leave slots alone.
- **Shape** — `look: classic|skin` plus two cosmetic knobs (`tool_rows: plain|accent`, `scrollbar: classic|slim`): colours in config, shape as a small enum, never free-form code. A theme with no `look` in its chain gets the token-driven `skin` render path.

Resolution flattens the `extends` chain (later overrides earlier per non-empty field), fills shape defaults from the `look`, then completes the palette through **derivations** — the common basis: `think`←`muted`, `raised`←lighten(`base`, 8%), `toast`←mix(`base`,`raised`), `thumb`←mix(`muted`,`hairline`), `accent_lo`←mix(`accent`,`base`), `focus`←mix(`base`,`accent`, 15%). Each rule fires only on present inputs, so a six-colour hand-written theme resolves complete and an absent token stays absent. `Validate` checks enums and hex syntax; invalid themes are rejected at load, never crash the render path.

### 2. Registry: embedded builtins, drop-in discovery

`internal/theme`'s registry seeds from **embedded JSON descriptors** (`internal/theme/themes/off.json`, `supermilk.json`) — the exact same schema as drop-ins, going through the exact same parse/validate path (builtin-vs-drop-in differs only in *discovery*, never representation) and doubling as the working examples for theme authors (`/theme view` prints any theme's JSON in the same shape). Discovery layers: **builtin → `~/.milk/themes/*.json` (state root) → `<cwd>/.milk/themes/*.json`** — project shadows user shadows builtin. Invalid files are skipped with a warning that `/theme list` surfaces. `/theme reload` rescans the dirs; no auto-reload.

### 3. Declared colours must be *rendered*

The rule learned across four fixes, now the contract: **a token that no render path consumes is a bug, not a gap.** The wiring today: `text` sets the markdown renderer's document ink (glamour is rebuilt per activation, so previews and background flips re-ink) and classic tool-fold bodies, and feeds the terminal ink; `base` tints the view body on every look and is pushed as the OSC ground; the `chrome` block resolves per surface token-if-present / classic-constant-if-absent (`resolveChrome`), so palette-less classic drop-ins still render exactly the original look. `themeTerminalOSCs` derives the pushed palette at push time (an explicit `terminal` block wins, palette fallbacks for ground/ink/cursor) and is re-pushed when background detection flips — the pushed dark/light variant can never go stale.

`Transform` (HSL lightness/saturation deltas) moves palette **and** chrome **and** the terminal ramp together — the builder's darken/lighten variant mode — and `Materialize` writes the result as a fully explicit theme (`derived_from` provenance) so a variant survives its base disappearing.

### 4. One entry point, previewable

ADR-0077's rule stands: `applyThemeDef` is the single activation path (registry lookup → `refreshThemeColors` → rebuild chrome styles, pulse, glamour, chrome tokens); the carousel previews candidate themes through it live (the whole TUI and the terminal palette track each step) and **any key but enter cancels and restores** — including the OSC palette — so a browse can never strand a half-painted terminal. `/theme new` builds themes through the same preview path.

## Consequences

- A third theme is a JSON file: drop it in a theme dir, `/theme reload`, done. No recompile, no new branch. The pre-registry literals survive only as goldens (`theme_parity_test.go`) pinning the builtins' converted form byte-identical to the look users know.
- **Boundaries (documented, not gaps):** ANSI *semantic* slots (error red, spinner yellow, the bold-yellow highlight, status agent 1/3/8) stay indexed and ride the terminal's 16-colour ramp (`terminal.ansi16`, which `Transform` shifts when declared); glamour's roles beyond the document ink (headings, links, code) stay the engine's style sheet; diff backgrounds stay `internal/diff` detection constants.
- Activating a theme now paints its ground and ink explicitly (off: `#000000` ground, `#d0d0d0` ink on dark) instead of inheriting terminal defaults — visually the standard look, but the palette drives it and variants move it; switching away restores the terminal's own colours (`OSC 104/110/111/112`).
- Non-TTY output stays byte-stable: every new paint is `isTTY`-gated like the old ones; `--output-format` is untouched.
- Theme shape stays bounded (the `look` enum + two knobs): fonts, layout, keybindings are not theme data — ADR-0077's "a skin restyles chrome only" rule stands.
