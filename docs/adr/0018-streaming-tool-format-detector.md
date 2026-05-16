# ADR 0018 — Streaming Tool-Format Detector

**Status:** proposed  
**Date:** 2026-05-16

---

## Context

`streamCompletion` in `internal/agent/local/local.go` streams SSE tokens from
llama.cpp, writing each text token to `out` immediately. At end-of-stream
`extractToolCalls` scans the accumulated `textBuf` for known tool-call markup.

This works for Gemma 4 when llama.cpp is started with the correct
`--chat-template` flag, because tool calls then arrive in the `tool_calls`
delta field and never touch `textBuf` at all. It breaks for Qwen2.5 (and any
other model) when the wrong (or no) chat template is used:

1. The model emits tool-call markup as raw text content.
2. Tokens are printed to `out` before the full buffer is parsed — the user sees
   raw XML/JSON leak.
3. The post-hoc regex scan on `textBuf` still works *structurally*, but it has
   no information about which format is in use, so every turn re-scans for all
   patterns.
4. When the user switches the model (e.g. via llama.cpp model hot-swap), the
   format can change mid-session; the current code has no way to detect or
   adapt.

Every known tool-call format emits a *distinguishable opening delimiter* early
in the stream. Recognising that delimiter lets us:

- Stop printing immediately, preventing raw markup from reaching `out`.
- Route the accumulation to the correct parser.
- Record the identified format for the session so subsequent turns skip
  detection overhead (but stay ready to re-detect if the format changes).

---

## Decision

Introduce a **streaming tool-format detector** (`internal/agent/local/detect.go`)
that wraps the token-by-token accumulation loop.

### Recognised formats

| ID | Opening trigger | Closing trigger | Notes |
| --- | --- | --- | --- |
| `native` | *(delta `tool_calls` field populated)* | — | No content involvement |
| `gemma_special` | `<\|tool_call>` | `<tool_call\|>` | Gemma 4 special tokens |
| `tool_call_tag` | `<tool_call>` | `</tool_call>` | Hermes/Qwen, Mistral |
| `tools_tag` | `<tools>` | `</tools>` | Gemma 4 fallback |
| `fenced_json` | ` ```json\n{` or ` ```xml\n{` | `}` then optional ` ``` ` | Qwen2.5 default, no template |
| `bracket_tool_calls` | `[TOOL_CALLS]` | `]` | Mistral/Llama instruct |
| `bare_json` | `{` at start of otherwise-empty content | `}` (balanced) | Raw JSON fallback |

### Detector states

The detector is a three-state machine:

```text
printing ──[prefix of known delimiter]──► matching_prefix ──[delimiter completes]──► in_block
                                                  │
                                     [no delimiter possible]
                                                  │
                                          flush pendingBuf to out
                                                  │
                                              printing ◄────────────────────────────────────
```

- **`printing`** — tokens are passed through to `out` immediately.
- **`matching_prefix`** — one or more tokens have arrived that are a prefix of at least one known opening delimiter. They are held in `pendingBuf` and *not* written to `out`. Each new token either extends a still-possible match or breaks all matches:
  - If `pendingBuf + token` is still a valid prefix of any delimiter → stay in `matching_prefix`.
  - If `pendingBuf + token` completes a delimiter → enter `in_block`, discard `pendingBuf` (it is markup, not content).
  - If `pendingBuf + token` can no longer match any delimiter → flush `pendingBuf + token` to the caller as printable bytes, return to `printing`.
- **`in_block`** — tokens accumulate in `blockBuf` until the closing delimiter for the identified format is seen. Nothing is written to `out`.

This ensures **no characters are lost or duplicated** and **no markup leaks to `out`**, even for partial false-start prefixes like `<tool_result>` (not a registered delimiter) or `<tools` followed by `do not`.

### Detector interface

```go
// ToolFormat identifies a model's tool-call encoding.
type ToolFormat string

