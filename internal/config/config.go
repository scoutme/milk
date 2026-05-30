package config

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	LogLevel            string `json:"log_level"` // DEBUG | INFO | ERROR (default INFO)
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

	// AWSAuthRefresh enables AWS credential injection for the claude subprocess.
	AWSAuthRefresh bool `json:"aws_auth_refresh,omitempty"`

	// ShowReasoning controls whether thinking/reasoning tokens are visible in the
	// transcript by default. Can be toggled live with /think on|off.
	// true (default) = show reasoning; false = show "[thinking…]" placeholder.
	ShowReasoning *bool `json:"show_reasoning,omitempty"`

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

// ShowReasoningDefault returns the configured default for reasoning visibility
// (true when unset, i.e. show reasoning by default).
func (c Config) ShowReasoningDefault() bool {
	if c.ShowReasoning == nil {
		return true
	}
	return *c.ShowReasoning
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
