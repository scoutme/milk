package workflow

import (
	"fmt"
	"strings"

	"github.com/scoutme/milk/internal/config"
)

const (
	AliasPrimary    = "primary"
	AliasEscalation = "escalation"
)

// ResolveAgentNames maps role specifiers to concrete agent names.
// Aliases "primary" and "escalation" are expanded using cfg.
// Returns an error if a specifier names an agent that does not exist in cfg.
// The returned map holds only names; the caller constructs actual runners.
func ResolveAgentNames(roles map[string]string, cfg config.Config) (map[string]string, error) {
	out := make(map[string]string, len(roles))
	for role, specifier := range roles {
		name, err := resolveOne(specifier, cfg)
		if err != nil {
			return nil, fmt.Errorf("workflow role %q: %w", role, err)
		}
		out[role] = name
	}
	return out, nil
}

func resolveOne(specifier string, cfg config.Config) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(specifier))
	switch norm {
	case AliasPrimary:
		return cfg.ActiveAgent().Name, nil
	case AliasEscalation:
		return cfg.EscalationAgentConfig().Name, nil
	}
	// Concrete name — verify it exists.
	for _, a := range cfg.Agents {
		if strings.EqualFold(a.Name, specifier) {
			return a.Name, nil
		}
	}
	// Accept the built-in claude-cli entry even when absent from cfg.Agents.
	if norm == "claude" || norm == "claude-cli" {
		return specifier, nil
	}
	return "", fmt.Errorf("agent %q not found in config", specifier)
}
