# 5. Session Storage as JSON Files Indexed by cwd

Date: 2026-05-05

## Status

Accepted

## Context

Sessions need to persist across invocations, support multiple concurrent sessions per directory, and be resumable by name or most-recent lookup.

## Decision

Store sessions as individual UUID-named JSON files under `~/.milk/sessions/`. A separate `index.json` maps cwd paths to ordered session metadata `[{id, name, last_used}]`, avoiding directory scans on startup.

## Consequences

The index file is the only file read on startup, keeping lookup O(1) regardless of total session count. JSON files are human-readable and easily inspected or backed up. The index can drift from session files if files are manually deleted; mitigated by an index repair step on startup (check that referenced UUIDs exist). SQLite would be more query-efficient but adds a dependency and makes session state harder to inspect.
