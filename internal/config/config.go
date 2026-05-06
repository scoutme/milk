package config

import (
	"encoding/json"
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

type Config struct {
	LlamaURL                  string `json:"llama_url"`
	LlamaModel                string `json:"llama_model"`
	ClaudeBin                 string `json:"claude_bin"`
	DefaultRoute              string `json:"default_route"`
	DangerouslySkipPermissions bool  `json:"dangerously_skip_permissions"`
	Rules                     Rules  `json:"rules"`
}

func defaults() Config {
	return Config{
		LlamaURL:     "http://localhost:8080",
		LlamaModel:   "qwen2.5-coder",
		ClaudeBin:    "claude",
		DefaultRoute: "local",
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

func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".milk"), nil
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
