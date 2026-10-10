package main

// The permission ask layer (ADR-0052): every permission ask is a race over up
// to three answer sources — direct input (the TUI prompt queue / the ACP
// client's dialog), remote oversight (Telegram), and, for background asks
// only, a *timed answer* that applies the configured default at a deadline.
// First real answer wins; silence is never itself an answer except through
// the timed answer, the one source allowed to speak for nobody. Background
// asks surface everywhere (main TUI included) instead of failing closed, and
// the deadline guarantees an answer within a bounded time.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/oversight"
)

// oversightPermMu serializes the *remote* side of permission asks
// process-wide: the Telegram backend has a single prompt slot
// (internal/oversight/telegram's permCh), so two concurrent AskPermission
// calls would steal it from each other — the loser would time out, and its
// reply could fall through to the input callback and be injected as a typed
// message. The lock is taken by the asking goroutine and held until
// AskPermission has fully returned (including its permCh cleanup), so a
// waiter can never clear the slot of the next one.
var oversightPermMu sync.Mutex

// permSource is where an ask's answer came from — metrics labels and
// messaging.
type permSource string

const (
	permSrcDirect        permSource = "direct"
	permSrcRemote        permSource = "remote"
	permSrcRemoteTimeout permSource = "remote_timeout" // silence resolved by the remote's timeout_action (foreground asks only)
	permSrcDefault       permSource = "timeout_default"
	permSrcNoSurface     permSource = "no_surface"
)

// askPolicy describes who answers an ask and what silence means.
type askPolicy struct {
	// deadline bounds the ask (background asks); 0 waits for an answer
	// indefinitely (foreground asks).
	deadline time.Duration
	// def is the timed answer applied at the deadline — and immediately when
	// no ask surface exists at all. false = deny.
	def bool
	// remoteTimeoutResolves: true for foreground asks — a remote-side timeout
	// resolves the ask with the remote's configured timeout_action, exactly
	// as before ADR-0052. false for background asks — remote silence keeps
	// the ask open for the direct surface until the deadline, which then
	// applies def instead (the timed answer owns silence there).
	remoteTimeoutResolves bool
}

// foregroundAskPolicy is the interactive ask: direct input and remote
// oversight, no deadline, remote timeout_action resolves remote-side silence
// (the pre-ADR-0052 behavior, unchanged).
var foregroundAskPolicy = askPolicy{remoteTimeoutResolves: true}

// askPermission races the direct surface and remote oversight and returns the
// first real answer plus its source. direct may be nil (no local surface); n
// may be nil or oversight.Noop (no remote surface). With no surface at all
// the ask resolves immediately with def (the timed answer's "nobody is
// watching" fast path) and src is permSrcNoSurface. A policy with a deadline
// resolves with def and src permSrcDefault when it expires.
func askPermission(ctx context.Context, n oversight.Notifier, req oversight.PermRequest, direct func(context.Context) bool, p askPolicy) (allow bool, src permSource) {
	remote := oversightRemote(n)
	if direct == nil && !remote {
		return p.def, permSrcNoSurface
	}
	// Always cancel on return: the losing surface's wait (the remote's
	// prompt slot, a queued TUI prompt) must stop as soon as the ask is
	// resolved — the remote goroutine holds oversightPermMu until its
	// AskPermission returns, so a leaked waiter would deadlock every later
	// ask.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		allow bool
		src   permSource
	}
	// Buffered for all three possible senders (direct, remote, timer).
	ch := make(chan result, 3)
	if direct != nil {
		go func() { ch <- result{direct(ctx), permSrcDirect} }()
	}
	if remote {
		go func() {
			oversightPermMu.Lock()
			defer oversightPermMu.Unlock()
			if ctx.Err() != nil {
				return // already answered elsewhere — don't prompt the backend
			}
			dec, answered := n.AskPermission(ctx, req)
			switch {
			case answered:
				ch <- result{dec == oversight.PermAllow, permSrcRemote}
			case p.remoteTimeoutResolves:
				// Foreground: the remote's timeout action is its answer.
				ch <- result{dec == oversight.PermAllow, permSrcRemoteTimeout}
			}
			// Background: remote silence is not an answer — the direct
			// surface or the timed answer resolves the ask.
		}()
	}
	if p.deadline > 0 {
		go func() {
			t := time.NewTimer(p.deadline)
			defer t.Stop()
			select {
			case <-t.C:
				ch <- result{p.def, permSrcDefault}
			case <-ctx.Done():
			}
		}()
	}
	select {
	case res := <-ch:
		return res.allow, res.src
	case <-ctx.Done():
		// External cancellation (a job's context ended) — resolve with the
		// policy default rather than block forever. The direct surface's
		// ctx-aware prompt is withdrawn by its own ctx handling.
		return p.def, permSrcDefault
	}
}

