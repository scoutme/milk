# Config-driven themes, `/theme` carousel, and the theme-builder wizard — plan

Status: **in progress** — PR 1 (Steps 1–2), PR 2 (Step 3 + `/theme
view`) and PR 3 (Step 4, the builder incl. the darken/lighten variant mode)
implemented and merged to `main`. Step 5 (ADR + docs) is the remaining
planned work.

Touchpoints: `cmd/milk/theme.go`, `cmd/milk/theme_chrome.go`, `cmd/milk/slashcmd.go`,
`cmd/milk/interactive.go`, `internal/config/config.go`, new `internal/theme/`,
new `cmd/milk/theme_carousel.go`, `cmd/milk/theme_wizard_core.go` +
`cmd/milk/theme_wizard_tui.go`. Decision record: a new ADR (themes as data),
updating [ADR-0077](../../adr/0077-tui-skins.md) and
[ADR-0098](../../adr/0098-one-panel-layout-every-theme.md) by reference.

## Analysis verdict (why this plan exists)

The premise is right, with one nuance.

- **Right:** a new theme today is new *Go code* — a `themeDef` literal plus a
  `themes` map entry (`theme.go:122–137`) and a recompile. Worse than "one
  literal per theme" is that the two themes are not data over a common basis:
  behaviour forks on a single `th.modern` boolean at ~30 call sites
  (`theme.go` chrome/pulse/scrollbars/think surface/agent labels,
  `status.go` status-bar grouping, `ansi.go` staleness ramp, `thinkblocks.go` /
  `toolblocks.go` header+body styling, `transcript.go` spacing,
  `diffsurface.go` tool-row restyle, `layout.go` body ground). A third theme can
  only inherit one of the two behavioural profiles; anything that mixes them
  means yet another branch.
- **Nuance:** the colour half is *already* data-shaped — `themeDef` is one
  struct short of a schema. So the refactor is natural, but the schema must
  capture the behavioural knobs too, not just colours. Two of the forks are
  **shapes, not colours** (agent/user label layout `agentTag`/`userEcho`, and
  the status-bar right group in `status.go`) and cannot be expressed as hex
  values — they become a small `look` enum in the schema, with two cosmetic
  flags (`tool_rows`, `scrollbar`) overridable per theme.
- Both requested features fit existing UX machinery exactly: the theme
  carousel is the agent-choice **flyover** (`picker.go` + `overlay.go`,
  `composeOverlays`) with live preview; the builder is the **init-wizard step
  machine** (`initwizard_core.go`) driving the same live preview. Both are
  TUI-only (`hostTUI`), like `/theme` today; all reusable logic goes in
  `internal/theme` (host-independent, unit-testable, the repo's `*_core`
  pattern).

## Design overview

**A theme is a JSON overlay on a common basis.** Missing fields derive from
the base; `extends` chains to another theme; behavioural shape is a small
enum, not per-theme code.

```jsonc
// ~/.milk/themes/oatmilk.json  (or .milk/themes/ for project-local)
{
  "name": "oatmilk",
  "extends": "supermilk",          // optional; default = the builtin defaults
  "derived_from": "supermilk",     // metadata only (written by the wizard)
  "look": "skin",                  // "classic" | "skin"; default: inherited, else "skin"
  "tagline": "oat milk, no sugar", // optional; "" = defaultTagline
  "colors": {                      // each: "#rrggbb" (fixed) or {"dark":"…","light":"…"}
    "base":    "#f6f2ea",
    "text":    "#3f2a26",
    "muted":   "#7d7b74",
    "accent":  "#6b6140",
    "hairline": "#dcd7c9"
    // raised, toast, thumb, accent_lo, think, focus → derived when absent
  },
  "tool_rows": "accent",           // "accent" | "plain"  (default from look)
  "scrollbar": "slim",             // "slim" | "classic"  (default from look)
  "terminal": {                    // optional; omit = don't touch the emulator
    "bg": "#f6f2ea", "fg": "#3f2a26", "cursor": "#3f2a26",
    "ansi16": [ "…16 hex…" ]       // omit = derive a same-family ramp (see below)
  }
}
```

- **Derivations (the common basis)**, applied after `extends` resolution, in
  HSL/mix helpers in `internal/theme`: `raised` = lighten(`base`, 8%),
  `toast` = mix(`base`, `raised`), `thumb` = mix(`muted`, `hairline`),
  `accent_lo` = mix(`accent`, `base`, 0.7), `think` = `muted`,
  `focus` = mix(`accent`, `base`, 0.15). A hand-written theme can therefore be
  six colours and still look complete. `supermilk` keeps its exact current
  values (it is defined with every field explicit).
