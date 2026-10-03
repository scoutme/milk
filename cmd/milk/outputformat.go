package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/streamjson"
)

// outputFormat is the validated value of --output-format. See
// docs/machine-readable-output-design.md §5/§6 and this file's accompanying
// Phase (batch mode) status note for scope — this is intentionally a narrow
// slice of the full event catalog, not full fidelity.
type outputFormat string

const (
	formatText       outputFormat = "text"
	formatJSON       outputFormat = "json"
	formatStreamJSON outputFormat = "stream-json"
)

// parseOutputFormat validates the raw --output-format flag value.
func parseOutputFormat(s string) (outputFormat, error) {
	switch outputFormat(s) {
	case formatText, formatJSON, formatStreamJSON:
		return outputFormat(s), nil
	default:
		return "", fmt.Errorf("invalid --output-format %q: must be text, json, or stream-json", s)
	}
}

// wireTarget maps router.Target's internal Go values ("local"/"escalation")
// onto the design doc's wire vocabulary ("primary"/"escalation") — the two
// do not match for the local/primary case, and reusing router.Target's own
// string value on the wire would be wrong.
func wireTarget(t router.Target) string {
	if t == router.TargetLocal {
		return "primary"
	}
	return "escalation"
}

// nowTS returns the current time in the RFC 3339 form every event's ts field
// uses. Centralized here because streamjson.Encoder.Encode auto-stamps seq
// but never ts — callers must set it themselves before Encode.
func nowTS() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// emit stamps ts and encodes ev, ignoring the write error (ev's wire write
// is to stdout; nothing meaningful can be done about a failed write there).
func emit(enc *streamjson.Encoder, ev *streamjson.Event) {
	ev.TS = nowTS()
	_ = enc.Encode(ev) //nolint:errcheck
}

// buildInitEvent builds the first line of a stream-json run (design §6.1).
// Scope note: Tools and MCPServers are omitted (no cheap call site at this
// boundary enumerates them); SessionState is "idle" (the run hasn't started
// yet); Capabilities is honestly just stream_v1 — no partial-message deltas,
// no task/workflow events are emitted by this run.
func buildInitEvent(sessionID, cwd string, cfg config.Config, decision router.Decision, target router.Target, warnings []string) *streamjson.Event {
	primary := cfg.ActiveAgent()
	agentInfo := streamjson.AgentInfo{
		Name:                primary.Name,
		Provider:            primary.Provider,
		Model:               primary.Model,
		Role:                "primary",
		ContextWindowTokens: int64(cfg.AgentContextWindowTokens(primary)),
	}
	agentJSON, _ := json.Marshal(agentInfo) //nolint:errcheck // AgentInfo always marshals

	esc := cfg.EscalationAgentConfig()

	return &streamjson.Event{
		Type:        streamjson.TypeSystem,
		Subtype:     streamjson.SubtypeInit,
		SessionID:   sessionID,
		CWD:         cwd,
		MilkVersion: version,
		Agent:       agentJSON,
		EscalationAgent: &streamjson.AgentInfo{
			Name:     esc.Name,
			Provider: esc.Provider,
			Model:    esc.Model,
		},
		Route: &streamjson.RouteDecision{
			Target:     wireTarget(target),
			Reason:     decision.Reason,
			Conclusive: decision.Conclusive,
		},
		SessionState: "idle",
		Warnings:     warnings,
		Capabilities: []string{streamjson.CapabilityStream},
	}
}

// buildAssistantEvent builds the one completed assistant message emitted for
// a turn's final text. Scope note: Agent (the role-label dual-form) is
// deliberately omitted — nothing at this call site can tell "primary's own
// response" apart from "escalation's response forwarded through primary's
// self-escalation path," and mislabeling it would be worse than omitting it.
func buildAssistantEvent(sessionID, text string) *streamjson.Event {
	ev := &streamjson.Event{
		Type:      streamjson.TypeAssistant,
		SessionID: sessionID,
	}
	_ = ev.SetMessageObject(&streamjson.Message{ //nolint:errcheck // Message always marshals
		Role:    "assistant",
		Content: []streamjson.ContentBlock{{Type: "text", Text: text}},
	})
	return ev
}

// tokenUsageDelta diffs two TokensSnapshot() calls into an aggregate Usage
// plus a per-model breakdown. A diff (not raw totals) is required because
// sess persists across invocations when resuming a session — raw totals
// would double-count prior turns. Entries with no change are dropped rather
// than emitted as spurious zero-usage rows for models untouched this turn.
func tokenUsageDelta(before, after map[string]session.TokenUsage) (*streamjson.Usage, map[string]*streamjson.Usage) {
	total := &streamjson.Usage{}
	perModel := map[string]*streamjson.Usage{}
	for key, a := range after {
		b := before[key] // zero value when the key is new this turn
		dPrompt := a.Prompt - b.Prompt
		dCompletion := a.Completion - b.Completion
		dCacheRead := a.CacheRead - b.CacheRead
		dCacheCreation := a.CacheCreation - b.CacheCreation
		if dPrompt == 0 && dCompletion == 0 && dCacheRead == 0 && dCacheCreation == 0 {
			continue
		}
		total.InputTokens += dPrompt
		total.OutputTokens += dCompletion
		total.CacheRead += dCacheRead
		total.CacheCreation += dCacheCreation

		m := perModel[a.Model]
		if m == nil {
			m = &streamjson.Usage{}
			perModel[a.Model] = m
		}
		m.InputTokens += dPrompt
		m.OutputTokens += dCompletion
		m.CacheRead += dCacheRead
		m.CacheCreation += dCacheCreation
	}
	return total, perModel
}

// buildResultEvent builds the terminal, always-exactly-one result line
// (design §6.4). Scope note: NumTurns is always 1 and RouteHistory is a
// single hop — this run has no role-aware signal to detect a mid-run
// self-escalation hand-off (see this file's companion status note).
func buildResultEvent(sessionID string, target router.Target, turnErr error, lastText string, durationMS int64, before, after map[string]session.TokenUsage) *streamjson.Event {
	isError := turnErr != nil
	subtype := streamjson.ResultSuccess
	stopReason := "end_turn"
	if isError {
		subtype = streamjson.ResultErrorDuringExecution
		stopReason = ""
	}

	usage, modelUsage := tokenUsageDelta(before, after)

	return &streamjson.Event{
		Type:         streamjson.TypeResult,
		Subtype:      subtype,
		SessionID:    sessionID,
		IsError:      &isError,
		NumTurns:     1,
		DurationMS:   durationMS,
		Result:       lastText,
		StopReason:   stopReason,
		RouteHistory: []streamjson.RouteHop{{Turn: 0, Target: wireTarget(target)}},
		Usage:        usage,
		ModelUsage:   modelUsage,
	}
}
