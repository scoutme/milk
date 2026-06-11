package config

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Rules struct {
	// Hard thresholds (conclusive)
	EscalateAboveTokens int      `json:"escalate_above_tokens"`
	EscalateKeywords    []string `json:"escalate_keywords"`
	LocalBelowTokens    int      `json:"local_below_tokens"`

	// Weighted scoring (soft signals)
	// Score >= EscalateThreshold → conclusive escalation
	// Score <= LocalThreshold    → conclusive local
	// Otherwise                  → inconclusive (LLM classifier)
	EscalateThreshold int `json:"escalate_threshold"`
	LocalThreshold    int `json:"local_threshold"`

	// Per-signal weights (positive = escalate, negative = local)
	LocalVerbWeight    int `json:"local_verb_weight"`
	EscalateVerbWeight int `json:"escalate_verb_weight"`
	PathRefWeight      int `json:"path_ref_weight"`
	CodeBlockWeight    int `json:"code_block_weight"`
	OpenQuestionWeight int `json:"open_question_weight"`

	// Configurable fallback when rules are inconclusive
	// "local" = call local LLM classifier; "claude" = escalate directly
	ClassifierFallback string `json:"classifier_fallback"`

	// Keyword lists (overridable)
	LocalVerbs    []string `json:"local_verbs"`
	EscalateVerbs []string `json:"escalate_verbs"`
}

// OtelConfig controls OpenTelemetry signal collection and file management.
type OtelConfig struct {
	Enabled             bool   `json:"enabled"`
	LogLevel            string `json:"log_level"`   // minimum log level: DEBUG | INFO | WARN | ERROR (default INFO)
	LogFormat           string `json:"log_format"`  // "" or "off" (disabled), "text" (human-readable), "json" (structured)
	LogContext          bool   `json:"log_context"` // when true, log the full serialised request payload on each inference call
	Traces              bool   `json:"traces"`
	Metrics             bool   `json:"metrics"`
	WarnMB              int    `json:"warn_mb"`               // warn when any otel file exceeds this (0 = off)
	MaxMB               int    `json:"max_mb"`                // hard cap, disable otel when exceeded (0 = off)
	MetricsFlushMinutes int    `json:"metrics_flush_minutes"` // periodic flush interval (0 = session-end only)
}

// AgentConfig holds configuration for a single agent backend.
// Multiple backends can be listed under agents; the active one is
// selected by name via the agent field (defaults to the first non-cli entry).
//
// Provider values:
//
//	"" or "local"  — plain HTTP, no auth (OpenAI-compat inference server)
//	"bedrock"      — AWS SigV4 signing, native Converse API
//	"claude-cli"   — Claude Code CLI subprocess (not an HTTP backend)
//	anything else  — Bearer token via APIKey or TokenCmd
type AgentConfig struct {
	Name string `json:"name"` // display name, used as selector key

	URL   string `json:"url,omitempty"`   // base URL of the inference server (unused for claude-cli)
	Model string `json:"model,omitempty"` // model name or ARN (unused for claude-cli)

	// Provider selects the backend type / auth transport.
	Provider string `json:"provider,omitempty"`

	// APIKey is a Bearer token / API key used when Provider is not "", "local", "bedrock", or "claude-cli".
	APIKey string `json:"api_key,omitempty"`

	// TokenCmd is a shell command whose stdout is used as the Bearer token,
	// evaluated once at startup. Takes precedence over APIKey when non-empty.
	// Example: "gh auth token --hostname myorg.ghe.com"
	TokenCmd string `json:"token_cmd,omitempty"`

	// Headers are extra HTTP headers injected on every request (e.g. "api-key" for Azure,
	// "HTTP-Referer" for OpenRouter).
	Headers map[string]string `json:"headers,omitempty"`

	// ChatPath overrides the inference endpoint path (default "/v1/chat/completions").
	ChatPath string `json:"chat_path,omitempty"`

	// TLSSkipVerify disables TLS certificate verification. Use only for dev/self-signed certs.
	TLSSkipVerify bool `json:"tls_skip_verify,omitempty"`
	// TLSCACert is a path to a PEM-encoded CA cert for private/self-signed endpoints.
	TLSCACert string `json:"tls_ca_cert,omitempty"`

	// AWS credentials for Provider = "bedrock".
	AWSRegion  string `json:"aws_region,omitempty"`
	AWSKeyID   string `json:"aws_key_id,omitempty"`
	AWSSecret  string `json:"aws_secret,omitempty"`
	AWSToken   string `json:"aws_token,omitempty"`   // optional session token
	AWSService string `json:"aws_service,omitempty"` // default "bedrock"
	// AWSRefreshCmd is a credential_process-compatible command whose JSON output
	// (AccessKeyId / SecretAccessKey / SessionToken) is used to refresh expired
	// STS session tokens mid-request.
	AWSRefreshCmd string `json:"aws_refresh_cmd,omitempty"`

	// Fields for Provider = "claude-cli".
	// Bin is the path to the claude binary (default "claude").
	Bin string `json:"bin,omitempty"`
	// DangerouslySkipPermissions passes --dangerously-skip-permissions to the CLI.
	DangerouslySkipPermissions bool `json:"dangerously_skip_permissions,omitempty"`
	// AllowedTools is a list of tools pre-approved for this CLI agent.
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// AddDirs is a list of extra directories to pass with --add-dir.
	AddDirs []string `json:"add_dirs,omitempty"`

	// Limits holds optional per-agent overrides for context caps and injection limits.
	// Nil fields fall back to the global Config defaults.
	Limits *AgentLimits `json:"limits,omitempty"`
}

