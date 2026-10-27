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
	"github.com/scoutme/milk/internal/mcp"
	"github.com/scoutme/milk/internal/memory"
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
// existingMCP, when non-nil, is an already-connected ToolSet for ac.Name to reuse
// instead of opening a second, independent connection — important for stdio
// servers like blender-mcp, whose backing Blender addon socket typically only
// accepts one client. It comes from the caller's live mcpToolSets cache (i.e.
// ac is also currently serving as primary or escalation in this session); when
// ac isn't currently active in either role (or there is no such cache, e.g.
// single-prompt CLI mode), existingMCP is nil and a fresh connection is opened.
//
// No session callbacks are wired — RunToolCall passes nil for session everywhere.
func buildToolRunner(ctx context.Context, ac config.AgentConfig, cfg config.Config, existingMCP *mcp.ToolSet, mem *memory.Store, caller memory.Consumer, wiring localAgentWiring) (TurnRunner, error) {
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
		r := newCLIRunner(cliAgt, name, permContext{}, nil)
		if servers := cfg.EffectiveMCPServers(ac.Name); len(servers) > 0 {
			r = r.withMCPServers(servers)
		}
		return r, nil
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
			r := newSubprocessRunner(smolagent.New(ac), name)
			// Native MCP (smolagent, issue #122): smolagent tool-agents get the
			// same resolved server list the primary/escalation paths carry, and
			// connect themselves per call. aider-cli tool-agents are left
			// untouched (no MCP at all, as before — its context-injection block
			// needs a ToolSet this path has never built).
			if servers := cfg.EffectiveMCPServers(ac.Name); len(servers) > 0 {
				r = r.withMCPServers(servers)
			}
			return r, nil
		default:
			return nil, fmt.Errorf("tool-agent %q uses unsupported subprocess provider %q", ac.Name, ac.Provider)
		}
	}

	freshAC := applyFreshAWSCreds(cfg, ac)
	la := local.NewFromConfig(freshAC).WithToolAgentRole().WithMemoryCaller(caller)

	if existingMCP != nil {
		la = la.WithMCPToolSet(existingMCP)
	} else {
		la, _ = attachMCPToolSet(ctx, cfg, ac.Name, la)
	}

	if od, err := config.OtelDir(); err == nil {
		la.WithOtelDir(od)
	}
	la.WithLogContext(cfg.Otel.LogContext)
	// No WithOnTokens: tool-agent calls are stateless — no session to record tokens into.

	// Permission handling goes through the shared wiring (issue #188): a
	// tool-agent must ask exactly like the session's own agents — the old
	// WithPermissions(lp, nil) wired a grant store with a nil ask, so these
	// agents could never prompt at all. Keep the best-effort store fallback
	// (and the config's skip flag) for callers with no host wiring at all.
	if wiring.permStore == nil {
		if cwd, err := os.Getwd(); err == nil {
			if lp, err := local.OpenPermStore(cwd); err == nil {
				wiring.permStore = lp
			}
		}
	}
	la = wireLocalAgent(la, localAgentWiring{
		permStore: wiring.permStore,
		permAsk:   wiring.permAsk,
		bgPermAsk: wiring.bgPermAsk,
		safetyAsk: wiring.safetyAsk,
		userAsk:   wiring.userAsk,
		bgUserAsk: wiring.bgUserAsk,
		skipPerms: wiring.skipPerms,
		permMode:  wiring.permMode,
	})
	la = la.WithSkipPermissions(skipPermissionsFor(cfg, ac))

	if dbg, err := openLocalDebugLog(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s warning: cannot open tool-agent debug log: %v\n", milkTag(), err)
	} else if dbg != nil {
		la = la.WithDebugLog(dbg)
	}

	r := newLocalRunner(la, name)
	r.mem = mem
	return r, nil
}

// getOrBuildToolRunner returns a cached TurnRunner for the named tool agent,
// building it on first use and caching it in da.toolRunners. The cache is keyed
// by (agent name, memory-visibility caller): the same peer agent invoked from
// the primary role and from the escalation role must see its invoker's
// percepts, so each combination gets its own runner (issue #172 gap 2).
func getOrBuildToolRunner(ctx context.Context, agentName string, cfg config.Config, da *dispatchAgents, mem *memory.Store, caller memory.Consumer) (TurnRunner, error) {
	cacheKey := agentName + "\x00" + string(caller)
	if da.toolRunners == nil {
		da.toolRunners = make(map[string]TurnRunner)
	}
	if tr, ok := da.toolRunners[cacheKey]; ok {
		return tr, nil
	}
	ac, ok := findAgentByName(cfg, agentName)
	if !ok {
		return nil, fmt.Errorf("tool-agent %q not found in config", agentName)
	}
	var existingMCP *mcp.ToolSet
	if da.mcpToolSets != nil {
		existingMCP = da.mcpToolSets[agentName]
	}
	tr, err := buildToolRunner(ctx, ac, cfg, existingMCP, mem, caller, da.agentWiring)
	if err != nil {
		return nil, err
	}
	da.toolRunners[cacheKey] = tr
	return tr, nil
}

// buildLocalPeerAgent returns the inference-server agent behind the named
// configured agent, built (and cached) like an agent-as-tool runner. It is
// how background jobs run on an agent other than the session's own
// primary/escalation one.
func buildLocalPeerAgent(ctx context.Context, name string, cfg config.Config, da *dispatchAgents, mem *memory.Store, caller memory.Consumer) (*local.Agent, error) {
	tr, err := getOrBuildToolRunner(ctx, name, cfg, da, mem, caller)
	if err != nil {
		return nil, err
	}
	lr, ok := tr.(*localRunner)
	if !ok {
		return nil, fmt.Errorf("not inference-server-backed")
	}
	return lr.agent, nil
}
