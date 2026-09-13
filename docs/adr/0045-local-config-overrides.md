# 45. Local Project-Level Config Overrides (.milk/)

- **Status:** accepted
- **Date:** 2026-09-15
- **Updated:** 2026-09-25 — aligned with implementation (agents merge-by-name, init --local full wizard)

## Context

milk currently stores its entire configuration in a single global file
(`~/.milk/config.json`). Every `milk config` CLI subcommand, every `/config`
TUI command, and the auto-reload watcher all read from and write to that one
path. This works for single-project users but creates friction in several
situations:

1. **Per-project agent tuning.** A user working on a large monorepo may want
   higher `local_context_budget_chars` or a different `context_window_tokens`
   than when working on a small script. Today they must edit the global config
   and remember to revert it.

2. **Per-project MCP servers.** A project may have its own MCP server
   (e.g. a project-specific documentation indexer or a database schema tool)
   that should not pollute the global config seen by other projects.

3. **Per-project escalation policy.** Some projects may warrant always
   escalating (complex architecture), while quick utility repos should stay
   local. The routing rules and `default_route` are global today.

4. **Team-shared defaults.** A team may want to commit a baseline milk config
   to the repo (e.g. recommended agent settings, MCP servers for the project's
   infra) alongside `.editorconfig` or `.claude/settings.json`. The global
   `~/.milk/config.json` is inherently per-user and cannot be committed.

### How other harnesses handle this

| Tool | Local config | Merge strategy | Prompt for scope |
|---|---|---|---|
| **Claude Code** | `.claude/settings.json` (project) vs `~/.claude/settings.json` (user) | Project overrides user, field by field | No — settings commands target the active scope based on CWD |
| **Cursor** | `.cursor/settings.json` in project root | Merges with global, project wins | No — implicit from file location |
| **Aider** | `.aider.conf.yml` in project root + `~/.aider.conf.yml` | Project overrides global, same keys | No — precedence is implicit |
| **GitHub Copilot** | `.github/copilot-instructions.md` | Appended to system prompt | N/A (instructions, not config) |
| **Windsurf / Codeium** | `.windsurfrules` / `.codeiumignore` in project root | Project appended to global | No — implicit |

The universal pattern is: **a dotfile/dot-directory in the project root
overrides the global equivalent, field by field, with no interactive prompt
about scope.** The user controls scope by choosing where to put the file.

milk should follow this convention but go one step further: since milk's
`config` commands write to config programmatically (unlike tools that expect
the user to hand-edit), it should **ask** when a local config exists, so the
user doesn't accidentally write a project-specific change to their global
config or vice versa.

## Decision

### 1. Local config path: `.milk/config.json`

When milk starts (or any config load occurs), it checks for
`<cwd>/.milk/config.json` in addition to `~/.milk/config.json`. Both are
optional; if neither exists, defaults are used (existing behaviour). If only
one exists, it is loaded as today. If both exist, they are **deep-merged**:

- **Local (`<cwd>/.milk/config.json`) takes priority** over global for every
  field that is explicitly set (non-zero, non-nil).