// AgentLimits holds optional per-agent overrides for context caps and injection
// limits. A nil field means "use the global default from Config". All integer
// fields use pointer types with the following semantics: nil = use global
// default, negative (e.g. -1) = disabled/unlimited (resolves to 0), zero =
// use the built-in hardcoded default, positive = exact value.
//
// These mirror the global fields on Config but without the Local* prefix — the
// distinction was a legacy artefact of fixed role assignments. When set on an
// AgentConfig, they override the global value for that specific agent regardless
// of whether it is acting as primary or escalation.
type AgentLimits struct {
	// ContextBudgetChars overrides context_budget_chars for this agent.
	ContextBudgetChars *int `json:"context_budget_chars,omitempty"`
	// MessageBudgetChars overrides local_context_budget_chars (message history trim).
	MessageBudgetChars *int `json:"message_budget_chars,omitempty"`
	// MemoryReinjectionTurns overrides memory_reinjection_turns / local_memory_reinjection_turns.
	MemoryReinjectionTurns *int `json:"memory_reinjection_turns,omitempty"`
	// MemoryReinjectionBytes overrides memory_reinjection_bytes / local_memory_reinjection_bytes.
	MemoryReinjectionBytes *int `json:"memory_reinjection_bytes,omitempty"`
	// MemoryResultMaxBytes overrides local_memory_result_max_bytes.
	MemoryResultMaxBytes *int `json:"memory_result_max_bytes,omitempty"`
	// PerceptInjectMax overrides percept_inject_max.
	PerceptInjectMax *int `json:"percept_inject_max,omitempty"`
	// PerceptInjectMaxBytes overrides percept_inject_max_bytes.
	PerceptInjectMaxBytes *int `json:"percept_inject_max_bytes,omitempty"`
	// PerceptRelevanceGate overrides percept_relevance_gate.
	PerceptRelevanceGate *bool `json:"percept_relevance_gate,omitempty"`
	// MaxToolIterations overrides local_max_tool_iterations for this agent.
	MaxToolIterations *int `json:"max_tool_iterations,omitempty"`
	// ReturningFreshStartLocalTurns overrides returning_fresh_start_local_turns for
	// this agent. When non-zero, a ContextModeReturning escalation is downgraded to
	// a fresh start (no --resume) when this many local turns have elapsed since the
	// last escalation turn. Set to -1 to disable the turn-gap check for this agent.
	ReturningFreshStartLocalTurns *int `json:"returning_fresh_start_local_turns,omitempty"`
}

