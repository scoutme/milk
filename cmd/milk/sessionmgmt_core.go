package main

// Session management — the host-neutral core of the /sessions /new /clear
// /resume /drop surface (docs/session-management-refactor-plan.md D2),
// following the established *_core.go split (workflow_core.go, update_core.go,
// initwizard_core.go, configview.go). Shared by the TUI (execNonPromptCmd /
// handleSlashInput), ACP (acp_sessionmgmt.go) and the CLI (runList/runDrop/
// openSession). Host side effects stay with the hosts: transcript reseeding
// on the TUI, view rebinding on ACP. Everything here works on plain
// cwd/session values and interactiveState-free inputs.
//
// One reference rule everywhere (D2): a session ref is an exact ID, an
// unambiguous ID prefix, or an exact name within the cwd — resolved by
// session.Resolve, which never guesses and never creates.

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/scoutme/milk/internal/session"
)

// Session-management command tokens (one vocabulary for TUI and ACP, D1).
const (
	cmdNew      = "/new"
	cmdClear    = "/clear" // alias of /new
	cmdDrop     = "/drop"
	cmdSessions = "/sessions"
	cmdResume   = "/resume"
	// cmdListLegacy is /list, the deprecated alias of /sessions kept for one
	// release with a rename hint. Hidden from /help and from ACP's
	// available_commands_update (the command table omits it), but still
	// executable in both hosts.
	cmdListLegacy = "/list"
)

// sessionRefHint is the placeholder help text and tab completion use for
// session references — the /resume and /drop argument and the --session flag
// all accept the same thing (id, unambiguous id prefix, or cwd-scoped name).
const sessionRefHint = "id|prefix|name"

// sessionRefCreateHintCLI / sessionRefCreateHintHost are the "how to create"
// tails appended to a failed resolution — the CLI recipe and the interactive
// recipe differ, so the shared core stops at the neutral base text
// (sessionRefErrText) and each caller adds its own tail.
const (
	sessionRefCreateHintCLI  = " — use --new --session <name> to create one"
	sessionRefCreateHintHost = " — /sessions lists what exists, /new starts fresh"
)

// sessionOpResult is the outcome of one session-management op: out is the
// rendered report (no trailing newline; milk-tagged lines, like every other
// exec* output), bind is the session the host should bind afterwards
// (nil = keep the current binding untouched), err is a resolution or store
// failure (out is empty when it is set — the CLI turns it into an exit code,
// interactive hosts render it via renderOpResult).
type sessionOpResult struct {
	bind *session.Session
	out  string
	err  error
	// droppedID is the store session ID the op deleted from disk (set by
	// opSessionDrop). ACP records it in the server's closed-set so
	// resume-by-default cannot resurrect a just-dropped session.
	droppedID string
}

// renderOpResult renders an op result for an interactive host: the success
// report as-is, or a milk-tagged error line carrying the interactive
// create hint on a ref miss.
func renderOpResult(res sessionOpResult, ref string) string {
	if res.err != nil {
		hint := ""
		// The create hint is for a plain miss only — an ambiguous ref already
		// carries its own "use more characters" recipe.
		if errors.Is(res.err, session.ErrNotFound) {
			hint = sessionRefCreateHintHost
		}
		return fmt.Sprintf("%s %s%s", milkTag(), sessionRefErrText(res.err, ref), hint)
	}
	return res.out
}

// openSession resolves the CLI/REPL session selection (D5): --new creates a
// (optionally named) session — ref is the new session's name; --session <ref>
// targets a stored session by id, unambiguous id prefix or cwd-scoped name and
// is an error when nothing matches (a typo'd ref must not silently create and
// "resume" nothing — the create path is --new); no flags resumes the cwd's
// most recent session, creating one when the directory has none.
func openSession(cwd string, startNew bool, ref string) (*session.Session, error) {
	if startNew {
		return session.New(cwd, ref)
	}
	if ref == "" {
		return session.Resume(cwd, "")
	}
	sess, err := session.Resolve(cwd, ref)
	if err != nil {
		return nil, cliSessionErr(err, ref)
	}
	return sess, nil
}

// cliSessionErr renders a resolution failure with the CLI create recipe.
func cliSessionErr(err error, ref string) error {
	if errors.Is(err, session.ErrNotFound) {
		return fmt.Errorf("%s%s", sessionRefErrText(err, ref), sessionRefCreateHintCLI)
	}
	return err
}

// sessionRefErrText renders a failed session-ref resolution as neutral base
// text (callers append their own create hint). Ambiguous refs keep the
// store's "use more characters" wording; misses get the plain not-found line.
func sessionRefErrText(err error, ref string) string {
	var amb *session.AmbiguousError
	switch {
	case errors.As(err, &amb):
		return amb.Error()
	case errors.Is(err, session.ErrNotFound):
		return fmt.Sprintf("no session matches %q", ref)
	default:
		return err.Error()
	}
}

// opSessionNew starts a fresh (optionally named) session for cwd. The
// previous session stays on disk, resumable — /new never deletes anything.
func opSessionNew(cwd, name string) sessionOpResult {
	sess, err := session.New(cwd, name)
	if err != nil {
		return sessionOpResult{err: err}
	}
	return sessionOpResult{bind: sess, out: fmt.Sprintf("%s new session %s", milkTag(), sessLabel(sess))}
}

