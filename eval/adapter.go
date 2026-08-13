package eval

import (
	"context"
	"fmt"
	"sort"
)

// AgentAdapter wraps a terminal-based agent for automated evaluation.
type AgentAdapter interface {
	// Name returns the display name (e.g. "milk-tui", "claude-code").
	Name() string

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
