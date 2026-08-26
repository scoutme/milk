package workflow

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// numberedIDRE matches the leading numeric ID segment of a workflow artefact
// filename once the "<sessionID>.workflow." prefix has been stripped, e.g.
// "2.plan.md" or "1.findings3.md.cleared" both start with "2." / "1.".
var numberedIDRE = regexp.MustCompile(`^(\d+)\.`)

// sessionWorkflowIDs returns the set of workflow IDs that have at least one
// file on disk for sessionID. ID 0 represents the original, suffix-less
// naming scheme (used before per-workflow IDs existed, and still used for a
// session's first workflow) — any file under the "<sessionID>.workflow."
// prefix without a numeric segment right after it (state, plan, sprint,
// findings, or an already ".cleared" file) counts toward ID 0.
func sessionWorkflowIDs(stateDir, sessionID string) (map[int]bool, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[int]bool{}, nil
		}
		return nil, err
	}

	prefix := sessionID + ".workflow."
	ids := map[int]bool{}
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok {
			continue
		}
		if m := numberedIDRE.FindStringSubmatch(rest); m != nil {
			id, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			ids[id] = true
			continue
		}
		ids[0] = true
	}
	return ids, nil
}

// NextWorkflowID returns the ID to use for a new workflow run in this
// session — one past the highest ID that already has any file on disk
// (state, plan, sprint, findings, or cleared), or 0 if the session has no
// workflow files yet. IDs are never reused, so a cleared workflow's renamed
// state file still reserves its ID against collision with a later run.
func NextWorkflowID(stateDir, sessionID string) (int, error) {
	ids, err := sessionWorkflowIDs(stateDir, sessionID)
	if err != nil {
		return 0, err
	}
	next := 0
	for id := range ids {
		if id+1 > next {
			next = id + 1
		}
	}
	return next, nil
}

// WorkflowKind distinguishes which on-disk format a resolved workflow ID's
// live file belongs to — dev.go's State (StatePath) or an interpreter-driven
// run's Checkpoint (InterpCheckpointPath). Callers that only care about
// "is there anything to resume" can compare against WorkflowKindNone; callers
// that need to load the right format branch on Dev vs. Interp.
type WorkflowKind int

const (
	WorkflowKindNone WorkflowKind = iota
	WorkflowKindDev
	WorkflowKindInterp
)

// CurrentWorkflowID returns the ID and kind of the most recent workflow run
// in this session that still has a live file (i.e. not renamed by
// /workflow clear), for resume/reconfigure/clear and the startup resume
// check to target. kind is WorkflowKindNone when no live file exists for
// any ID.
func CurrentWorkflowID(stateDir, sessionID string) (id int, kind WorkflowKind, err error) {
	ids, err := sessionWorkflowIDs(stateDir, sessionID)
	if err != nil {
		return 0, WorkflowKindNone, err
	}
	candidates := make([]int, 0, len(ids))
	for id := range ids {
		candidates = append(candidates, id)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(candidates)))
	for _, id := range candidates {
		if _, statErr := os.Stat(StatePath(stateDir, sessionID, id)); statErr == nil {
			return id, WorkflowKindDev, nil
		}
		if _, statErr := os.Stat(InterpCheckpointPath(stateDir, sessionID, id)); statErr == nil {
			return id, WorkflowKindInterp, nil
		}
	}
	return 0, WorkflowKindNone, nil
}
