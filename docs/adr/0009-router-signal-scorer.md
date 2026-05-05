# ADR-0009: Router signal extractor and weighted scorer

## Status
Accepted

## Context

The original rules layer had two hard rules: a token-length threshold and a keyword list. For the majority of real prompts, neither fired, so every turn fell through to the local LLM classifier. That classifier call adds ~300–800 ms of latency on every routing decision, even for obviously local tasks like "grep for TODOs" or obvious escalations like open-ended conceptual questions.

The goal is to make a conclusive routing decision — without calling the LLM — for the large majority of prompts.

## Decision

Replace the two-rule function with a layered system:

### Layer 1 — Hard conclusive rules (unchanged)
- Prompt exceeds `escalate_above_tokens` → Claude
- Prompt matches `escalate_keywords` → Claude

### Layer 2 — Short-prompt shortcut (new)
- Prompt is ≤ `local_below_tokens` tokens → conclusive local.  
  Very short prompts are almost always shell one-liners or quick task requests.

### Layer 3 — Weighted signal scorer (new)
Each detected signal contributes a signed score:

| Signal | Default weight | Rationale |
|---|---|---|
| local verb (grep, find, list, run, read, fix…) | −3 | imperative shell/task verb → local |
| escalate verb (architect, design, evaluate…) | +4 | conceptual/planning verb → Claude |
| path reference (token looks like a path, resolves on disk) | −2 | file-specific task → local |
| code block in prompt | −2 | pasting code for a task → local |
| open question start (what/why/how/could you…) | +3 | conceptual question → Claude |

If score ≥ `escalate_threshold` → conclusive Claude.  
If score ≤ `local_threshold` → conclusive local.  
Otherwise → inconclusive → proceed to LLM classifier.

### Configurable classifier fallback
When the scorer is inconclusive, the fallback is configurable via `classifier_fallback`:
- `"local"` (default) — call the local LLM classifier (Qwen2.5/Gemma 4)
- `"claude"` — escalate directly, skipping the local classifier entirely

All weights, thresholds, verb lists, and the fallback mode are exposed in `~/.milk/config.json`.

## Consequences

- Most prompts with any detectable signal resolve in microseconds, eliminating the classifier round-trip.
- False positives (e.g., a prompt that mentions a path but is actually a conceptual question) can occur; the weights are conservative so that ambiguous prompts fall through to the LLM rather than being misrouted.
- The `classifier_fallback=claude` option is useful when the local model is slow or unavailable and the user prefers over-escalation to under-capability.
- All signal metadata is stored in `Decision.Reason` for transparency in logs and debugging.
