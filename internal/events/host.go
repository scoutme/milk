// Package events defines Host — the host-agnostic interface for milk's
// interactive surfaces (notifications, permission prompts, structured user
// input, session state) per docs/machine-readable-output-design.md §8.1.
// Today it has exactly one implementation (cmd/milk's tuiHost, wrapping the
// existing TUI machinery unchanged) and exactly one real caller.
//
// This package is deliberately minimal: it carries only what Host's four
// methods need, not the full §6 content-event union (text/reasoning deltas,
// tool lifecycle, usage). That catalog has no consumer yet — it belongs to
// whichever later phase first needs it (ACP wiring or the batch-mode
// emitter), built against that consumer's actual requirements rather than
// guessed at here. See docs/machine-readable-output-design.md's Phase 1
// status note for the full scope rationale.
//
// internal/transport/acp already defines its own Host interface, but every
// one of its parameter types is ACP-wire-shaped (ElicitationSchema,
// PermissionOptionKind, a SessionUpdate discriminator) — a TUI implementing
// it would have to speak ACP vocabulary. This package is the host-agnostic
// interface that was actually missing; internal/transport/acp.Host remains
// untouched as a future translation target (canonical events in, ACP
// session/update kinds out), not something this package reuses or extends.
package events

import "context"

// Notification is a turn-unrelated informational event (ADR-0048 toasts
// today). CommandHint, when non-empty, is the slash command a user would
// type to act on or learn more about the notification.
type Notification struct {
	Text        string
	CommandHint string
}

// SessionState is the coarse running/idle/needs-input classification of a
// session. Intentionally not wired to any production state in Phase 1 — see
// the design doc's Phase 1 status note for why.
type SessionState string

const (
	StateRunning        SessionState = "running"
	StateIdle           SessionState = "idle"
	StateRequiresAction SessionState = "requires_action"
)

// StateUpdate carries a session's current state. StopReason is set only when
// State is StateIdle.
type StateUpdate struct {
	State      SessionState
	StopReason string
}

// PermissionOptionKind mirrors ACP's PermissionOptionKind vocabulary
// (allow_once|allow_always|reject_once|reject_always per design §7.1) so a
// future structured host can reuse it without a translation table. Phase 1's
// only caller doesn't populate Options at all.
type PermissionOptionKind string

const (
	PermissionAllowOnce    PermissionOptionKind = "allow_once"
	PermissionAllowAlways  PermissionOptionKind = "allow_always"
	PermissionRejectOnce   PermissionOptionKind = "reject_once"
	PermissionRejectAlways PermissionOptionKind = "reject_always"
)

// PermissionOption is one selectable choice in a PermissionRequest.
type PermissionOption struct {
	ID    string
	Label string
	Kind  PermissionOptionKind
}

// PermissionRequest asks the host to approve or deny an action. Prompt is a
// fully-rendered, ready-to-display string — Phase 1's hosts render it
// verbatim rather than reconstructing it from Title/Subject/Options, which
// exist for a future structured host (e.g. an ACP client rendering its own
// permission UI) and may be empty today.
type PermissionRequest struct {
	Prompt  string
	Title   string
	Subject string
	Options []PermissionOption
}

// PermissionOutcome is the host's answer to a PermissionRequest.
type PermissionOutcome struct {
	Allow     bool
	OptionID  string
	Cancelled bool
}

// ElicitationRequest asks the host to collect a line of structured input
// from the user. Prompt is fully-rendered; Label distinguishes the prompt's
// purpose for hosts that render prompts differently depending on it (the TUI
// uses this to show e.g. "[select]" instead of a plain y/n hint).
type ElicitationRequest struct {
	Prompt string
	Label  string
}

// ElicitationResult is the user's raw input in response to an
// ElicitationRequest.
type ElicitationResult struct {
	Value     string
	Cancelled bool
}

// Host is the interface every interactive surface in milk's turn-execution
// path should go through instead of writing directly to a terminal or
// mutating UI state inline. See the package doc for current scope.
type Host interface {
	// Notify surfaces a turn-unrelated informational event.
	Notify(Notification)
	// RequestPermission asks for approval to perform an action and blocks
	// until the host answers (or ctx is cancelled).
	RequestPermission(ctx context.Context, req PermissionRequest) (PermissionOutcome, error)
	// Elicit asks the user for a line of structured input and blocks until
	// the host answers (or ctx is cancelled).
	Elicit(ctx context.Context, req ElicitationRequest) (ElicitationResult, error)
	// State reports the session's current running/idle/needs-input state.
	State(StateUpdate)
}