const (
    ToolFormatUnknown      ToolFormat = ""
    ToolFormatNative       ToolFormat = "native"
    ToolFormatGemmaSpecial ToolFormat = "gemma_special"
    ToolFormatToolCallTag  ToolFormat = "tool_call_tag"
    ToolFormatToolsTag     ToolFormat = "tools_tag"
    ToolFormatFencedJSON   ToolFormat = "fenced_json"
    ToolFormatBracketCalls ToolFormat = "bracket_tool_calls"
    ToolFormatBareJSON     ToolFormat = "bare_json"
)

// detectorState is the internal FSM state.
type detectorState int

const (
    statePrinting       detectorState = iota
    stateMatchingPrefix               // accumulating a potential delimiter prefix
    stateInBlock                      // inside a confirmed tool-call block
)

// StreamDetector accumulates tokens, detects tool-call format on the fly,
// and signals when a complete tool-call block has been received.
type StreamDetector struct {
    Format     ToolFormat    // identified format (empty until confirmed)
    state      detectorState
    pendingBuf strings.Builder // held during stateMatchingPrefix
    blockBuf   strings.Builder // held during stateInBlock
}

// Feed accepts one token from the stream. Returns:
//   - flush: bytes to write to out immediately (non-nil only when a partial
//     prefix turned out not to be a delimiter and is being released)
//   - complete: whether a complete tool block is now in blockBuf
//
// Callers must write flush to out before checking complete.
func (d *StreamDetector) Feed(token string) (flush []byte, complete bool)

// Extract parses and returns tool calls from the accumulated blockBuf.
// Must only be called when Feed returned complete=true.
func (d *StreamDetector) Extract() []toolCall

// Reset clears all buffers and FSM state, preserving Format for reuse on the
// next turn.
func (d *StreamDetector) Reset()
```

### Session-level format tracking

`Agent` grows a `detectedFormat ToolFormat` field. Each call to
`streamCompletion` starts a `StreamDetector` seeded with `a.detectedFormat`.
When detection confirms a format, `a.detectedFormat` is updated. On the next
turn the detector starts in `confirmed` mode and only watches for its known
delimiter, skipping the multi-pattern scan.

If a turn arrives where the opening token matches a *different* format than the
stored one, the detector re-enters discovery mode and updates `a.detectedFormat`
at turn end. This handles live model swaps.

### Streaming behaviour change

Current flow:
```
token → print to out → accumulate in textBuf → post-hoc parse
```

New flow:
```
token → detector.Feed(token)
           ├─ not in tool block, no trigger prefix → print to out, accumulate text
           ├─ trigger prefix detected → suppress print, set inToolBlock, accumulate to buf
           ├─ in tool block → suppress print, accumulate to buf
           └─ closing delimiter reached → complete=true → extract, suppress all markup
