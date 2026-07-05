# milk — active plan

## MCP + ORAS/OCI integration (session 2026-07-05)

### ✅ Task 1 — MCP config schema
`internal/config/config.go`: `MCPServerConfig`, `MCPServers` top-level array, per-agent `MCPServers` opt-in list, validation, `EffectiveMCPServers` resolver.

### ✅ Task 2 — MCP runtime client + tool loop integration
- `internal/mcp/mcp.go`: minimal MCP 2025-03-26 client (Streamable HTTP, JSON-RPC 2.0, initialize, tools/list pagination, tools/call, SSE, bearer/token_cmd auth)
- `internal/mcp/toolset.go`: `ToolSet` aggregates multiple clients, routes by `mcp_<name>_` prefix
- `internal/agent/local/local.go`: `mcpToolSet` interface (avoids import cycle), `WithMCPToolSet`, MCP schemas appended, MCP dispatch before built-ins
- `cmd/milk/main.go`: `attachMCPToolSet` wires ToolSet into primary and escalation runners at startup

### ✅ Task 3 — MCP TUI commands
`cmd/milk/interactive.go`: `/mcp list|add|remove|enable|disable|tools|assign|unassign`

### ⬜ Task 4 — ORAS/OCI: registry client + artifact pull
- New `internal/oci/` package
- Config: `OCIRegistryConfig` struct, `OCIRegistries []OCIRegistryConfig` in `Config`
- OCI Distribution Spec v1 pull: manifest → layers → blobs
- Vendor media types: `application/vnd.posteitaliane.agent.<type>.v1+json`
- Artifact types from schemas: `prompt`, `tool`, `personality`, `skill`, `bundle`, `project`
- Local disk cache: `~/.milk/oci/<registry>/<repo>/<digest>/`
- Auth: anonymous, bearer (token_cmd or api_key), basic

### ⬜ Task 5 — ORAS/OCI: artifact injection into agent context
- Pulled `prompt` artifacts → prepended system message
- Pulled `tool` artifacts with `interface.type == "mcp"` → synthesise an MCPServerConfig and add to runtime ToolSet
- Pulled `personality` artifacts → appended behavioral instructions
- Wire into `buildPrimaryRunner` / `buildEscalationRunner` (after MCP tool set)

### ⬜ Task 6 — ORAS/OCI: TUI commands
`/oci pull|list|inspect|clear` in `cmd/milk/interactive.go`

### 🔖 Tracked issues
- [#73](https://github.com/scoutme/milk/issues/73) — Persistent task/plan tracking in milk memory
- Task #7 (session) — Migrate MCP client to official `github.com/modelcontextprotocol/go-sdk`

---

# milk — closed issue archive

> **Deprecated as a backlog.** Open items are now tracked as GitHub Issues.
> This file is kept as an archive of closed/done items for historical reference.

---

# milk — feature backlog

## Remote oversight interface (Telegram or similar)

Allow the user to follow agent work and approve permission prompts from a mobile device when not at the PC.

- Configurable notification backend (Telegram bot as first target; design for extensibility to other transports)
- Push notifications for agent turns: routing decision, model used, tool calls, escalations
- Permission prompt forwarding: when Claude requests a permission, send it to the remote interface and await approval/denial before unblocking the subprocess
- Bidirectional: user can also send a prompt remotely to inject into the next turn
- Timeout/fallback behavior when remote approval doesn't arrive within a configurable window (auto-approve, auto-deny, or pause)
- Config keys under `remote_oversight` in `~/.milk/config.json`

## Reasoning visibility control

Keep chain-of-thought / thinking tokens separated from conversation history, with user control over display.

- Reasoning content stored separately from the regular message history (never mixed into the transcript context sent to the next turn)
- Per-session toggle: show or hide reasoning blocks (`/think on` / `/think off`)
- Retroactive: toggling applies to all past turns in the current view, not just future ones
- When hidden: show a collapsible placeholder (e.g. `[thinking…]`) that the user can expand inline
- Persisted preference in session state; default configurable in `~/.milk/config.json`
- Applies to both local model `<think>` blocks and Claude extended thinking tokens

## Local Inference Automation (llama.cpp)

Analyze the possibility to automate the llama.cpp process launch (or similar solution) in order to grant local model inference on milk start. The launch should be configurable via milk configuration, and commands and tools must be implemented to interact with llama.cpp for model switching. Keep evolution in mind, since llama.cpp is just an option: in the future we'll add support for remote inference or other inference server, keeping functionalities intact.

## ~~Input area bug~~ DONE

when typing multiline content, sometimes text not fitting in current line disappears, to appear then only when it's long enough to be seen in subsequent line. This doesn't happen between first and second line

## Code linting

Add code colorization

## ~~Check memory decay~~ DONE

I didn't see a single percept decaying

## ~~Move notification into status bar~~ DONE

When trying to submit input during agent response, "[milk] agent is responding..." is added to chat view. Show it in status bar instead

## ~~Slash commands and @files colorization broken when using multiline input~~ DONE

Works only in single line

## ~~Permissions~~ DONE

Claude keeps asking the same permissions, as if milk isn't saving them into claude settings

## ~~Selection hint too present~~ DONE

Selection hint should be visible only when selection is started, that is when mouse as been moved at least a bit after press, before release

## ~~Possible permission issue~~ DONE

Claude is asking many times between different turns the same permission requests, sa if milk isn't updating correctly its configurations

## ~~Input Navigation vs History~~ DONE

When in input area, if up arrow is pressed while at the beginning of the first line, history should be navigated

## ~~Prompt label refactor~~ DONE

The input prompt currently shows the next-turn agent (e.g. `[local]`, `[claude]`). Since the header bar and status bar already carry full agent/mode info, the prompt label could be simplified to just `>` or a very short marker. Evaluate whether removing the agent name from the prompt reduces clutter without losing context.

## ~~Input area: lines beyond the first not visible~~ DONE

When typing a long enough input to overflow the first textarea line, subsequent lines are not visible — only the first line is shown in the input area. The text is being buffered (it can be submitted), but the visual display doesn't grow to show continuation lines.

## ~~Ctrl+Z undo paste~~ DONE

When the user pastes content (via Ctrl+V or right-click) and wants to undo it, Ctrl+Z should revert the textarea to its previous state. Currently there is no undo history in the input area.

## ~~Memory tuning~~ DONE

Nothing decays, all becomes global

## ~~Dangerous permission skip via command~~ DONE

A command should enable permission management mode switching

## Agent generalization

The "local" slot assumes a locally-running inference server but already supports any OpenAI-compatible endpoint — including remote ones. The "claude" slot is hardwired to the Claude Code CLI subprocess. Both slots should become configurable agent types:

- Rename "local" → "agent A" (or a user-configured name); support any OpenAI-compatible endpoint, local or remote
- Make "claude" replaceable with a second OpenAI-compatible agent (plain API, no CLI subprocess required), keeping the Claude Code CLI path as one possible backend
- Routing labels, prompt labels, session state names, config keys, and UI copy should reflect the configured agent names rather than hardcoded "local"/"claude"
- Escalation and sticky-mode mechanics should be backend-agnostic
