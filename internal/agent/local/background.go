package local

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/scoutme/milk/internal/session"
)

// JobStatus is the lifecycle state of a background job (ADR-0043).
type JobStatus string

const (
	JobRunning   JobStatus = "running"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
)

// Job is one spawn_background_agent invocation: an independent, self-forked
// tool loop researching a single self-contained task, tracked from Spawn
// until it completes or fails.
type Job struct {
	ID        string
	Label     string
	Task      string
	Status    JobStatus
	Result    string
	Err       error
	Role      string // "primary" or "escalation" — role of the spawning agent
	Model     string
	Tokens    session.TokenUsage
	StartedAt time.Time
	EndedAt   time.Time
}

// Manager tracks background jobs spawned by an agent's spawn_background_agent
// tool calls across however many turns a session runs. Concurrency is
// bounded by a semaphore so a single turn (or a burst of turns) can't fan out
// unbounded background work; jobs queue for a slot inside their own
// goroutine rather than blocking the caller of Spawn.
//
// Spawn intentionally does not take a per-call context. The TUI cancels each
// turn's context the instant that turn's runTurn call returns (see
// cmd/milk/repl.go's `defer cancel()` immediately after starting it) — a job
// spawned mid-turn must survive past that instant, since its entire purpose
// is to keep running across whatever later turns eventually drain it. The
// Manager holds one context for its own lifetime instead, supplied at
// construction (typically the session/TUI-root context, not any turn's).
type Manager struct {
	mu            sync.Mutex
	baseCtx       context.Context
	sem           chan struct{}
	jobs          map[string]*Job
	pending       []*Job
	onStart       func(*Job)
	onDone        func(*Job)
	onBatchDone   func()
	batchSignaled bool
	nextID        int
	jobTimeout    time.Duration
}

// defaultJobTimeout bounds how long a single job may run once it starts
// executing (not counting time spent queued for a concurrency slot). This
// is a deliberate hard-stop, not a retry, and a broad backstop rather than
// the primary defense against a stuck job: the loop-detection suite (streak
// tracker, streaming n-gram monitor, text-loop tracker, duplicate-tool-call
// detection) already runs on a background job exactly as it would on a
// normal turn — cloneForBackground leaves workflowRole false specifically
// so none of the workflow-role skip conditions apply — so an actually-stuck
// job (repeating itself, re-issuing the same tool call) gets caught and
// terminated by that well before this fires. What this timeout guards
// against is different: a job that's genuinely still making progress but
// on a slow path (a heavy reasoning model, a large multi-file analysis)
// with no upper bound at all, which would otherwise hold its concurrency
// slot forever, degrading max_background_agents for the rest of the
// session. Set generously for that reason, live-verified against a task
// that was still issuing new tool-loop requests 8+ minutes in. Overridable
// per-Config via EffectiveBackgroundAgentTimeout (see cmd/milk's Manager
// construction site) — production code calling SetJobTimeout is expected,
// not just tests.
const defaultJobTimeout = 20 * time.Minute

// NewManager returns a Manager allowing at most maxConcurrent jobs to
// actually execute (queue past that) at once, running jobs under baseCtx —
// which should outlive individual turns (see the Manager doc comment).
// maxConcurrent <= 0 is treated as 1.
func NewManager(baseCtx context.Context, maxConcurrent int) *Manager {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return &Manager{
		baseCtx:    baseCtx,
		sem:        make(chan struct{}, maxConcurrent),
		jobs:       make(map[string]*Job),
		jobTimeout: defaultJobTimeout,
	}
}

// SetOnStart registers a callback fired exactly once per job, immediately
// when Spawn creates it (before the job's goroutine acquires a concurrency
// slot or starts running) — off the goroutine that called Spawn. Typically
// wired to a live UI so a panel showing background-job activity can open as
// soon as a job exists, rather than only once it finishes.
func (m *Manager) SetOnStart(fn func(*Job)) {
	m.mu.Lock()
	m.onStart = fn
	m.mu.Unlock()
}

// SetOnDone registers a callback fired exactly once per job, immediately on
// completion or failure, off the goroutine that ran the job — not on the
// caller of Spawn. Typically wired to notify a live UI; the turn-boundary
// context injection path uses Drain instead, independently.
func (m *Manager) SetOnDone(fn func(*Job)) {
	m.mu.Lock()
	m.onDone = fn
	m.mu.Unlock()
}

// SetOnBatchDone registers a callback fired exactly once when the last
// currently-outstanding job finishes (ActiveCount reaches 0) — i.e. once
// per "wave" of spawn_background_agent calls, not once per job. The flag
// guarding this resets on the next Drain, so a later wave can signal again.
// Without an explicit "the whole batch is done" signal distinct from
// per-job SetOnDone, nothing ever proactively turns "results are ready"
// into an actual response: the turn-boundary drain path (drainBackgroundJobs
// in cmd/milk/dispatch.go) only runs when a turn happens to be dispatched
// for some other reason — e.g. the user typing again — and per-job SetOnDone
// only appends a transcript line, it doesn't generate a real response
// either. This is the hook cmd/milk uses to actually dispatch a follow-up
// turn automatically once every spawned job has finished.
func (m *Manager) SetOnBatchDone(fn func()) {
	m.mu.Lock()
	m.onBatchDone = fn
	m.mu.Unlock()
}

