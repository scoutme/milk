# ADR-0104: Agent Plugins — package format, loading, and distribution

- **Status:** Accepted
- **Date:** 2026-11-08

## Context

milk has MCP servers (config-declared) and no skills. The [Agent Plugins
Specification v1.0.0](https://agent-plugins.org/specification) standardizes
directory-based plugin packages — `plugin.json` manifest, `skills/*/SKILL.md`
(Agent Skills), `mcp.json` MCP servers — that install by copying a directory
and work across conformant clients. Adopting it gives milk a real plugin
ecosystem for free and finally lands the skills half of the Phase-5 parking
lot (#252).

The **distribution** half has no standard: the spec states explicitly that it
"does not prescribe: installation sources, registries, or marketplaces" —
installation is client-owned. The ecosystem answers that freedom with one
de-facto shape (evidence: the MiniCut plugin ships five parallel layouts,
one per harness):

- **Claude Code**: `.claude-plugin/marketplace.json` catalogs plugins with
  per-entry `source` refs (relative path, `github`, `git-subdir`, `url`,
  `archive` zip, `npm`, `command`); UX is `marketplace add` + install by
  `name@marketplace`.
- **Gemini CLI**: `gemini-extension.json` bundles (prompts, MCP, commands,
  themes, hooks, sub-agents, skills); UX is install-by-GitHub-URL plus a
  curated gallery.
- **OpenAI Codex / Cursor**: per-client manifests (`<.harness>-plugin/plugin.json`)
  with Claude-shaped fields; repo-based install.
- **DeepSeek Harness**: plugin-as-code (an npm module patching the harness
  event framework) — the outlier.
- **MCP-first harnesses**: plugins ≈ MCP server + instructions; the MCP
  Registry `server.json` (npm naming `io.github.<owner>/<repo>`) is their
  standard.

Convergent patterns: git repo (or zip/npm tarball) as the distribution unit,
install by URL/slug, a machine-readable catalog paired with `add`/`install`
commands, and nobody makes the user run git themselves.

## Decision

milk is a **conformant Agent Plugins v1 client** (`internal/plugin`) and adds
a **client-owned distribution layer** that reuses the ecosystem's de-facto
catalog interchange. The theme is one pipeline: *catalog → fetch → shim if
needed → load*.

### Part I — Loading & conformance (the portable core)

Plugins load from two roots at startup:

- **user:** `<state root>/plugins/` (~/.milk/plugins)
- **project:** `<cwd>/.milk/plugins/` (repo-local, like the rest of `.milk`)

`PLUGIN_DATA` (spec §9.1) is `<state root>/plugin-data/<plugin name>`,
created when a stdio server launches and preserved across updates.

| Spec | milk |
| --- | --- |
| manifest validation (§5, closed schema) | `internal/plugin/manifest.go` — unknown fields report+ignore, every other violation rejects the plugin |
| package containment (§4.1) | `paths.go` — `./`-relative paths resolved and symlink-checked against the plugin root, narrowest failure boundary ladder |
| skills (§7.1 → Agent Skills spec) | `skills.go` — `skills/*/SKILL.md`, immediate children only, frontmatter validated (name/description constraints), invalid skills skipped with a report |
| MCP `mcp.json` (§7.2) | `mcp.go` — closed per-entry union (stdio / streamable-http / sse), single-token `command`, cwd forms, reserved env names |
| placeholder expansion (§9.2) | `expand.go` — single-pass `${PLUGIN_ROOT}`/`${PLUGIN_DATA}` in args/env-values/cwd only, replacement text never rescanned |
| reserved env (§9.1) | `internal/mcp.env` — `PLUGIN_ROOT`/`PLUGIN_DATA` set after the configured env overlay, replacing same-name entries |
| failure isolation (§11.3) | loader boundaries: manifest fatal rejects the plugin; a broken component location disables only that component type; a broken skill/server entry skips only that entry |
| unsupported transports (§11.3 rule 4) | legacy `sse` servers are skipped with a report (milk connects Streamable HTTP); `streamable-http` maps to milk's `http` transport |

**Client extension namespace** — `dev.milkai.milk` (the domain milk controls):

```json
"extensions": { "dev.milkai.milk": { "mcp_assign": ["primary"], "disable_skills": true } }
```

- `mcp_assign`: agent names that receive the plugin's MCP servers (default:
  every agent). Names resolve like `AgentConfig.MCPServers` refs.
- `disable_skills`: don't surface this plugin's skills in the `skill` tool.

**How components reach milk:**

- **MCP servers** go through a **runtime seam**, `config.SetPluginMCPServers` —
  `EffectiveMCPServers` appends plugin servers after the agent's own refs.
  They are *never persisted*: config writes would otherwise fossilize
  plugin-derived entries into `config.json`. Names are namespaced
  `plugin.<plugin>.<server>` (collision-proof).
- **Skills** feed a `skill` tool (`internal/agent/local/skill.go`) using
  progressive disclosure exactly as the Agent Skills spec prescribes: the tool
  description carries **name + description lines only**; `skill(name)` returns
  the SKILL.md body (re-read from disk at activation, 64 KB cap). The tool is
  omitted entirely when no skills are installed (zero token cost), and it is
  a base tool, so `included_tools`/`excluded_tools` apply.
- Stdio servers run with `cwd` (default: plugin root) and the configured env
  overlay plus the reserved variables — through milk's own MCP client. The
  claude-cli/smolagent native-MCP paths pass command/args/env to their config
  generators; `cwd` is honored by milk's client only.

### Part II — Distribution: familiar catalogs, no git required

**Interchange format.** milk reads the ecosystem's de-facto marketplace
manifest — Claude Code's **`marketplace.json`** — from
`.claude-plugin/marketplace.json`, falling back to a root `marketplace.json`
(the portable spelling). This is conformant by the spec's own division of
responsibility (installation is client-owned) and maximally familiar: the
catalogs authors already publish are the catalogs milk consumes. Marketplace
*hosting* stays outside milk (push the catalog repo anywhere).

**Install UX.** Same gestures as the ecosystem, one new default:

```
milk plugins add <owner/repo | git-url | https://…/pkg.zip | npm:pkg>
milk plugins marketplace add <owner/repo | path>    # register a catalog
milk plugins install <name>[@<marketplace>]
milk plugins list · details <name> · update [name] · remove <name>
milk plugins shim <dir>                             # write portable manifests
```

- `add <slug>` is the familiar one-liner (Gemini's UX): treats the target as
  an implicit single-plugin marketplace — fetch, shim if needed, install.
- Install ids are `name@marketplace` (Claude's convention); bare `name`
  resolves when unambiguous.

**No-clone fetching (milk's one deliberate divergence).** Distribution units
are fetched over plain HTTPS — a git binary is never invoked and no history
is downloaded. Per `marketplace.json` source type:

| source | fetch |
| --- | --- |
| relative path (`"./"`, `./plugins/x`) | one download of the marketplace snapshot, extract the subtree |
| `github` (`owner/repo`, optional `ref`/`sha`) | GitHub tarball/codeload archive endpoint, pinned ref honored |
| `git-subdir` (`url` + `path`) | archive endpoint of that repo, extract `path/` only |
| `url` (any git URL) | host's archive endpoint where one exists; otherwise rejected with a named error |
| `archive` (zip over HTTPS) | straight download + extraction filter |
| `npm` | registry tarball |
| `command` | **rejected** (executes arbitrary code at install time; revisit only with a consent story) |

An **extraction filter** keeps only what a package needs —
`plugin.json`, `mcp.json`, `skills/**`, `mcp/**`, `scripts/**`, `LICENSE`
plus files a SKILL.md body references by relative path — and reports
everything skipped. (MiniCut: 34 archive files → 8 kept.)

**Shim generation (compatibility bridge).** When an install target lacks a
portable root `plugin.json`, `shim` synthesizes it (and `mcp.json` from
Claude-form `mcpServers`/`mcp/server.json`), mapping name/description/version
through. `add`/`install` do this automatically and record it in the install
bookkeeping; `milk plugins shim <dir>` does it in place for hand-managed
trees. The shim only ever *creates missing manifests* — it never overwrites
portable files.

**Bookkeeping & lifecycle.** Installs are recorded in
`<state root>/plugin-installs.json` per plugin: marketplace id, source spec,
resolved ref/sha, installed version, shim flag. Installed trees live in the
user plugin root (`~/.milk/plugins/<name>`); they are **copies** — removing a
marketplace never removes its plugins. `update` re-resolves the source (honoring
`ref`/`sha` pins), replaces the tree atomically (tmp + rename) and preserves
`PLUGIN_DATA`; `remove` deletes the tree and the record. Project-level
`.milk/plugins/` stays drop-in-only (no bookkeeping) — that is the hand-made
path, and `git clone` there remains fully supported.

**Trust posture.** Installing is trust-by-user (the same trust as editing
`config.json`): stdio MCP servers execute local commands, so every install is
user-initiated and inspectable — no background fetching, no auto-update.
`/plugins` surfaces what a plugin brings (skills, servers, extension keys)
and every loader report; a future ADR-0052-style consent gate is the recorded
escape hatch if plugins ever arrive through unattended paths.

## Consequences

- Three install paths, one loader: **drop a directory** (hand-made, project
  or user), **`shim` a tree**, or **`add`/`install` from a catalog** — all
  converge on a conformant plugin directory, so Part I never changes.
- No git dependency anywhere in the install path; the only network verbs are
  HTTPS GETs to archive/registry endpoints, honoring `ref`/`sha` pins.
- milk consumes Claude-format marketplaces without speaking Claude: the
  format is an interchange, the runtime is 100% Agent Plugins. If a neutral
  marketplace format emerges (the spec TSC spans Amazon, Cursor, Microsoft,
  OpenAI, Vercel), accepting it is a new fetcher, not a redesign.
- Plugins load at startup; editing a plugin's `mcp.json`/manifest takes
  effect on restart (skill bodies, conversely, are re-read per activation).
  Distribution commands (`add`, `install`, `update`, `remove`) mutate the
  plugin roots and are effective on the next restart.
- milk claims conformance for **both** component types with **stdio +
  streamable-http** MCP transports; `sse` is the only declared-but-unsupported
  piece, skipped per spec §11.3 rule 4.
- Out of scope, deliberately: marketplace hosting/publishing, auto-update
  policy, `command` sources, hooks/commands/themes beyond skills + MCP, and
  extension namespaces other than `dev.milkai.milk` — each gets its own ADR
  if ever pulled.
