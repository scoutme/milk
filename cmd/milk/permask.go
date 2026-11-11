package main

// The permission ask layer (ADR-0052): every permission ask is a race over up
// to three answer sources — direct input (the TUI prompt queue / the ACP
// client's dialog), remote oversight (Telegram), and, for background asks
// only, a *timed answer* that applies the configured default at a deadline.
// First real answer wins; silence is never itself an answer except through
// the timed answer, the one source allowed to speak for nobody. Background
// asks surface everywhere (main TUI included) instead of failing closed, and
// the deadline guarantees an answer within a bounded time.
//
// Questions (the ask_user tool, issue #249) race the same three sources via
// askUserQuestion: a question instead of a permission, free text instead of
// y/n, and a timed "no answer" instead of a timed default — but silence is
// still never an answer, and the remote surface's timeout never resolves one.

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

// sessionPermMode is the session-wide "answer every tool ask the same way"
// mode (issue #302): the a/d prompt keys and /permissions mode … set it so a
// multi-tool turn stops asking one prompt after another. Session-only — like
// /skip-permissions it never writes the persistent grant store — and scoped
// to tool asks only: the doom-loop safety gate (safetyAskPolicy) deliberately
// does not consult it, so "allow all" can never re-enable unattended runaway
// loops. Off by default; /permissions mode off (or a /skip-permissions
// toggle) clears it.
type sessionPermMode int

const (
	sessionPermOff sessionPermMode = iota
	sessionPermAllowAll
	sessionPermDenyAll
)

// decided reports whether the mode answers tool asks without prompting.
func (m sessionPermMode) decided() bool { return m != sessionPermOff }

// allow reports the mode's answer (only meaningful when decided).
func (m sessionPermMode) allow() bool { return m == sessionPermAllowAll }

// label is the mode's short name for prompts, listings and the status bar.
func (m sessionPermMode) label() string {
	switch m {
	case sessionPermAllowAll:
		return "allow-all"
	case sessionPermDenyAll:
		return "deny-all"
	}
	return "off"
}

// parseSessionPermMode maps the /permissions mode argument onto a mode.
func parseSessionPermMode(s string) (sessionPermMode, bool) {
	switch s {
	case "allow-all":
		return sessionPermAllowAll, true
	case "deny-all":
		return sessionPermDenyAll, true
	case "off":
		return sessionPermOff, true
	}
	return sessionPermOff, false
}

// oversightAskMu serializes the *remote* side of asks process-wide: the
// Telegram backend has a single prompt slot (internal/oversight/telegram's
// permCh, shared by AskPermission and AskInput), so two concurrent remote
// asks would steal it from each other — the loser would time out, and its
// reply could fall through to the input callback and be injected as a typed
// message. The lock is taken by the asking goroutine and held until the
// remote ask has fully returned (including its permCh cleanup), so a waiter
// can never clear the slot of the next one.
var oversightAskMu sync.Mutex

// permSource is where an ask's answer came from — metrics labels and
// messaging.
type permSource string

const (
	permSrcDirect        permSource = "direct"
	permSrcRemote        permSource = "remote"
	permSrcRemoteTimeout permSource = "remote_timeout" // silence resolved by the remote's timeout_action (foreground asks only)
	permSrcDefault       permSource = "timeout_default"
	permSrcNoSurface     permSource = "no_surface"
	permSrcSessionMode   permSource = "session_mode" // the allow-all/deny-all session mode answered without prompting (issue #302)
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
	// sessionMode is the session allow-all/deny-all mode (issue #302): when
	// decided it answers the ask immediately, before any surface is
	// prompted. Only tool-ask policies set it — the doom-loop gate's
	// safetyAskPolicy must never consult it.
	sessionMode sessionPermMode
	// sticky records a surface's "answer this and everything after it the
	// same way" choice (issue #302 — the TUI a/d keys set the mode at the
	// key handler; this is the remote-oversight and ACP-dialog path). nil
	// means the ask offers no sticky answer (safety gate, questions).
	sticky func(allow bool)
}

// foregroundAskPolicy is the interactive ask: direct input and remote
// oversight, no deadline, remote timeout_action resolves remote-side silence
// (the pre-ADR-0052 behavior, unchanged).
var foregroundAskPolicy = askPolicy{remoteTimeoutResolves: true}

// foregroundPermPolicy is foregroundAskPolicy plus the session allow-all /
// deny-all mode (issue #302) from the host's live override snapshot — tool
// asks only; questions (askUserQuestion) never consult the mode.
func foregroundPermPolicy(ov permOverrides) askPolicy {
	p := foregroundAskPolicy
	p.sessionMode = ov.sessionMode
	return p
}