// SetJobTimeout overrides the per-job execution timeout (see Spawn). Called
// in production from cmd/milk's Manager construction site with the
// configured (or default) value from Config.EffectiveBackgroundAgentTimeout;
// tests also use it directly to avoid waiting out the real default.
func (m *Manager) SetJobTimeout(d time.Duration) {
	m.mu.Lock()
	m.jobTimeout = d
	m.mu.Unlock()
}

// Spawn launches run in a goroutine and returns immediately with a Job
// handle in JobRunning status. run does not start executing until a
// concurrency slot is free — Spawn itself never blocks the caller waiting
// for one, the queued job's own goroutine does. run receives a context
// derived from the Manager's own baseCtx (not any context belonging to the
// turn that called Spawn), bounded by the Manager's jobTimeout once it
// starts executing.
func (m *Manager) Spawn(label, task, role, model string, run func(context.Context) (string, session.TokenUsage, error)) *Job {
	m.mu.Lock()
	m.nextID++
	job := &Job{
		ID:        fmt.Sprintf("job_%d", m.nextID),
		Label:     label,
		Task:      task,
		Status:    JobRunning,
		Role:      role,
		Model:     model,
		StartedAt: time.Now(),
	}
	m.jobs[job.ID] = job
	timeout := m.jobTimeout
	onStart := m.onStart
	m.mu.Unlock()

	if onStart != nil {
		onStart(job)
	}

	go func() {
		select {
		case m.sem <- struct{}{}:
		case <-m.baseCtx.Done():
			m.finish(job, "", session.TokenUsage{}, m.baseCtx.Err())
			return
		}
		defer func() { <-m.sem }()

		jobCtx, cancel := context.WithTimeout(m.baseCtx, timeout)
		defer cancel()
		result, tokens, err := run(jobCtx)
		m.finish(job, result, tokens, err)
	}()

	return job
}

func (m *Manager) finish(job *Job, result string, tokens session.TokenUsage, err error) {
	m.mu.Lock()
	job.Result = result
	job.Tokens = tokens
	job.Err = err
	job.EndedAt = time.Now()
	if err != nil {
		job.Status = JobFailed
	} else {
		job.Status = JobCompleted
	}
	m.pending = append(m.pending, job)
	onDone := m.onDone

	// Fire onBatchDone at most once per wave: guarded by batchSignaled so a
	// second job finishing microseconds after the one that already saw
	// ActiveCount hit 0 can't also see 0 and signal a second time (both
	// reads happen under m.mu, so they're serialized relative to each
	// other regardless of how close together the two jobs actually
	// finished). Drain resets the guard for the next wave.
	var fireBatchDone func()
	if m.onBatchDone != nil && !m.batchSignaled && m.activeCountLocked() == 0 {
		m.batchSignaled = true
		fireBatchDone = m.onBatchDone
	}
	m.mu.Unlock()

	if onDone != nil {
		onDone(job)
	}
	if fireBatchDone != nil {
		fireBatchDone()
	}
}

// Drain returns all jobs that have completed or failed since the last Drain
// call, and clears the pending list. This is the turn-boundary path: callers
// inject each returned job's result into the next turn's context. Also
// resets the SetOnBatchDone guard so the next wave of jobs can signal again.
func (m *Manager) Drain() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batchSignaled = false
	if len(m.pending) == 0 {
		return nil
	}
	out := m.pending
	m.pending = nil
	return out
}

// ActiveCount returns the number of jobs not yet completed or failed
// (queued or executing), for a live "N background agents running" indicator.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeCountLocked()
}

// activeCountLocked is ActiveCount's body, callable when m.mu is already held.
func (m *Manager) activeCountLocked() int {
	n := 0
	for _, j := range m.jobs {
		if j.Status == JobRunning {
			n++
		}
	}
	return n
}

// Jobs returns a snapshot of every job the Manager knows about (running,
// completed, or failed — including ones already Drain()ed, since Drain only
// clears the pending-delivery queue, not the job registry), oldest first.
// For display (e.g. a background-jobs panel) rather than delivery — unlike
// Drain, calling this has no side effects and can be called on every render.
//
// Returns values, not pointers: finish() mutates a Job's fields under m.mu
// from whichever goroutine ran it, so handing out live pointers would let a
// renderer on the UI goroutine race that write. A snapshot copy under the
// same lock is race-free and cheap — Job has no fields that need a deep copy.
func (m *Manager) Jobs() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].StartedAt.Before(out[k].StartedAt) })
	return out
}
