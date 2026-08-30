# 41. Time-Based CurrentNeed Expiry

- **Status:** accepted
- **Date:** 2026-08-30
- **Issue:** #134

## Context

`CurrentNeed` tracks what the user is trying to accomplish across turns and sessions.
It was persisted as a turn index (`CurrentNeedSetAt = len(History)`) but had no
time-based staleness mechanism. A need set three days ago in a long-idle session was
treated identically to one set five minutes ago.

The only staleness logic was `NeedChangedSinceLastEscalation()`, which checks whether
an escalation turn happened *after* the need was set — useful for fulfilled needs, but
not for needs that simply went stale due to time.

## Decision

Add a `CurrentNeedUpdatedAt time.Time` field alongside the existing `CurrentNeedSetAt`
(turn index). On session load (`session.Load()`), compare `time.Since(UpdatedAt)` against
a configurable threshold (default 24 hours). If the need is older than the threshold,
clear it silently — the user can always set a new `/need` if still relevant.

### Implementation

| File | Change |
|------|--------|
| `internal/session/session.go` | `CurrentNeedUpdatedAt time.Time` field; set in `RecordNeed()` |
| `internal/session/store.go` | Time-based expiry in `Load()` via `NeedExpiryDuration` package var |
| `internal/config/config.go` | `NeedExpiryHours *int` config field + `AgentNeedExpiryHours()` resolver (default 24) |
| `cmd/milk/main.go` | Wires config into `session.NeedExpiryDuration` at startup |

### Configuration

```json
{
  "need_expiry_hours": 24
}
```

Set to `0` to disable time-based expiry (needs persist until fulfilled or manually cleared).

### Expiry logic in `Load()`

Three conditions, checked in order:

1. **Already fulfilled** — escalation turn occurred after `CurrentNeedSetAt` → clear
2. **Time-expired** — `time.Since(UpdatedAt) > NeedExpiryDuration` and duration > 0 → clear
3. **Otherwise** — keep as-is

## Alternatives Considered

- **Reconfirm prompt** (`"still working on <need>? [y/N]"`) — more interactive but
  interrupts user flow on every resume. Deferred to a future iteration if needed.
- **Turn-count expiry** (e.g. expire after N turns) — already partially covered by
  `NeedChangedSinceLastEscalation`. Adding wall-clock time covers the idle-session case.
