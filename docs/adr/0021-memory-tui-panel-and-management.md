# 21. Memory TUI Panel and Interactive Management Commands

Date: 2026-05-18

## Status

Accepted

## Context

The memory system (ADR-0016) provided a persistent Percept store and agent tools (`record_memory`, `get_memory`, `list_memory`), but offered no at-a-glance visibility into memory state during a session. Users could run `/memory` to list Percepts, but had no continuous view of what the system was remembering. Deleting a specific Percept required knowing its full UUID — there was no fuzzy search or confirmation flow. The full details of a Percept (producer, roles, timestamps, core flag) were not accessible from the TUI.

These gaps made memory feel opaque and hard to manage interactively.

## Decision

**Memory panel (`cmd/milk/panel_memory.go`).** A 34-column right-side TUI panel is added to the bubbletea layout. It is open by default and toggled with `/panel memory`. The panel polls the store every 5 seconds and renders three sections in order: SESSION, GLOBAL, GLOBAL (core). Each Percept entry shows:
- A short ID (`#<first-6-hex-chars-of-UUID>`, dim) on the first line
- Content wrapped to at most 2 lines
- Weight right-aligned on the first line
- Bold+yellow highlight for Percepts updated within the last 60 seconds

**Percept short IDs.** The first 6 hex characters of a Percept's UUID serve as a human-readable short ID displayed in the panel and usable in management commands. `Store.FindByIDPrefix(prefix string) []Percept` resolves a short ID to the matching Percept(s).

**`/forget <pattern or #id>`.** Fuzzy deletion command. Accepts either a content substring or an exact `#<6hex>` prefix. On a single match, asks for confirmation (`y` to confirm). On multiple matches, displays a numbered list with short IDs and prompts for a position number or `#id`. On confirmation, calls `Store.Delete(id)`. `Store.Delete(id string) (bool, error)` is the new deletion primitive on the store.

**`/memory show <pattern or #id>`.** Displays the full details of a matching Percept: ID, scope, W, producer, core flag, content, created/updated timestamps, and semantic roles. Implemented using `FormatListVerbose`.

## Consequences

Users can inspect the live memory state without leaving the TUI. Recent changes are visually distinguished, making it easy to see what the agent just recorded. Short IDs make Percepts referenceable across commands without copying UUIDs.

The panel costs approximately 34 terminal columns. On narrow terminals this reduces the transcript viewport width. Users can reclaim the space with `/panel memory`.

`Store.Delete` and `Store.FindByIDPrefix` extend the Store API and are available to any future caller (e.g., a future `/unlearn` command or MCP server integration).
