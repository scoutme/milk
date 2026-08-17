package eval

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// AgentAdapter wraps a terminal-based agent for automated evaluation.
type AgentAdapter interface {
	// Name returns the display name (e.g. "milk-tui", "claude-code").
	Name() string

	// SetArgs passes extra arguments from the --agents spec (e.g. ["--agent", "mimo-local"]).
	// Called by the harness after Get() and before Start().
	SetArgs(args []string)

	// Start launches the agent in a tmux session with the given working directory.
	Start(ctx context.Context, workdir string) error

	// RunPrompt sends a prompt and waits for the agent's complete response.
	// Returns a RunResult with per-turn TokenUsage (snapshot-diff or direct read).
	RunPrompt(ctx context.Context, prompt string) (RunResult, error)

	// Stop kills the tmux session and cleans up.
	Stop() error
}

// AdapterFactory creates a new adapter instance.
type AdapterFactory func() AgentAdapter

// AdapterRegistry maps adapter names to their factory functions.
type AdapterRegistry = map[string]AdapterFactory

// registry is the global adapter registry.
var registry = AdapterRegistry{}

// Register adds an adapter factory under the given name.
// Call from init() in each adapter file.
func Register(name string, factory AdapterFactory) {
	registry[name] = factory
}

// Get returns a new adapter instance by name, or an error if not found.
func Get(name string) (AgentAdapter, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("adapter %q not registered", name)
	}
	return f(), nil
}

// List returns all registered adapter names in sorted order.
func List() []string {
	names := make([]string, 0, len(registry))
	for k := range registry {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// ParseAdapterSpec parses an adapter spec string into name and extra args.
// Supported formats:
//
//	"milk-tui"                          → name="milk-tui", args=[]
//	"milk-tui[--agent,mimo-local]"      → name="milk-tui", args=["--agent","mimo-local"]
//	"claude-code[--verbose]"            → name="claude-code", args=["--verbose"]
func ParseAdapterSpec(spec string) (name string, args []string) {
	// Check for bracket syntax: name[args]
	if idx := strings.Index(spec, "["); idx > 0 && strings.HasSuffix(spec, "]") {
		name = spec[:idx]
		inner := spec[idx+1 : len(spec)-1]
		if inner != "" {
			args = strings.Split(inner, ",")
			for i := range args {
				args[i] = strings.TrimSpace(args[i])
			}
		}
		return name, args
	}
	return spec, nil
}