// sessionPermModeGetter adapts a host's override snapshot accessor onto the
// local agent's permission-mode hook (local.WithPermissionModeFunc): the
// tool loop consults it before the persistent grant store, so an allow-all
// answer never leaves a persistent grant behind (issue #302).
func sessionPermModeGetter(ov func() permOverrides) func() (bool, bool) {
	return func() (bool, bool) {
		m := ov().sessionMode
		return m.decided(), m.allow()
	}
}

// clearSessionPermMode drops the session allow-all/deny-all mode and returns
// a note when one was active (issue #302) — used by the /skip-permissions
// handlers so the two "stop asking" states can't stack. Callers hold their
// override lock.
func clearSessionPermMode(ov *permOverrides) string {
	if !ov.sessionMode.decided() {
		return ""
	}
	ov.sessionMode = sessionPermOff
	return "  (session permission mode cleared)"
}

// sessionPermModeNote is the one-line announcement of a mode change — the
// mode acts silently from then on, so setting it must never be silent.
func sessionPermModeNote(mode sessionPermMode) string {
	return milkTag() + " session permission mode: " + mode.label() +
		" — every tool ask answered without prompting (safety gate unaffected; /permissions mode off to clear)"
}

// stickySetter is the askPolicy.sticky callback for hosts whose session mode
// lives in their permOverrides (issue #302): a remote oversight "answer all
// the same way" reply lands here. note, when non-nil, is told once about the
// change so the surface keeps its transcript honest; it must be safe to call
// from the ask layer's goroutines (both hosts' Notify are).
func stickySetter(setMode func(sessionPermMode), note func(string)) func(bool) {
	return func(allow bool) {
		mode := sessionPermDenyAll
		if allow {
			mode = sessionPermAllowAll
		}
		setMode(mode)
		if note != nil {
			note(sessionPermModeNote(mode))
		}
	}
}

// permDismissNoteKey carries the ask's resolving source (a note string) onto
// the direct surface's context so a withdrawn prompt can say *who* answered
// instead of the vague "(answered elsewhere)" (ADR-0052's honest-transcript
// rule). askPermission/askUserQuestion write the note before cancel() — the
// Done channel read by the surface carries the happens-before edge.
type permDismissNoteKey struct{}

// permWithdrawNote is the withdrawal reason for a permission prompt: who
// resolved the ask and what it applied. Empty src keeps the generic wording.
func permWithdrawNote(src permSource, allow bool) string {
	act := "deny"
	if allow {
		act = "allow"
	}
	switch src {
	case permSrcRemote:
		return "answered via remote oversight"
	case permSrcRemoteTimeout:
		return "remote oversight timed out — " + act + " applied"
	case permSrcDefault:
		return "timed answer — " + act + " applied"
	default:
		return ""
	}
}

// questionWithdrawNote is permWithdrawNote for an ask_user question: silence
// resolves as "no answer", never as a default.
func questionWithdrawNote(src permSource) string {
	switch src {
	case permSrcRemote:
		return "answered via remote oversight"
	case permSrcDefault:
		return "no answer in time"
	default:
		return ""
	}
}

// dismissNoteFrom reads the resolving source's note the ask layer recorded on
// ctx before cancelling (permDismissNoteKey) — "" keeps the generic wording.
func dismissNoteFrom(ctx context.Context) string {
	if s, ok := ctx.Value(permDismissNoteKey{}).(*string); ok {
		return *s
	}
	return ""
}

// askPermission races the direct surface and remote oversight and returns the
// first real answer plus its source. direct may be nil (no local surface); n
// may be nil or oversight.Noop (no remote surface). With no surface at all
// the ask resolves immediately with def (the timed answer's "nobody is
// watching" fast path) and src is permSrcNoSurface. A policy with a deadline
// resolves with def and src permSrcDefault when it expires.
func askPermission(ctx context.Context, n oversight.Notifier, req oversight.PermRequest, direct func(context.Context) bool, p askPolicy) (allow bool, src permSource) {
	// Session mode first (issue #302) — before even the no-surface fast
	// path: a background ask with a mode set must not wait out its deadline
	// for an answer the mode already gives, on any surface configuration.
	if p.sessionMode.decided() {
		return p.sessionMode.allow(), permSrcSessionMode
	}
	// A sticky answer can only be offered where it can be honored.
	req.AllowAlways = p.sticky != nil
	remote := oversightRemote(n)
	if direct == nil && !remote {
		return p.def, permSrcNoSurface
	}
	// Always cancel on return: the losing surface's wait (the remote's
	// prompt slot, a queued TUI prompt) must stop as soon as the ask is
	// resolved — the remote goroutine holds oversightAskMu until its
	// AskPermission returns, so a leaked waiter would deadlock every later
	// ask.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The withdrawal note is written before cancel() on every resolution path:
	// the direct surface reads it after observing ctx.Done() (see
	// readLineCtx's permDismissMsg).
	var note string
	ctx = context.WithValue(ctx, permDismissNoteKey{}, &note)
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
			oversightAskMu.Lock()
			defer oversightAskMu.Unlock()
			if ctx.Err() != nil {
				return // already answered elsewhere — don't prompt the backend
			}
			dec, answered := n.AskPermission(ctx, req)
			switch {
			case answered:
				if dec.Always() && p.sticky != nil {
					p.sticky(dec.Allow())
				}
				ch <- result{dec.Allow(), permSrcRemote}
			case p.remoteTimeoutResolves:
				// Foreground: the remote's timeout action is its answer.
				ch <- result{dec.Allow(), permSrcRemoteTimeout}
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
		note = permWithdrawNote(res.src, res.allow)
		return res.allow, res.src
	case <-ctx.Done():
		// External cancellation (a job's context ended) — resolve with the
		// policy default rather than block forever. The direct surface's
		// ctx-aware prompt is withdrawn by its own ctx handling. No note is
		// recorded here: the direct side can observe this Done before the
		// write below would land, so it keeps the generic fallback wording.
		return p.def, permSrcDefault
	}
}

