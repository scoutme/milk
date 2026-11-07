# ADR-0101: Config write safety — atomic, backed-up, guarded, attributable

- **Status:** Accepted 2026-11-05
- **Context:** Issue #404 (2026-11-05 incident: `agents` and six other keys wiped from `~/.milk/config.json`; forensics on the issue)
- **Supersedes/amends:** nothing — complements #405 (the `.bak` no-op guard, whose commit message deferred the rest to this design)

## Context

milk rewrites its own configuration from several runtime paths (update-check stamps, wizard commits, theme/agent/telegram slash commands, CLI config commands, ACP `set_config_option`) with **no atomicity, no lock, no meaningful backup and no record**:

1. `Save()` truncated `config.json` in place — any concurrent reader (and several milk processes share `~/.milk`) could see half a file, trip `defaults()` on parse, and the *next* save persisted the agentless defaults over a good config. A failure to load became a write of a wiped config.
2. The backup (`config.json.bak`) was refreshed from loads and saved with the *new* bytes — a backup of what you just wrote restores nothing, and a wiped-but-valid file replaced the last-known-good copy at the very next start.
3. The startup update-check write-back saved a **15-second-old in-memory snapshot** of the whole config — a lost-update race against any edit made during the network call.
4. Nothing logged config mutations: the incident's corrupting writer left no trace (mtimes overwritten, no `obs` event), so attribution was impossible after the fact.
5. Test guards against the real state root were per-test convention (`t.Setenv("HOME")`) — while no test wrote `config.json`, the same runs littered the real `~/.milk/sessions` with hundreds of fixtures.

## Decision

**Write model** (`internal/config/safety.go`, `lock_*.go`):

- **Atomic + serialized.** Every write goes temp-file → `rename` (readers see old or new bytes, never a torn file) under an advisory sidecar lock (`flock` / `LockFileEx`, auto-released on process death; no-op fallback on exotic platforms).
- **Backup = previous content.** The writer rotates the bytes it is about to replace into `.bak` (old `.bak` → `.bak.1`, N=1), only for effective changes (`configsEquivalent`, #405's rule) and never from an unparseable file. **Loads never touch `.bak`** — a refresh there would let a wiped-but-valid file replace the last-known-good copy at the next start.
- **Persist guard (refuse-persisting-defaults).** A load that ends with an unparseable file and no usable backup arms the guard: every ordinary save refuses with `ErrPersistRefused` (user-facing text). The guard clears on a successful load/recovery or on the **init wizard's forced save** (`SaveForced` / `SaveScopeInForced` — the one explicit recovery path allowed to write after a failed load). Read errors (absent overlay file = normal) leave the guard untouched in both directions.

**Consent model — by construction, not by prompt:**

- After migrating `updateCheck`/`skipUpdateRelease` to **`PatchBookkeeping`** (re-read the file, parse it, set only whitelisted stamp keys `update_last_check` / `update_skipped_version`, write only on change — never a snapshot), **every full-config writer in milk is reachable only from an explicit user action** (slash command, wizard, CLI, ACP option).
- The remaining knob, `config_consent: "strict" (default) | "open"`, only controls the guard: `"open"` restores the legacy permissive behavior for whoever wants it.
- An interactive ask-gate on the ADR-0052 permission machinery was considered and **rejected for now**: with no non-explicit full-config writer left it would be unreachable UI with real plumbing cost (config writes happen inside `Update` *and* in goroutines). The safety net for future writers is the observer below plus the guard; the ask-gate stays the documented option if a non-explicit writer ever needs it.
- **Attribution.** Every save, patch and refusal emits a `SaveEvent` through `SetSaveObserver`; `cmd/milk` wires it to `obs` (`config_write` / `config_write_refused` in the milk log when OTel is on). The next incident gets a log line.

**Test hygiene:**

- `internal/testutil.SandboxStateRoot()` — `TestMain`-level redirection of `HOME`/`USERPROFILE`/`MILK_CONFIG_ROOT` for `cmd/milk`, `internal/config`, `internal/session`, `internal/agent/local`, plus a **canary** that fails the run (exit 98) if the real `config.json` changed bytes during it. Convention became a property of the suite. Synthesized homes are **rc-complete**: `EnsureShellRC` seeds a comments-only `.zshrc` (sandbox always; the showcase pane only under `$SHELL=zsh`), because an rc-less HOME makes zsh run its first-run menu, which swallows harness keystrokes — env-prefixed launch lines included — and silently launches against the real state root (the #404 forensics \"fixture artifact\"). A passing canary also **drain-removes the sandbox root**, and per-test homes use `internal/testutil.SandboxHome` with the same drain-aware cleanup: a test's machinery can outlive its return (a turn or job completion handler writing state after the test ended), and the drain waits that late writer out instead of failing `TempDir RemoveAll cleanup: … directory not empty` — the CI flake family that hit `TestACPStreaming_BackgroundJobOutputV2AndV1` and `TestHandleBusyKey_EnterDoesNotSpawn`. A writer that persists past the drain timeout fails loudly **naming the leftover files**, so genuine orphans diagnose themselves. Known limit: a completion handler bound to env at write time can still write under another test's HOME; the drain absorbs it into the victim's cleanup, bounding writers to their spawn-time root remains the larger refactor (not done).

## Consequences

- Greenfield stamping changes shape: `PatchBookkeeping` writes nothing when the file is absent — but `Load()` creates the config on first start, so the normal flow is unaffected; a stamp is simply skipped when there is no config to stamp.
- `.bak` semantics flip from "last parsed bytes" to "content the previous save replaced"; `.bak.1` appears (single predecessor). #405's no-op suppression is preserved via `configsEquivalent`.
- A session that starts against a corrupt config now runs read-mostly: slash commands that persist settings report the refusal until `/config init` fixes the file (or `config_consent: "open"` opts out).
- Two new OS-locked files exist next to config (`config.json.lock`, tiny, permanent by design — unlinking would race the lock) and occasional `.tmp` writes (removed on failure or rename).
- Tests in the four sandboxed packages never touch the real state root again; `internal/showcase` (gated live test needs the *real* config presence as its skip signal) and `eval` (isolated roots by design) deliberately keep the convention model.
