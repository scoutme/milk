package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/session"
)

// jobRecord is the on-disk JSON shape of one Job (see Job). Err is rendered
// as a plain string — the error value itself is not serialisable, and triage
// only ever needs its message ("timed out after 20m0s: …", "panic in
// background job …", …), which already carries the cause.
type jobRecord struct {
	ID          string             `json:"id"`
	Label       string             `json:"label"`
	Task        string             `json:"task"`
	Status      JobStatus          `json:"status"`
	Result      string             `json:"result,omitempty"`
	Error       string             `json:"error,omitempty"`
	TimedOut    bool               `json:"timed_out,omitempty"`
	Role        string             `json:"role"`
	Model       string             `json:"model,omitempty"`
	Tokens      session.TokenUsage `json:"tokens"`
	StartedAt   time.Time          `json:"started_at"`
	EndedAt     time.Time          `json:"ended_at,omitempty"`
	LastAliveAt time.Time          `json:"last_alive_at,omitempty"`
}

// jobStateFile is the full on-disk document written by persistLocked — one
// per milk session (see SetStateFile's path convention and persistLocked's
// per-job attribution), rewritten atomically on every state change to that
// session's jobs.
type jobStateFile struct {
	UpdatedAt time.Time   `json:"updated_at"`
	Jobs      []jobRecord `json:"jobs"`
}

// persistLocked writes every job's record to its own state file as JSON, via
// a temp file + rename so a kill mid-write can never leave a torn document —
// triage either finds the previous consistent state or the new one.
//
// Each job belongs to the state file that was current when it was Spawned
// (Job.stateFile; jobs spawned before any file was configured follow the
// current one), so a mid-run SetStateFile re-point — the TUI does exactly this
// on /new, /clear and /drop, when a fresh session takes over — never drags
// already-spawned jobs across sessions: an in-flight job keeps updating the
// old session's file (which would otherwise freeze mid-run, a state
// LoadJobs reads as "milk died mid-job") while new jobs land in the new
// session's file. One document is written per distinct file, containing just
// that session's jobs; the currently-configured file is always written, even
// empty, so SetStateFile eagerly creates it.
//
// Best effort by design: a persistence failure (read-only home, full disk)
// must never fail the job itself, so errors are logged at debug and swallowed.
// No-op when no state file is configured. Callers must hold m.mu.
func (m *Manager) persistLocked() {
	byFile := map[string][]jobRecord{}
	for _, j := range m.jobs {
		path := j.stateFile
		if path == "" {
			path = m.stateFile
		}
		if path == "" {
			continue
		}
		byFile[path] = append(byFile[path], newJobRecord(j))
	}
	if m.stateFile != "" {
		if _, ok := byFile[m.stateFile]; !ok {
			byFile[m.stateFile] = []jobRecord{}
		}
	}
	for path, recs := range byFile {
		sort.Slice(recs, func(i, k int) bool { return recs[i].StartedAt.Before(recs[k].StartedAt) })
		writeJobStateFile(path, recs)
	}
}

// writeJobStateFile atomically rewrites one per-session job document (see
// persistLocked). Callers must hold m.mu.
func writeJobStateFile(path string, recs []jobRecord) {
	data, err := json.MarshalIndent(jobStateFile{UpdatedAt: time.Now(), Jobs: recs}, "", "  ")
	if err != nil {
		obs.Debug("job state marshal failed", "path", path, "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		obs.Debug("job state mkdir failed", "path", path, "err", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		obs.Debug("job state write failed", "path", path, "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		obs.Debug("job state rename failed", "path", path, "err", err)
	}
}

// newJobRecord snapshots one Job for persistence. Called under m.mu (finish
// mutates a Job's fields from its own goroutine).
func newJobRecord(j *Job) jobRecord {
	rec := jobRecord{
		ID:          j.ID,
		Label:       j.Label,
		Task:        j.Task,
		Status:      j.Status,
		Result:      j.Result,
		TimedOut:    j.TimedOut,
		Role:        j.Role,
		Model:       j.Model,
		Tokens:      j.Tokens,
		StartedAt:   j.StartedAt,
		EndedAt:     j.EndedAt,
		LastAliveAt: j.LastAliveAt,
	}
	if j.Err != nil {
		rec.Error = j.Err.Error()
	}
	return rec
}

// LoadJobs reads back a job state file written by the Manager (see
// SetStateFile), for triage of a previous — possibly hard-killed — milk
// session. Job.Err is reconstructed as a plain error carrying the stored
// text (the original error type is gone; Job.TimedOut separately preserves
// the timeout classification). A job still marked JobRunning in the file is
// by definition from a milk that died mid-job: LastAliveAt then says how
// recently it was actually alive.
func LoadJobs(path string) ([]Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f jobStateFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse job state %s: %w", path, err)
	}
	out := make([]Job, 0, len(f.Jobs))
	for _, rec := range f.Jobs {
		j := Job{
			ID:          rec.ID,
			Label:       rec.Label,
			Task:        rec.Task,
			Status:      rec.Status,
			Result:      rec.Result,
			TimedOut:    rec.TimedOut,
			Role:        rec.Role,
			Model:       rec.Model,
			Tokens:      rec.Tokens,
			StartedAt:   rec.StartedAt,
			EndedAt:     rec.EndedAt,
			LastAliveAt: rec.LastAliveAt,
		}
		if rec.Error != "" {
			j.Err = errors.New(rec.Error)
		}
		out = append(out, j)
	}
	return out, nil
}