- Fields absent from local fall through to global.
- Arrays (`agents`, `mcp_servers`, `agent_tools`) use **replace semantics**:
  if the local config defines `agents`, it replaces the global `agents` list
  entirely (same as Claude Code's project-settings model). This avoids
  confusing merge-by-name semantics for complex nested objects. Users who want
  to extend rather than replace can copy the global entry into local and edit.
  - Exception: `mcp_servers` uses **merge-by-name** — a local entry whose
    `name` matches a global entry replaces it; new local names are appended.
    This is the natural expectation: a project adds its own MCP servers
    without losing the user's global ones.
- Scalar fields, nested objects (`rules`, `otel`, `loop_detection`), and
  pointer fields (`show_reasoning`, `sticky_escalation`) are deep-merged:
  local overrides only the fields it explicitly sets.

The merge function lives in `internal/config` as `MergeConfigs(global, local Config) Config`.

### 2. New `LoadWithLocal()` and `SaveWithScope()` functions

```go
// LoadWithLocal loads and merges global + local config.
// localPath is "" when no .milk/config.json exists in cwd.
func LoadWithLocal(cwd string) (Config, string /*localPath*/, error)

// SaveWithScope writes cfg to the appropriate target.
// scope == "local"  → <cwd>/.milk/config.json
// scope == "global" → ~/.milk/config.json
func SaveWithScope(cfg Config, scope string) error
```

The existing `Load()` and `Save()` remain unchanged for backward
compatibility. `Load()` is modified internally to call `LoadWithLocal` with the
current working directory so that the **running config always reflects the
merge** — no call-site changes needed for readers.

### 3. Dual-file watcher

The existing `config.Watcher` is extended to accept a second, optional path
(the local config). When either file changes, `onChange` is called with the
re-merged config:

```go
func NewDualWatcher(globalPath, localPath string, onChange func(Config, error)) (*Watcher, error)
```

`localPath` may be empty (no `.milk/config.json`). The existing
`NewWatcher(path, onChange)` remains for callers that only care about the
global file (e.g. tests). The TUI's `repl.go` switches to `NewDualWatcher`.

### 4. `milk config` commands: scope prompt

When a `.milk/config.json` exists in cwd, commands that modify config (`milk
config mcp add`, `milk config agent add`, `milk config open`, and the TUI's
`/config` commands) print:

```
Apply to local project (.milk/config.json)? [Y/n]
```

Default is **Yes** (local) when a local config already exists, **No** (global)
when no local config exists yet. If the user answers "n" (or there is no local
config), the change goes to `~/.milk/config.json` as today. If "Y" (or
Enter), the change goes to `<cwd>/.milk/config.json`, creating the `.milk/`
directory if needed.

A `--local` / `--global` flag on the CLI subcommands bypasses the prompt for
scripting:

```bash
milk config mcp add name=foo url=http://... --local
milk config mcp add name=foo url=http://... --global
```

### 4a. Creating a local config file

When no `.milk/config.json` exists yet, the user needs a way to bootstrap one.
Several entry points:

- **`milk config init --local`** creates a minimal `.milk/config.json` containing
  only `{}` (empty object — all fields inherit from global). The user can then
  run further `milk config` commands to populate it, or open it in their editor.
  The `.milk/` directory is created if needed.

- **Any `milk config` command with the scope prompt** (Decision #4): when the
  user answers "Y" (local) but no local config exists yet, milk creates
  `.milk/config.json` with the **single changed field** — not a copy of the
  entire global config. This keeps the local file minimal and intentional.

- **Manual creation**: the user creates `.milk/config.json` by hand (an empty
  `{}` is valid). milk detects it on next load/merge. This is the expected path
  for teams committing a project config to the repo.

`milk config init` (no flag) continues to target the global config. If a local
config already exists, it asks whether to reinitialise local or global (existing
Decision #5 behaviour).

### 5. `milk config init` respects local

If `.milk/config.json` already exists when `milk config init` runs, the wizard
asks whether to overwrite the local config or create/update the global one.

### 6. `.milk/` in `.gitignore` guidance

The docs recommend adding `.milk/config.json` to `.gitignore` when it
contains secrets (API keys) and committing it when it contains only
team-shared settings (agent URLs, MCP servers, routing rules). The default
scaffolding does **not** auto-add `.milk/` to `.gitignore` — that is the
team's decision, same as `.claude/settings.json`.

### 7. `MILK_CONFIG` environment variable

When `MILK_CONFIG` is set (existing behaviour), it overrides the global path
but does **not** disable local config — local is always `<cwd>/.milk/config.json`.
This matches the mental model: `MILK_CONFIG` replaces *where the global file
is*, not *whether local overrides exist*.

### 8. `config open` command

`milk config open` opens the **local** config file directly in the editor when
one exists. When no local config exists, it opens the global file (current
behaviour). When a local config is present it prints a note:

```
Editing local config (.milk/config.json).
Fields not set here are inherited from global (~/.milk/config.json).
Use --global to edit the global file instead.
```

**Why not a temporary merged file?**

The previous proposal opened a temp file containing the merged JSON and wrote
the result back to local on save. This was rejected for several reasons:

1. **VS Code `--wait` semantics.** `code --wait` blocks until the tab is
   *closed*, not when the file is *saved*. Intermediate saves go to the temp
   file and are invisible to milk's watcher. Only on tab close does the
   editor-exit callback fire. Users expect saves to take effect immediately.

2. **Stale snapshot.** The merged content is a point-in-time snapshot. If the
   global config is edited externally while the temp file is open, stale global
   values get "frozen" into the local config on save.

3. **Back-write pollution.** Saving the merged content to local writes *every*
   field — including those inherited from global. The local file bloats to a
   full copy of the config, and subsequent merges become meaningless (local has
   everything, global is ignored).

4. **Temp file lifecycle.** The temp file needs cleanup (crash-safe), collision
   handling (two concurrent `config open` calls), and a location — none of
   which are specified. The existing watcher infrastructure watches real files,
   not temp files.

**How the direct-edit approach works:**

- The editor opens `.milk/config.json` (or `~/.milk/config.json` with
  `--global`).
- The watcher (Decision #3) detects any save to the opened file within ≤200ms,
  re-merges, and hot-reloads. Saves take effect while the file is still open.
- The user edits only the fields they want to override — the file stays
  minimal. Unset fields continue to inherit from global.
- This matches the editing model of Claude Code (`.claude/settings.json` is
  edited directly, not as a merged view).
- The editor-exit callback (`tea.ExecProcess`) still fires on editor close and
  triggers a final re-merge, as a safety net.

## Consequences

**Positive:**
- Per-project configuration is a common expectation (Claude Code, Cursor,
  Aider all support it). Closing this gap removes friction for multi-project
  users.
- Deep-merge with local priority is the standard pattern — no novel semantics.
- The dual-watcher extends the existing polling infrastructure rather than
  replacing it.
- The scope prompt prevents accidental cross-project config pollution, which
  is the one improvement over tools that use implicit precedence.
- `config open` edits the local file directly, keeping save semantics identical
  to the current implementation — saves are detected by the watcher within
  200ms, no temp-file lifecycle management needed.
- Multiple bootstrap paths for local config (`init --local`, scope prompt
  answer, manual creation) cover all user workflows.

**Negative:**
- Deep merge adds complexity to the config loading path. The merge function
  must handle every field type correctly (pointers, slices, nested structs).
  Comprehensive unit tests are essential.
- Two config files to reason about in bug reports ("which config am I
  actually using?"). A `milk config show --sources` command that annotates
  each field with its origin (global / local / default) mitigates this.
- Array replace semantics for `agents` may surprise users who expect
  extension. The documentation must be explicit about this.
- `config open` shows only the local file, not the merged result. Users must
  understand inheritance ("fields not set here are inherited from global").
  `milk config show` with the merged view mitigates this.

**Neutral:**
- `~/.milk/config.json` is unchanged for users who don't create a local
  config. Zero migration, zero breakage.
- The `MILK_CONFIG` env var continues to work as before.
