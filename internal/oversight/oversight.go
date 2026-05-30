// Package oversight defines the Notifier interface for remote oversight
// of agent turns and permission prompts.
package oversight

import "context"

// PermDecision is the result of a remote permission query.
type PermDecision int

const (
	PermAllow  PermDecision = iota
	PermDeny
	PermTimeout // remote did not respond within deadline
)

// PermRequest carries the details of a pending permission prompt.
type PermRequest struct {
	ToolName    string
	Input       string // human-readable one-liner (from cliToolArgSummary)
	Description string
	BlockedPath string
}

// Notifier is the interface for remote oversight backends.
// All methods are best-effort — implementations must not block the caller
// on network errors and must return quickly.
type Notifier interface {
	// NotifyTurnStart fires when a turn is dispatched to an agent.
	// agent is the display name; target is "local" or "escalation".
	NotifyTurnStart(ctx context.Context, agent, target, prompt string)

	// NotifyToolUse fires when the escalation agent begins a tool call.
	NotifyToolUse(ctx context.Context, toolName, summary string)

	// NotifyTurnDone fires when a turn completes (or errors).
	NotifyTurnDone(ctx context.Context, agent string, err error)

	// AskPermission sends a permission request to the remote interface and
	// waits for an allow/deny reply up to the configured timeout.
	// Returns PermTimeout when the deadline expires before a reply arrives.
	AskPermission(ctx context.Context, req PermRequest) PermDecision
}

// Noop is a no-op Notifier used when remote oversight is disabled.
type Noop struct{}

func (Noop) NotifyTurnStart(_ context.Context, _, _, _ string) {}
func (Noop) NotifyToolUse(_ context.Context, _, _ string)      {}
func (Noop) NotifyTurnDone(_ context.Context, _ string, _ error) {}
func (Noop) AskPermission(_ context.Context, _ PermRequest) PermDecision {
	return PermAllow
}