// safetyAskRun is the doom-loop gate's unattended-context ask (ADR-0052):
// the full background flow — every surface plus the timed answer — mapped to
// the outcome vocabulary the gate's termination wording keys off.
func safetyAskRun(ctx context.Context, n oversight.Notifier, jobID, tool, summary string, direct func(context.Context) bool, p askPolicy) local.SafetyOutcome {
	allow, src := askPermission(ctx, n, oversight.PermRequest{ToolName: tool, Input: jobPermSummary(summary, jobID)}, direct, p)
	switch {
	case src == permSrcNoSurface:
		return local.SafetyUnreachable
	case allow:
		return local.SafetyApproved
	case src == permSrcDefault:
		return local.SafetyTimedOut
	default:
		return local.SafetyDeclined
	}
}

// permOverrides are the ad-hoc session overrides for the timed answer, set
// via /permissions default|timeout. Session-only (like /skip-permissions):
// they never write config. The zero value means "no override".
type permOverrides struct {
	bgTimeout        time.Duration
	bgTimeoutSet     bool
	bgAllow          bool
	bgAllowSet       bool
	safetyTimeout    time.Duration
	safetyTimeoutSet bool
	safetyAllow      bool
	safetyAllowSet   bool
}

// bgAskPolicy is the tool-ask policy for background work (background-job
// tool asks and workflow-step tool asks).
func bgAskPolicy(cfg config.Config, ov permOverrides) askPolicy {
	p := askPolicy{deadline: cfg.PermBackgroundTimeout(), def: cfg.PermBackgroundAllow()}
	if ov.bgTimeoutSet {
		p.deadline = ov.bgTimeout
	}
	if ov.bgAllowSet {
		p.def = ov.bgAllow
	}
	return p
}

// safetyAskPolicy is the doom-loop gate's unattended policy. Its default is
// config-separate from the tool default so one `allow` cannot silently
// re-enable unattended runaway loops.
func safetyAskPolicy(cfg config.Config, ov permOverrides) askPolicy {
	p := askPolicy{deadline: cfg.PermSafetyTimeout(), def: cfg.PermSafetyAllow()}
	if ov.safetyTimeoutSet {
		p.deadline = ov.safetyTimeout
	}
	if ov.safetyAllowSet {
		p.def = ov.safetyAllow
	}
	return p
}

// autoNote renders the timed answer for a background prompt, e.g.
// "auto-deny in 6m0s".
func autoNote(defAllow bool, d time.Duration) string {
	act := "deny"
	if defAllow {
		act = "allow"
	}
	return fmt.Sprintf("auto-%s in %s", act, d)
}

// autoDefaultAnswer is the answer string the TUI prompt queue applies on
// behalf of the timed answer ("y"/"n").
func autoDefaultAnswer(defAllow bool) string {
	if defAllow {
		return "y"
	}
	return "n"
}

// permAskPromptAuto builds the TUI prompt for a background ask: the ordinary
// permission prompt plus the timed answer and bulk-answer hints. Bulk keys
// ("a"/"d") keep a queued burst of job prompts answerable in one keystroke.
func permAskPromptAuto(tool, summary, note string) string {
	prompt := fmt.Sprintf("\n%s permission request — primary agent tool: %s", milkTag(), bold(tool))
	if summary != "" {
		prompt += fmt.Sprintf("  (%s)", dim(summary))
	}
	prompt += fmt.Sprintf("\n%s %s — Allow? [Y/n] (a: allow all, d: deny all) %s ", milkTag(), yellow(note), milkTag())
	return prompt
}

// formatPermPolicy renders the timed-answer policy lines — shared by the TUI
// and ACP so the two surfaces can't drift.
func formatPermPolicy(cfg config.Config, ov permOverrides) string {
	bg := bgAskPolicy(cfg, ov)
	sf := safetyAskPolicy(cfg, ov)
	bgNote, sfNote := "", ""
	if ov.bgTimeoutSet || ov.bgAllowSet {
		bgNote = "  (session override)"
	}
	if ov.safetyTimeoutSet || ov.safetyAllowSet {
		sfNote = "  (session override)"
	}
	return fmt.Sprintf(
		"  background asks (job/workflow tool prompts): %s%s\n  safety asks (doom-loop confirmations):        %s%s",
		autoNote(bg.def, bg.deadline), bgNote,
		autoNote(sf.def, sf.deadline), sfNote,
	)
}

