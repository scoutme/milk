# ADR-0109: Telegram delivery policy — harden first, then pace

- **Status:** Accepted 2026-11-24
- **Context:** issues [#439](https://github.com/scoutme/milk/issues/439) (live-probed delivery-path bugs) and [#449](https://github.com/scoutme/milk/issues/449) (Bot API limits analysis)
- **Supersedes/amends:** nothing — hardens the paths issues #253/#325 built and keeps ADR-0070's message shapes; complements [ADR-0088-style observability expectations](../guides/operations.md) through `obs`

## Context

Issue #439 was filed from five verified defects found by live probes against the real Bot API (harness: `internal/oversight/telegram/live_probe_test.go`):

1. `escMD` never escaped a literal backslash — MarkdownV2 treats `\` as an escape introducer, so `C:\Users` rendered as `C:Users`, and a bad pairing (trailing `\`, `\[` …) got the whole message rejected with `400 can't parse entities`. Worse: the chunk part marker `(n/m)` was emitted **unescaped**, so every *chunked* response failed parse (`Character '(' is reserved`) — the live-verified mechanism behind the original vanished-analysis report.
2. `sendOnce` swallowed **every** non-429 failure silently (400 parse errors, 403s, long-penalty drops): no log, no event, no fallback — while `NotifyResponse` promises "in full, never trimmed (#253)". This is how an analysis vanished with zero evidence.
3. One failed startup ping disabled oversight for the whole session (issue #388's fail-closed was one-shot).
4. Turn-start and permission prompts put multi-line text into MarkdownV2 code spans without `codeText` — the latent 400 of (1)+(2).
5. Zero observability: nothing in `internal/oversight` emitted telemetry.

Issue #449 then asked what Telegram's *published* limits are and how long sessions should respect them: ≤1 msg/s per chat sustained (bursts tolerated), ≤20 messages/minute per group, ~30 messages/second aggregate (free tier), 4096-char messages, `retry_after` on 429, updates retained ≤24h. The reactive gate of #325 could wait out penalties but never prevented them; a long session's *cumulative* burst history is exactly what escalates — and #325's own comment records penalties running to hours.

## Decision

**Two layers on one wire: proactive pacing (never cause the 429) and honest reaction (never lose content silently).**

### 1. escMD escapes backslash; code spans use codeText + escCode (#439-1, #439-4)

- `escMD` gained `\ → \\`. Escaping is now lossless: Telegram's MarkdownV2 unescaper round-trips every input (pinned by `TestEscMDRoundTripsThroughTelegramParser`).
- Anything inside a code span (turn-start prompt preview, permission input/path, tool summaries) goes through `codeText` (newlines → ` ⏎ `) + `escCode` (`\` and `` ` `` only) — never raw `escMD`, never a multi-line span.

### 2. Failures are outcomes, not silence (#439-2, #439-5)

`sendOnce` returns a typed `sendAttempt` (id / retryAfter / apiCode / parseError / transport). `send()` interprets:

- **429** → arm the gate, count, `obs.Event("telegram.flood_wait")`, retry as before (short) or drop (long).
- **400 `can't parse entities`** → re-send **once as plain text** (`plain_fallback` counter + `obs.EventWarn`) — content is never lost to escaping.
- **Anything else** → count (`send_failed`) + `obs.EventWarn("telegram.send_failed")`; still no retry storm.
- **Long-penalty drops** → first one logged with a preview (`flood_drop`), all counted; when the gate expires, **one summary** goes out — an `obs.EventWarn("telegram.flood_summary")` *and* a chat notice ("N messages dropped during a flood penalty") — so hours of invisible drops end with an explanation.
- Queue-full overflow now counts and logs (`queue_overflow`) instead of firing out of band invisibly.

### 3. Verification retries (#439-3)

`armOversight` pings immediately, then retries on a bounded schedule (10s/30s/60s) before issue #388's fail-closed verdict fires — the wrapped error states attempts and elapsed time. A transient startup blip self-heals; a genuinely dead endpoint still ends as `oversight.Noop` with the warning (and `/setup telegram` re-arms). Test seam: `oversightRetrySchedule`.

### 4. Proactive pacing — token buckets (#449-A)

Before any wire touch, `send()` reserves from two lazy-refill buckets (all under `sendMu`):

| bucket | rate | burst | source |
|---|---|---|---|
| per-chat, private | 1/s | 2 | Bot FAQ "avoid >1 msg/s per chat" |
| per-chat, group/supergroup | 20/min | 2 | Bot FAQ group hard limit |
| global | 25/s | 10 | headroom under ~30/s aggregate |

Chat type is discovered from `getUpdates` (`chat.type`), flipping the per-chat rate once. Pacing applies **only to the real api.telegram.org** — test servers construct with `rate 0` (tests inject rates directly); this keeps suites fast without test churn and is pinned by `TestPacingOnlyOnTheRealAPI`. Chunks each cost a token: a 6-chunk reply drains over ~6s instead of 1.2s.

### 5. One penalty gate for every method (#449-B)

`notBefore` became an atomic, **shared** gate: `sendMessage`, `editMessageText` and `sendChatAction` all arm it on 429, and both the sender and the status-line renderer read it (`gateUntil`), only-forward via `armGate`. Telegram's flood control is bot-wide; previously an edit's penalty left sends firing and vice versa. Status renders also skip while the gate holds.

### 6. Polling (#449-F)

`getUpdates` failures back off exponentially (3s → 30s, `pollBackoffDelay`) instead of hammering every 3s; a successful poll after a >24h gap emits `telegram.poll_gap` (updates older than 24h were dropped by Telegram — say so after a laptop sleep).

## Consequences

- **Space vs. latency:** a chunked response now takes ~1s/message to drain — imperceptible for the 1–3-message turns milk produces; in exchange, multi-hour sessions stop accumulating flood penalties (the failure shape of #439: burst → 429 → silent drops → hours-long penalty → hours of invisibility).
- **Every failure path is visible:** `Notifier.Stats()` exposes `sent / paced_wait / flood_wait_429 / flood_dropped / plain_fallback / send_failed / overflow` (tests and diagnostics), and each anomaly shape lands in `milk.log`/`logs.jsonl` via `obs` — the next "did Telegram eat it?" is a grep. Per-success events stay out of the log (noise).
- **Plain-text fallback is one-shot and explicit:** if the plain retry also fails, the message is lost but counted and logged — never a silent fourth state.
- **Deliberately not in this decision:** message coalescing/queue priorities under backlog (the 256-deep FIFO basically never overflows outside penalties, which drop fast by design; `queue_overflow` now measures it — revisit only if it fires), `parse_mode` fallback for `editMessageText` (an edit failure keeps the original message; the base text is now escaped correctly), and any config knob for rates (published limits are constants; a knob invites unsafe values).
- The live-probe harness's P3/P4 upgraded from "latent reproducer" to controls — the fix is assertable end to end (`TG_LIVE_TEST=1 … go test -run TestLiveProbe`). P5 then **proved the headline bug live**: all three chunks initially 400'd on the unescaped `(n/m)` marker and were rescued by the plain-text fallback; after escaping the marker they deliver formatted (0 fallbacks). Unit pin: `TestChunkHeaderEscapesPartMarker`.
