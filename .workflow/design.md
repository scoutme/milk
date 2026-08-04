# Refactor Design

## Sprint 1: Remove CWD injection from Run()

**Scope:** Delete the `cwdContext` call and the `cachedCwd`/`cachedCwdContext` fields from `internal/agent/local/local.go`. The CWD is already in the system prompt via `buildSystemPrompt`; the live `list_dir` output appended on every turn wastes tokens and busts the prompt cache. Remove the two struct fields, the cache-invalidation block in the tool-result path, and the `cwdContext` helper function.

**Done when:** `go build ./...` succeeds; `go test ./internal/agent/local/...` passes; the phrase `cachedCwdContext` no longer appears anywhere in the codebase; and the `cwdContext` function is gone.

**Testing strategy:** Run `go test ./internal/agent/local/... -v` and confirm no regressions. Manually start milk TUI, open a session, and verify the first assistant turn does not include a `Working directory listing` block in the OTel log (`debug_local: true` + `log_context: true`). Confirm the model can still call `list_dir` as a tool and receive a correct result.

---

## Sprint 2: Tool whitelist — IncludedTools / ExcludedTools on AgentLimits

**Scope:** Add `IncludedTools []string` and `ExcludedTools []string` to `AgentLimits` in `internal/config/config.go` (JSON: `included_tools`, `excluded_tools`). In `schemas()` in `internal/agent/local/tools.go`, accept the resolved limits and filter the base tool list: if `IncludedTools` is non-empty keep only those names; then remove any names in `ExcludedTools`. Wire the limits into the `schemas()` call site in `internal/agent/local/local.go`.

**Done when:** `go build ./...` and `go test ./...` pass; an agent config with `"limits": {"included_tools": ["bash", "read_file"]}` causes only those two tools to appear in the outgoing request payload (observable via `debug_local: true` + `log_context: true`); an agent config with `"excluded_tools": ["http_get", "http_request"]` removes those two from the default set.

**Testing strategy:** Add unit tests in `internal/agent/local/tools_test.go` covering: (a) empty limits returns full set, (b) `included_tools` filters correctly, (c) `excluded_tools` removes named tools, (d) both set simultaneously, `included_tools` takes precedence then `excluded_tools` is applied. Run `go test ./internal/agent/local/... -run TestSchemas`.

---

## Sprint 3: system_prompt_tier on AgentConfig

**Scope:** Add `SystemPromptTier string` to `AgentConfig` in `internal/config/config.go` (JSON: `system_prompt_tier`; valid values: `"minimal"`, `"standard"` (default), `"full"`). In `buildSystemPrompt` in `internal/agent/local/local.go`, branch on tier: `minimal` omits the tool-use instructions and reasoning guidance sections; `standard` is current behavior; `full` adds additional verbose guidance. The tier is resolved from `AgentConfig` and threaded into `buildSystemPrompt`.

**Done when:** `go build ./...` and `go test ./internal/agent/local/...` pass; an agent with `"system_prompt_tier": "minimal"` produces a system prompt shorter than the `"standard"` prompt by at least the tool-use instruction block; `"full"` produces a prompt at least as long as `"standard"`.

**Testing strategy:** Add unit tests in `internal/agent/local/local_test.go` (or the appropriate test file) asserting character-count and presence/absence of known section markers for each tier value. Run `go test ./internal/agent/local/... -run TestBuildSystemPrompt`.

---

## Sprint 4: context_window_tokens on AgentConfig — auto-derive MessageBudgetChars and cap MaxToolIterations

**Scope:** Add `ContextWindowTokens int` to `AgentConfig` in `internal/config/config.go` (JSON: `context_window_tokens`). Add an `AgentContextWindowTokens` resolver on `Config` that returns the value or 0 when unset. In `internal/config/config.go`, update `AgentMessageBudget` to auto-derive `MessageBudgetChars` from `ContextWindowTokens` when `MessageBudgetChars` is not explicitly set (formula: `ContextWindowTokens * 3 / 4 * 4` chars, i.e. assume ~4 chars/token, use 75% for history). Update `AgentMaxToolIterations` to cap `MaxToolIterations` at `max(5, ContextWindowTokens/4096)` when `ContextWindowTokens` is set and no explicit `MaxToolIterations` is provided.

**Done when:** `go build ./...` and `go test ./internal/config/...` pass; an agent with `"context_window_tokens": 8192` and no explicit `message_budget_chars` resolves `AgentMessageBudget` to 24576 (8192 * 3); `AgentMaxToolIterations` returns the correct capped value. Explicit overrides via `limits.message_budget_chars` still win.

**Testing strategy:** Add table-driven unit tests in `internal/config/config_test.go` covering: no `context_window_tokens` falls back to global default; `context_window_tokens` set auto-derives budget and tool-iteration cap; explicit `limits.message_budget_chars` overrides the auto-derivation. Run `go test ./internal/config/... -run TestAgentMessageBudget -run TestAgentMaxToolIterations`.