- **`look`** carries the shape decisions that `th.modern` carries today:
  `classic` = inline `name:` agent labels, status-bar classic groups, chrome
  background tinting, `ansiDim` recede, yellow pulse ramp;
  `skin` = block labels (`NAME · ROLE ──`), the skin's own status-bar right
  group, no chrome tint, muted-ink recede, computed pulse ramp, OSC palette
  push when `terminal` is present. `tool_rows` and `scrollbar` default from
  `look` but flip individually (they are colour/glyph choices, not layout).
- **Registry & precedence** (`internal/theme.Registry`):
  builtins (`off`, `supermilk`) → `~/.milk/themes/*.json` → `.milk/themes/*.json`
  (project-local, following the plugins two-level convention). Later wins on a
  name collision (a project may shadow a global theme; builtins may be shadowed
  but not deleted). `off` is special: `look: classic`, no `terminal`, its
  current hardcoded colours become its explicit palette so render call sites
  stay untouched. `/theme list` shows each theme's source (builtin/user/project).
- **Validation:** hex forms `#rgb`/`#rrggbb` (case-insensitive), known keys
  only (unknown key = warning + file skipped, never a startup crash), `extends`
  must resolve (cycle detection), name = the file's `name` field or basename,
  slug rule `[a-z0-9][a-z0-9_-]*`. Malformed files are reported once via the
  transcript toast on `/theme` use, silently skipped at startup.

## Step 1 — `internal/theme`: schema, resolution, transforms (no UI change)

New package with pure logic, fully unit-tested:

- `Color` (dark/light pair + `Fixed(hex)` + JSON unmarshal accepting both the
  string and the object form), `Palette` (the 11 tokens), `Terminal` block,
  `Theme` (name/extends/look/flags/palette/terminal), `Resolve(registry) (
  *ResolvedTheme, error)` implementing the chain + derivations.
- `Registry`: `Load(userDir, projectDir string)`, `Get(name)`, `Names()`
  (deterministic order: builtins, then user A–Z, then project A–Z),
  `Sources()` for `/theme list`. File IO lives here; no knowledge of the TUI.
- `Transform(t *ResolvedTheme, dLight, dSat float64) *ResolvedTheme`: HSL
  lightness/saturation deltas over every palette token **and** the ANSI-16
  ramp (skip `""` entries), used by the wizard's variant mode.
- `DeriveANSI16(p Palette) [16]string`: the same-family ramp supermilk carries
  by hand today (hue of the accent family, text/muted in slots 7/8/15),
  so themes without an explicit `terminal.ansi16` still get readable code
  spans/links. Golden-tested against supermilk's ramp to prove parity.
- Golden tests: `Resolve` of `supermilk` reproduces today's `themeDef`
  field-for-field; derivation maths (`lighten`, `mix`) pinned by hex goldens;
  `Transform` goldens (e.g. darken-10% of `#f8f5ee`); invalid files skipped
  with the expected error; `extends` cycles rejected.

## Step 2 — registry migration in `cmd/milk`, `/theme list|reload`

Behaviour-neutral wiring; the render layer keeps its `tk*` globals and
`rebuildStyles()` single entry point (ADR-0077's rule, untouched).

- `setTheme(name)` looks up the registry and copies tokens as today; the two
  builtins are `themeDef` values **generated from `internal/theme`** (or kept as
  literals but verified equal by the Step-1 parity test — either way the
  registry is the single lookup path and `themes` map goes away).
- Every `th.modern` site becomes a check on the resolved shape field
  (`th.look == themeLookSkin`, `th.toolRows`, `th.scrollbar` …) — a mechanical
  rename, no behaviour change; `theme_test.go`'s parity assertions keep
  pinning both builtins.
- `handleThemeCmd` (`theme_chrome.go`) splits into: bare `/theme` → carousel
  (Step 3); `/theme <name>` → today's switch path (unchanged semantics incl.
  `restoreTerminalColors`, `tea.ClearScreen` + `tea.RequestBackgroundColor`
  batch, config save via `saveLocalOrGlobal`); `/theme list` → table with
  name, source, look, tagline; `/theme reload` → rescan the two theme dirs
  (hot add of a dropped-in file without restart), notify counts.