// IsCLI reports whether this agent uses the Claude Code CLI backend.
func (a AgentConfig) IsCLI() bool {
	return strings.ToLower(strings.TrimSpace(a.Provider)) == "claude-cli"
}

// defaultCLIAgent is the built-in claude-cli entry added when no agent
// named "claude" exists. It is never written to disk unless the user edits it.
func defaultCLIAgent() AgentConfig {
	return AgentConfig{
		Name:     "claude",
		Provider: "claude-cli",
		Bin:      "claude",
	}
}

type Config struct {
	// Agent is the name of the active primary backend from Agents.
	// If empty, the first non-claude-cli entry is used.
	Agent  string        `json:"agent,omitempty"`
	Agents []AgentConfig `json:"agents,omitempty"`

	// EscalationAgent selects which backend handles escalated turns.
	// Defaults to "claude" (the built-in claude-cli entry).
	// Set to the name of any agents entry to route escalated turns there.
	EscalationAgent string `json:"escalation_agent,omitempty"`

	DefaultRoute string     `json:"default_route,omitempty"`
	Rules        Rules      `json:"rules"`
	Otel         OtelConfig `json:"otel"`

	// Colorization controls transcript syntax highlighting.
	// "off"      — no colorization
	// "fenced"   — fenced code blocks only
	// "balanced" — fenced blocks + inline Markdown (default)
	// "full"     — full Markdown render via glamour
	Colorization string `json:"colorization,omitempty"`

	// DebugCLILog writes every raw NDJSON line from the claude subprocess to
	// ~/.milk/claude_debug.ndjson.
	DebugCLILog bool `json:"debug_claude_code,omitempty"`

	// DebugLocalLog writes every raw SSE line from the local agent's HTTP
	// stream to ~/.milk/local_debug.log, including lines that are skipped or
	// fail to parse. Useful for diagnosing dropped tokens or unknown events.
	DebugLocalLog bool `json:"debug_local,omitempty"`

	// AWSAuthRefresh enables AWS credential injection for the claude subprocess.
	AWSAuthRefresh bool `json:"aws_auth_refresh,omitempty"`

	// ShowReasoning controls whether thinking/reasoning tokens are visible in the
	// transcript by default. Can be toggled live with /think on|off.
	// false (default) = show "[thinking…]" placeholder; true = show reasoning.
	ShowReasoning *bool `json:"show_reasoning,omitempty"`

	// StickyEscalation controls whether the escalation agent is automatically
	// kept for subsequent turns after the router first escalates (without an
	// explicit /escalate command). true (default) — router-triggered escalations
	// are sticky until the user types /primary or Ctrl+C. false — routing is
	// re-evaluated every turn as before. Explicit /escalate commands always pin
	// regardless of this setting.
	StickyEscalation *bool `json:"sticky_escalation,omitempty"`

	// ContextBudgetChars is the maximum number of characters injected per
	// agent summary brick (last_local_summary / last_claude_summary) in the
	// escalation system prompt. Turns are included newest-first until the
	// budget is exhausted. Default: 12000.
	ContextBudgetChars int `json:"context_budget_chars,omitempty"`

	// MemoryReinjectionTurns is the number of escalation turns after which the
	// memory/need instruction block is unconditionally re-injected, even when it
	// was already sent in a prior turn. Guards against agent-side context truncation.
	// Default: 20. Set to 0 to disable this threshold.
	MemoryReinjectionTurns int `json:"memory_reinjection_turns,omitempty"`

	// MemoryReinjectionBytes is the total bytes of escalation assistant output
	// after which the memory/need instruction block is unconditionally re-injected.
	// Default: 40000. Set to 0 to disable this threshold.
	MemoryReinjectionBytes int `json:"memory_reinjection_bytes,omitempty"`

	// PerceptInjectMax caps the number of percepts injected into the escalation
	// context per turn. Lowest-weight percepts are dropped when over budget.
	// Default: 25. Set to 0 for no limit.
	PerceptInjectMax int `json:"percept_inject_max,omitempty"`

	// PerceptInjectMaxBytes caps the total byte size of percept content injected
	// per turn. Lowest-weight percepts are dropped when over budget.
	// Default: 2048. Set to 0 for no limit.
	PerceptInjectMaxBytes int `json:"percept_inject_max_bytes,omitempty"`

	// PerceptStoreMax caps the total number of percepts in the global store.
	// After consolidation, lowest-weight non-core percepts are pruned to this limit.
	// Default: 0 (no limit).
	PerceptStoreMax int `json:"percept_store_max,omitempty"`

	// PerceptRelevanceGate enables keyword-intersection filtering before injection:
	// percepts with zero token overlap with the current prompt are skipped.
	// Default: true. Set to false to disable.
	PerceptRelevanceGate *bool `json:"percept_relevance_gate,omitempty"`

	// LocalMemoryResultMaxBytes caps the byte size of memory tool results
	// (get_memory, list_memory) returned to the local agent per tool call.
	// Results are truncated to this limit before being appended to the
	// local context. Default: 2048. Set to 0 for no limit.
	LocalMemoryResultMaxBytes int `json:"local_memory_result_max_bytes,omitempty"`

	// LocalMemoryReinjectionTurns is the number of local agent turns after which
	// the memory/need instruction block is unconditionally re-appended to the
	// local agent's context. Mirrors memory_reinjection_turns for the escalation
	// path. Default: 20. Set to -1 to disable.
	LocalMemoryReinjectionTurns int `json:"local_memory_reinjection_turns,omitempty"`

	// LocalMemoryReinjectionBytes is the total bytes of local agent output
	// after which the memory/need instruction block is re-appended.
	// Default: 40000. Set to -1 to disable.
	LocalMemoryReinjectionBytes int `json:"local_memory_reinjection_bytes,omitempty"`

	// LocalContextBudgetChars is the maximum total character count of the
	// messages array passed to the local inference server per turn. When the
	// accumulated history exceeds this budget, the oldest user+assistant pairs
	// are dropped until it fits. Default: 24000. Set to 0 for no limit.
	LocalContextBudgetChars int `json:"local_context_budget_chars,omitempty"`

	// LocalMaxToolIterations caps the number of consecutive tool-call / response
	// cycles the local agent may execute before the turn is aborted with an error.
	// Default: 20. Set to 0 to use the default; set to -1 for unlimited.
	LocalMaxToolIterations int `json:"local_max_tool_iterations,omitempty"`

	// ReturningFreshStartLocalTurns is the number of local-agent turns since the
	// last escalation turn after which a ContextModeReturning escalation is
	// downgraded to a fresh start (no --resume). The prior escalation session is
	// considered stale at that point; the re-orientation context files already
	// provide equivalent guidance. Default: 8. Set to 0 to disable the turn-gap
	// condition entirely (need-staleness check is unaffected).
	ReturningFreshStartLocalTurns int `json:"returning_fresh_start_local_turns,omitempty"`

	// RemoteOversight configures the remote oversight interface. When non-nil
	// and a backend is configured, agent turn notifications and permission
	// prompts are forwarded to the configured backend (e.g. Telegram).
	RemoteOversight *RemoteOversightConfig `json:"remote_oversight,omitempty"`
}

