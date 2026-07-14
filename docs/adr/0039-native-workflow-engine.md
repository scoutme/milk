# ADR 0039 — Native Workflow Engine

**Status:** Proposed  
**Date:** 2026-07-14

---

## Context

The singulatris session (and similar multi-sprint development workflows) currently relies on Claude Code's `Workflow` tool to orchestrate a designer→generator→evaluator loop. This approach has a fundamental incompatibility with milk's execution model: the `Workflow` tool always runs in background, so `--print` exits before the workflow finishes, and milk has no reliable way to receive the completion notification. The current workaround — journal polling and auto-resume — is fragile and adds latency between workflow stages.

The root cause is that milk delegates orchestration to Claude Code, a tool designed for interactive sessions, not for programmatic multi-agent pipelines. Every time the workflow executes a stage boundary, milk must re-enter Claude Code and hope the journal state is consistent. There is no structured handoff, no typed verdict, and no clear ownership of loop control.

A native workflow engine would give milk direct control over stage transitions, structured inter-stage data, and a well-defined completion contract — none of which are possible when orchestration lives inside an opaque subprocess.

## Decision

Implement a native workflow engine inside milk. A workflow is a named, reusable multi-agent pipeline defined as a Go struct. The first workflow is the `dev` workflow, consisting of three roles:

- **designer**: reads the task description, produces a spec and sprint plan, writes them to a file (`<session-id>.workflow.plan.md`).
- **generator**: executes sprint tasks according to the current sprint in the plan (writes code, writes tests, writes testing instructions).
- **evaluator**: reviews the current sprint's output; writes a findings file (`<session-id>.workflow.findings.md`) and returns a structured verdict from the set `{good_to_go, needs_refinement, next_sprint}`.

Loop semantics:
- `needs_refinement` → generator re-runs for the same sprint (up to a configurable pass limit).
- `next_sprint` → generator runs for the next sprint; evaluator follows.
- `good_to_go` on the final sprint → workflow ends successfully.
- Pass limit exceeded → workflow halts with an error verdict; the TUI shows the last findings file path.

Each role is assigned to a configured agent. Valid agent specifiers: any name from `config.agents`, plus the aliases `primary` (the currently assigned primary agent) and `escalation` (the currently assigned escalation agent). Aliases are resolved at workflow start, not at each stage invocation, so mid-workflow agent switches do not affect a running workflow.

The workflow is invoked via:

```
/workflow [name] [task description] [--designer <agent>] [--generator <agent>] [--evaluator <agent>]
```

Without a name, the command lists available workflows. Without `--designer`/`--generator`/`--evaluator`, a wizard collects them interactively (asking one question at a time in the TUI). Without a task description, the wizard collects it too.

Workflow state is persisted to `~/.milk/sessions/<session-id>.workflow.json` on each stage transition so the workflow can be resumed after a TUI restart. At startup, if a `.workflow.json` file exists for the current session, the TUI offers to resume.

The TUI shows a workflow progress panel (inline, collapsible, toggled with `/panel workflow`) displaying: workflow name, current sprint, current role, pass count for the current sprint, and a verdict history log.

## Consequences

**Positive:**
- Eliminates journal polling and auto-resume fragility; stage transitions are synchronous and owned by milk.
- First-class TUI progress panel gives the user visibility into sprint/pass/verdict state without reading log files.
- Workflow state is persisted as typed JSON, so resumption after a restart is deterministic.
- Agent assignment is explicit per role, making it easy to assign the designer to a cheap local model and the evaluator to the escalation agent.
- The engine is extensible: new workflows are new Go structs implementing the same interface; no script runner or external DSL is needed.
- Verdict handling is structured (`WorkflowVerdict` type), so the loop termination condition is a Go expression, not a string parse of LLM output.

