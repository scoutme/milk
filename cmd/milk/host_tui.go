package main

import (
	"context"
	"strings"

	"github.com/scoutme/milk/internal/events"
)

// tuiHost implements events.Host for the TUI by wrapping the existing,
// unchanged tuiInputReader/m.notify machinery — Phase 1 of
// docs/machine-readable-output-design.md. See internal/events's package doc
// for scope: State is a deliberate no-op here (see the design doc's Phase 1
// status note for why), and Notify/Elicit are exercised only by tests today
// — makeLocalPermAsk's RequestPermission call is the one production path
// wired through this in Phase 1.
type tuiHost struct {
	ir *tuiInputReader
}

func newTUIHost(ir *tuiInputReader) *tuiHost { return &tuiHost{ir: ir} }

var _ events.Host = (*tuiHost)(nil)

// Notify sends a notifyMsg for the Update() loop to display as a toast via
// the existing m.notify — see notifyMsg's doc comment.
func (h *tuiHost) Notify(n events.Notification) {
	h.ir.send(notifyMsg{text: n.Text, hint: n.CommandHint})
}

// RequestPermission asks a yes/no question via the existing permRequestMsg
// flow. req.Prompt is rendered verbatim; a blank or "y"/"Y" answer allows.
func (h *tuiHost) RequestPermission(ctx context.Context, req events.PermissionRequest) (events.PermissionOutcome, error) {
	answer, err := h.ir.readLine(req.Prompt)
	if err != nil {
		return events.PermissionOutcome{}, err
	}
	allow := answer == "" || strings.EqualFold(answer, "y")
	return events.PermissionOutcome{Allow: allow}, nil
}

// Elicit collects one line of structured input via the existing
// permRequestMsg flow, labeled per req.Label.
func (h *tuiHost) Elicit(ctx context.Context, req events.ElicitationRequest) (events.ElicitationResult, error) {
	line, err := h.ir.readLineLabeled(req.Prompt, req.Label)
	if err != nil {
		return events.ElicitationResult{}, err
	}
	return events.ElicitationResult{Value: line}, nil
}

// State is a deliberate no-op in Phase 1 — see internal/events's package doc
// and the design doc's Phase 1 status note for why wiring m.busy/
// m.pendingPerm into this now would be premature.
func (h *tuiHost) State(events.StateUpdate) {}
