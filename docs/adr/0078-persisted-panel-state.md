# 78. Persisted Panel State: `remember_panel_state` and the Content-Driven Memory Panel

- **Status:** accepted — amended by [ADR-0087](0087-panel-auto-open-master-switch.md) (restored preferences are *startup layout* only: they no longer seed the auto-open veto, and the in-session veto always expires per ADR-0059 when the dismissed content finalizes)
- **Date:** 2026-10-24

## Context

Three threads converged on side-panel state:

1. **ADR-0044** made auto-open override-aware (a manual `/panel`/F-key toggle
   suppresses automatic management), and **ADR-0059** bounded that override by
   content finalization — a close sticks only while the content it was
   dismissed over stays live, so one dismissal never blinds a panel to
   genuinely new activity.
2. The TUI skin work (#318) introduced a **memory-panel-only** persistence
   (`~/.milk/memory-panel.state`) plus a memory-only early return in
   `resetManualOverrideIfFinalized`, so the memory close survived
   finalization. Deliberate, but it made memory an exception to ADR-0059
   with a mechanism no other panel had.
3. The memory panel was also the only panel **open by default**, even when
   the percept store was empty — every other panel earns its space by
   content.

Review of #318 called for generalizing the state management to every panel,
feature-gating it, and bringing the memory panel onto the content-driven
open rule instead of special-casing it.

## Decision

### One durable preference mechanism for all panels

A manual toggle (`/panel <name>` or F1-F6) is a **preference**, not just a
session event:

- persisted to a **folder-bound** file — `~/.milk/panel-state/<name>-<hash>.json`,
  one per project folder (atomic temp+rename; `version` plus a `panels` map
  of `panelSubName` → open state), saved on every
  manual toggle;
- restored at startup, seeding both the panel's open state *and* its
  `panelManualOverride` entry — so content-driven auto-open never fights
  what the user last chose;
- with the feature on, **content finalization never expires the override**
  (`resetManualOverrideIfFinalized` is a no-op): the ADR-0059 exception the
  memory panel alone had is now the rule for every region.

Only *manually toggled* panels are written — `panelManualOverride` is
exactly the preference set. Auto-opened state is transient by design: a
workflow panel that happened to be open never reopens empty next session.

### Feature gate: `remember_panel_state` (default on)

The whole mechanism is config-gated. Unset = on. With
`"remember_panel_state": false` nothing is read or written and panels are
session-only again — and with them the **ADR-0059 expiry semantics**:
`resetManualOverrideIfFinalized` drops the override when content finalizes,
so the next new content auto-opens the panel. ADR-0059 therefore stands as
the feature-off fallback; its contract tests run under that setting.

### Memory panel: closed by default, opened by content

The memory panel joins the other panels' rule — its *content* opens it:

- at startup `applyMemoryContentDefault` opens it iff the percept store has
  content (any scope), quietly (no toast: the panel is part of the first
  screen, not a surprise event);
- mid-session the 5s memory poll (now armed unconditionally) detects content
  arriving and auto-opens like any other panel's content event, respecting
  a manual/remembered choice;
- a remembered state always wins over the content check.

### Superseded

`~/.milk/memory-panel.state` (memory-only, introduced and superseded within
#318 — never released) is replaced outright by `panel-state.json`; no
migration.

## Consequences

- Closing a panel is now a durable statement ("don't show me this") rather
  than "not for this content" — the user re-opens via the same F-key or
  `/panel`, and that is persisted too.
- ADR-0059's expiry remains fully implemented and tested behind the
  feature flag; the two contracts are pinned side by side
  (`TestPanelClose_FinalizedContentDoesNotStick` for the fallback,
  `TestPanelClose_RememberedPreferenceSticks` for the default).
- The memory poll tick runs for the session's lifetime (one cheap wake per
  5s next to the 80ms spinner) so content arrival is noticed with the panel
  closed.
- `panelOpenPtr` is the single raw-state accessor shared by toggling,
  auto-open and persistence, so the six panel setters cannot drift.

## Amendment (2026-10-25): folder-bound files

The state file was one global `~/.milk/panel-state.json` shared by every
project. The follow-up rule: *the panel state files should be folder-bound,
or at least session-bound* — the same concern that previously surfaced as
the background-job state file pointing at the startup session only (state
files must be context-bound, never singletons). The file is now per folder:

- **`~/.milk/panel-state/<name>-<hash>.json`** — `<name>` is the folder's
  sanitized base name (human-findable), `<hash>` is `session.CWDHash` (12
  hex chars of the folder's sha256, so same-named folders stay distinct);
  the folder's full path is also stored inside the file (`folder` field,
  `omitempty`). Resolution (`panelStatePathFor`) uses `activeConfigDir(st)` —
  the session's folder, the same scoping rule config views already use —
  **once at startup**: `/new`//`resume`//`clear` change the conversation,
  not the folder the layout belongs to.
- **One-time adoption.** The old global file is *moved* (never copied, never
  discarded) to the first folder that loads after the upgrade
  (`adoptLegacyPanelState`): existing preferences survive the migration
  without re-appearing in every other folder. An existing folder file always
  wins over a leftover legacy file, and a failed move falls back to reading
  the legacy path rather than dropping state.
- **Consequences.** Two folders can never clobber each other's state, and a
  second folder starts from its own empty preferences instead of inheriting
  whatever the first one saved. Concurrent sessions in the *same* folder
  still share one file — panel layout is a preference, like the theme —
  with session-level binding inside a folder left open if that ever bites.
  Older milk binaries looking for `~/.milk/panel-state.json` simply start
  fresh. Format: `version` unchanged (content shape identical); files
  written without a bound folder keep the old byte shape.