// askUserQuestion races the direct surface and remote oversight for a
// free-text answer to an agent's question (the ask_user tool — issue #249)
// and returns the first real answer plus its source. Unlike permissions,
// silence is never an answer: questions have no remote timeout action, so
// only the direct surface, the timed "no answer" at p.deadline (background
// asks), or external cancellation may resolve an unanswered question. With
// no surface at all the call returns immediately with answered=false and
// src=permSrcNoSurface. direct reports (answer, ok); ok=false is a surface
// that withdrew or gave up (a dismissed dialog), which resolves the race as
// "nobody answered" unless another source still speaks before cancel.
func askUserQuestion(ctx context.Context, n oversight.Notifier, req oversight.InputRequest, direct func(context.Context) (string, bool), p askPolicy) (answer string, answered bool, src permSource) {
	remote := oversightRemote(n)
	if direct == nil && !remote {
		return "", false, permSrcNoSurface
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// See askPermission: the withdrawal note is recorded before cancel() so the
	// direct surface can say who answered.
	var note string
	ctx = context.WithValue(ctx, permDismissNoteKey{}, &note)
	type result struct {
		answer string
		ok     bool
		src    permSource
	}
	ch := make(chan result, 3)
	if direct != nil {
		go func() {
			a, ok := direct(ctx)
			if ok {
				ch <- result{a, true, permSrcDirect}
				return
			}
			if ctx.Err() == nil {
				// The surface gave up without answering (e.g. a dismissed
				// dialog) — a resolution, not a pending race.
				ch <- result{"", false, permSrcDirect}
			}
		}()
	}
	if remote {
		go func() {
			oversightAskMu.Lock()
			defer oversightAskMu.Unlock()
			if ctx.Err() != nil {
				return // already answered elsewhere — don't prompt the backend
			}
			if a, ok := n.AskInput(ctx, req); ok {
				ch <- result{a, true, permSrcRemote}
			}
			// Remote silence is never an answer for a question.
		}()
	}
	if p.deadline > 0 {
		go func() {
			t := time.NewTimer(p.deadline)
			defer t.Stop()
			select {
			case <-t.C:
				ch <- result{"", false, permSrcDefault}
			case <-ctx.Done():
			}
		}()
	}
	select {
	case res := <-ch:
		note = questionWithdrawNote(res.src)
		return res.answer, res.ok, res.src
	case <-ctx.Done():
		// External cancellation (a job's context ended) — nobody answered.
		// No note recorded (see askPermission's ctx.Done path): the generic
		// fallback wording applies.
		return "", false, permSrcDefault
	}
}

// userAskPrompt renders the ask_user question for the direct surface: the
// question, the options when any, and — for timed background asks — the note
// about the timed answer. These prompts are free text (no y/n): the TUI
// routes them through the same prompt queue with the freeText flag set, so
// the "a"/"d" bulk keys never swallow an answer.
func userAskPrompt(question string, options []string, note string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n%s agent question:\n%s\n", milkTag(), bold(strings.TrimSpace(question))))
	for i, o := range options {
		b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, o))
	}
	if note != "" {
		b.WriteString(fmt.Sprintf("%s %s\n", milkTag(), yellow(note)))
	}
	b.WriteString(fmt.Sprintf("%s your answer: ", milkTag()))
	return b.String()
}

