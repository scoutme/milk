# ADR 0031 — Local Agent Percept Writing, Dynamic Consumer Hints, and CurrentNeed Early-Exit Fallback

Date: 2026-05-30

## Status

Accepted — partially supersedes [ADR-0022](0022-claude-percept-skill-nonce-tags.md)

## Context

ADR-0022 established bidirectional memory for the escalation (Claude) agent: it reads percepts via
`[Remembered facts]` injection and writes percepts via nonce-tagged stream interception. Two gaps
remained:

1. **Local agent had no write path.** Important facts surfaced during local-agent turns (reasoning
   traces, discovered constraints, user preferences) were lost unless the user explicitly called
   `record_memory`. The write path was escalation-only.

2. **Consumer hints were hardcoded.** `MemoryInstruction` told agents to use `@local: ` or
   `@claude: ` prefixes, but agent names are configurable (`agents[].name` in
   `~/.milk/config.json`). A deployment with a primary agent named `llama` and escalation agent
   named `deepseek` would never match the hardcoded prefixes, so all percepts fell through as
   `ConsumerAll` regardless of the intended target.

3. **`CurrentNeed` missed the `isRepeatedPrompt` early-exit path.** When the local agent detects
   that the user has repeated a prompt it just answered, it exits before any streaming begins —
   `onNeed` is never called. `Session.CurrentNeed` remained stale (or blank for a new session),
   so the escalation agent received no current-need context on handoff.

## Decision

**Local agent percept writing.** `runLocal` now calls `agent.WithTagCallbacks` before handing off to
`agent.Run`. The same `TagWriter` / `PerceptWriter` stream-interception pattern used for the Claude
agent is applied: `<milk:need:NONCE>` tags update `Session.CurrentNeed`; `<milk:percept:NONCE>` tags
are recorded with `ProducerLocal`. Consumer routing follows the same switch on `consumerHint`:
escalation-targeted percepts → `ConsumerEscalation`; everything else → `ConsumerLocal`.

**Dynamic consumer hint names.** `MemoryInstruction(nonce, primaryName, escalationName string)` now
embeds the actual configured agent names in the instruction text. The `@local: ` / `@claude: `
examples are replaced with `@<primaryName>: ` / `@<escalationName>: `. `ConsumerHintFrom` and
`consumerHintFrom` now accept a `names ...string` variadic rather than a hardcoded list, so the
parsers match whatever names are passed in at runtime.

`WithTagCallbacks` (local agent) and `WithOnPercept` (Claude agent) both accept `primaryName` and
`escalationName`, store them in `agentNames []string`, and forward them to `PerceptWriter` /
`perceptWriter` respectively.

**`CurrentNeed` early-exit fallback.** In the `isRepeatedPrompt` escalation branch of `runLocal`,
after the repeat is detected but before handing off to the escalation agent, the code now sets
`sess.CurrentNeed = prompt` if `CurrentNeed` is still empty. This ensures the escalation agent
always has at minimum the literal prompt as its current-need context, even when no `<milk:need>`
tag was emitted during the (skipped) streaming phase.

## Consequences

- Both agents can now contribute facts to the shared memory store. Local-agent percepts arrive with
  `ProducerLocal` attribution; escalation-agent percepts with `ProducerEscalation`.
- Consumer hint routing is now configuration-aware. Deployments with non-default agent names see
  correct `@<name>:` prefix matching.
- The `CurrentNeed` field is reliable on all escalation paths, including `isRepeatedPrompt`
  early exits.
- The local agent's percept nonce is generated fresh per turn (same `claude.GenerateNonce()` call),
  consistent with the escalation agent's per-turn nonce.
