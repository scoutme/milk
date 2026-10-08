package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/loop"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/transport/acp"
)

// Routing decisions and loop/consumption warnings reach the ACP client twice:
// as plain message text (every client renders that) and as the matching
// _milk/* custom notification (machine-readable; clients that don't know it
// ignore it, as ACP requires). The route is also attached as _meta to the
// turn's closing state_update.

// sumTokens totals prompt and completion tokens across every model/role the
// session has used, for per-turn deltas.
func (as *acpSession) sumTokens() (prompt, completion int64) {
	for _, u := range as.sess.TokensSnapshot() {
		prompt += int64(u.Prompt)
		completion += int64(u.Completion)
	}
	return prompt, completion
}

// announceRoute publishes the routing decision for the turn about to run and
// returns the _meta snapshot for the closing state_update. The message line
// is only sent when the handling target changes from the previous model turn,
// so a steady session isn't narrated on every prompt.
func (as *acpSession) announceRoute(d router.Decision, target router.Target, agent string) map[string]any {
	n := (&acp.Mapper{Session: as.id}).Route(d)
	if p, ok := n.Params.(acp.ExtNotification); ok {
		if rp, ok := p.Params.(acp.RoutePayload); ok {
			rp.Agent = agent
			n = (&acp.Mapper{Session: as.id}).Ext(acp.RouteExt(rp))
		}
	}
	as.conn.Notify(n.Method, n.Params) //nolint:errcheck // stdout write; nothing meaningful to do with the error

	as.loopMu.Lock()
	changed := as.lastTarget != "" && as.lastTarget != target
	as.lastTarget = target
	as.loopMu.Unlock()
	if changed {
		line := fmt.Sprintf("→ now handled by %s (%s)", agent, target)
		if d.Reason != "" {
			line += ": " + d.Reason
		}
		as.notify(acp.AgentMessageChunk(as.liveID("route"), line))
	}
	return map[string]any{"milk/route": map[string]any{
		"agent": agent, "target": string(target), "reason": d.Reason,
	}}
}

// surfaceVerdicts reports loop-detector verdicts to the client and, for an
// auto-interrupt verdict, cancels the running turn.
func (as *acpSession) surfaceVerdicts(vs []loop.Verdict) {
	for _, v := range vs {
		if v.Confidence < 0.5 {
			continue
		}
		msg := v.Message
		as.loopMu.Lock()
		pinned := as.st.stickyEscalate
		as.loopMu.Unlock()
		if v.Signal == loop.SignalTokenVelocity && !pinned {
			msg += " — consider /escalate"
		}
		line := strings.TrimSpace(loopSignalLine(v, msg))
		if v.ShouldInterrupt {
			line += " — turn auto-interrupted"
		}
		as.notify(acp.AgentMessageChunk(as.liveID("warn"), line))
		n := (&acp.Mapper{Session: as.id}).LoopWarning(v, 0, 0)
		as.conn.Notify(n.Method, n.Params) //nolint:errcheck // see above
		if v.ShouldInterrupt {
			as.mu.Lock()
			cancel := as.cancel
			as.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	}
}

// feedThinking runs a streamed reasoning chunk through the loop detector.
// Workflow steps are skipped: reasoning legitimately repeats across their
// passes, and the workflow interpreter handles recovery for those itself.
func (as *acpSession) feedThinking(text string) {
	if as.loop == nil || as.workflowRunning.Load() {
		return
	}
	as.loopMu.Lock()
	vs := as.loop.FeedReasoningChunk(text)
	as.loopMu.Unlock()
	as.surfaceVerdicts(vs)
}

// endTurnSignals feeds the finished turn to the cross-turn detectors
// (token velocity, silent burn, turn flood, repetition across turns).
func (as *acpSession) endTurnSignals(promptBefore, completionBefore int64) {
	if as.loop == nil {
		return
	}
	var text, reasoning string
	if hist := as.sess.History; len(hist) > 0 {
		text, reasoning = hist[len(hist)-1].Content, hist[len(hist)-1].Thinking
	}
	p, c := as.sumTokens()
	as.loopMu.Lock()
	vs := as.loop.Feed(loop.TurnSummary{
		Text:          text,
		ReasoningText: reasoning,
		InputTokens:   p - promptBefore,
		OutputTokens:  c - completionBefore,
		Timestamp:     time.Now(),
	})
	as.loopMu.Unlock()
	as.surfaceVerdicts(vs)
}
