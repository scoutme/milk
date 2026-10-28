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

// SandboxConfig instructs a sandbox-capable adapter to run its agent against
// a throwaway state root instead of the live one: always fresh per adapter
// start (per scenario run / per repetition), so no run ever sees a previous
// run's sessions, memory, or history. Seeding with the current config keeps
// the run honest — same agent set and settings, none of the state.
type SandboxConfig struct {
	// Enabled turns the sandbox on.
	Enabled bool
	// SeedConfigPath is a config.json whose contents seed the fresh root's
	// config.json (the current agent set and settings). Empty = no seeding
	// (the agent starts from first-run defaults).
	SeedConfigPath string
	// MilkBinary optionally pins the binary to drive ("" = adapter default).
	MilkBinary string
	// AllowTools writes dangerously_skip_permissions onto every seeded agent
	// so batch runs may actually execute mutating tools (bash, edit_file, …)
	// instead of having them denied for lack of an interactive permission
	// answer. Applies to the seeded sandbox config only — the live config is
	// never touched.
	AllowTools bool
}

// Sandboxable is implemented by adapters that can isolate their agent's
// state root. The harness applies an experiment's sandbox: section through
// this interface; adapters that don't implement it (claude-code — a tmux
// session against the real ~/.claude; mimo-code — batch runs against the
// live ~/.mimocode) simply run unsandboxed.
type Sandboxable interface {
	SetSandbox(SandboxConfig)
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
