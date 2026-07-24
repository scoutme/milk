package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/scoutme/milk/internal/agent/aider"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/agent/smolagent"
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
// Local (OpenAI-compat), subprocess (aider-cli, subprocess), and claude-cli agents
// are supported.
//
// claude-cli tool-agents run headlessly — there is no permission back-channel.
// The agent config MUST have dangerously_skip_permissions: true; buildToolRunner
// returns an error when that flag is absent to prevent silent hangs.
//
// No session callbacks are wired — RunToolCall passes nil for session everywhere.
func buildToolRunner(_ context.Context, ac config.AgentConfig, cfg config.Config) (TurnRunner, error) {
	if ac.IsCLI() {
		if !ac.DangerouslySkipPermissions {
			return nil, fmt.Errorf(
				"tool-agent %q uses the claude-cli provider but dangerously_skip_permissions is not set; "+
					"claude-cli tool-agents have no interactive permission back-channel — "+
					"set dangerously_skip_permissions: true in the agent config to proceed",
				ac.Name,
			)
		}
		name := ac.Name
		if name == "" {
			name = "tool-agent"
		}
		cliAgt := newCLIAgent(ac)
		// Zero permContext and nil newInput — headless; permissions handled by
		// --dangerously-skip-permissions which is already set on the agent above.
		return newCLIRunner(cliAgt, name, permContext{}, nil), nil
	}

	name := ac.Name
	if name == "" {
		name = "tool-agent"
	}

	// Subprocess agents (aider-cli, subprocess/smolagent) run stateless per-call.
	if ac.IsExternalProcess() {
		switch {
		case ac.IsAiderCLI():
			return newSubprocessRunner(aider.New(ac), name), nil
		case ac.IsSubprocess():
			if ac.Bin == "" {
				if scriptPath, scriptErr := ensureSmolagentScript(); scriptErr != nil {
					return nil, fmt.Errorf("building subprocess tool agent %q: %w", ac.Name, scriptErr)
				} else {
					ac.Bin = scriptPath
				}
			}
			return newSubprocessRunner(smolagent.New(ac), name), nil
		default:
			return nil, fmt.Errorf("tool-agent %q uses unsupported subprocess provider %q", ac.Name, ac.Provider)
		}
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
