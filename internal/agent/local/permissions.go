package local

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// PermStore persists tool grants for the local agent.
// Grants are stored per working directory at ~/.milk/permissions/<cwd-hash>.json,
// mirroring the per-project structure of Claude's claudesettings store.
type PermStore struct {
	path string
	mu   sync.Mutex
}

type permFile struct {
	AllowedTools []string `json:"allowed_tools,omitempty"`
}

// OpenPermStore returns a PermStore for the given working directory.
// The file is created on first write; reads on a missing file return empty grants.
func OpenPermStore(cwd string) (*PermStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".milk", "permissions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	hash := fmt.Sprintf("%x", md5.Sum([]byte(cwd))) //nolint:gosec
	return &PermStore{path: filepath.Join(dir, hash+".json")}, nil
}

// IsAllowed reports whether tool has a persistent grant.
func (p *PermStore) IsAllowed(tool string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	pf, _ := p.load()
	for _, t := range pf.AllowedTools {
		if strings.EqualFold(t, tool) {
			return true
		}
	}
	return false
}

// Allow persists a grant for tool.
func (p *PermStore) Allow(tool string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	pf, _ := p.load()
	for _, t := range pf.AllowedTools {
		if strings.EqualFold(t, tool) {
			return nil // already granted
		}
	}
	pf.AllowedTools = append(pf.AllowedTools, tool)
	return p.save(pf)
}

// AllowedTools returns all persistently granted tool names.
func (p *PermStore) AllowedTools() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	pf, _ := p.load()
	out := make([]string, len(pf.AllowedTools))
	copy(out, pf.AllowedTools)
	return out
}

func (p *PermStore) load() (permFile, error) {
	var pf permFile
	data, err := os.ReadFile(p.path)
	if os.IsNotExist(err) {
		return pf, nil
	}
	if err != nil {
		return pf, err
	}
	err = json.Unmarshal(data, &pf)
	return pf, err
}

func (p *PermStore) save(pf permFile) error {
	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.path, data, 0o600)
}
