package main

import (
	"context"

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
}

func newACPHost(conn acp.Conn, sessionID acp.SessionID) *acpHost {
	return &acpHost{host: acp.NewACPHost(conn, sessionID)}
}

var _ events.Host = (*acpHost)(nil)

// Notify maps a plain events.Notification onto milk's milk/notification
// ExtNotification channel.
func (h *acpHost) Notify(n events.Notification) {
	h.host.Notify(acp.NotificationExt(acp.NotificationPayload{
		Severity:    "info",
		CommandHint: n.CommandHint,
		Body:        n.Text,
	}))
}

// RequestPermission synthesizes a binary allow/deny ACP permission prompt
// from req.Prompt — events.PermissionRequest's one caller (makeLocalPermAsk)
// only ever supplies a rendered prompt string, no structured Title/Subject/
// Options, so this is the one place that structure gets invented.
func (h *acpHost) RequestPermission(ctx context.Context, req events.PermissionRequest) (events.PermissionOutcome, error) {
	outcome, err := h.host.RequestPermission(ctx, acp.PermissionRequest{
		Title: req.Prompt,
		Options: []acp.PermissionOption{
			{OptionID: acpPermAllowOptionID, Name: "Allow", Kind: acp.PermissionAllowOnce},
			{OptionID: acpPermDenyOptionID, Name: "Deny", Kind: acp.PermissionRejectOnce},
		},
	})
	if err != nil {
		return events.PermissionOutcome{}, err
	}
	if outcome.Cancelled {
		return events.PermissionOutcome{Cancelled: true}, nil
	}
	return events.PermissionOutcome{Allow: outcome.OptionID == acpPermAllowOptionID}, nil
}

// Elicit is not wired this round (see the design doc's status note) —
// returns a cancelled result rather than blocking on a round trip nothing
// drives yet.
func (h *acpHost) Elicit(ctx context.Context, req events.ElicitationRequest) (events.ElicitationResult, error) {
	return events.ElicitationResult{Cancelled: true}, nil
}

// State is a no-op this round, matching tuiHost — see internal/events's
// package doc for why.
func (h *acpHost) State(events.StateUpdate) {}