**Negative / risks:**
- Go struct workflows are less flexible than scripts or DSL definitions. Adding a new workflow requires a code change and a recompile; there is no hot-loadable workflow file format.
- The `dev` workflow's generator and evaluator may be assigned the same agent (e.g., both set to `escalation`). The engine must not conflate their sessions — each role invocation must be a fresh prompt to the agent, not a resume of the previous role's session. The `--resume` flag must only apply within the same role across passes, not across roles.
- Inter-stage file I/O (plan file, findings file) creates a coordination contract between roles that is outside milk's structured data model. If the generator writes nothing to the expected path, the evaluator's context is empty. The engine should detect missing files and surface a clear error rather than silently sending an empty prompt.
- The TUI panel adds a new message type and a new panel slot to the bubbletea model; this increases the surface area of `repl.go` and `panel_memory.go` to maintain.

---

## Implementation Plan

### Step 1 — Workflow engine core (`internal/workflow/`)

Create `internal/workflow/workflow.go`:

```go
// Workflow is a named multi-agent pipeline.
type Workflow interface {
    Name() string
    // Run executes the workflow to completion or until an unrecoverable error.
    // ctx is the bubbletea program context; send is used to stream output back to the TUI.
    Run(ctx context.Context, cfg RunConfig) error
}

// RunConfig carries everything a workflow stage needs from the outside.
type RunConfig struct {
    Session    *session.Session
    Runners    map[string]runner.TurnRunner // role name → resolved runner
    Send       func(tea.Msg)               // TUI message bus
    StateDir   string                      // ~/.milk/sessions/
}
```

Create `internal/workflow/state.go`:

```go
// State is persisted between TUI restarts.
type State struct {
    WorkflowName   string          `json:"workflow_name"`
    Sprint         int             `json:"sprint"`
    Pass           int             `json:"pass"`
    Role           string          `json:"role"`
    VerdictHistory []VerdictEntry  `json:"verdict_history"`
    AgentMap       map[string]string `json:"agent_map"` // role → resolved agent name
}

type VerdictEntry struct {
    Sprint  int    `json:"sprint"`
    Pass    int    `json:"pass"`
    Verdict string `json:"verdict"`
}
```

Persistence helpers: `LoadState(path string) (*State, error)` and `SaveState(path string, s *State) error`. Path convention: `<stateDir>/<sessionID>.workflow.json`.

Create `internal/workflow/turn.go`:

```go
// Turn invokes a single agent turn for a workflow stage.
// prompt is the full constructed prompt for this stage.
// Returns the agent's text response.
func Turn(ctx context.Context, r runner.TurnRunner, sess *session.Session, prompt string, send func(tea.Msg)) (string, error)
```

`Turn` is a thin wrapper around `r.Execute()` that handles streaming output to the TUI via `send` and extracts the plain-text response from `runner.Result`.

Create `internal/workflow/verdict.go`:

```go
type WorkflowVerdict int

const (
    VerdictUnknown      WorkflowVerdict = iota
    VerdictGoodToGo                     // workflow stage: pass or complete
    VerdictNeedsRefinement              // retry same sprint
    VerdictNextSprint                   // advance sprint
)

// ParseVerdict extracts a structured verdict from the evaluator's text response.
// It looks for the canonical keywords in order of precedence.
func ParseVerdict(response string) WorkflowVerdict
```

### Step 2 — `dev` workflow implementation (`internal/workflow/dev/`)

Create `internal/workflow/dev/dev.go`:

```go
type DevWorkflow struct {
    MaxPasses int // default 3; configurable via flag
}

func (w *DevWorkflow) Name() string { return "dev" }
func (w *DevWorkflow) Run(ctx context.Context, cfg workflow.RunConfig) error
```

`Run` implements the loop:

1. Call designer once; write output to `<stateDir>/<sessionID>.workflow.plan.md`.
2. Load the sprint list from the plan file (number of sprints = count of `## Sprint N` headings; default 1 if none found).
3. For each sprint:
   a. Call generator with sprint context; write output to `<stateDir>/<sessionID>.workflow.sprint<N>.md`.
   b. Call evaluator with sprint output; extract verdict via `ParseVerdict`.
   c. Save state after each evaluator call.
   d. If `needs_refinement` and `pass < MaxPasses`: increment pass, go to (a).
   e. If `needs_refinement` and `pass >= MaxPasses`: halt with error.
   f. If `next_sprint`: advance sprint, reset pass.
   g. If `good_to_go` on final sprint: return nil (success).

