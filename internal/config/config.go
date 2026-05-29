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
	// Score >= EscalateThreshold → conclusive Claude
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

// LocalAgentConfig holds configuration for a single local-agent backend.
// Multiple backends can be listed under local_agents; the active one is
// selected by name via the local_agent field (defaults to the first entry).
type LocalAgentConfig struct {
	Name string `json:"name"` // display name, used as selector key

	URL   string `json:"url"`   // base URL of the inference server
	Model string `json:"model"` // model name or ARN

	// Provider selects the auth transport.
	// "" or "local" = no auth (default)
	// "bedrock"     = AWS SigV4 signing (native Converse API)
	// anything else = Bearer token via APIKey
	Provider string `json:"provider,omitempty"`

	// APIKey is a Bearer token / API key used when Provider is not "" or "bedrock".
	APIKey string `json:"api_key,omitempty"`

	// TokenCmd is a shell command whose stdout is used as the Bearer token,
	// evaluated once at startup. Takes precedence over APIKey when non-empty.
	// Example: "gh auth token --hostname myorg.ghe.com"
	TokenCmd string `json:"token_cmd,omitempty"`

	// Headers are extra HTTP headers injected on every request (e.g. "api-key" for Azure,
	// "HTTP-Referer" for OpenRouter).
	//
	// Azure OpenAI workaround: Azure uses a non-standard URL path and "api-key" header instead
	// of Bearer auth. Set url to the full deployment endpoint and add {"api-key": "<key>"} here.
	// A dedicated azure provider with URL templating is tracked in GitHub Issues.
	Headers map[string]string `json:"headers,omitempty"`

	// ChatPath overrides the inference endpoint path (default "/v1/chat/completions").
	// Use when the server does not follow the standard /v1 prefix
	// (e.g. some enterprise proxies expose "/chat/completions" directly).
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
	// STS session tokens mid-request. When set, the SigV4 transport retries
	// automatically on 403 without requiring an agent rebuild.
	AWSRefreshCmd string `json:"aws_refresh_cmd,omitempty"`
}

type Config struct {
	// LocalAgent is the name of the active backend from LocalAgents.
	// If empty, the first entry in LocalAgents is used.
	LocalAgent  string             `json:"local_agent,omitempty"`
	LocalAgents []LocalAgentConfig `json:"local_agents,omitempty"`

	ClaudeBin                  string     `json:"claude_bin,omitempty"`
	DefaultRoute               string     `json:"default_route,omitempty"`
	DangerouslySkipPermissions bool       `json:"dangerously_skip_permissions,omitempty"`
	AllowedTools               []string   `json:"allowed_tools,omitempty"`
	AddDirs                    []string   `json:"add_dirs,omitempty"`
	Rules                      Rules      `json:"rules"`
	Otel                       OtelConfig `json:"otel"`
	// Colorization controls transcript syntax highlighting.
	// "off"      — no colorization
	// "fenced"   — fenced code blocks only
	// "balanced" — fenced blocks + inline Markdown (bold, italic, headings, bullets, inline code) (default)
	// "full"    — full Markdown render via glamour
	Colorization string `json:"colorization,omitempty"`

	// DebugClaudeCode writes every raw NDJSON line from the claude subprocess to
	// ~/.milk/claude_debug.ndjson — useful for understanding stream protocol issues.
	DebugClaudeCode bool `json:"debug_claude_code,omitempty"`
	// AWSAuthRefresh enables AWS credential injection for the claude subprocess.
	// When true, milk reads the awsAuthRefresh command from ~/.claude/settings.json,
	// runs it, and injects the resulting credentials as explicit AWS_* env vars.
	// This prevents stale or wrong-account credentials in the shell environment
	// from overriding the correct ones. Requires awsAuthRefresh to be set in
	// ~/.claude/settings.json (it is set automatically by claude-code-with-bedrock).
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
}

func defaults() Config {
	return Config{
		ClaudeBin:    "claude",
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
			EscalateKeywords:    []string{"architect", "refactor entire", "design", "explain why", "analyze", "describe", "summarize"},
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

// ShowReasoningDefault returns the configured default for reasoning visibility
// (true when unset, i.e. show reasoning by default).
func (c Config) ShowReasoningDefault() bool {
	if c.ShowReasoning == nil {
		return true
	}
	return *c.ShowReasoning
}

// ActiveLocalAgent returns the resolved LocalAgentConfig to use.
// Selects by name from local_agents (case-insensitive), falls back to the first
// entry, or returns an empty config when the list is empty.
func (c Config) ActiveLocalAgent() LocalAgentConfig {
	if len(c.LocalAgents) == 0 {
		return LocalAgentConfig{} // no provider configured
	}
	if c.LocalAgent != "" {
		for _, a := range c.LocalAgents {
			if strings.EqualFold(a.Name, c.LocalAgent) {
				return a
			}
		}
	}
	return c.LocalAgents[0]
}

// HasLocalAgentConfig reports whether the user has explicitly configured a
// local-agent backend. Used by the TUI to decide whether to show setup hints.
func (c Config) HasLocalAgentConfig() bool {
	return len(c.LocalAgents) > 0
}

// ClaudeDebugLogPath returns the path for the Claude raw NDJSON debug log.
func ClaudeDebugLogPath() (string, error) {
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
// Each working directory gets its own file so prompts don't mix across projects.
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
		// First launch: scaffold ~/.milk/ and write defaults so the file is
		// discoverable and self-documenting from the start.
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