- `slashcmd.go`: hint becomes `[name|list|reload|new]`, help text rewritten;
  `interactive.go` gains theme-name arg completion (the registry names) and
  `/theme` stays in `busySafeCommands`.
- Startup (`repl.go:5809`) unchanged: `setTheme(st.cfg.Theme)` — an unknown
  persisted name already falls back to `off`; keep that.

Tests: registry-backed `setTheme` incl. a user theme loaded from a temp dir
(`testutil` sandbox); shadowing (project beats user beats builtin);
`/theme list` render; `/theme reload` picks up a new file; invalid file →
skipped + warning, session keeps running; existing theme/chrome/panel tests
green unchanged.

## Step 3 — the carousel (`/theme` bare)

New `cmd/milk/theme_carousel.go`, model state `themeCarouselState{names []string,
idx int, orig string}` on `model` (`repl.go` gains one pending field + key/mouse
routing, mirroring `pendingPluginBrowse`).

- **Open:** bare `/theme` (also `/theme carousel`) opens the flyover; the
  transcript keeps the current one-line record ("theme: X") as the audit trail
  (the picker precedent).
- **Keys** (captured before the input path while open — same dispatch point as
  `agentPickerOpen()`): `←`/`→` (and `↑`/`↓` as aliases) cycle the ordered
  registry names; **mouse wheel** up/down cycles too (the wheel is hijacked
  while open — extend `mouse.go` routing before the panel/transcript scroll
  handlers); `enter` **confirms**; **any other key closes** (per the requested
  UX — including `esc`; no typed filtering in the carousel).
- **Live preview:** each step runs the real switch path minus persistence
  (`setTheme` + `applyThemeTerminal()` + `colorizeForce`/`viewportDirty`/
  `syncLayout`), so the whole TUI — transcript chrome included — repaints in
  the candidate skin while the flyover floats above it.
- **Confirm:** persist `cfg.Theme` (`saveLocalOrGlobal`), notify
  (`theme: <name>`), `tea.Batch(tea.ClearScreen, tea.RequestBackgroundColor)`
  — exactly `handleThemeCmd`'s tail. **Close-without-confirm restores `orig`**
  through the same path (including terminal palette restore + re-detect
  background), so a browse can never strand the terminal in a half-painted
  skin.
- **Box** (`renderThemeCarousel`, `composeOverlays` + `overlayLayer`, centered
  like `renderAgentPicker`): title `theme`, a one-row carousel
  `‹ name · name · [cur] · name ›` (scrolling window of 5), a 4-swatch preview
  strip painted with the candidate's own `base/raised/text/accent` SGRs
  (hex captions), source + look + tagline line, footer
  `←/→ or wheel switch · enter apply · any other key cancel`.
- Narrow-terminal guard like the picker (`boxWidth < paletteMinWidth` → no
  overlay, fall back to the plain `/theme` listing).
- **`/theme view [name]`** (same PR, small sibling): render any theme's JSON
  to the transcript — builtins included (they *are* JSON now), so the whole
  catalogue has view-JSON parity and a theme author can copy a working
  example straight from `/theme view off` or `supermilk`. For drop-in files
  the dump is the file's own content; for builtins it is the embedded
  descriptor. The wizard's Review step (PR 3) shows the same JSON shape.

Tests (style of `plugins_browse_test.go`): open via bare `/theme`; arrow
cycles `th.name` and the box cursor; wheel cycles; enter persists `st.cfg.Theme`
and closes; a random key (e.g. `x`) closes **and** restores the original
`th.name`; preview marks `colorizeForce`; two themes only → box still renders;
narrow width → listing fallback.

## Step 4 — the theme-builder wizard + darken/lighten variants

`/theme new` (alias `/theme build`). Core in `cmd/milk/theme_wizard_core.go`
(step machine, validation, candidate assembly — pure, like
`initwizard_core.go`), TUI face in `cmd/milk/theme_wizard_tui.go` (typed input
steps + the carousel-style preview overlay). Every accepted step applies the
candidate theme **live** (Step-3 preview machinery); cancel at any step
restores the starting theme exactly like a cancelled carousel.

1. **Mode** (flyover picker, `picker.go` rows):
   `from an existing theme…` / `from scratch` / `darken or lighten current into
   a variant`.
2. **Base** (mode 1; flyover of registry names) — the candidate starts as
   `Resolve(base)`.
3. **Name** (typed; slug-validated; collision with a builtin/user theme =
   inline error with "pick another name" — shadowing stays a hand-edit
   feature, not a wizard accident).