```

Plain text turns are unaffected: tokens are printed immediately as before.

### Fallback

If `Extract()` returns zero tool calls after a complete block is signalled, the
raw buffer is handed to the existing `extractToolCalls` function. This preserves
the current broad-match behaviour as a safety net and handles formats not yet
registered in the detector.

### Model info endpoint

`Agent.Ping()` is extended to optionally call `GET /v1/models` after confirming
liveness. If the response includes a model name that matches a known
model-to-format mapping table, `a.detectedFormat` is pre-seeded at startup
without waiting for the first tool call. The mapping table is kept in
`detect.go` alongside the format definitions.

Known seeds:

| Model name pattern | Default format |
| --- | --- |
| `gemma*` | `gemma_special` (falls back to `tools_tag`) |
| `qwen*` (with template) | `tool_call_tag` |
| `qwen*` (no template) | `fenced_json` |
| `mistral*`, `llama*` | `bracket_tool_calls` or `tool_call_tag` |

Pre-seeding does not lock the format — it only skips the cold-start scan on the
first tool-bearing turn.

---

## Consequences

**Good:**
- Raw tool-call markup never leaks to `out` regardless of model or template.
- Per-format parsers are small, focused, and independently testable.
- Live model switching is handled transparently.
- The existing `extractToolCalls` / `extractGemmaSpecial` logic is reused or
  superseded incrementally — no flag day.
- No llama.cpp server changes needed; no `--chat-template` requirement for Qwen.

**Bad / Trade-offs:**
- The detector adds per-token branching to the hot path; negligible in practice
  (string prefix checks on short tokens).
- Detecting `bare_json` requires tracking brace depth, which adds state.
- Format pre-seeding from `/v1/models` requires an extra HTTP call on startup;
  skipped gracefully if the endpoint is absent.

---

## Alternatives considered

### A — Post-hoc only (current)
Keep scanning `textBuf` at end-of-stream. Simple, but tokens leak to `out` and
there is no format memory between turns.

### B — Require correct `--chat-template`
Mandate that llama.cpp is always started with the right flag. Pushes the problem
to the operator; breaks for ad-hoc model switches and cold-starts without docs.

### C — Per-model config in `~/.milk/config.json`
Let the user declare `"tool_format": "qwen"`. Workable but requires manual
setup and breaks on model hot-swap.

Decision B and C are useful as *fallbacks* but not as the primary mechanism —
auto-detection is strictly better UX.

---

## Implementation plan

### Step 1 — `detect.go`: `ToolFormat` constants + `StreamDetector`
- `Feed(token string) (print bool, complete bool)`
- `Extract() []toolCall` — delegates to per-format extractors
- `Reset()` — clear buffer, keep format
- Unit tests covering each format's open/close cycle and the fallback path

### Step 2 — `local.go`: wire detector into `streamCompletion`
- Replace the bare `textBuf` / immediate-print loop with `detector.Feed`
- Store `detectedFormat` on `Agent`; seed and update per turn
- Preserve `fallbackRaw` logic for history

### Step 3 — `local.go`: model-name pre-seeding in `Ping` / startup
- Call `GET /v1/models` (graceful if absent)
- Match model name against the seed table
- Store result in `a.detectedFormat`

### Step 4 — tests
- Table-driven tests for every format in `toolparse_test.go` / new `detect_test.go`
- Integration smoke test: send a fenced-JSON tool call through `streamCompletion`
  and verify no markup reaches `out`

### Step 5 — `docs/memory-design.md` / `README.md`
- Document `--chat-template` as optional (recommended for performance, not
  required for correctness)
- Add a note on supported model families

### Step 6 — Tool-usage UI (design only, implementation deferred)

The detector now has precise knowledge of when a tool call starts and ends.
This makes a richer UI possible. Design questions to resolve before
implementing:

**What to show:** currently the user sees raw markup leaking (the bug being
fixed) or nothing (native tool_calls path). Options:

- A single status line while the tool is running: `⚙ bash: ls -la`
- A collapsible block in the TUI viewport: tool name + args on entry,
  result summary on exit, expandable on click
- An inline indicator in the transcript: `[tool: bash → exit 0]`

**Where it lives:** the TUI (`cmd/milk/repl.go` + bubbletea model) already
manages the viewport. The detector lives in `internal/agent/local` and
currently writes to an `io.Writer`. Bridging these requires either:

- A `ToolEvent` channel that the TUI listens to alongside the text stream
- A callback/hook on `StreamDetector` (`OnToolStart`, `OnToolComplete`)
- Treating tool events as structured tea.Msg sent via `tea.Program.Send`

**Deferred work (separate branch `feat/tool-usage-ui`):**

- Define `ToolEvent` type (start / complete / error) in `internal/agent/local`
- Thread events from `streamCompletion` → `Run` → REPL via `tea.Program.Send`
- Design bubbletea component: spinner while running, summary line on complete
- Decide truncation strategy for long tool results in the viewport