Prompts for each role are constructed in `internal/workflow/dev/prompts.go`. Each prompt function takes the plan file path, the findings file path (if any), and the current sprint/pass context.

### Step 3 — Agent resolution (`internal/workflow/resolve.go`)

```go
// ResolveAgents maps role specifiers to TurnRunner instances.
// Aliases "primary" and "escalation" are expanded using the session's current agent assignment.
func ResolveAgents(roles map[string]string, cfg *config.Config, sess *session.Session) (map[string]runner.TurnRunner, error)
```

This is called once at `/workflow` invocation time. The returned map is stored in `RunConfig.Runners` and in `State.AgentMap` (names only, for persistence). On resume, `ResolveAgents` is called again with the persisted names (aliases already resolved, so resume is stable even if the user changes their primary/escalation assignment between restarts).

### Step 4 — `/workflow` slash command and wizard (`cmd/milk/interactive.go`)

Add `"workflow"` to the slash command dispatch table in `handleSlashCommand`.

`handleWorkflow(args []string, m *model) tea.Cmd`:
- No args → list available workflows (currently just `dev`).
- First arg is workflow name → check if remaining args satisfy the wizard; if not, start the interactive wizard.
- Wizard state is stored in `m.workflowWizard *WorkflowWizard`. The wizard sends `WorkflowWizardMsg` messages back via `p.Send()` to request the next input field, and the TUI renders the current question in place of the normal prompt label.
- On wizard completion, resolve agents via `ResolveAgents`, construct `RunConfig`, and launch `workflow.Run` in a goroutine. Progress messages are sent via `p.Send(WorkflowProgressMsg{...})`.

Flag parsing for `--designer`, `--generator`, `--evaluator`: use a simple flag scan over `args` before the wizard runs, so inline invocations bypass the wizard entirely.

### Step 5 — TUI progress panel (`cmd/milk/panel_workflow.go`)

Create `cmd/milk/panel_workflow.go` following the pattern of `panel_memory.go`.

New message types (add to `cmd/milk/repl.go`):
```go
type WorkflowProgressMsg struct {
    State workflow.State
}
type WorkflowDoneMsg struct {
    Err error
}
```

The panel renders as a collapsible block at the bottom of the transcript area (above the status bar), showing:
```
[dev workflow] Sprint 2/3  Pass 1  Role: generator
  ✓ sprint 1 pass 1 → good_to_go
  ✓ sprint 2 pass 1 → needs_refinement
  → sprint 2 pass 2 → running…
```

Toggle: `/panel workflow`. The panel is hidden by default (unlike the memory panel); it becomes visible automatically when a workflow starts.

Add `workflowPanelOpen bool` and `workflowState *workflow.State` to the bubbletea `model` struct. Update `Update` to handle `WorkflowProgressMsg` and `WorkflowDoneMsg`. Update `View` to include the panel when open.

### Step 6 — Tests

`internal/workflow/verdict_test.go`: table-driven tests for `ParseVerdict` covering all three verdict strings, mixed-case, surrounding prose, and the unknown fallback.

`internal/workflow/dev/dev_test.go`: unit tests for the designer/generator/evaluator prompt constructors (check required fields appear in output without calling any agent).

`internal/workflow/resolve_test.go`: unit tests for `ResolveAgents` covering alias expansion and unknown-name error.

`internal/workflow/turn_test.go`: test `Turn` with a mock `TurnRunner` that returns a fixed response; verify the response is returned and the send callback is called.

`cmd/milk/workflow_integration_test.go` (optional, follow `evalCaptureServer` pattern): drive a full `/workflow dev` invocation with mock HTTP servers for each role; verify the state file is written and the `WorkflowDoneMsg` is sent.

---

## Commit sequence

```
feat(workflow): add workflow engine core — State, Turn, ParseVerdict         ← Step 1
feat(workflow): add dev workflow with designer/generator/evaluator loop      ← Step 2
feat(workflow): add agent resolution with alias expansion                    ← Step 3
feat(workflow/tui): add /workflow slash command and interactive wizard       ← Step 4
feat(workflow/tui): add workflow progress panel                              ← Step 5
test(workflow): add unit and integration tests for engine and TUI            ← Step 6
```
