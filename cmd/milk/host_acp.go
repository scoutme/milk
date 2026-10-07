package main

import (
	"context"
	"strings"

	"github.com/scoutme/milk/internal/events"
	"github.com/scoutme/milk/internal/transport/acp"
)

// Permission option IDs for the binary allow/deny prompt makeLocalPermAsk
// drives — the only shape events.PermissionRequest's one caller produces
// today (a pre-rendered yes/no prompt, no structured Title/Subject/Options).
const (
	acpPermAllowOptionID = "allow"
	acpPermDenyOptionID  = "deny"
)

// acpHost implements events.Host for an ACP session by translating to/from
// acp.ACPHost/acp.Conn — mirrors host_tui.go's tuiHost structurally. Built
// once per ACP session (acp_session.go) and wired for local-provider agents'
// permission asks (makeLocalPermAsk) only — see
// docs/machine-readable-output-design.md's status note for why
// claude-cli-as-escalation isn't wired through this.
type acpHost struct {
	host *acp.ACPHost
	// callID returns the tool call a permission prompt for tool is about
	// ("" if unknown); failed is told when the request itself could not be
	// delivered or answered. Both are optional.
	callID func(tool string) acp.ToolCallID
	failed func(tool string, err error)
}

func newACPHost(conn acp.Conn, sessionID acp.SessionID) *acpHost {
	return &acpHost{host: acp.NewACPHost(conn, sessionID)}
}

var _ events.Host = (*acpHost)(nil)

// Notify maps a plain events.Notification onto milk's _milk/notification
// ExtNotification channel.
func (h *acpHost) Notify(n events.Notification) {
	h.host.Notify(acp.NotificationExt(acp.NotificationPayload{
		Severity:    "info",
		CommandHint: n.CommandHint,
		Body:        n.Text,
	}))
}

// RequestPermission turns a permission ask into a session/request_permission
// prompt: title "Allow <tool>?" with the call summary as its description and
// the pending tool call as its subject. (The TUI's rendered Prompt, with its
// ANSI codes and "[Y/n]", is only used when no structured tool is supplied.)
// A request the client cannot answer is reported through failed and treated
// as a denial.
func (h *acpHost) RequestPermission(ctx context.Context, req events.PermissionRequest) (events.PermissionOutcome, error) {
	title, desc := strings.TrimSpace(stripANSI(req.Prompt)), ""
	var callID acp.ToolCallID
	if req.Tool != "" {
		title, desc = "Allow "+req.Tool+"?", stripANSI(req.Summary)
		callID = acp.ToolCallID(req.ToolCallID)
		if callID == "" && h.callID != nil {
			callID = h.callID(req.Tool)
		}
	}
	outcome, err := h.host.RequestPermission(ctx, acp.PermissionRequest{
		Title:       title,
		Description: desc,
		ToolCallID:  callID,
		Options: []acp.PermissionOption{
			{OptionID: acpPermAllowOptionID, Name: "Allow", Kind: acp.PermissionAllowOnce},
			{OptionID: acpPermDenyOptionID, Name: "Deny", Kind: acp.PermissionRejectOnce},
		},
	})
	if err != nil {
		if h.failed != nil {
			h.failed(req.Tool, err)
		}
		return events.PermissionOutcome{}, err
	}
	if outcome.Cancelled {
		return events.PermissionOutcome{Cancelled: true}, nil
	}
	return events.PermissionOutcome{Allow: outcome.OptionID == acpPermAllowOptionID}, nil
}

// Elicit is the events.Host line-input seam and is still not wired (see the
// design doc's status note) — it returns a cancelled result rather than
// blocking on a round trip nothing drives. Note the setup wizard's form
// dialogs do use elicitation, but the form-capable acp.ACPHost.Elicit
// directly (acp_initwizard.go): this seam's Prompt/Label shape cannot
// express a form schema.
func (h *acpHost) Elicit(ctx context.Context, req events.ElicitationRequest) (events.ElicitationResult, error) {
	return events.ElicitationResult{Cancelled: true}, nil
}

// State is a no-op this round, matching tuiHost — see internal/events's
// package doc for why.
func (h *acpHost) State(events.StateUpdate) {}
