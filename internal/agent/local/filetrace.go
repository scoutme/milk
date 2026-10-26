package local

import (
	"github.com/scoutme/milk/internal/session"
)

// File-change tracing at the dispatch boundary (issue #331): the five
// mutating tools declare their intended deltas; they are recorded only after
// the tool ran without error (a failed tool changed no files). Each record is
// an O(1) map insert into the session's changeset — no stat, hash, or diff.

// strArg is the one-line string-argument fetch the tracing sites share.
func strArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// fileDelta is one path a mutating tool call changed.
type fileDelta struct {
	path string // raw (unresolved) path; "" is refused
	kind session.ChangeKind
}

// traceFileChanges records the deltas of one mutating tool call. Paths are
// resolved against the session cwd exactly like the handlers resolve them.
// write_file records ChangeCreated when the path is unknown: without a
// pre-write stat (forbidden by issue #331's budget) create and overwrite are
// indistinguishable at dispatch; `git status` corrects the kind wherever a
// repo exists, and outside a repo write_file is overwhelmingly "new file"
// (edit_file/multiedit are the modify tools).
func traceFileChanges(sess *session.Session, cwd, result string, deltas ...fileDelta) {
	if sess == nil || sess.ID == "" || isToolError(result) {
		return
	}
	for _, d := range deltas {
		if d.path == "" {
			continue
		}
		session.RecordFileChange(sess.ID, resolvePath(d.path, cwd), d.kind)
	}
}

// traceFileMove records both sides of a move: a file this session itself
// created keeps net-kind Created at its new path (old side removed); any
// other source is deleted and the destination is renamed.
func traceFileMove(sess *session.Session, cwd, result, src, dst string) {
	if sess == nil || sess.ID == "" || src == "" || dst == "" || isToolError(result) {
		return
	}
	absSrc := resolvePath(src, cwd)
	absDst := resolvePath(dst, cwd)
	if k, ok := session.FileChange(sess.ID, absSrc); ok && k == session.ChangeCreated {
		session.RemoveFileChange(sess.ID, absSrc)
		session.RecordFileChange(sess.ID, absDst, session.ChangeCreated)
		return
	}
	session.RecordFileChange(sess.ID, absSrc, session.ChangeDeleted)
	session.RecordFileChange(sess.ID, absDst, session.ChangeRenamed)
}
