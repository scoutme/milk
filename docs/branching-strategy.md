# Branching Strategy

## Branch model

```
main                    ← always stable, reflects shipped state
└── feat/<scope>        ← feature branches (one per implementation step)
└── fix/<scope>         ← bug fixes
└── chore/<scope>       ← tooling, config, non-functional changes
└── docs/<scope>        ← documentation-only changes
```

`main` is protected: no direct commits during implementation. Every change lands via a feature branch.

## Branch naming

```
<type>/<short-kebab-description>

feat/cobra-skeleton
feat/session-store
feat/local-agent
feat/router
feat/claude-agent
feat/escalation-builder
feat/state-machine
fix/session-index-repair
chore/ci
docs/adr-branching
```

Types mirror conventional commit types: `feat`, `fix`, `chore`, `docs`, `refactor`, `test`.

## Conventional commits

Format: `<type>(<scope>): <description>`

```
feat(session): add session store with index lookup
feat(router): implement rules layer
fix(claude): handle empty stream-json response
chore: add go.mod and cobra skeleton
docs(adr): add branching strategy ADR
```

Scopes track the internal package: `config`, `session`, `router`, `local`, `claude`, `escalation`.

## Implementation branch plan

Each step from the implementation order gets its own branch, merged to main when complete:

| Branch | Content |
|--------|---------|
| `feat/cobra-skeleton` | `go.mod`, `cmd/milk/main.go`, config loader |
| `feat/session-store` | `internal/session/` — store, index, state machine types |
| `feat/local-agent` | `internal/agent/local/` — OpenAI client + tool loop |
| `feat/router` | `internal/router/` — rules + model classification |
| `feat/claude-agent` | `internal/agent/claude/` — subprocess + stream parser |
| `feat/escalation-builder` | `internal/escalation/` — transcript formatter |
| `feat/state-machine` | wiring all components, end-to-end flow |

## Merge strategy

Prefer `--no-ff` merge (or PR merge commit) to preserve branch history. Squash only when a branch has noisy WIP commits that add no archaeological value.
