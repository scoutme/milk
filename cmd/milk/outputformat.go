package main

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/streamjson"
	"github.com/scoutme/milk/internal/workflow"
)

// outputFormat is the validated value of --output-format. See
// docs/internal/design/machine-readable-output-design.md §5/§6 and this file's accompanying
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

// trailToolInput renders one trail call's arguments as the tool_use block's
// free-form input payload. The stored trail may carry plain text (non-JSON
// arguments are cut as plain text at capture) — marshal those as a JSON
// string so the stream stays parseable (eval's parseMilkStream rejects
// invalid JSON in tool input outright).
func trailToolInput(args string) json.RawMessage {
	if args == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(args)) {
		return json.RawMessage(args)
	}
	b, _ := json.Marshal(args) //nolint:errcheck // string always marshals
	return b
}

// trailToolResult renders one trail call's result as the tool_result block's
// free-form content payload (a JSON string), or nil when the result was
// cleared at capture (trail budget) — an absent payload is honest, a
// fabricated one is not.
func trailToolResult(result string, cleared bool) json.RawMessage {
	if cleared {
		return nil
	}
	b, _ := json.Marshal(result) //nolint:errcheck // string always marshals
	return b
}

// buildToolTrailEvents expands a turn's tool trail (session.TrailStep, the
// tool activity local-provider agents record per turn) into the §6.2 message
// pairs the event catalog expects for tool activity: one assistant message
// per tool-calling iteration — its narration text plus one tool_use block per
// call — followed by one user message carrying the matching tool_result
// blocks. Call IDs may be absent in the stored trail (older turns, providers
// without ids); synthetic per-event ids keep tool_use/tool_result pairing
// intact without inventing provenance. This is post-hoc reconstruction from
// the recorded trail, not live mid-turn streaming — the turn's final-text
// assistant event and the result event follow after these.
func buildToolTrailEvents(sessionID string, trail []session.TrailStep) []*streamjson.Event {
	var events []*streamjson.Event
	seq := 0
	for _, step := range trail {
		if len(step.Calls) == 0 {
			continue
		}
		var blocks []streamjson.ContentBlock
		if step.Text != "" {
			blocks = append(blocks, streamjson.ContentBlock{Type: "text", Text: step.Text})
		}
		type paired struct {
			id      string
			result  json.RawMessage
			cleared bool
		}
		var pairs []paired
		for _, call := range step.Calls {
			seq++
			id := call.ID
			if id == "" {
				id = fmt.Sprintf("tool_%d", seq)
			}
			blocks = append(blocks, streamjson.ContentBlock{
				Type:  "tool_use",
				ID:    id,
				Name:  call.Name,
				Input: trailToolInput(call.Args),
			})
			pairs = append(pairs, paired{id: id, result: trailToolResult(call.Result, call.Cleared), cleared: call.Cleared})
		}

		asst := &streamjson.Event{Type: streamjson.TypeAssistant, SessionID: sessionID}
		_ = asst.SetMessageObject(&streamjson.Message{Role: "assistant", Content: blocks}) //nolint:errcheck // Message always marshals
		events = append(events, asst)

		var resultBlocks []streamjson.ContentBlock
		for _, p := range pairs {
			resultBlocks = append(resultBlocks, streamjson.ContentBlock{
				Type:      "tool_result",
				ToolUseID: p.id,
				Content:   p.result,
			})
		}
		user := &streamjson.Event{Type: streamjson.TypeUser, SessionID: sessionID}
		_ = user.SetMessageObject(&streamjson.Message{Role: "user", Content: resultBlocks}) //nolint:errcheck // Message always marshals
		events = append(events, user)
	}
	return events
}

