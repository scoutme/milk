// Package claudesettings manages the Claude Code project-level settings file
// (~/.claude/projects/<encoded-cwd>/settings.json). milk uses this to persist
// tool and directory approvals granted during a session so Claude never asks
// for the same permission twice.
package claudesettings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// userSettings mirrors the relevant subset of ~/.claude/settings.json.
type userSettings struct {
	AWSAuthRefresh string `json:"awsAuthRefresh"`
}

// AWSAuthRefreshCommand reads the awsAuthRefresh command from the user-level
// ~/.claude/settings.json. Returns "" if the key is absent or the file is unreadable.
func AWSAuthRefreshCommand() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return ""
	}
	var s userSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return ""
	}
	return s.AWSAuthRefresh
}

// Settings mirrors the relevant subset of Claude's project settings.json.
type Settings struct {
	Permissions struct {
		Allow                 []string `json:"allow"`
		AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
	} `json:"permissions"`
}

// Store provides thread-safe read/modify/write access to a Claude project
// settings.json file. The zero value is not usable; use Open.
type Store struct {
	path string
	mu   sync.Mutex
}

// Open returns a Store for the Claude project settings file corresponding to
// the given working directory. The file and its parent directory are created
// on first write if they don't exist.
func Open(cwd string) (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	encoded := strings.ReplaceAll(cwd, "/", "-")
	dir := filepath.Join(home, ".claude", "projects", encoded)
	return &Store{path: filepath.Join(dir, "settings.json")}, nil
}

// AllowTool appends "ToolName(**)" to the allow list if not already present.
func (s *Store) AllowTool(toolName string) error {
	entry := toolName + "(**)"
	return s.appendAllow(entry)
}

// AllowDirectory appends the directory to additionalDirectories and a
// Read/Bash wildcard allow entry if not already present.
func (s *Store) AllowDirectory(dir string) error {
	return s.modify(func(cfg *Settings) {
		glob := filepath.Join(dir, "**")
		readEntry := "Read(" + glob + ")"
		if !contains(cfg.Permissions.Allow, readEntry) {
			cfg.Permissions.Allow = append(cfg.Permissions.Allow, readEntry)
		}
		bashEntry := "Bash(" + dir + "/**)"
		if !contains(cfg.Permissions.Allow, bashEntry) {
			cfg.Permissions.Allow = append(cfg.Permissions.Allow, bashEntry)
		}
		if !contains(cfg.Permissions.AdditionalDirectories, dir) {
			cfg.Permissions.AdditionalDirectories = append(cfg.Permissions.AdditionalDirectories, dir)
		}
	})
}

// AllowedTools returns the list of tool names that have a wildcard allow entry
// (i.e. entries of the form "ToolName(**)"). Used to pre-populate --allowedTools
// on each new agent invocation so grants survive across turns.
func (s *Store) AllowedTools() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.load()
	if err != nil {
		return nil, err
	}
	var tools []string
	for _, entry := range cfg.Permissions.Allow {
		if name, ok := strings.CutSuffix(entry, "(**)"); ok {
			tools = append(tools, name)
		}
	}
	return tools, nil
}

// AllowedDirectories returns the additionalDirectories list.
// Used to pre-populate --add-dir on each new agent invocation.
func (s *Store) AllowedDirectories() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.load()
	if err != nil {
		return nil, err
	}
	return cfg.Permissions.AdditionalDirectories, nil
}

// IsToolAllowed returns true if a wildcard allow entry exists for the tool.
func (s *Store) IsToolAllowed(toolName string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.load()
	if err != nil {
		return false, err
	}
	entry := toolName + "(**)"
	return contains(cfg.Permissions.Allow, entry), nil
}

func (s *Store) appendAllow(entry string) error {
	return s.modify(func(cfg *Settings) {
		if !contains(cfg.Permissions.Allow, entry) {
			cfg.Permissions.Allow = append(cfg.Permissions.Allow, entry)
		}
	})
}

func (s *Store) modify(fn func(*Settings)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.load()
	if err != nil {
		return err
	}
	fn(&cfg)
	return s.save(cfg)
}

func (s *Store) load() (Settings, error) {
	var cfg Settings
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(data, &cfg)
	return cfg, err
}

func (s *Store) save(cfg Settings) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
