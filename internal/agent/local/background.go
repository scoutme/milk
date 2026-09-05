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
type Manager struct {
	mu      sync.Mutex
	sem     chan struct{}
	jobs    map[string]*Job
	pending []*Job
	onDone  func(*Job)
	nextID  int
}

// NewManager returns a Manager allowing at most maxConcurrent jobs to
// actually execute (queue past that) at once. maxConcurrent <= 0 is treated
// as 1.
func NewManager(maxConcurrent int) *Manager {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return &Manager{
		sem:  make(chan struct{}, maxConcurrent),
		jobs: make(map[string]*Job),
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

// Spawn launches run in a goroutine and returns immediately with a Job
// handle in JobRunning status. run does not start executing until a
// concurrency slot is free — Spawn itself never blocks the caller waiting
// for one, the queued job's own goroutine does.
func (m *Manager) Spawn(ctx context.Context, label, task, role, model string, run func(context.Context) (string, session.TokenUsage, error)) *Job {
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
	m.mu.Unlock()

	go func() {
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			m.finish(job, "", session.TokenUsage{}, ctx.Err())
			return
		}
		defer func() { <-m.sem }()

		result, tokens, err := run(ctx)
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
