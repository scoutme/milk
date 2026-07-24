// Package tasks provides a lightweight persistent task tracker for milk sessions.
// Tasks are stored in two JSON files per session:
//
//   - ~/.milk/tasks/<session-id>.json  — session-local tasks
//   - ~/.milk/tasks/global.json        — cross-session global tasks
package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Status values for a Task.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusDone       = "done"
	StatusBlocked    = "blocked"
)

// Task is a single trackable item.
type Task struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	SessionID string    `json:"session_id,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
}

// Store manages session and global tasks backed by JSON files on disk.
type Store struct {
	mu        sync.Mutex
	dir       string
	sessionID string
	onChange  func() // called after every mutating operation; may be nil
}

// New creates or opens a task store rooted at dir for the given sessionID.
// dir is typically ~/.milk/tasks/. It is created if it does not exist.
func New(dir, sessionID string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("tasks: mkdir %s: %w", dir, err)
	}
	return &Store{dir: dir, sessionID: sessionID}, nil
}

// SetOnChange registers a callback fired after Create/Update/Complete/Delete.
func (s *Store) SetOnChange(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = fn
}

// Create adds a new task to the session store and returns the new Task.
func (s *Store) Create(title string, tags []string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := Task{
		ID:        uuid.New().String()[:8],
		Title:     title,
		Status:    StatusPending,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		SessionID: s.sessionID,
		Tags:      tags,
	}
	tasks, err := s.readSession()
	if err != nil {
		return Task{}, err
	}
	tasks = append(tasks, t)
	if err := s.writeSession(tasks); err != nil {
		return Task{}, err
	}
	s.notify()
	return t, nil
}

// Update sets the status (and optionally title) of a task by ID.
// It searches session tasks first, then global tasks.
func (s *Store) Update(id, status, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Try session tasks first.
	tasks, err := s.readSession()
	if err != nil {
		return err
	}
	for i, t := range tasks {
		if t.ID == id || idPrefixMatch(t.ID, id) {
			tasks[i].Status = status
			if title != "" {
				tasks[i].Title = title
			}
			tasks[i].UpdatedAt = time.Now()
			if err := s.writeSession(tasks); err != nil {
				return err
			}
			s.notify()
			return nil
		}
	}
	// Try global tasks.
	global, err := s.readGlobal()
	if err != nil {
		return err
	}
	for i, t := range global {
		if t.ID == id || idPrefixMatch(t.ID, id) {
			global[i].Status = status
			if title != "" {
				global[i].Title = title
			}
			global[i].UpdatedAt = time.Now()
			if err := s.writeGlobal(global); err != nil {
				return err
			}
			s.notify()
			return nil
		}
	}
	return fmt.Errorf("task %q not found", id)
}

// Complete marks a task done. Equivalent to Update(id, StatusDone, "").
func (s *Store) Complete(id string) error {
	return s.Update(id, StatusDone, "")
}

// Delete removes a task by ID from session or global store.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.readSession()
	if err != nil {
		return err
	}
	filtered := tasks[:0]
	deleted := false
	for _, t := range tasks {
		if t.ID == id || idPrefixMatch(t.ID, id) {
			deleted = true
			continue
		}
		filtered = append(filtered, t)
	}
	if deleted {
		if err := s.writeSession(filtered); err != nil {
			return err
		}
		s.notify()
		return nil
	}
	global, err := s.readGlobal()
	if err != nil {
		return err
	}
	filteredG := global[:0]
	for _, t := range global {
		if t.ID == id || idPrefixMatch(t.ID, id) {
			deleted = true
			continue
		}
		filteredG = append(filteredG, t)
	}
	if deleted {
		if err := s.writeGlobal(filteredG); err != nil {
			return err
		}
		s.notify()
		return nil
	}
	return fmt.Errorf("task %q not found", id)
}

// Promote moves a session task to the global store so it survives session end.
func (s *Store) Promote(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks, err := s.readSession()
	if err != nil {
		return err
	}
	var found *Task
	var remaining []Task
	for _, t := range tasks {
		tc := t
		if t.ID == id || idPrefixMatch(t.ID, id) {
			found = &tc
		} else {
			remaining = append(remaining, tc)
		}
	}
	if found == nil {
		return fmt.Errorf("task %q not found in session", id)
	}
	global, err := s.readGlobal()
	if err != nil {
		return err
	}
	global = append(global, *found)
	if err := s.writeGlobal(global); err != nil {
		return err
	}
	if err := s.writeSession(remaining); err != nil {
		return err
	}
	s.notify()
	return nil
}

// ListOpts controls what List returns.
type ListOpts struct {
	IncludeGlobal bool
	StatusFilter  string // empty = all statuses
}

// List returns tasks matching opts. Session tasks come first, then global.
func (s *Store) List(opts ListOpts) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.readSession()
	if err != nil {
		return nil, err
	}
	var out []Task
	for _, t := range session {
		if opts.StatusFilter != "" && t.Status != opts.StatusFilter {
			continue
		}
		out = append(out, t)
	}
	if opts.IncludeGlobal {
		global, err := s.readGlobal()
		if err != nil {
			return nil, err
		}
		for _, t := range global {
			if opts.StatusFilter != "" && t.Status != opts.StatusFilter {
				continue
			}
			out = append(out, t)
		}
	}
	return out, nil
}

// --- internal helpers ---

func (s *Store) sessionPath() string {
	return filepath.Join(s.dir, s.sessionID+".json")
}

func (s *Store) globalPath() string {
	return filepath.Join(s.dir, "global.json")
}

func (s *Store) readSession() ([]Task, error) {
	return readTasks(s.sessionPath())
}

func (s *Store) readGlobal() ([]Task, error) {
	return readTasks(s.globalPath())
}

func (s *Store) writeSession(tasks []Task) error {
	return writeTasks(s.sessionPath(), tasks)
}

func (s *Store) writeGlobal(tasks []Task) error {
	return writeTasks(s.globalPath(), tasks)
}

func (s *Store) notify() {
	if s.onChange != nil {
		s.onChange()
	}
}

func readTasks(path string) ([]Task, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []Task{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tasks: read %s: %w", path, err)
	}
	var tasks []Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("tasks: parse %s: %w", path, err)
	}
	return tasks, nil
}

func writeTasks(path string, tasks []Task) error {
	if tasks == nil {
		tasks = []Task{}
	}
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return fmt.Errorf("tasks: marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("tasks: write %s: %w", path, err)
	}
	return nil
}

// idPrefixMatch returns true when id starts with prefix (case-sensitive).
// Allows short-prefix matching, e.g. "a1b2c3" matches "a1b2c3d4".
func idPrefixMatch(id, prefix string) bool {
	return len(prefix) >= 4 && len(id) >= len(prefix) && id[:len(prefix)] == prefix
}
