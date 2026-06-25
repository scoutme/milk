package session

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/obs"
)

type IndexEntry struct {
	ID       string    `json:"id"`
	Name     string    `json:"name,omitempty"`
	LastUsed time.Time `json:"last_used"`
}

// index maps cwd → []IndexEntry sorted by last_used desc
type index map[string][]IndexEntry

func sessionsDir() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sessions"), nil
}

func indexPath() (string, error) {
	dir, err := sessionsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "index.json"), nil
}

func sessionPath(id string) (string, error) {
	dir, err := sessionsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

func loadIndex() (index, error) {
	path, err := indexPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return make(index), nil
	}
	if err != nil {
		return nil, err
	}
	var idx index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	return idx, nil
}

func saveIndex(idx index) error {
	dir, err := sessionsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	path, err := indexPath()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func repairIndex(idx index) (index, error) {
	dir, err := sessionsDir()
	if err != nil {
		return idx, err
	}
	for cwd, entries := range idx {
		valid := entries[:0]
		for _, e := range entries {
			p := filepath.Join(dir, e.ID+".json")
			if _, err := os.Stat(p); err == nil {
				valid = append(valid, e)
			}
		}
		if len(valid) == 0 {
			delete(idx, cwd)
		} else {
			idx[cwd] = valid
		}
	}
	return idx, nil
}

func upsertIndex(idx index, cwd string, s *Session) {
	entries := idx[cwd]
	for i, e := range entries {
		if e.ID == s.ID {
			entries[i].Name = s.Name
			entries[i].LastUsed = s.LastUsed
			sort.Slice(entries, func(a, b int) bool {
				return entries[a].LastUsed.After(entries[b].LastUsed)
			})
			idx[cwd] = entries
			return
		}
	}
	entries = append([]IndexEntry{{ID: s.ID, Name: s.Name, LastUsed: s.LastUsed}}, entries...)
	sort.Slice(entries, func(a, b int) bool {
		return entries[a].LastUsed.After(entries[b].LastUsed)
	})
	idx[cwd] = entries
}

// New creates a new session for the given cwd and name, persists it, and returns it.
func New(cwd, name string) (*Session, error) {
	s := &Session{
		ID:        uuid.New().String(),
		Name:      name,
		CWD:       cwd,
		CreatedAt: time.Now(),
		LastUsed:  time.Now(),
		State:     StateRouting,
		History:   []Turn{},
	}
	if err := Save(s); err != nil {
		return nil, err
	}
	return s, nil
}

// Save writes the session file and updates the index.
func Save(s *Session) error {
	dir, err := sessionsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, s.ID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		obs.Error("session save failed", "id", s.ID, "err", err)
		return err
	}
	obs.Debug("session saved", "id", s.ID)

	idx, err := loadIndex()
	if err != nil {
		return err
	}
	upsertIndex(idx, s.CWD, s)
	return saveIndex(idx)
}

// Load reads a session file by ID.
func Load(id string) (*Session, error) {
	path, err := sessionPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	// Clear CurrentNeed if it was already fulfilled before the session ended:
	// if any escalation assistant turn occurred after CurrentNeedSetAt, the need
	// was handled and should not be shown as active on resume.
	if s.CurrentNeed != "" && !s.NeedChangedSinceLastEscalation() {
		s.CurrentNeed = ""
		s.CurrentNeedSetAt = 0
	}
	obs.Debug("session loaded", "id", s.ID)
	return &s, nil
}

// Resume returns the most recent session for cwd, or creates a new one if none exists.
// If name is non-empty, it finds or creates a session with that name.
func Resume(cwd, name string) (*Session, error) {
	idx, err := loadIndex()
	if err != nil {
		return nil, err
	}
	idx, err = repairIndex(idx)
	if err != nil {
		return nil, err
	}

	entries := idx[cwd]
	if name != "" {
		for _, e := range entries {
			if e.Name == name {
				return Load(e.ID)
			}
		}
		return New(cwd, name)
	}
	if len(entries) > 0 {
		return Load(entries[0].ID)
	}
	return New(cwd, "")
}

// Drop removes a session file and its index entry.
func Drop(id, cwd string) error {
	path, err := sessionPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}

	idx, err := loadIndex()
	if err != nil {
		return err
	}
	entries := idx[cwd]
	filtered := entries[:0]
	for _, e := range entries {
		if e.ID != id {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == 0 {
		delete(idx, cwd)
	} else {
		idx[cwd] = filtered
	}
	return saveIndex(idx)
}

// List returns index entries for the given cwd (or all cwds if cwd is empty).
func List(cwd string) (map[string][]IndexEntry, error) {
	idx, err := loadIndex()
	if err != nil {
		return nil, err
	}
	idx, err = repairIndex(idx)
	if err != nil {
		return nil, err
	}
	if cwd == "" {
		return idx, nil
	}
	return map[string][]IndexEntry{cwd: idx[cwd]}, nil
}

// CWDHash returns a short hash of a path, used for display/debug purposes only.
func CWDHash(cwd string) string {
	h := sha256.Sum256([]byte(cwd))
	return fmt.Sprintf("%x", h[:6])
}
