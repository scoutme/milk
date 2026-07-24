package tasks

import (
	"github.com/scoutme/milk/internal/agent/local"
)

// Adapter wraps a Store and implements local.TaskStore so it can be passed
// directly to local.Agent.WithTaskStore without import cycles.
type Adapter struct {
	store *Store
}

// NewAdapter returns an Adapter wrapping s.
func NewAdapter(s *Store) *Adapter {
	return &Adapter{store: s}
}

// Create implements local.TaskStore.
func (a *Adapter) Create(title string, tags []string) (local.TaskEntry, error) {
	t, err := a.store.Create(title, tags)
	if err != nil {
		return local.TaskEntry{}, err
	}
	return local.TaskEntry{ID: t.ID, Title: t.Title, Status: t.Status, Tags: t.Tags}, nil
}

// Update implements local.TaskStore.
func (a *Adapter) Update(id, status, title string) error {
	return a.store.Update(id, status, title)
}

// Complete implements local.TaskStore.
func (a *Adapter) Complete(id string) error {
	return a.store.Complete(id)
}

// List implements local.TaskStore.
func (a *Adapter) List(includeGlobal bool) ([]local.TaskEntry, error) {
	tasks, err := a.store.List(ListOpts{IncludeGlobal: includeGlobal})
	if err != nil {
		return nil, err
	}
	out := make([]local.TaskEntry, len(tasks))
	for i, t := range tasks {
		out[i] = local.TaskEntry{ID: t.ID, Title: t.Title, Status: t.Status, Tags: t.Tags}
	}
	return out, nil
}
