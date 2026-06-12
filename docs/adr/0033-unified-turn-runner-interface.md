# 33. Unified TurnRunner interface for agent dispatch

Date: 2026-06-12
Status: Accepted

## Context

milk's dispatch layer grew organically as new agent backends were added. Each combination of (role × provider type) acquired its own `run*` function:

| Role       | Provider       | Function                   |
|------------|---------------|----------------------------|
| Primary    | Local HTTP     | `runLocal`                 |
| Primary    | Subprocess     | `runSubprocessPrimaryWith` |
| Escalation | Local HTTP     | `runEscalationLocal`       |
| Escalation | Claude CLI     | `runCLIEscalationWith`     |
| Escalation | Subprocess     | `runSubprocessEscalationWith` |

Every function contains the same ~40-line skeleton: determine context mode (First/Returning/Resume), apply fresh-start check, add user turn, transition state, resolve nonce, wire callbacks, build context, call the agent, record tokens, add assistant turn, rebuild summary bricks, transition back to ROUTING, save session. The differences are only which session fields store the session ID and nonce, and how the underlying agent is invoked.

This made adding the aider self-escalation path (ADR-0032) require copying the escalation-resolution logic a third time into `runSubprocessPrimaryWith`.

## Decision

Introduce a `TurnRunner` interface in `cmd/milk/runner.go` that abstracts provider-specific inference. Provide three implementations:

- **`localRunner`** — wraps `*local.Agent`; builds message history from session, handles context trimming, handles `EscalationSignal`
- **`cliRunner`** — wraps `*claude.Agent`; builds static/dynamic context files, holds `permContext` for tool-approval routing
- **`subprocessRunner`** — wraps `*subprocess.Agent`; builds static/dynamic context files, handles `<milk:escalate>` tag

Introduce two role-specific dispatch functions in `cmd/milk/dispatch.go` that replace all five `run*` functions:

- **`runPrimary(ctx, cfg, sess, runner, mem, prompt, out)`**
- **`runEscalation(ctx, cfg, sess, runner, brief, mem, prompt, out)`**

Each accepts a `TurnRunner` and handles all session bookkeeping (state transitions, nonce management, context-mode resolution, turn recording, token accounting, summary rebuilding). The runner's `Execute` method handles only inference.

The `dispatchAgents` struct in `repl.go` becomes `{primary, escalation TurnRunner}`, and `runTurn` calls `runPrimary` or `runEscalation` based on the router target.

### TurnRunner interface

```go
type TurnResult struct {
    Text, NewSessionID, EscalationReason string
    EndsWithQ                            bool
    InputTokens, OutputTokens            int64
    CacheRead, CacheCreate               int64
    CostUSD                              float64
}

type TurnCallbacks struct {
    OnNeed     func(body string)
    OnPercept  func(body, consumerHint string)
    OnEscalate func(reason string) // nil for escalation runners
}

type TurnRunner interface {
    Name()  string
    Ping()  error
    // Execute runs one inference turn.
    // sessionID is pre-extracted from the appropriate session field by the caller;
    // TurnResult.NewSessionID must be persisted by the caller when non-empty.
    Execute(ctx context.Context,
            cfg    config.Config,
            sess   *session.Session,
            mem    *memory.Store,
            role   AgentRole,
            ctxMode escalation.ContextMode,
            sessionID, nonce string,
            percepts []string, injectInstructions bool,
            prompt string,
            cbs TurnCallbacks,
            out io.Writer) (TurnResult, error)
}
```

`sessionID` is passed in (pre-extracted by dispatch based on role) and returned as `NewSessionID` when the runner opens a new session. This keeps the runner free of role-specific field names on `*session.Session`.

### Role-specific session fields

The dispatcher maps role → session fields:

| Role       | Session ID field         | Nonce field          | State active         | State done         |
|------------|--------------------------|----------------------|----------------------|--------------------|
| Primary    | `PrimarySessionID`       | `PrimaryNonce`       | `StateLocal`         | `StateRouting`     |
| Escalation | `EscalationSessionID`    | `EscalationNonce`    | `StateEscalation`    | `StateRouting` / `StateEscalationWaiting` |

### Context-mode resolution

`runPrimary` and `runEscalation` compute `ctxMode` locally, applying the fresh-start check (need-stale or turn-gap) before calling Execute. The runner does not decide context mode.

### Self-escalation

When a subprocess primary's Execute returns a non-empty `EscalationReason`, `runPrimary` dispatches immediately to `runEscalation` with the escalation runner, passing the reason as `brief`. This replaces the inline escalation-resolution block that was duplicated inside `runSubprocessPrimaryWith`.

## Consequences

**Positive:**
- 5 `run*` functions replaced by 2 dispatch functions + 3 runner implementations
- Adding a new provider backend requires only a new `TurnRunner` implementation — no changes to dispatch logic
- Role-specific session bookkeeping lives in one place; provider-specific inference lives in another
- Self-escalation dispatch is centralised in `runPrimary`, not scattered across runner implementations

**Negative:**
- `Execute` signature is wide (12 parameters) because each runner type uses a different subset; this is a known trade-off of the single-interface approach
- The `cliRunner` still carries CLI-specific fields (`permContext`, `newInput func() inputReader`) that have no equivalent in other runners — these are implementation details not exposed through the interface

**Neutral:**
- `localRunner` still branches on `role` internally (different history-building strategy for primary vs escalation); this is a provider-specific concern, not a duplication of role logic
- `dispatchAgents` in `repl.go` simplifies from 5 typed fields to `{primary, escalation TurnRunner}` plus availability flags