// RemoteOversightConfig holds settings for the remote oversight interface.
type RemoteOversightConfig struct {
	// Backend selects the transport. Currently only "telegram" is supported.
	Backend string `json:"backend,omitempty"`

	// Telegram holds Telegram-specific settings. Used when Backend == "telegram".
	Telegram *TelegramConfig `json:"telegram,omitempty"`

	// PermTimeoutSecs is how long to wait for a remote permission reply before
	// falling back to TimeoutAction. Default: 120 (2 minutes).
	PermTimeoutSecs int `json:"perm_timeout_secs,omitempty"`

	// TimeoutAction is the fallback when no remote reply arrives within
	// PermTimeoutSecs. One of "allow" or "deny". Default: "deny".
	TimeoutAction string `json:"timeout_action,omitempty"`

	// NotifyTools controls whether tool-use events are forwarded.
	// Default: true.
	NotifyTools *bool `json:"notify_tools,omitempty"`
}

// TelegramConfig holds Telegram Bot API settings.
type TelegramConfig struct {
	// Token is the bot token from @BotFather (e.g. "123456:ABC-DEF...").
	Token string `json:"token,omitempty"`
	// ChatID is the numeric chat/user ID to send messages to.
	ChatID int64 `json:"chat_id,omitempty"`
}

