# 87. Panel auto-open master switch

- **Status:** accepted
- **Date:** 2026-10-27

## Context

Content-driven panel auto-open (ADR-0044, amended by ADR-0059/0078/0079) has
been unconditional since it shipped: when a task is created, a background job
spawns, a workflow starts, or percepts arrive, the matching panel opens
itself (with a toast explaining the open), and the memory panel follows the
same rule at startup via `applyMemoryContentDefault`. The only escape hatch
was the per-panel manual override — which the user has to re-assert per
panel, and which with `remember_panel_state` on becomes a per-folder
preference to manage.

Some users do not want panels moving at all: a layout in motion is exactly
the kind of surprise the toast layer (ADR-0044/issue #162) was built to
explain, but explaining a surprise is not the same as not wanting it. There
was no way to say "panels open when I open them".

## Decision

Add one master switch for the whole behaviour, default **on** (existing
users keep exactly the behaviour they have today; bothered users opt out
once):

- **Config:** `"panel_auto_open": bool` (`internal/config`, unset = on),
  sitting beside `remember_panel_state`. Unlike panel *state* (which is
  per-folder, ADR-0078), this is a user-level disposition and therefore a
  plain global config key.
- **Command:** `/panel autoopen [on|off]` — the skip-permissions grammar
  (`on|off` sets, bare query answers in the transcript, anything else is a
  usage line), persisted like `/tips on|off` via `saveLocalOrGlobal`, with
  state changes as notification toasts (issue #162) and the bare query in
  the transcript. `autoopen` is a `/panel` subword but not a panel region:
  it is intercepted in `handlePanelCmd` before the region lookup, so it
  never collides with a panel name and is offered by the `panel` completion
  namespace.
- **Gate:** `autoOpenPanel` returns immediately when the switch is off —
  no open, no toast, no tab activation — and `applyMemoryContentDefault`
  skips its startup open too. Nothing else changes: manual `/panel <name>`
  and F1-F6 toggles, the per-panel manual override, the finalization reset
  (ADR-0059), per-folder panel state (ADR-0078), and the tabbed-theme slot
  gate (ADR-0079) all behave exactly as before, layering on top when the
  switch is on.

### Companion change: saved panel state is startup layout (amends ADR-0078)

Shipping the master switch exposed a contradiction: with the switch on
(the default), a restored panel-state file could still silently veto every
auto-open. `loadPanelState` seeded `panelManualOverride` for every stored
entry, and with `remember_panel_state` on (default) the ADR-0059 expiry
never ran — so any panel the user had ever toggled was locked out of its
next content forever, per folder. Real user state accumulated into exactly
that: all six panels overridden, auto-open visibly "gone" with nothing
wrong in config.

ADR-0078's promise ("content-driven auto-open never fights what the user
last chose") is retired; the master switch now covers that case properly
(`panel_autoopen off` = layout content never touches). The durable half of
a toggle moves to a new `panelPrefs` map:

- A manual toggle records both a `panelPrefs` entry (durable, persisted) —
  and an in-session `panelManualOverride` veto.
- `loadPanelState` restores open/closed state plus `panelPrefs` — startup
  layout only. It never seeds the veto.
- `savePanelState` persists `panelPrefs` ∪ current overrides, so choices
  accumulate across sessions without ever becoming a veto.
- `resetManualOverrideIfFinalized` always applies (the
  `remember_panelState` early-return is gone): an in-session veto covers
  exactly the live content the user dismissed and expires when it
  finalizes (ADR-0059, in full, for both persistence modes).

## Consequences

- `/panel autoopen off` gives a fully static panel layout: panels change
  only on explicit request. `on` (the default) restores today's behaviour;
  the choice survives restarts.
- The switch is global while the manual override is per-folder — with the
  switch on nothing changes about the override semantics; with it off the
  override is moot (nothing auto-opens to override).
- Panels re-open on new content even across sessions (the amended
  ADR-0078 contract). Users who want a settled layout now have the
  explicit tool for it: `/panel autoopen off`.
- Model tests that build `model{}` literals directly (rather than through
  `newModel`) must set `panelAutoOpen: true` to exercise auto-open — the
  zero value means "off", while production always seeds from config
  (default on).