// autoNoteQuestion renders the timed answer for a background question, e.g.
// "no answer in 6m0s" (the permission wording is autoNote's allow/deny).
func autoNoteQuestion(d time.Duration) string {
	return fmt.Sprintf("no answer in %s", d)
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
// via /permissions default|timeout, plus the session allow-all/deny-all mode
// (/permissions mode …, the a/d prompt keys — issue #302). Session-only
// (like /skip-permissions): they never write config or the persistent grant
// store. The zero value means "no override". Guarded by the host's permMu.
type permOverrides struct {
	bgTimeout        time.Duration
	bgTimeoutSet     bool
	bgAllow          bool
	bgAllowSet       bool
	safetyTimeout    time.Duration
	safetyTimeoutSet bool
	safetyAllow      bool
	safetyAllowSet   bool
	sessionMode      sessionPermMode
}

// permOverridesNow returns a snapshot of the session's timed-answer overrides.
func (st *interactiveState) permOverridesNow() permOverrides {
	st.permMu.RLock()
	defer st.permMu.RUnlock()
	return st.permOverrides
}

// setSessionPermMode records the session mode (issue #302) — state only,
// guarded by permMu so the ask layer's goroutines may set it; callers (or
// stickySetter's note) announce the change.
func (st *interactiveState) setSessionPermMode(mode sessionPermMode) {
	st.permMu.Lock()
	st.permOverrides.sessionMode = mode
	st.permMu.Unlock()
}

// bgAskPolicy is the tool-ask policy for background work (background-job
// tool asks and workflow-step tool asks). The session mode answers these
// too (issue #302) — "all subsequent permission asks" means what it says.
func bgAskPolicy(cfg config.Config, ov permOverrides) askPolicy {
	p := askPolicy{deadline: cfg.PermBackgroundTimeout(), def: cfg.PermBackgroundAllow(), sessionMode: ov.sessionMode}
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
// re-enable unattended runaway loops — and the session allow-all/deny-all
// mode deliberately never reaches it (issue #302): the gate keeps prompting
// no matter what the tool asks answer.
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
// ("a"/"d") answer a queued burst of job prompts in one keystroke AND set the
// session allow-all/deny-all mode for what follows (issue #302).
func permAskPromptAuto(tool, summary, note string) string {
	prompt := fmt.Sprintf("\n%s permission request — primary agent tool: %s", milkTag(), bold(tool))
	if summary != "" {
		prompt += fmt.Sprintf("  (%s)", dim(summary))
	}
	prompt += fmt.Sprintf("\n%s %s — Allow? [Y/n/a/d] (a: allow all, d: deny all) %s ", milkTag(), yellow(note), milkTag())
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
	modeLine := "  session mode (answers every tool ask):             " + ov.sessionMode.label()
	if ov.sessionMode.decided() {
		modeLine += "  (session override — /permissions mode off to clear)"
	}
	return fmt.Sprintf(
		"%s\n  background asks (job/workflow tool prompts): %s%s\n  safety asks (doom-loop confirmations):        %s%s",
		modeLine,
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

	case parts[0] == "mode":
		if len(parts) != 2 {
			return "usage: /permissions mode allow-all|deny-all|off", false
		}
		mode, ok := parseSessionPermMode(parts[1])
		if !ok {
			return "usage: /permissions mode allow-all|deny-all|off", false
		}
		ov.sessionMode = mode
		if mode == sessionPermOff {
			return milkTag() + " session permission mode: off — agents prompt again", false
		}
		verb := "allowed"
		if mode == sessionPermDenyAll {
			verb = "denied"
		}
		return milkTag() + " session permission mode: " + mode.label() + " — every tool ask is " + verb + " without prompting (doom-loop safety gate unaffected; /permissions mode off to clear)", false

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
	return "usage: /permissions [mode allow-all|deny-all|off | default allow|deny [safety] | timeout <seconds> [safety]]", false
}

// handlePermissionsCmd implements /permissions over the TUI: the
// timed-answer policy and the pending-ask listing on the bare form (with
// per-prompt countdowns), session overrides otherwise — the grammar lives in
// execPermissionsCore, shared with ACP's acpPermissions.
func (m model) handlePermissionsCmd(rest string) model {
	m.st.permMu.Lock()
	out, listing := execPermissionsCore(rest, m.st.cfg, &m.st.permOverrides)
	ov := m.st.permOverrides
	m.st.permMu.Unlock()
	if !listing {
		if out != "" {
			m.appendTranscript(out + "\n")
		}
		return m
	}
	var sb strings.Builder
	sb.WriteString(milkTag() + " permission asks — every ask races direct input, remote oversight, and (for background asks) the timed answer:\n")
	sb.WriteString(formatPermPolicy(m.st.cfg, ov) + "\n")
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
			if msg.freeText {
				line += "  (" + autoNoteQuestion(rem) + ")"
			} else {
				line += "  (" + autoNote(msg.autoDefault == "y", rem) + ")"
			}
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