4. **Palette** (typed hex, pre-filled with the candidate's *effective* colour,
   blank keeps it; core six first — `base`, `text`, `muted`, `accent`,
   `hairline`, `raised` — then an optional `more colors…` branch for
   `toast/thumb/accent_lo/think/focus`; everything else derives per the
   common basis). Invalid hex = inline error, step stays.
5. **Terminal block** (choice flyover): `derive a matching palette` /
   `leave the terminal's own colours` — the ANSI-16 ramp comes from
   `DeriveANSI16`; hand-editing the ramp stays a JSON-level feature.
6. **Tagline** (typed, blank = default).
7. **Review** — the carousel-style overlay again: live preview of the final
   theme, swatch strip, and the exact JSON to be written (scrollable).
   `enter` saves.

**Variant mode** (mode 3) — the requested darken/lighten, with the carousel
UX: a single overlay shows the candidate at the current level
`-25% … +25%` lightness in 5% steps (`←`/`→`/wheel step; `shift+←/→` also
nudges saturation ±5%); the whole TUI previews live via `Transform`; `enter`
→ name step (default `<parent>-dark`/`<parent>-light`) → Review; any other key
cancels and restores. The saved file **materializes the transformed colours**
(stable if the parent later changes) and records `"derived_from": "<parent>"`.

**Save:** `~/.milk/themes/<name>.json` (user scope default; an explicit
`project` choice in Review writes `.milk/themes/<name>.json` — scope picker as
one more flyover row in Review), written atomically (tmp+rename, 0644);
registry picks it up (it is reloaded on save), the theme becomes active and
`cfg.Theme` is persisted — the same tail as a carousel confirm.

Tests: wizard-core step machine (advance/validate/cancel) table tests;
darken/lighten transform goldens; end-to-end TUI test (mode 3 → level +2 →
name → review → save) asserting the JSON file content and that the active
theme is the new one; cancel-restore at each step; collision and bad-hex
errors keep the step open; derived-vs-explicit fields pinned by a
`Resolve` round-trip.

## Step 5 — docs and decision record

- New `docs/adr/01xx-themes-as-data.md`: theme = JSON overlay on a common
  basis; `look` enum instead of per-theme branches; two-level discovery
  (user/project, shadowing allowed); builtins stay first-class; wizard writes
  materialized colours. Update [ADR-0077](../../adr/0077-tui-skins.md) and
  [ADR-0098](../../adr/0098-one-panel-layout-every-theme.md) status notes by
  reference ("superseded in part: skins are now data").
- `docs/reference/spec.md` `/theme` section: new subcommands (incl. `view`),
  carousel keys, the theme-file schema + a full example, discovery order —
  and `internal/theme/themes/*.json` called out as the copy-paste templates
  (`off.json` = minimal classic, `supermilk.json` = fully explicit skin).
- `docs/guides/providers.md` config-keys table: `theme` row updated (any
  registry name), one line on `~/.milk/themes/` + `.milk/themes/`.
- README "TUI and skins" bullet: "two built-in skins plus drop-in JSON
  themes, a live-preview carousel (`/theme`) and a theme builder (`/theme
  new`)".
- CLAUDE.md `theme.go` bullet: themes are registry data (`internal/theme`);
  new theme = JSON file, never new branches.

## Implementation notes (PR 1, deviations from the text above)

- **Shape fields:** `th.modern` was renamed to `th.skin` (`look == skin`) and
  kept as the render layer's shortcut instead of rewriting ~30 call sites to
  `th.look == themeLookSkin` — same behaviour, less churn. The two cosmetic
  knobs do reach their own call sites: `tool_rows` (diffsurface tool-row
  restyle) and `scrollbar` (scrollbar glyphs); both default from the look, so
  the builtins render byte-identically (pinned by `theme_parity_test.go`'s
  goldens — the pre-registry literals).
- **focus derivation** is `mix(base, accent, 0.15)` (mostly base — a panel
  tint); the formula in the design sketch had the mix direction reversed.
- **Shadowing replaces, it does not merge:** a user/project file with a
  builtin's name fully replaces that definition; `extends` is the incremental
  way to modify an existing theme.
- **Tab completion** for `/theme <name>` landed through the existing
  namespace system (`nsTheme` → `themeRegistry.Names()`), not a bespoke hook:
  the candidates are exactly what `setTheme` accepts.
