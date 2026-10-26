package session

import (
	"fmt"
	"sync"
	"testing"
)

func TestFileChanges_RecordSnapshotClear(t *testing.T) {
	const id = "changeset-t1"
	RecordFileChange(id, "/w/a.go", ChangeModified)
	RecordFileChange(id, "/w/new.go", ChangeCreated)
	RecordFileChange(id, "/w/gone.go", ChangeDeleted)

	got := FileChanges(id)
	if len(got) != 3 || got["/w/a.go"] != ChangeModified || got["/w/new.go"] != ChangeCreated || got["/w/gone.go"] != ChangeDeleted {
		t.Fatalf("snapshot = %v", got)
	}
	// Mutating the snapshot must not touch the store.
	got["/w/a.go"] = ChangeDeleted
	if k, _ := FileChange(id, "/w/a.go"); k != ChangeModified {
		t.Fatalf("snapshot leaked mutation: %v", k)
	}

	ClearFileChanges(id)
	if len(FileChanges(id)) != 0 {
		t.Fatalf("clear left %v", FileChanges(id))
	}
}

func TestFileChanges_NetAwareKinds(t *testing.T) {
	const id = "changeset-t2"
	// created then deleted → net zero, the path disappears.
	RecordFileChange(id, "/w/tmp.txt", ChangeCreated)
	RecordFileChange(id, "/w/tmp.txt", ChangeDeleted)
	if _, ok := FileChange(id, "/w/tmp.txt"); ok {
		t.Fatal("created-then-deleted must net to nothing")
	}
	// modified then deleted → deleted (it existed before the session).
	RecordFileChange(id, "/w/old.txt", ChangeModified)
	RecordFileChange(id, "/w/old.txt", ChangeDeleted)
	if k, _ := FileChange(id, "/w/old.txt"); k != ChangeDeleted {
		t.Fatalf("kind = %v, want deleted", k)
	}
	// write after create stays created; write after modify stays modified.
	RecordFileChange(id, "/w/new.go", ChangeCreated)
	RecordFileChange(id, "/w/new.go", ChangeCreated)
	RecordFileChange(id, "/w/old.go", ChangeModified)
	RecordFileChange(id, "/w/old.go", ChangeCreated) // write_file on a modified file
	if k, _ := FileChange(id, "/w/new.go"); k != ChangeCreated {
		t.Fatalf("new.go = %v, want created", k)
	}
	if k, _ := FileChange(id, "/w/old.go"); k != ChangeModified {
		t.Fatalf("old.go = %v, want modified", k)
	}
	// a modified file later deleted → deleted; then recreated → created.
	RecordFileChange(id, "/w/old.go", ChangeDeleted)
	RecordFileChange(id, "/w/old.go", ChangeCreated)
	if k, _ := FileChange(id, "/w/old.go"); k != ChangeCreated {
		t.Fatalf("recreated = %v, want created", k)
	}
	// rename keeps Created for a file this session created.
	RecordFileChange(id, "/w/made.go", ChangeCreated)
	RecordFileChange(id, "/w/made.go", ChangeRenamed)
	if k, _ := FileChange(id, "/w/made.go"); k != ChangeCreated {
		t.Fatalf("created-then-moved = %v, want created", k)
	}
}

func TestFileChanges_CapsAndGuards(t *testing.T) {
	const id = "changeset-t3"
	// Per-session path cap: FIFO eviction of the oldest path.
	for i := 0; i < fileChangesCap+10; i++ {
		RecordFileChange(id, fmt.Sprintf("/w/f%04d.go", i), ChangeModified)
	}
	got := FileChanges(id)
	if len(got) != fileChangesCap {
		t.Fatalf("len = %d, want %d", len(got), fileChangesCap)
	}
	if _, ok := FileChange(id, "/w/f0000.go"); ok {
		t.Fatal("oldest path not evicted")
	}
	// Empty ids and paths are refused (no shared "" bucket across sessions).
	RecordFileChange("", "/w/x.go", ChangeCreated)
	RecordFileChange(id, "", ChangeCreated)
	if len(FileChanges("")) != 0 {
		t.Fatal("empty id must not collect")
	}
	// Session-count cap: creating beyond the limit evicts the oldest
	// sessions first (t2/t3 predate the flood; the newest flood session
	// must survive).
	total := fileChangesSessionsCap + 6
	for i := 0; i < total; i++ {
		RecordFileChange(fmt.Sprintf("changeset-flood-%d", i), fmt.Sprintf("/w/%d", i), ChangeCreated)
	}
	if _, ok := FileChange("changeset-flood-0", "/w/0"); ok {
		t.Fatal("oldest session not evicted from the registry")
	}
	if _, ok := FileChange(id, "/w/f0001.go"); ok {
		t.Fatal("older session not evicted by registry flood")
	}
	if _, ok := FileChange(fmt.Sprintf("changeset-flood-%d", total-1), fmt.Sprintf("/w/%d", total-1)); !ok {
		t.Fatal("newest session evicted from the registry")
	}
	ClearFileChanges(id)
}

func TestFileChanges_ConcurrentAccess(t *testing.T) {
	const id = "changeset-t4"
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				RecordFileChange(id, fmt.Sprintf("/w/%d/%d.go", g, i), ChangeModified)
				_ = FileChanges(id)
			}
		}(g)
	}
	wg.Wait()
	if len(FileChanges(id)) != 1600 {
		t.Fatalf("len = %d, want 1600", len(FileChanges(id)))
	}
	ClearFileChanges(id)
}