func defaults() Config {
	return Config{
		DefaultRoute: "local",
		Colorization: "balanced",
		Otel: OtelConfig{
			Enabled:             true,
			LogLevel:            "INFO",
			Traces:              true,
			Metrics:             true,
			WarnMB:              50,
			MaxMB:               0,
			MetricsFlushMinutes: 5,
		},
		Rules: Rules{
			EscalateAboveTokens: 2000,
			EscalateKeywords:    []string{"architect", "refactor entire", "design", "explain why", "analyze", "describe", "summarize", "context brick", "memory panel", "panel memory"},
			LocalBelowTokens:    30,

			EscalateThreshold: 6,
			LocalThreshold:    -4,

			LocalVerbWeight:    -3,
			EscalateVerbWeight: 4,
			PathRefWeight:      -2,
			CodeBlockWeight:    -2,
			OpenQuestionWeight: 3,

			ClassifierFallback: "local",

			LocalVerbs:    []string{"grep", "find", "list", "run", "read", "fix", "debug", "show", "cat", "ls", "check", "print", "count", "search"},
			EscalateVerbs: []string{"architect", "design", "refactor entire", "explain why", "compare", "evaluate", "plan", "propose", "summarize", "review"},
		},
	}
}

// ContextBudget returns the configured context budget in characters,
// falling back to 12000 when unset.
func (c Config) ContextBudget() int {
	if c.ContextBudgetChars <= 0 {
		return 12000
	}
	return c.ContextBudgetChars
}

// MemoryReinjectionTurnThreshold returns the escalation-turn interval for
// unconditional memory instruction re-injection, defaulting to 20.
// Returns 0 when the threshold is explicitly disabled.
func (c Config) MemoryReinjectionTurnThreshold() int {
	if c.MemoryReinjectionTurns < 0 {
		return 0
	}
	if c.MemoryReinjectionTurns == 0 {
		return 20
	}
	return c.MemoryReinjectionTurns
}

// MemoryReinjectionByteThreshold returns the escalation assistant output byte
// threshold for unconditional memory instruction re-injection, defaulting to 40000.
// Returns 0 when the threshold is explicitly disabled.
func (c Config) MemoryReinjectionByteThreshold() int {
	if c.MemoryReinjectionBytes < 0 {
		return 0
	}
	if c.MemoryReinjectionBytes == 0 {
		return 40000
	}
	return c.MemoryReinjectionBytes
}

// PerceptInjectMaxCount returns the max number of percepts to inject per turn,
// defaulting to 25. Returns 0 when explicitly disabled (unlimited).
func (c Config) PerceptInjectMaxCount() int {
	if c.PerceptInjectMax < 0 {
		return 0
	}
	if c.PerceptInjectMax == 0 {
		return 25
	}
	return c.PerceptInjectMax
}

// PerceptInjectMaxByteCount returns the max byte size of percept content to
// inject per turn, defaulting to 2048. Returns 0 when explicitly disabled.
func (c Config) PerceptInjectMaxByteCount() int {
	if c.PerceptInjectMaxBytes < 0 {
		return 0
	}
	if c.PerceptInjectMaxBytes == 0 {
		return 2048
	}
	return c.PerceptInjectMaxBytes
}

// PerceptStoreSizeLimit returns the configured global store size cap.
// Returns 0 when not set (no limit).
func (c Config) PerceptStoreSizeLimit() int {
	if c.PerceptStoreMax < 0 {
		return 0
	}
	return c.PerceptStoreMax
}

// PerceptRelevanceGateEnabled returns whether relevance-gating is active.
// Defaults to true when unset.
func (c Config) PerceptRelevanceGateEnabled() bool {
	if c.PerceptRelevanceGate == nil {
		return true
	}
	return *c.PerceptRelevanceGate
}