// execPermissionsCore handles /permissions' argument grammar — shared by the
// TUI (commands.go's handlePermissionsCmd) and ACP (acp_commands.go's
// acpPermissions) so the two can't drift. Returns the reply and whether the
// caller should append its pending-ask listing (bare form).
func execPermissionsCore(rest string, cfg config.Config, ov *permOverrides) (string, bool) {
	parts := strings.Fields(strings.TrimSpace(rest))
	switch {
	case len(parts) == 0:
		return "", true

	case parts[0] == "default" && len(parts) >= 2:
		allow := parts[1] == "allow"
		if parts[1] != "allow" && parts[1] != "deny" {
			return "usage: /permissions default allow|deny [safety]", false
		}
		if len(parts) == 3 && parts[2] != "safety" {
			return "usage: /permissions default allow|deny [safety]", false
		}
		if len(parts) > 3 {
			return "usage: /permissions default allow|deny [safety]", false
		}
		if len(parts) == 3 {
			ov.safetyAllow, ov.safetyAllowSet = allow, true
			return milkTag() + " safety timed answer (doom-loop confirmations): " + autoNote(allow, safetyAskPolicy(cfg, *ov).deadline) + " — session override", false
		}
		ov.bgAllow, ov.bgAllowSet = allow, true
		return milkTag() + " background timed answer (job/workflow tool prompts): " + autoNote(allow, bgAskPolicy(cfg, *ov).deadline) + " — session override", false

	case parts[0] == "timeout" && len(parts) >= 2:
		secs, err := strconv.Atoi(parts[1])
		if err != nil || secs <= 0 {
			return "usage: /permissions timeout <seconds> [safety]", false
		}
		if len(parts) == 3 && parts[2] != "safety" {
			return "usage: /permissions timeout <seconds> [safety]", false
		}
		if len(parts) > 3 {
			return "usage: /permissions timeout <seconds> [safety]", false
		}
		d := time.Duration(secs) * time.Second
		if len(parts) == 3 {
			ov.safetyTimeout, ov.safetyTimeoutSet = d, true
			return milkTag() + " safety ask deadline: " + d.String() + " — session override", false
		}
		ov.bgTimeout, ov.bgTimeoutSet = d, true
		return milkTag() + " background ask deadline: " + d.String() + " — session override", false
	}
	return "usage: /permissions [default allow|deny [safety] | timeout <seconds> [safety]]", false
}

// handlePermissionsCmd implements /permissions over the TUI: the
// timed-answer policy and the pending-ask listing on the bare form (with
// per-prompt countdowns), session overrides otherwise — the grammar lives in
// execPermissionsCore, shared with ACP's acpPermissions.
func (m model) handlePermissionsCmd(rest string) model {
	out, listing := execPermissionsCore(rest, m.st.cfg, &m.st.permOverrides)
	if !listing {
		if out != "" {
			m.appendTranscript(out + "\n")
		}
		return m
	}
	var sb strings.Builder
	sb.WriteString(milkTag() + " permission asks — every ask races direct input, remote oversight, and (for background asks) the timed answer:\n")
	sb.WriteString(formatPermPolicy(m.st.cfg, m.st.permOverrides) + "\n")
	rows := 0
	appendRow := func(state string, msg *permRequestMsg) {
		rows++
		line := strings.TrimSpace(msg.prompt)
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		if !msg.autoAt.IsZero() {
			rem := time.Until(msg.autoAt).Round(time.Second)
			if rem < 0 {
				rem = 0
			}
			line += "  (" + autoNote(msg.autoDefault == "y", rem) + ")"
		}
		sb.WriteString(fmt.Sprintf("  [%s] %s\n", state, line))
	}
	if m.pendingPerm != nil {
		appendRow("pending", m.pendingPerm)
	}
	for i := range m.permQueue {
		appendRow("queued", &m.permQueue[i])
	}
	if rows == 0 {
		sb.WriteString("  no permission prompts pending\n")
	}
	sb.WriteString("  (a: allow all · d: deny all while a prompt is showing)")
	m.appendTranscript(sb.String())
	return m
}
