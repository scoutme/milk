# 42. Preserve `reasoning_content` Across Multi-Turn Tool Calls

- **Status:** accepted
- **Date:** 2026-08-30
- **Issue:** MiMo deep-thinking API compatibility

## Context

MiMo's deep thinking API requires that when historical conversations contain tool
calls, assistant responses passed back in subsequent rounds **must include the
`reasoning_content` field**. Omitting it returns a 400 error.

The local agent's `scanSSE` already captured `reasoning_content` from streaming
deltas into `reasoningBuf`, but it was discarded — the `Message` struct had no
`ReasoningContent` field, and `streamCompletion` didn't return it. Every tool-call
loop iteration sent history back to MiMo without the reasoning field.

## Decision

**Option C** — add `ReasoningContent string` with `json:"reasoning_content,omitempty"`
to the `Message` struct. The `omitempty` tag makes it transparent for non-MiMo providers
(OpenAI, Anthropic, Bedrock won't see the field).

### Changes

| File | Change |
|------|--------|
| `internal/agent/local/local.go` | `ReasoningContent` on `Message`; included in both `MarshalJSON` branches (multipart + plain) |
| `internal/agent/local/local.go` | `streamCompletion` returns `reasoningText` as 6th value; stored in assistant messages at all terminal paths and before tool-call execution |
| `internal/agent/local/local.go` | `executeToolCalls` receives and stores reasoning in the assistant message it creates |
| `internal/agent/local/bedrock.go` | Signature updated to match (returns `""` — Bedrock doesn't use this field) |
| `internal/agent/local/responses.go` | Signature updated to match (returns `""` — Responses API doesn't use this field) |

### Code paths covered

- Terminal response (no tool calls) — `ReasoningContent: reasoningText` ✅
- Dedup-loop terminal — same ✅
- Max-iter fallback — `ReasoningContent: lastReasoningText` ✅
- Tool-call assistant message — `ReasoningContent: reasoningContent` ✅

## Alternatives Considered

- **Option A** (add to generic `Message` without `omitempty`) — would pollute
  serialization for all providers. Rejected.
- **Option B** (provider-specific field only on MiMo path) — more complex, harder
  to maintain. Rejected.