- `Registry.Load` re-baselines from the builtins on every call (that is what
  `/theme reload` relies on); a lookup miss is not a warning, only broken
  files and broken `extends` chains are.
- **Builtins are embedded JSON descriptors**, not Go literals: the two files
  under `internal/theme/themes/*.json` (`go:embed`) go through the same
  `parseTheme` path as drop-ins, so builtin-vs-drop-in differs only in
  discovery. The descriptors double as the working examples to copy when
  writing a theme — `off.json` is the minimal classic declaration,
  `supermilk.json` the fully explicit skin. `/theme view` (below) makes the
  same JSON reachable at runtime.

## PR slicing

1. **PR 1** — `internal/theme` + builtin migration + `/theme list|reload` +
   arg completion (Step 1–2). Behaviour-neutral; parity goldens prove both
   builtins render byte-identical.
2. **PR 2** — carousel (Step 3) + `/theme view` JSON dump (view-JSON parity
   across builtins and drop-ins; the copy-paste starting point for authors).
3. **PR 3** — wizard + variant transforms (Step 4).
4. **PR 4** — ADR + docs (Step 5), or folded per-PR with the ADR last.

Each PR live-tests in a real TUI before merge per the project rule (PR 2:
browse/cancel/confirm incl. wheel; PR 3: build a variant, hand-edit its JSON,
`/theme reload`, re-apply).

## Risks and notes

- **Preview vs. background detection:** every preview step must run the
  `ClearScreen` + `RequestBackgroundColor` batch (as `/theme` does today), or
  the light/dark verdict lags the painted ground and the classic/adaptive
  pairs render wrong. A confirm while the terminal ignores OSC is already
  degraded-gracefully per ADR-0077; unchanged.
- **Global state:** `th`/`tk*` globals and `rebuildStyles()` stay (ADR-0077's
  one-entry-point rule); the refactor must not become a styling rewrite.
- **Config save failure on confirm** keeps the applied theme in-session and
  reports the error (today's `handleThemeCmd` behaviour).
- **Non-TTY stability:** every new render path gates on `isTTY` as existing
  ones do; `--output-format` output stays byte-stable.
- **Wheel hijack scope:** only while the carousel/wizard overlay is open;
  otherwise wheel behaviour is untouched (panels/transcript scroll).

## Follow-up — classic chrome as data (the `chrome` block)

Two PRs past the original slicing, the classic look's remaining surfaces
became schema too ("off declares its palette" + "close the off-theme color
gaps"): first the eleven palette slots drove the classic chrome (bars,
header border, accent, toast), then a `chrome` sub-block covered the roles
no slot can serve — panel titles/section, generic border, panel alt ground,
think surface, permission prompt pair, wordmark gold, agent sage/steel tags,
staleness ramp (3 stops), changeset roles (4), input selection and
completion grounds, and the 16-frame spinner pulse ramp. `off.json` declares
every one with the exact values the render code carried; `resolveChrome()`
resolves token-if-present / constant-if-absent per surface; `Transform`
moves the whole block so darken/lighten variants shift every visible
surface. `goldenOff` pins the block like the palette.

Later fix (the "standard black background" pass): `base` is the look's
*ground* — off declares the standard black (`#000000`/`#ffffff`) there —
and the header bar's own `#1b1b1f` moved to a `chrome.header_bg` slot
(fallback: base), so the palette carries the real background like
supermilk's does. off also declares `text` (the classic body ink), which
the swatch strip needs to show the full base/raised/text/accent quartet.

Boundaries (documented, not gaps): **ANSI semantic slots** — error red,
spinner yellow, the bold-yellow highlight, status agent 1/3/8 — stay
indexed and ride the terminal's own palette (`terminal.ansi16`, which
`Transform` shifts when a theme declares one). **glamour's markdown body
styling** is the rendering engine's style sheet, not chrome; wiring it to
the palette is its own piece of work. The 256-colour cube constants (think
surface `48;5;235`, selection `240`, completion `238`) are now expressed as
their exact truecolor hexes when a theme declares them.

## Out of scope

- Theme marketplace / shipping themes as plugins (the marketplace fetcher
  could carry them later; discovery stays file-based here).
- Auto-reload on theme-file change (explicit `/theme reload` only).
- Fonts, layout, keybindings as theme data (a skin restyles chrome only —
  ADR-0077's rule stands).
- ACP/CLI wizard faces (`/theme` remains `hostTUI`); animated colour
  transitions; random shuffle in the carousel.