// opSessionResume attaches the referenced stored session. A bare ref means
// the cwd's most recent session. The already-current case is a report, not a
// bind — /resume is a switch, there is nothing to switch to.
func opSessionResume(cur *session.Session, cwd, ref string) sessionOpResult {
	sess, err := session.Resolve(cwd, ref)
	if err != nil {
		if ref == "" && errors.Is(err, session.ErrNotFound) {
			return sessionOpResult{err: errors.New("no stored session for this directory — start one with /new")}
		}
		return sessionOpResult{err: err}
	}
	if cur != nil && cur.ID == sess.ID {
		return sessionOpResult{out: fmt.Sprintf("%s already in session %s", milkTag(), sessLabel(sess))}
	}
	return sessionOpResult{bind: sess, out: fmt.Sprintf("%s resumed session %s (%d turns)", milkTag(), sessLabel(sess), len(sess.History))}
}

// opSessionDrop deletes the referenced session from disk. A bare ref targets
// the host's current binding (cur) — or, with cur == nil (the headless CLI),
// the cwd's most recent session. Dropping the current binding lands on a
// fresh unnamed session (bind != nil) so the next turn — and the next plain
// `milk` — starts clean instead of silently resuming an older conversation;
// `/drop <ref>` on a non-current session deletes only that one and leaves the
// conversation alone (bind == nil).
func opSessionDrop(cur *session.Session, cwd, ref string) sessionOpResult {
	target := cur
	if ref != "" {
		sess, err := session.Resolve(cwd, ref)
		if err != nil {
			return sessionOpResult{err: err}
		}
		target = sess
	} else if target == nil {
		sess, err := session.Resolve(cwd, "")
		if err != nil {
			return sessionOpResult{err: errors.New("no session to drop for this directory")}
		}
		target = sess
	}

	wasCurrent := false
	if cur != nil {
		wasCurrent = cur.ID == target.ID
	} else if top, err := session.Resolve(cwd, ""); err == nil {
		// Headless: "current" is the cwd's most recent session.
		wasCurrent = top.ID == target.ID
	}

	if err := session.Drop(target.ID, cwd); err != nil {
		return sessionOpResult{err: err}
	}
	lines := fmt.Sprintf("%s dropped session %s", milkTag(), sessDropLabel(target))
	if !wasCurrent {
		return sessionOpResult{out: lines, droppedID: target.ID}
	}
	fresh, err := session.New(cwd, "")
	if err != nil {
		return sessionOpResult{out: lines + fmt.Sprintf("\n%s warning: could not create fresh session: %v", milkTag(), err), droppedID: target.ID}
	}
	return sessionOpResult{bind: fresh, out: lines + fmt.Sprintf("\n%s new session %s", milkTag(), sessLabel(fresh)), droppedID: target.ID}
}

// sessionListText renders the /sessions listing for cwd (all: every
// directory), marking currentID as the active entry. Plain output — no
// milk tag — matching `milk --list` byte for byte.
func sessionListText(cwd, currentID string, all bool) (string, error) {
	target := cwd
	if all {
		target = ""
	}
	entries, err := session.List(target)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "no sessions found", nil
	}
	// session.List returns the target cwd as a (possibly empty) group, so
	// emptiness is "no rows rendered", not "no map keys".
	if out := renderSessionList(entries, currentID); out != "" {
		return out, nil
	}
	return "no sessions found", nil
}

// renderSessionList renders grouped index entries — one line per session with
// its short ID, name, turn count and last use, "*" marking currentID. Entry
// order within a directory is the store's (LastUsed desc); directories are
// sorted for a stable listing. Plain output, no trailing newline.
func renderSessionList(entries map[string][]session.IndexEntry, currentID string) string {
	dirs := make([]string, 0, len(entries))
	for dir := range entries {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	var b strings.Builder
	for _, dir := range dirs {
		list := entries[dir]
		if len(list) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(dir + "\n")
		for _, e := range list {
			name := e.Name
			if name == "" {
				name = "(unnamed)"
			}
			turns := "-"
			if s, err := session.Load(e.ID); err == nil {
				turns = strconv.Itoa(len(s.History))
			}
			marker := " "
			if e.ID == currentID {
				marker = "*"
			}
			b.WriteString(fmt.Sprintf("%s %-8s %-20s %5s  %s\n",
				marker, shortID(e.ID), name, turns, e.LastUsed.Format("2006-01-02 15:04")))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// shortID is the display form of a session ID (first 8 chars).
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// sessLabel is the display form of a session: the short ID plus the name when
// it has one — the id8 granularity the rest of the UI uses (status bar,
// seeded-transcript banner).
func sessLabel(s *session.Session) string {
	if s.Name != "" {
		return fmt.Sprintf("%s (%s)", shortID(s.ID), s.Name)
	}
	return shortID(s.ID)
}

// sessDropLabel identifies a dropped session precisely: the full ID (D5 —
// drop output stops hiding what it deleted) plus the name when it has one.
func sessDropLabel(s *session.Session) string {
	if s.Name != "" {
		return fmt.Sprintf("%s (%s)", s.ID, s.Name)
	}
	return s.ID
}