// LocalMemoryResultMaxByteCount returns the max byte size of memory tool results
// returned to the local agent per call, defaulting to 2048. Returns 0 when
// explicitly disabled (unlimited).
func (c Config) LocalMemoryResultMaxByteCount() int {
	if c.LocalMemoryResultMaxBytes < 0 {
		return 0
	}
	if c.LocalMemoryResultMaxBytes == 0 {
		return 2048
	}
	return c.LocalMemoryResultMaxBytes
}

// LocalMemoryReinjectionTurnThreshold returns the local-turn interval for
// memory instruction re-injection, defaulting to 20. Returns 0 when disabled.
func (c Config) LocalMemoryReinjectionTurnThreshold() int {
	if c.LocalMemoryReinjectionTurns < 0 {
		return 0
	}
	if c.LocalMemoryReinjectionTurns == 0 {
		return 20
	}
	return c.LocalMemoryReinjectionTurns
}

// LocalMemoryReinjectionByteThreshold returns the local output byte threshold
// for memory instruction re-injection, defaulting to 40000. Returns 0 when disabled.
func (c Config) LocalMemoryReinjectionByteThreshold() int {
	if c.LocalMemoryReinjectionBytes < 0 {
		return 0
	}
	if c.LocalMemoryReinjectionBytes == 0 {
		return 40000
	}
	return c.LocalMemoryReinjectionBytes
}

// PermTimeoutDuration returns the configured remote permission timeout,
// defaulting to 120 seconds.
func (r *RemoteOversightConfig) PermTimeoutDuration() time.Duration {
	if r == nil || r.PermTimeoutSecs <= 0 {
		return 120 * time.Second
	}
	return time.Duration(r.PermTimeoutSecs) * time.Second
}

// TimeoutActionValue returns the configured timeout action ("allow" or "deny"),
// defaulting to "deny".
func (r *RemoteOversightConfig) TimeoutActionValue() string {
	if r == nil || r.TimeoutAction == "" {
		return "deny"
	}
	return r.TimeoutAction
}

// NotifyToolsEnabled returns whether tool notifications are enabled.
// Defaults to true.
func (r *RemoteOversightConfig) NotifyToolsEnabled() bool {
	if r == nil || r.NotifyTools == nil {
		return true
	}
	return *r.NotifyTools
}

// LocalContextBudget returns the maximum total character count of the local
// agent's messages array, defaulting to 24000. Returns 0 when explicitly
// disabled (no limit).
func (c Config) LocalContextBudget() int {
	if c.LocalContextBudgetChars < 0 {
		return 0
	}
	if c.LocalContextBudgetChars == 0 {
		return 24000
	}
	return c.LocalContextBudgetChars
}

// --- Per-agent resolvers ---
// Each resolver accepts an AgentConfig and returns the effective value for that
// agent: the per-agent override when set, otherwise the global Config default.

// AgentContextBudget returns the summary-brick budget for the given agent,
// falling back to the global ContextBudget().
func (c Config) AgentContextBudget(a AgentConfig) int {
	if a.Limits != nil && a.Limits.ContextBudgetChars != nil {
		v := *a.Limits.ContextBudgetChars
		if v < 0 {
			return 0
		}
		return intOr(v, 12000)
	}
	return c.ContextBudget()
}

// AgentMessageBudget returns the message-history trim budget for the given agent,
// falling back to the global LocalContextBudget().
func (c Config) AgentMessageBudget(a AgentConfig) int {
	if a.Limits != nil && a.Limits.MessageBudgetChars != nil {
		v := *a.Limits.MessageBudgetChars
		if v < 0 {
			return 0
		}
		return intOr(v, 24000)
	}
	return c.LocalContextBudget()
}

