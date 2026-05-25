package config

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

type Config struct {
	LlamaURL   string `json:"llama_url"`
	LlamaModel string `json:"llama_model"`

	// LlamaProvider selects the auth transport.
	// "" or "local" = no auth (default)
	// "bedrock"     = AWS SigV4 signing
	// anything else = Bearer token via LlamaAPIKey
	LlamaProvider string `json:"llama_provider"`

	// LlamaAPIKey is a Bearer token / API key used when LlamaProvider is not "" or "bedrock".
	// Injected as "Authorization: Bearer <key>".
	LlamaAPIKey string `json:"llama_api_key"`

	// LlamaHeaders are extra HTTP headers injected on every request (e.g. "api-key" for Azure,
	// "HTTP-Referer" for OpenRouter).
	LlamaHeaders map[string]string `json:"llama_headers"`

	// AWS credentials for LlamaProvider = "bedrock".
	LlamaAWSRegion  string `json:"llama_aws_region"`
	LlamaAWSKeyID   string `json:"llama_aws_key_id"`
	LlamaAWSSecret  string `json:"llama_aws_secret"`
	LlamaAWSToken   string `json:"llama_aws_token"`   // optional session token
	LlamaAWSService string `json:"llama_aws_service"`  // default "bedrock"

	ClaudeBin string `json:"claude_bin"`
	DefaultRoute               string   `json:"default_route"`
	DangerouslySkipPermissions bool     `json:"dangerously_skip_permissions"`
	AllowedTools               []string `json:"allowed_tools"`
	AddDirs                    []string `json:"add_dirs"`
	// PermissionPhrases and DirRestrictionPhrases are merged with built-in
	// defaults (EN + IT). Add extra phrases here for other languages.
	PermissionPhrases     []string   `json:"permission_phrases"`
	DirRestrictionPhrases []string   `json:"dir_restriction_phrases"`
	Rules                 Rules      `json:"rules"`
	Otel                  OtelConfig `json:"otel"`
	// DebugClaudeCode writes every raw NDJSON line from the claude subprocess to
	// ~/.milk/claude_debug.ndjson — useful for understanding stream protocol issues.
	DebugClaudeCode bool `json:"debug_claude_code"`
	// AWSAuthRefresh enables AWS credential injection for the claude subprocess.
	// When true, milk reads the awsAuthRefresh command from ~/.claude/settings.json,
	// runs it, and injects the resulting credentials as explicit AWS_* env vars.
	// This prevents stale or wrong-account credentials in the shell environment
	// from overriding the correct ones. Requires awsAuthRefresh to be set in
	// ~/.claude/settings.json (it is set automatically by claude-code-with-bedrock).
	AWSAuthRefresh bool `json:"aws_auth_refresh"`
}

func defaults() Config {
	return Config{
		LlamaURL:     "http://localhost:8080",
		LlamaModel:   "qwen2.5-coder",
		ClaudeBin:    "claude",
		DefaultRoute: "local",
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

// builtinPermissionPhrases are language-specific substrings that appear when
// Claude explains it cannot proceed due to a tool permission restriction.
var builtinPermissionPhrases = []string{
	// English
	"approve the", "approve this", "need permission",
	"require permission", "waiting for approval",
	"permission to", "grant permission", "allow me to",
	"tool was blocked", "tool call was blocked",
	"blocked by", "is blocked", "being blocked",
	// Italian
	"viene bloccato", "è bloccato", "è stata bloccata",
	"bloccato dalle", "bloccata dalle",
	"di permesso", "impostazioni di permesso",
	"autorizzazione necessaria", "richiede autorizzazione",
	"non autorizzato", "non ho i permessi",
	"permesso negato", "accesso negato allo strumento",
}

// builtinDirRestrictionPhrases are language-specific substrings that appear
// when Claude refuses due to directory access restrictions.
var builtinDirRestrictionPhrases = []string{
	// English
	"is restricted", "outside the allowed", "not within the allowed",
	"only list files within", "cannot access",
	"outside of the", "not allowed to access", "directory is not",
	// Italian
	"accesso limitato", "fuori dalla directory",
	"non posso accedere alla directory", "non posso accedere a",
	"directory non consentita", "percorso non autorizzato",
	"non è consentito accedere", "directory limitata",
	"cartella non accessibile",
}

// EffectivePermissionPhrases merges built-in phrases with any user-supplied extras.
func (c Config) EffectivePermissionPhrases() []string {
	return mergeStrings(builtinPermissionPhrases, c.PermissionPhrases)
}

// EffectiveDirRestrictionPhrases merges built-in phrases with any user-supplied extras.
func (c Config) EffectiveDirRestrictionPhrases() []string {
	return mergeStrings(builtinDirRestrictionPhrases, c.DirRestrictionPhrases)
}

func mergeStrings(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base))
	for _, s := range base {
		seen[s] = true
	}
	out := make([]string, len(base), len(base)+len(extra))
	copy(out, base)
	for _, s := range extra {
		if !seen[s] {
			out = append(out, s)
		}
	}
	return out
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
