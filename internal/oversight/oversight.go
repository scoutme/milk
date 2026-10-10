// Package oversight defines the Notifier interface for remote oversight
// of agent turns and permission prompts.
package oversight

import "context"

// PermDecision is the result of a remote permission query.
type PermDecision int

const (
	PermAllow PermDecision = iota
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

	// NotifyToolResult fires when a tool call completes, forwarding a
	// truncated summary of its output. isError reports whether the tool
	// itself reported a failure (not a transport-level error).
	NotifyToolResult(ctx context.Context, toolName, summary string, isError bool)

	// NotifyTurnDone fires when a turn completes (or errors).
	NotifyTurnDone(ctx context.Context, agent string, err error)

	// NotifyResponse forwards the agent's final response text.
	NotifyResponse(ctx context.Context, agent, text string)

	// AskPermission sends a permission request to the remote interface and
	// waits for a reply. answered reports whether a human actually replied;
	// a timeout — the backend's own prompt timeout or ctx cancellation — is
	// not an answer (see ADR-0052). On silence, decision carries the
	// backend's configured timeout action, and the caller decides what
	// silence means: the timeout action for foreground asks, the timed
	// default for background ones.
	AskPermission(ctx context.Context, req PermRequest) (decision PermDecision, answered bool)
}

// Noop is a no-op Notifier used when remote oversight is disabled.
type Noop struct{}

func (Noop) NotifyTurnStart(_ context.Context, _, _, _ string)       {}
func (Noop) NotifyToolUse(_ context.Context, _, _ string)            {}
func (Noop) NotifyToolResult(_ context.Context, _, _ string, _ bool) {}
func (Noop) NotifyTurnDone(_ context.Context, _ string, _ error)     {}
func (Noop) NotifyResponse(_ context.Context, _, _ string)           {}
func (Noop) AskPermission(_ context.Context, _ PermRequest) (PermDecision, bool) {
	// Nobody to answer: silence, with the timeout-action-shaped decision the
	// callers fall back to for foreground asks (deny, like the default
	// timeout action).
	return PermDeny, false
}
