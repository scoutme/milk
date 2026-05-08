# ADR-0012: Claude tool and directory permission handling

Date: 2026-05-08

## Status
Accepted

## Context

Claude Code CLI gates tool use (Bash, Read, Write, Edit, etc.) and directory access behind interactive approval prompts. In pipe-only mode (ADR-0011) these prompts never reach the user — Claude either silently skips the tool or emits a natural-language explanation that it cannot proceed without approval.

Three approaches were considered:

1. **Always use `--dangerously-skip-permissions`**: Claude auto-approves all tool and directory requests. Simple, but gives up all access control — Claude can read/write/execute anything reachable from its working directory.

2. **Proactive allow-list via `--allowedTools` and `--add-dir`**: Specific tools and directories are pre-approved in config and passed on every invocation. Claude never stops to ask for listed items.

3. **Reactive detection and retry**: Parse Claude's text output for natural-language permission-request phrases. If detected, ask the user y/n, then re-run the session via `--resume` with the newly approved tool or directory added.

## Decision

Implement both proactive (2) and reactive (3) together. `--dangerously-skip-permissions` remains available as a config escape hatch but defaults to off.

### Proactive allow-list

`allowed_tools` and `add_dirs` are config fields (default: empty — all tools/dirs require permission). Values are passed as `--allowedTools <comma-list>` and `--add-dir <path>` on every Claude invocation. The tool list comes entirely from config; no tool names are hardcoded in milk.

### Reactive detection

`Stream()` scans the final assembled text for a set of known permission-request phrases (`"need permission"`, `"is restricted"`, `"access to"`, etc.). Two distinct signals are detected:

- `PermissionDenied` + `DeniedTool`: tool approval needed. Tool name is matched against `allowed_tools` config values appearing in the text.
- `DirRestricted`: directory access refused.

On detection, `runClaude` asks the user interactively:
- Tool: `y/n` prompt; on approval, `WithExtraTools(tool)` clones the agent with the tool appended to the allowed list and retries via `--resume`.
- Directory: free-text path prompt; on input, `WithExtraDirs(dir)` clones the agent and retries via `--resume` with the granted path in the resume message so Claude operates on the correct target.

### Why not reactive-only

Reactive detection is inherently fragile — Claude's phrasing can vary across versions. The proactive allow-list handles the common case (known safe tools) with zero latency and zero false-negative risk. Reactive covers the remainder without requiring the user to pre-enumerate every tool.

### Why not proactive-only

Without reactive detection, any tool or directory not in the config list silently fails — Claude explains the restriction in plain text and the user has no path to grant access without editing config and re-running.

## Consequences

- `allowed_tools` and `add_dirs` are per-machine config, not checked into the project.
- Permission-phrase detection can produce false positives (e.g. Claude discussing permissions conceptually). A false positive triggers a benign y/n prompt; the user can decline.
- The retry sends a fixed resume message with the granted resource; Claude may occasionally re-explain context before continuing.
- `WithExtraTools` / `WithExtraDirs` grant permission for the current turn's retry only — they are not persisted to config or session state.
