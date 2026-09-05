package local

import (
	"context"
	"fmt"
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
	mu         sync.Mutex
	baseCtx    context.Context
	sem        chan struct{}
	jobs       map[string]*Job
	pending    []*Job
	onDone     func(*Job)
	nextID     int
	jobTimeout time.Duration
}

// defaultJobTimeout bounds how long a single job may run once it starts
// executing (not counting time spent queued for a concurrency slot). This
// is a deliberate hard-stop, not a retry: a job that runs this long is far
// more likely stuck than making real progress on a task meant to be narrow
// and self-contained (ADR-0043), and a permanently stuck job would
// otherwise hold its concurrency slot forever, degrading max_background_agents
// for the rest of the session. Terminating cleanly guarantees the Manager's
// core promise — every job eventually reaches JobCompleted or JobFailed and
// fires onDone — holds even when a job's own execution never would on its
// own (a hung network call, or any future bug in the tool loop).
const defaultJobTimeout = 10 * time.Minute

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

// SetOnDone registers a callback fired exactly once per job, immediately on
// completion or failure, off the goroutine that ran the job — not on the
// caller of Spawn. Typically wired to notify a live UI; the turn-boundary
// context injection path uses Drain instead, independently.
func (m *Manager) SetOnDone(fn func(*Job)) {
	m.mu.Lock()
	m.onDone = fn
	m.mu.Unlock()
}

// SetJobTimeout overrides the per-job execution timeout (see Spawn). Tests
// use this to avoid waiting out defaultJobTimeout for real; production
// code generally has no reason to call it.
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
	m.mu.Unlock()

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
	m.mu.Unlock()

	if onDone != nil {
		onDone(job)
	}
}

// Drain returns all jobs that have completed or failed since the last Drain
// call, and clears the pending list. This is the turn-boundary path: callers
// inject each returned job's result into the next turn's context.
func (m *Manager) Drain() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	n := 0
	for _, j := range m.jobs {
		if j.Status == JobRunning {
			n++
		}
	}
	return n
}
