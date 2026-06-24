package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
)

// findAgentByName looks up an agent config by name in cfg.Agents.
// Name comparison is case-insensitive, matching the convention in ActiveAgent.
func findAgentByName(cfg config.Config, name string) (config.AgentConfig, bool) {
	lower := strings.ToLower(name)
	for _, ac := range cfg.Agents {
		if strings.ToLower(ac.Name) == lower {
			return ac, true
		}
	}
	return config.AgentConfig{}, false
}

// buildToolRunner constructs a TurnRunner for use as a tool-agent call.
// Only local (OpenAI-compat) agents are supported; subprocess/CLI agents return an error.
// No session callbacks are wired — RunToolCall passes nil for session everywhere.
func buildToolRunner(_ context.Context, ac config.AgentConfig, cfg config.Config) (TurnRunner, error) {
	if ac.IsExternalProcess() {
		return nil, fmt.Errorf("tool-agent %q uses an external-process provider (%s); only local HTTP agents are supported as tool agents", ac.Name, ac.Provider)
	}

	freshAC := applyFreshAWSCreds(cfg, ac)
	la := local.NewFromConfig(freshAC)

	if od, err := config.OtelDir(); err == nil {
		la.WithOtelDir(od)
	}
	la.WithLogContext(cfg.Otel.LogContext)
	// No WithOnTokens: tool-agent calls are stateless — no session to record tokens into.

	if cwd, err := os.Getwd(); err == nil {
		if lp, err := local.OpenPermStore(cwd); err == nil {
			la.WithPermissions(lp, nil)
		}
	}
	la.WithSkipPermissions(cliAgentConfig(cfg).DangerouslySkipPermissions)

	if dbg, err := openLocalDebugLog(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: cannot open tool-agent debug log: %v\n", milkTag(), err)
	} else if dbg != nil {
		la = la.WithDebugLog(dbg)
	}

	name := ac.Name
	if name == "" {
		name = "tool-agent"
	}
	return newLocalRunner(la, name), nil
}

// getOrBuildToolRunner returns a cached TurnRunner for the named tool agent,
// building it on first use and caching it in da.toolRunners.
func getOrBuildToolRunner(ctx context.Context, agentName string, cfg config.Config, da *dispatchAgents) (TurnRunner, error) {
	if da.toolRunners == nil {
		da.toolRunners = make(map[string]TurnRunner)
	}
	if tr, ok := da.toolRunners[agentName]; ok {
		return tr, nil
	}
	ac, ok := findAgentByName(cfg, agentName)
	if !ok {
		return nil, fmt.Errorf("tool-agent %q not found in config", agentName)
	}
	tr, err := buildToolRunner(ctx, ac, cfg)
	if err != nil {
		return nil, err
	}
	da.toolRunners[agentName] = tr
	return tr, nil
}