// AgentMemoryReinjectionTurnThreshold returns the memory re-injection turn interval
// for the given agent, falling back to the appropriate global default.
// useLocalDefault selects which global field to fall back to (primary vs escalation).
func (c Config) AgentMemoryReinjectionTurnThreshold(a AgentConfig, useLocalDefault bool) int {
	if a.Limits != nil && a.Limits.MemoryReinjectionTurns != nil {
		v := *a.Limits.MemoryReinjectionTurns
		if v < 0 {
			return 0
		}
		return intOr(v, 20)
	}
	if useLocalDefault {
		return c.LocalMemoryReinjectionTurnThreshold()
	}
	return c.MemoryReinjectionTurnThreshold()
}

// AgentMemoryReinjectionByteThreshold returns the memory re-injection byte threshold
// for the given agent, falling back to the appropriate global default.
func (c Config) AgentMemoryReinjectionByteThreshold(a AgentConfig, useLocalDefault bool) int {
	if a.Limits != nil && a.Limits.MemoryReinjectionBytes != nil {
		v := *a.Limits.MemoryReinjectionBytes
		if v < 0 {
			return 0
		}
		return intOr(v, 40000)
	}
	if useLocalDefault {
		return c.LocalMemoryReinjectionByteThreshold()
	}
	return c.MemoryReinjectionByteThreshold()
}

// AgentReturningFreshStartLocalTurns returns the local-turn threshold after which
// a ContextModeReturning escalation is treated as a fresh start (no --resume),
// for the given agent. Falls back to the global ReturningFreshStartLocalTurns.
// Returns 0 when the turn-gap condition is disabled.
func (c Config) AgentReturningFreshStartLocalTurns(a AgentConfig) int {
	if a.Limits != nil && a.Limits.ReturningFreshStartLocalTurns != nil {
		v := *a.Limits.ReturningFreshStartLocalTurns
		if v < 0 {
			return 0
		}
		return intOr(v, 8)
	}
	return intOr(c.ReturningFreshStartLocalTurns, 8)
}

// AgentMemoryResultMaxByteCount returns the memory tool result size cap for the
// given agent, falling back to the global LocalMemoryResultMaxByteCount().
func (c Config) AgentMemoryResultMaxByteCount(a AgentConfig) int {
	if a.Limits != nil && a.Limits.MemoryResultMaxBytes != nil {
		v := *a.Limits.MemoryResultMaxBytes
		if v < 0 {
			return 0
		}
		return intOr(v, 2048)
	}
	return c.LocalMemoryResultMaxByteCount()
}

// AgentPerceptInjectMaxCount returns the percept injection count cap for the
// given agent, falling back to the global PerceptInjectMaxCount().
func (c Config) AgentPerceptInjectMaxCount(a AgentConfig) int {
	if a.Limits != nil && a.Limits.PerceptInjectMax != nil {
		v := *a.Limits.PerceptInjectMax
		if v < 0 {
			return 0
		}
		return intOr(v, 25)
	}
	return c.PerceptInjectMaxCount()
}

// AgentPerceptInjectMaxByteCount returns the percept injection byte cap for the
// given agent, falling back to the global PerceptInjectMaxByteCount().
func (c Config) AgentPerceptInjectMaxByteCount(a AgentConfig) int {
	if a.Limits != nil && a.Limits.PerceptInjectMaxBytes != nil {
		v := *a.Limits.PerceptInjectMaxBytes
		if v < 0 {
			return 0
		}
		return intOr(v, 2048)
	}
	return c.PerceptInjectMaxByteCount()
}

// AgentPerceptRelevanceGateEnabled returns whether relevance gating is active for
// the given agent, falling back to the global PerceptRelevanceGateEnabled().
func (c Config) AgentPerceptRelevanceGateEnabled(a AgentConfig) bool {
	if a.Limits != nil && a.Limits.PerceptRelevanceGate != nil {
		return *a.Limits.PerceptRelevanceGate
	}
	return c.PerceptRelevanceGateEnabled()
}

// AgentMaxToolIterations returns the tool-call chain limit for the given agent.
// Returns 0 to signal "unlimited" when the value resolves to negative.
// Default: 20.
func (c Config) AgentMaxToolIterations(a AgentConfig) int {
	if a.Limits != nil && a.Limits.MaxToolIterations != nil {
		v := *a.Limits.MaxToolIterations
		if v < 0 {
			return 0 // unlimited
		}
		return intOr(v, 20)
	}
	if c.LocalMaxToolIterations < 0 {
		return 0 // unlimited
	}
	return intOr(c.LocalMaxToolIterations, 20)
}

