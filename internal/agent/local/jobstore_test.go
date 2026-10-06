package local

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/session"
)

// TestSetStateFile_RepointKeepsJobsAtTheirSpawnFile covers the session-swap
// attribution contract: SetStateFile may be re-pointed mid-run — the TUI does
// exactly this when /new, /clear or /drop swaps the active session while the
// Manager lives on (cmd/milk's refreshSessionScopedState) — and jobs spawned
// before the re-point must keep persisting to their spawn-time file (the old
// session's triage record) while jobs spawned after it land in the new file.
// Without that split, an in-flight job's records drift into the wrong
// session's file and the old file freezes mid-run, a state LoadJobs reads as
// "milk died mid-job".
func TestSetStateFile_RepointKeepsJobsAtTheirSpawnFile(t *testing.T) {
	tmp := t.TempDir()
	file1 := filepath.Join(tmp, "jobs-old-session.json")
	file2 := filepath.Join(tmp, "jobs-new-session.json")

	mgr := NewManager(context.Background(), 2)
	mgr.SetStateFile(file1)
	done := make(chan *Job, 2)
	mgr.SetOnDone(func(j *Job) { done <- j })

	// "old-session-job" stays running across the re-point, like a real
	// spawn_background_agent fork that outlives the /new that swapped away
	// the session that spawned it.
	release := make(chan struct{})
	mgr.Spawn("old-session-job", "t", "primary", "m",
		func(context.Context, string, io.Writer) (string, session.TokenUsage, error) {
			<-release
			return "from old", session.TokenUsage{}, nil
		})

	// Re-point mid-run, exactly like refreshSessionScopedState does on /new.
	mgr.SetStateFile(file2)
	mgr.Spawn("new-session-job", "t", "primary", "m",
		func(context.Context, string, io.Writer) (string, session.TokenUsage, error) {
			return "from new", session.TokenUsage{}, nil
		})

	close(release)
	waitDone(t, done, 5*time.Second)
	waitDone(t, done, 5*time.Second)

	oldRecs, err := LoadJobs(file1)
	if err != nil {
		t.Fatalf("LoadJobs(%s): %v", file1, err)
	}
	if len(oldRecs) != 1 || oldRecs[0].Label != "old-session-job" ||
		oldRecs[0].Status != JobCompleted || oldRecs[0].Result != "from old" {
		t.Fatalf("old session file records = %+v, want exactly the completed old-session job", oldRecs)
	}
	newRecs, err := LoadJobs(file2)
	if err != nil {
		t.Fatalf("LoadJobs(%s): %v", file2, err)
	}
	if len(newRecs) != 1 || newRecs[0].Label != "new-session-job" ||
		newRecs[0].Status != JobCompleted || newRecs[0].Result != "from new" {
		t.Fatalf("new session file records = %+v, want exactly the completed new-session job", newRecs)
	}
}

// TestSetStateFile_CreatesEmptyFileImmediately pins the eager-create side of
// SetStateFile: the configured file exists (with an empty jobs list) as soon
// as persistence is enabled, before any job runs — triage of a milk killed
// before its first spawn can still tell "no jobs" from "no persistence".
func TestSetStateFile_CreatesEmptyFileImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs", "sess.json")
	mgr := NewManager(context.Background(), 1)
	mgr.SetStateFile(path)

	recs, err := LoadJobs(path)
	if err != nil {
		t.Fatalf("LoadJobs(%s): %v", path, err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected an empty jobs list, got %+v", recs)
	}
}
