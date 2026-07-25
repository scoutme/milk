package tasks

import (
	"fmt"
	"sync"
	"testing"
)

func TestStore_CreateAndList(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "test-session")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	task, err := s.Create("implement login", []string{"auth", "ui"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if task.ID == "" || task.Title != "implement login" || task.Status != StatusPending {
		t.Errorf("unexpected task: %+v", task)
	}
	if len(task.Tags) != 2 {
		t.Errorf("tags = %v, want 2", task.Tags)
	}

	tasks, err := s.List(ListOpts{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Errorf("List returned %v", tasks)
	}
}

func TestStore_Update(t *testing.T) {
	s, _ := New(t.TempDir(), "sess")
	task, _ := s.Create("write tests", nil)

	if err := s.Update(task.ID, StatusInProgress, ""); err != nil {
		t.Fatalf("Update: %v", err)
	}
	tasks, _ := s.List(ListOpts{})
	if tasks[0].Status != StatusInProgress {
		t.Errorf("status = %q, want in_progress", tasks[0].Status)
	}

	// Update with title change.
	if err := s.Update(task.ID, StatusDone, "write tests v2"); err != nil {
		t.Fatalf("Update with title: %v", err)
	}
	tasks, _ = s.List(ListOpts{})
	if tasks[0].Title != "write tests v2" {
		t.Errorf("title = %q after update", tasks[0].Title)
	}
}

func TestStore_Complete(t *testing.T) {
	s, _ := New(t.TempDir(), "sess")
	task, _ := s.Create("deploy", nil)

	if err := s.Complete(task.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	tasks, _ := s.List(ListOpts{})
	if tasks[0].Status != StatusDone {
		t.Errorf("status = %q after Complete", tasks[0].Status)
	}
}

func TestStore_Delete(t *testing.T) {
	s, _ := New(t.TempDir(), "sess")
	task, _ := s.Create("to delete", nil)
	s.Create("keep", nil) //nolint:errcheck

	if err := s.Delete(task.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	tasks, _ := s.List(ListOpts{})
	if len(tasks) != 1 || tasks[0].Title != "keep" {
		t.Errorf("after Delete: %v", tasks)
	}
}

func TestStore_GlobalPersists(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, "sess1")
	task, _ := s.Create("global task", nil)
	if err := s.Promote(task.ID); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	// Open a new store with a different session ID — global task should be visible.
	s2, _ := New(dir, "sess2")
	tasks, err := s2.List(ListOpts{IncludeGlobal: true})
	if err != nil {
		t.Fatalf("List global: %v", err)
	}
	found := false
	for _, t := range tasks {
		if t.ID == task.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("global task not found after re-open; tasks: %v", tasks)
	}
}

func TestStore_PersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, "sess")
	t1, _ := s.Create("task one", nil)
	s.Create("task two", nil) //nolint:errcheck
	s.Complete(t1.ID)         //nolint:errcheck

	// Re-open the same store.
	s2, _ := New(dir, "sess")
	tasks, _ := s2.List(ListOpts{})
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks after re-open, got %d", len(tasks))
	}
	for _, task := range tasks {
		if task.ID == t1.ID && task.Status != StatusDone {
			t.Errorf("task one status = %q, want done", task.Status)
		}
	}
}

func TestStore_ConcurrentSafety(t *testing.T) {
	s, _ := New(t.TempDir(), "sess")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := s.Create(fmt.Sprintf("task %d", n), nil)
			if err != nil {
				t.Errorf("concurrent Create: %v", err)
			}
		}(i)
	}
	wg.Wait()
	tasks, _ := s.List(ListOpts{})
	if len(tasks) != 20 {
		t.Errorf("expected 20 tasks after concurrent creates, got %d", len(tasks))
	}
}

func TestStore_OnChange(t *testing.T) {
	s, _ := New(t.TempDir(), "sess")
	var count int
	s.SetOnChange(func() { count++ })

	s.Create("t1", nil) //nolint:errcheck
	s.Create("t2", nil) //nolint:errcheck
	tasks, _ := s.List(ListOpts{})
	s.Complete(tasks[0].ID) //nolint:errcheck

	if count != 3 {
		t.Errorf("onChange called %d times, want 3", count)
	}
}

func TestStore_UpdateNotFound(t *testing.T) {
	s, _ := New(t.TempDir(), "sess")
	if err := s.Update("nonexistent", StatusDone, ""); err == nil {
		t.Error("expected error for unknown task ID, got nil")
	}
}
