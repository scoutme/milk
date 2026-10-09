// ACP session management — the slash-command half of the one session
// vocabulary (plan D1: /sessions, /new, /clear, /resume, /drop, plus the
// deprecated /list alias) and the view-rebinding model that makes switching
// possible over ACP at all (plan D4, ADR-0051).
//
// A chat-typed /new or /resume cannot change the wire handle the client holds
// (session/resume's response carries no sessionId), so an acpSession is a
// **conversation view** whose handle is fixed at creation and whose current
// binding (as.sess) can move between store sessions. The handlers below run
// the host-neutral ops (sessionmgmt_core.go) and adopt their binding through
// applySessionOp → as.rebind, which tears down and rebuilds exactly the
// session-scoped state newACPSession builds (memory store, task store,
// runners' session context) while view/user state survives.
//
// Switching never hides state: a rebind replays the new binding's retained
// history (bounded, like adoption and session/resume) with a one-line notice;
// a fresh binding replays nothing and the op report says so.
package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/acp"
)

// rebind points this conversation view at a different store session
// (ADR-0051). The wire handle as.id never changes — only the current binding.
// Session-scoped state is rebuilt through the seams newACPSession uses:
// memory store, task store (setupTasks), and the runners' session context
// (buildRunners). View/user state survives on purpose: as.cfg and the config
// options, /think and routing pins, skipPerms, the client capability flags —
// they belong to the conversation, not the store session. The background-job
// manager is kept and its state file re-pointed (the TUI's
// refreshSessionScopedState does the same), so jobs from the previous binding
// keep their own attribution.
func (as *acpSession) rebind(sess *session.Session) error {
	as.sess = sess
	as.st.sess = sess
	// Replay IDs: plain hist-* on the first binding (already-replayed panes
	// keep patching), session-qualified after any rebind (acp_history.go).
	if sess.ID == as.firstBindingID {
		as.replayKey = ""
	} else {
		as.replayKey = shortID(sess.ID)
	}

	if dir, err := memoryDir(); err == nil {
		mem, memErr := memory.NewStore(dir, sess.ID)
		if memErr != nil {
			mem = nil // best-effort, same as newACPSession
		}
		as.mem, as.st.mem = mem, mem
	}
	as.setupTasks()
	if as.mgr != nil {
		if dir, err := config.Dir(); err == nil && sess.ID != "" {
			as.mgr.SetStateFile(filepath.Join(dir, "jobs", sess.ID+".json"))
		}
	}
	if err := as.buildRunners(as.cfg); err != nil {
		return err
	}
	as.loopMu.Lock()
	as.loop.ResetTurn()
	as.loopMu.Unlock()
	return nil
}

// applySessionOp adopts a session op's outcome (sessionmgmt_core.go) in this
// view: renders the error report on failure, records a dropped session in the
// server's closed-set (so resume-by-default cannot resurrect it), and on a
// binding change rebinds the view and replays the new binding's retained
// history with a one-line notice. A fresh binding replays nothing.
func (as *acpSession) applySessionOp(res sessionOpResult, ref string) (string, string) {
	if res.err != nil {
		return renderOpResult(res, ref), ""
	}
	if res.droppedID != "" && as.srv != nil {
		as.srv.markClosed(acp.SessionID(res.droppedID))
	}
	if res.bind != nil {
		if err := as.rebind(res.bind); err != nil {
			return res.out + fmt.Sprintf("\n%s warning: switched session, but rebuilding state failed: %v", milkTag(), err), ""
		}
		as.replayBinding(len(res.bind.History) > 0, fmt.Sprintf(
			"switched this conversation to session %.8s", res.bind.ID))
	}
	return res.out, ""
}

// replayBinding is the "switching never hides state" half of a rebind: the
// new binding's bounded history replay followed by a one-line notice naming
// it (fresh bindings have no history to replay and simply say so through the
// op report).
func (as *acpSession) replayBinding(announceTurns bool, note string) {
	if !announceTurns {
		return
	}
	as.replayHistory()
	as.notify(acp.AgentMessageChunk(as.liveID("rebind"), fmt.Sprintf(
		"%s — replaying %d earlier turns; /export prints the full transcript", note, len(as.sess.History))))
}

// acpSessionsCmd implements /sessions [all] — the /list rename (D1). The
// listing is the shared renderer, "*" marking this view's current binding.
func acpSessionsCmd(as *acpSession, rest string) (string, string) {
	all := strings.EqualFold(rest, "all")
	if rest != "" && !all {
		return milkTag() + " usage: /sessions [all]", ""
	}
	out, err := sessionListText(as.st.cwd, as.sess.ID, all)
	if err != nil {
		return fmt.Sprintf(errFmt, err), ""
	}
	return out, ""
}

// acpNewCmd implements /new [name] and its alias /clear: start a fresh
// (optionally named) session and rebind the view to it. The previous binding
// stays on disk, resumable via /resume or session/resume.
func acpNewCmd(as *acpSession, rest string) (string, string) {
	name := strings.TrimSpace(rest)
	return as.applySessionOp(opSessionNew(as.st.cwd, name), name)
}

// acpResumeCmd implements /resume <id|prefix|name> — switch this
// conversation to a stored session. The referenced session must not be bound
// by another live view (the one-view-per-binding invariant, ADR-0051); it is
// refused with a pointer to that conversation instead of split-writing the
// store session from two panes. Bare /resume lists what is resumable.
func acpResumeCmd(as *acpSession, rest string) (string, string) {
	ref := strings.TrimSpace(rest)
	if ref == "" {
		listing, _ := acpSessionsCmd(as, "")
		return fmt.Sprintf("%s usage: /resume <%s>\n\n%s", milkTag(), sessionRefHint, listing), ""
	}
	res := opSessionResume(as.sess, as.st.cwd, ref)
	if res.err == nil && res.bind != nil && as.srv != nil {
		if other := as.srv.viewBinding(res.bind.ID); other != nil && other != as {
			return fmt.Sprintf("%s session %s is open in another conversation — close it there first",
				milkTag(), sessLabel(res.bind)), ""
		}
	}
	return as.applySessionOp(res, ref)
}

// acpDropCmd implements /drop [<ref>] — delete the current session (landing
// the view on a fresh one) or exactly the referenced one (leaving this
// conversation alone). The dropped ID joins the server's closed-set.
func acpDropCmd(as *acpSession, rest string) (string, string) {
	ref := strings.TrimSpace(rest)
	return as.applySessionOp(opSessionDrop(as.sess, as.st.cwd, ref), ref)
}
