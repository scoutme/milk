package session

import "sync"

// File-change tracing (issue #331): which paths this session's tools touched,
// and how — the session-scoped changeset the files panel renders in git-less
// workspaces (where `git status` cannot answer). Recording happens at the
// tool dispatch boundary as an O(1) map insert: no stat, hash, or diff (the
// issue's performance budget).
//
// Storage is a package-level registry keyed by session ID rather than a
// Session field: sessions are constructed and persisted in several places
// (new/resume/sibling stores), a json:"-" field would need lazy, race-free
// initialization everywhere, and the registry keeps the trace explicitly
// in-memory/transient — it dies with the process, never enters session JSON,
// and is bounded per session and across sessions (see the caps below).

// ChangeKind classifies one path's net change during the session.
type ChangeKind string

const (
	ChangeCreated  ChangeKind = "created"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
	ChangeRenamed  ChangeKind = "renamed"
)

const (
	// fileChangesCap bounds paths per session (FIFO: the oldest path is
	// evicted first — a session's tool activity realistically stays far
	// below this; it exists so a pathological loop cannot grow unbounded).
	fileChangesCap = 4096
	// fileChangesSessionsCap bounds how many sessions the registry keeps
	// traced at once (FIFO by first record). milk runs a handful of
	// sessions in-process; older ones lose only their transient trace.
	fileChangesSessionsCap = 8
)

// ChangeSet is one session's path → kind trace. Not safe for concurrent use
// by itself — the registry below holds the lock.
type ChangeSet struct {
	kinds map[string]ChangeKind
	order []string // insertion order, for FIFO eviction
}

// set records one path change with net-aware merging: the kind is the path's
// net change since the session started, not a log of operations.
func (c *ChangeSet) set(path string, kind ChangeKind) {
	if c.kinds == nil {
		c.kinds = map[string]ChangeKind{}
	}
	prev, ok := c.kinds[path]
	if !ok {
		c.kinds[path] = kind
		c.order = append(c.order, path)
		if len(c.order) > fileChangesCap {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.kinds, oldest)
		}
		return
	}
	switch kind {
	case ChangeDeleted:
		if prev == ChangeCreated {
			// created then deleted this session → net zero, forget it.
			delete(c.kinds, path)
			return
		}
		c.kinds[path] = ChangeDeleted
	case ChangeCreated:
		// A path deleted earlier and now recreated is created again;
		// anything else keeps its kind (write_file cannot tell create
		// from overwrite without a pre-stat, which the issue forbids —
		// git status corrects the kind where a repo exists).
		if prev == ChangeDeleted {
			c.kinds[path] = ChangeCreated
		}
	case ChangeModified:
		if prev == ChangeCreated || prev == ChangeRenamed {
			return // still a net-new / moved path
		}
		c.kinds[path] = ChangeModified
	case ChangeRenamed:
		if prev == ChangeCreated {
			return // a file created this session and then moved is still net-created
		}
		c.kinds[path] = ChangeRenamed
	}
}

func (c *ChangeSet) delete(path string) {
	delete(c.kinds, path)
}

// snapshot returns a copy of the path → kind map.
func (c *ChangeSet) snapshot() map[string]ChangeKind {
	out := make(map[string]ChangeKind, len(c.kinds))
	for p, k := range c.kinds {
		out[p] = k
	}
	return out
}

var (
	fileChangesMu    sync.Mutex
	fileChanges      = map[string]*ChangeSet{}
	fileChangesOrder []string // session IDs, oldest first
)

// fileChangesFor returns the set for id, creating it and evicting the oldest
// session when the registry is full. Callers must hold fileChangesMu; ids
// must not be empty (empty ids are refused by the exported entry points).
func fileChangesFor(id string) *ChangeSet {
	if c, ok := fileChanges[id]; ok {
		return c
	}
	c := &ChangeSet{}
	fileChanges[id] = c
	fileChangesOrder = append(fileChangesOrder, id)
	for len(fileChangesOrder) > fileChangesSessionsCap {
		oldest := fileChangesOrder[0]
		fileChangesOrder = fileChangesOrder[1:]
		delete(fileChanges, oldest)
	}
	return c
}

// RecordFileChange records that a tool changed path (absolute path, as the
// tools resolve it) for session id. O(1); empty ids/paths are ignored.
func RecordFileChange(id, path string, kind ChangeKind) {
	if id == "" || path == "" {
		return
	}
	fileChangesMu.Lock()
	defer fileChangesMu.Unlock()
	fileChangesFor(id).set(path, kind)
}

// RemoveFileChange drops a path from the trace (net-zero bookkeeping, e.g. a
// move of a file this session itself created).
func RemoveFileChange(id, path string) {
	if id == "" || path == "" {
		return
	}
	fileChangesMu.Lock()
	defer fileChangesMu.Unlock()
	if c, ok := fileChanges[id]; ok {
		c.delete(path)
	}
}

// FileChange returns the recorded kind for one path.
func FileChange(id, path string) (ChangeKind, bool) {
	if id == "" || path == "" {
		return "", false
	}
	fileChangesMu.Lock()
	defer fileChangesMu.Unlock()
	c, ok := fileChanges[id]
	if !ok {
		return "", false
	}
	k, ok := c.kinds[path]
	return k, ok
}

// FileChanges returns a copy of session id's traced changeset (empty map
// when the session has none).
func FileChanges(id string) map[string]ChangeKind {
	if id == "" {
		return nil
	}
	fileChangesMu.Lock()
	defer fileChangesMu.Unlock()
	c, ok := fileChanges[id]
	if !ok {
		return nil
	}
	return c.snapshot()
}

// ClearFileChanges drops a session's trace entirely (session dropped /
// replaced). The registry's FIFO also bounds forgotten sessions.
func ClearFileChanges(id string) {
	if id == "" {
		return
	}
	fileChangesMu.Lock()
	defer fileChangesMu.Unlock()
	delete(fileChanges, id)
	for i, s := range fileChangesOrder {
		if s == id {
			fileChangesOrder = append(fileChangesOrder[:i], fileChangesOrder[i+1:]...)
			break
		}
	}
}
