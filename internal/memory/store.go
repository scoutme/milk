package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// storeFile is the JSON structure persisted to disk.
type storeFile struct {
	Percepts []Percept `json:"percepts"`
	Engrams  []Engram  `json:"engrams"`
	Edges    []Edge    `json:"edges"`
}

// Store is the in-process memory store. It holds global and session-scoped
// Percepts in memory and persists to ~/.milk/memory/<scope>.json.
type Store struct {
	mu          sync.Mutex
	globalPath  string
	sessionPath string // empty when no session scope
	sessionID   string

	global  storeFile
	session storeFile
}

// NewStore opens (or creates) the global store and optional session store.
// baseDir is typically ~/.milk/memory. sessionID may be empty.
func NewStore(baseDir, sessionID string) (*Store, error) {
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		globalPath: filepath.Join(baseDir, "global.json"),
		sessionID:  sessionID,
	}
	if sessionID != "" {
		s.sessionPath = filepath.Join(baseDir, sessionID+".json")
	}

	if err := s.loadFile(s.globalPath, &s.global); err != nil {
		return nil, err
	}
	if s.sessionPath != "" {
		if err := s.loadFile(s.sessionPath, &s.session); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) loadFile(path string, dst *storeFile) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func (s *Store) saveFile(path string, src storeFile) error {
	data, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Record adds a new Percept to the session store (or global if no session).
// Returns the new Percept's ID.
func (s *Store) Record(ctx context.Context, content string, producer Producer, roles Roles, core bool) (string, error) {
	ctx, end := traceRecord(ctx, producer)
	defer func() { end(nil) }()

	s.mu.Lock()
	defer s.mu.Unlock()

	p := Percept{
		ID:        uuid.New().String(),
		Content:   content,
		Producer:  producer,
		W:         initialWeight(producer, core),
		Roles:     roles,
		Core:      core,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	var scopeLabel string
	var saveErr error
	if core || s.sessionPath == "" {
		s.global.Percepts = append(s.global.Percepts, p)
		saveErr = s.saveFile(s.globalPath, s.global)
		scopeLabel = "global"
	} else {
		s.session.Percepts = append(s.session.Percepts, p)
		saveErr = s.saveFile(s.sessionPath, s.session)
		scopeLabel = "session"
	}

	logPercept(ctx, p, s.sessionID)
	metricsRecord(ctx, producer, scopeLabel)
	return p.ID, saveErr
}

func initialWeight(producer Producer, core bool) float64 {
	if core {
		return 1.0
	}
	switch producer {
	case ProducerUser:
		return 1.0
	case ProducerSystem:
		return 0.5
	default:
		return 0.7
	}
}

// RecordGlobal writes a Percept directly to the global store regardless of
// session scope. Used by /learn.
func (s *Store) RecordGlobal(ctx context.Context, content string, producer Producer, roles Roles) (string, error) {
	ctx, end := traceRecord(ctx, producer)
	defer func() { end(nil) }()

	s.mu.Lock()
	defer s.mu.Unlock()

	p := Percept{
		ID:        uuid.New().String(),
		Content:   content,
		Producer:  producer,
		W:         1.0,
		Core:      true,
		Roles:     roles,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	s.global.Percepts = append(s.global.Percepts, p)

	logPercept(ctx, p, s.sessionID)
	metricsRecord(ctx, producer, "global")
	return p.ID, s.saveFile(s.globalPath, s.global)
}

// Query returns Percepts matching a keyword query, ordered by weight desc.
// maxResults <= 0 means no limit. minConfidence filters by W.
func (s *Store) Query(ctx context.Context, query string, minConfidence float64, maxResults int) []Percept {
	ctx, end := traceRecall(ctx, query)

	s.mu.Lock()
	defer s.mu.Unlock()

	all := s.allPercepts()
	lower := strings.ToLower(query)

	var candidates []Percept
	for _, p := range all {
		if p.W < minConfidence {
			continue
		}
		if lower == "" || strings.Contains(strings.ToLower(p.Content), lower) {
			candidates = append(candidates, p)
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].W > candidates[j].W
	})

	if maxResults > 0 && len(candidates) > maxResults {
		candidates = candidates[:maxResults]
	}

	end(len(candidates), nil)
	logRecall(ctx, query, len(candidates), s.sessionID)
	metricsRecall(ctx)
	return candidates
}

// ListOpts filters for List.
type ListOpts struct {
	Scope    string  // "global" | "session" | "" (both)
	MinW     float64 // 0 means no floor
	Producer string  // filter by producer, "" means all
	Pattern  string  // case-insensitive substring
}

// List returns Percepts matching opts, ordered by weight desc.
func (s *Store) List(opts ListOpts) []Percept {
	s.mu.Lock()
	defer s.mu.Unlock()

	var pool []Percept
	switch opts.Scope {
	case "global":
		pool = append(pool, s.global.Percepts...)
	case "session":
		pool = append(pool, s.session.Percepts...)
	default:
		pool = append(pool, s.global.Percepts...)
		pool = append(pool, s.session.Percepts...)
	}

	lower := strings.ToLower(opts.Pattern)
	var out []Percept
	for _, p := range pool {
		if opts.MinW > 0 && p.W < opts.MinW {
			continue
		}
		if opts.Producer != "" && string(p.Producer) != opts.Producer {
			continue
		}
		if lower != "" && !strings.Contains(strings.ToLower(p.Content), lower) {
			continue
		}
		out = append(out, p)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].W > out[j].W
	})
	return out
}

// AllGlobal returns all Percepts in the global store (for consolidation / recall injection).
func (s *Store) AllGlobal() []Percept {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Percept, len(s.global.Percepts))
	copy(out, s.global.Percepts)
	return out
}

func (s *Store) allPercepts() []Percept {
	out := make([]Percept, 0, len(s.global.Percepts)+len(s.session.Percepts))
	out = append(out, s.global.Percepts...)
	out = append(out, s.session.Percepts...)
	return out
}

// Flush persists both stores to disk.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveFile(s.globalPath, s.global); err != nil {
		return err
	}
	if s.sessionPath != "" {
		return s.saveFile(s.sessionPath, s.session)
	}
	return nil
}

// sessionPercepts returns a copy of the session-scoped Percept list (locked by caller).
func (s *Store) sessionPercepts() []Percept {
	out := make([]Percept, len(s.session.Percepts))
	copy(out, s.session.Percepts)
	return out
}

// replaceSessionPercepts overwrites the in-memory session list (locked by caller).
func (s *Store) replaceSessionPercepts(percepts []Percept) {
	s.session.Percepts = percepts
}

// promoteToGlobal appends a list of Percepts to the global store (locked by caller).
func (s *Store) promoteToGlobal(percepts []Percept) {
	s.global.Percepts = append(s.global.Percepts, percepts...)
}