// intOr returns v when v > 0, otherwise returns def.
func intOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// ShowReasoningDefault returns the configured default for reasoning visibility
// (false when unset, i.e. hide reasoning by default).
func (c Config) ShowReasoningDefault() bool {
	if c.ShowReasoning == nil {
		return false
	}
	return *c.ShowReasoning
}

// StickyEscalationEnabled returns true when router-triggered escalations
// should be kept sticky across turns (the default). Returns false only when
// explicitly disabled via sticky_escalation: false in config.
func (c Config) StickyEscalationEnabled() bool {
	if c.StickyEscalation == nil {
		return true
	}
	return *c.StickyEscalation
}

// effectiveAgents returns Agents with the built-in claude-cli entry appended
// if no entry named "claude" already exists. This ensures there is always a
// claude-cli agent available without requiring it to be in every config file.
func (c Config) effectiveAgents() []AgentConfig {
	for _, a := range c.Agents {
		if strings.EqualFold(a.Name, "claude") {
			return c.Agents
		}
	}
	return append(c.Agents, defaultCLIAgent())
}

// ActiveAgent returns the resolved AgentConfig to use as the primary agent.
// Skips claude-cli entries — the primary agent must be an inference-server backend.
// Selects by name from agents (case-insensitive), falls back to the first
// non-claude-cli entry, or returns an empty config when none exists.
func (c Config) ActiveAgent() AgentConfig {
	agents := c.effectiveAgents()
	if c.Agent != "" {
		for _, a := range agents {
			if strings.EqualFold(a.Name, c.Agent) && !a.IsCLI() {
				return a
			}
		}
	}
	for _, a := range agents {
		if !a.IsCLI() {
			return a
		}
	}
	return AgentConfig{}
}

// EscalationAgentConfig returns the AgentConfig for the escalation backend.
// Defaults to the built-in claude-cli entry when EscalationAgent is empty or "claude".
func (c Config) EscalationAgentConfig() AgentConfig {
	name := strings.TrimSpace(c.EscalationAgent)
	if name == "" {
		name = "claude"
	}
	for _, a := range c.effectiveAgents() {
		if strings.EqualFold(a.Name, name) {
			return a
		}
	}
	// Named agent not found — fall back to claude-cli default.
	return defaultCLIAgent()
}

// HasInferenceAgent reports whether the user has configured at least one
// non-claude-cli agent backend. Used by the TUI to decide whether to show
// setup hints.
func (c Config) HasInferenceAgent() bool {
	for _, a := range c.Agents {
		if !a.IsCLI() {
			return true
		}
	}
	return false
}

// CLIDebugLogPath returns the path for the Claude raw NDJSON debug log.
func CLIDebugLogPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "claude_debug.ndjson"), nil
}

// LocalDebugLogPath returns the path for the local agent raw SSE debug log.
func LocalDebugLogPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "local_debug.log"), nil
}

func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".milk"), nil
}

// OtelDir returns the directory for OTel signal files (~/.milk/otel).
func OtelDir() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "otel"), nil
}

// MilkLogPath returns the path for the milk log file (~/.milk/otel/milk.log).
func MilkLogPath() (string, error) {
	d, err := OtelDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "milk.log"), nil
}

// HistoryPath returns the readline history file path for the given cwd.
func HistoryPath(cwd string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	histDir := filepath.Join(dir, "history")
	if err := os.MkdirAll(histDir, 0o700); err != nil {
		return "", err
	}
	hash := fmt.Sprintf("%x", md5.Sum([]byte(cwd))) //nolint:gosec
	return filepath.Join(histDir, hash+".txt"), nil
}

func Load() (Config, error) {
	cfg := defaults()

	dir, err := Dir()
	if err != nil {
		return cfg, err
	}

	path := filepath.Join(dir, "config.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		_ = Save(cfg)
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func Save(cfg Config) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600)
}