// newTurnTrails collects the tool trails of every assistant turn appended to
// the session during this run (history[start:] — one per dispatch hop when a
// self-escalation added both a primary and an escalation turn), in order.
func newTurnTrails(sess *session.Session, start int) []session.TrailStep {
	var trail []session.TrailStep
	if sess == nil {
		return nil
	}
	for _, t := range sess.History[min(start, len(sess.History)):] {
		if t.Role == session.RoleAssistant {
			trail = append(trail, t.Trail...)
		}
	}
	return trail
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

// timingDelta sums throughput timing (see session.TokenUsage) across the
// changed entries of a TokensSnapshot diff, mirroring tokenUsageDelta's
// diff semantics: entries whose timing didn't change contribute nothing,
// and negative deltas (a session swap between the snapshots) are clamped
// away rather than dragged into the aggregate.
func timingDelta(before, after map[string]session.TokenUsage) (ttftSec, decodeSec float64, reqs int64) {
	for key, a := range after {
		b := before[key]
		dTTFT := a.TTFTSeconds - b.TTFTSeconds
		dDecode := a.DecodeSeconds - b.DecodeSeconds
		dReqs := a.Requests - b.Requests
		if dTTFT <= 0 && dDecode <= 0 && dReqs <= 0 {
			continue
		}
		ttftSec += max(dTTFT, 0)
		decodeSec += max(dDecode, 0)
		reqs += max(dReqs, 0)
	}
	return ttftSec, decodeSec, reqs
}

// responseCapture collects a one-shot turn's response text for the
// machine-readable formats (text mode keeps the raw stdout passthrough
// instead). onSegment receives each contiguous chunk as it streams — so a
// turn killed by the --turn-timeout budget keeps its partial text — and
// onResponse receives the final full text on success, replacing the
// accumulated chunks.
type responseCapture struct {
	text string
}

func (c *responseCapture) onSegment(chunk string) { c.text += chunk }

func (c *responseCapture) onResponse(final string) { c.text = final }

// oneShotResultText is the terminal result line's text: the captured
// response, falling back to the turn error when the turn failed before any
// text streamed (a budget kill with zero output still needs a diagnosable
// result line — the design's failed-turn example puts the error message
// there, and --output-format json has no separate error banner line).
func oneShotResultText(captured string, turnErr error) string {
	if captured == "" && turnErr != nil {
		return turnErr.Error()
	}
	return captured
}

// buildErrorEvent builds the system/error banner emitted before the terminal
// result when a one-shot turn fails (design §6.3: is_error, message,
// recoverable — "error banner (terminal result still emitted)"). recoverable
// mirrors milk's own retry classification: transient HTTP/2 stream failures
// may succeed on a retry as-is; a budget expiry or context cancellation will
// not (workflow.IsRetryableTurnError).
func buildErrorEvent(sessionID string, turnErr error) *streamjson.Event {
	isErr := true
	recoverable := workflow.IsRetryableTurnError(turnErr)
	ev := &streamjson.Event{
		Type:        streamjson.TypeSystem,
		Subtype:     streamjson.SubtypeError,
		SessionID:   sessionID,
		IsError:     &isErr,
		Recoverable: &recoverable,
	}
	_ = ev.SetMessageText(turnErr.Error()) // string form of a Go error always marshals
	return ev
}

// emitOneShotOutcome writes a finished one-shot turn's terminal sequence for
// --output-format stream-json: the assistant message whenever any text was
// captured (including a killed turn's partial stream), the system/error
// banner when the turn failed, and exactly one result event last.
func emitOneShotOutcome(enc *streamjson.Encoder, sessionID string, target router.Target, turnErr error, captured string, durationMS, numTurns int64, before, after map[string]session.TokenUsage) {
	if captured != "" {
		emit(enc, buildAssistantEvent(sessionID, captured))
	}
	if turnErr != nil {
		emit(enc, buildErrorEvent(sessionID, turnErr))
	}
	emit(enc, buildResultEvent(sessionID, target, turnErr, oneShotResultText(captured, turnErr), durationMS, numTurns, before, after))
}

// buildResultEvent builds the terminal, always-exactly-one result line
// (design §6.4). Scope note: NumTurns reflects the turn's real model
// iterations (1 + the tool-calling iterations recorded in the trail — see
// buildToolTrailEvents) and RouteHistory is a single hop — this run has no
// role-aware signal to detect a mid-run self-escalation hand-off (see this
// file's companion status note).
func buildResultEvent(sessionID string, target router.Target, turnErr error, lastText string, durationMS int64, numTurns int64, before, after map[string]session.TokenUsage) *streamjson.Event {
	isError := turnErr != nil
	subtype := streamjson.ResultSuccess
	stopReason := "end_turn"
	if isError {
		subtype = streamjson.ResultErrorDuringExecution
		stopReason = ""
	}
	if numTurns < 1 {
		numTurns = 1
	}

	usage, modelUsage := tokenUsageDelta(before, after)
	ttftSec, decodeSec, reqs := timingDelta(before, after)

	ev := &streamjson.Event{
		Type:         streamjson.TypeResult,
		Subtype:      subtype,
		SessionID:    sessionID,
		IsError:      &isError,
		NumTurns:     numTurns,
		DurationMS:   durationMS,
		Result:       lastText,
		StopReason:   stopReason,
		RouteHistory: []streamjson.RouteHop{{Turn: 0, Target: wireTarget(target)}},
		Usage:        usage,
		ModelUsage:   modelUsage,
	}
	// Throughput of the turn's generation window (see session.TokenUsage):
	// omitted rather than zeroed when the providers involved reported no
	// timing — a fabricated 0.0 tok/s would be indistinguishable from a stall.
	if decodeSec > 0 && usage.OutputTokens > 0 {
		ev.OutputTokensPerSecond = float64(usage.OutputTokens) / decodeSec
	}
	if reqs > 0 {
		ev.TTFTMS = int64(math.Round(1000 * ttftSec / float64(reqs)))
	}
	return ev
}
