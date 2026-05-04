package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Rules struct {
	EscalateAboveTokens int      `json:"escalate_above_tokens"`
	EscalateKeywords    []string `json:"escalate_keywords"`
}

type Config struct {
	LlamaURL     string `json:"llama_url"`
	LlamaModel   string `json:"llama_model"`
	ClaudeBin    string `json:"claude_bin"`
	DefaultRoute string `json:"default_route"`
	Rules        Rules  `json:"rules"`
}

func defaults() Config {
	return Config{
		LlamaURL:     "http://localhost:8080",
		LlamaModel:   "qwen2.5-coder",
		ClaudeBin:    "claude",
		DefaultRoute: "local",
		Rules: Rules{
			EscalateAboveTokens: 2000,
			EscalateKeywords:    []string{"architect", "refactor entire", "design", "explain why"},
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
