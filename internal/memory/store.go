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

	"github.com/scoutme/milk/internal/obs"
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
//
// If the incoming content is too similar to an existing Percept (Jaccard token
// overlap ≥ DuplicateSimilarityThreshold), Record skips insertion and returns
// the existing Percept's ID together with a *DuplicateError. Callers that want
// to surface a user-facing hint should type-assert the error; callers that
// want silent deduplication can ignore the error when IsDuplicate returns true.
func (s *Store) Record(ctx context.Context, content string, producer Producer, consumer Consumer, roles Roles, core bool) (string, error) {
	ctx, end := traceRecord(ctx, producer)
	defer func() { end(nil) }()

	s.mu.Lock()
	defer s.mu.Unlock()

	if dup := s.findSimilarLocked(content, DuplicateSimilarityThreshold); dup != nil {
		dupErr := &DuplicateError{Existing: *dup, Similarity: jaccardSimilarity(tokenize(content), tokenize(dup.Content))}
		obs.Debug("percept duplicate skipped", "existing_id", dup.ID, "similarity", dupErr.Similarity)
		return dup.ID, dupErr
	}

	p := Percept{
		ID:        uuid.New().String(),
		Content:   content,
		Producer:  producer,
		Consumer:  consumer,
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
	obs.Debug("percept recorded", "id", p.ID, "scope", scopeLabel, "producer", string(producer))
	return p.ID, saveErr
}

func initialWeight(producer Producer, core bool) float64 {
	if core {
		return 1.0
	}
	switch producer {
	case ProducerUser:
		return 0.9 // promotes after one session (0.9−0.10 = 0.80 == promoteThreshold)
	case ProducerSystem:
		return 0.4 // low-confidence hint; pruned after two sessions
	default: // ProducerLocal, ProducerEscalation
		return 0.7 // inference; decays over ~5 sessions; needs edge boost to promote
	}
}

// RecordGlobal writes a Percept directly to the global store regardless of
// session scope. Used by /learn.
//
// Like Record, it returns a *DuplicateError when the incoming content is too
// similar to an existing global percept, along with the ID of that percept.
func (s *Store) RecordGlobal(ctx context.Context, content string, producer Producer, consumer Consumer, roles Roles) (string, error) {
	ctx, end := traceRecord(ctx, producer)
	defer func() { end(nil) }()

	s.mu.Lock()
	defer s.mu.Unlock()

	if dup := s.findSimilarLocked(content, DuplicateSimilarityThreshold); dup != nil {
		dupErr := &DuplicateError{Existing: *dup, Similarity: jaccardSimilarity(tokenize(content), tokenize(dup.Content))}
		obs.Debug("percept duplicate skipped", "existing_id", dup.ID, "similarity", dupErr.Similarity)
		return dup.ID, dupErr
	}

	p := Percept{
		ID:        uuid.New().String(),
		Content:   content,
		Producer:  producer,
		Consumer:  consumer,
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
// caller restricts results to percepts visible to that agent (ConsumerAll = no restriction).
func (s *Store) Query(ctx context.Context, query string, minConfidence float64, maxResults int, caller Consumer) []Percept {
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
		if caller != ConsumerAll && p.Consumer != ConsumerAll && p.Consumer != caller {
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
	Scope    string   // "global" | "session" | "" (both)
	MinW     float64  // 0 means no floor
	Producer string   // filter by producer, "" means all
	Consumer Consumer // filter by consumer visibility; ConsumerAll ("") means no filter
	Pattern  string   // case-insensitive substring
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
		// Consumer filter: when set, keep only percepts whose consumer matches or is ConsumerAll.
		if opts.Consumer != ConsumerAll && p.Consumer != ConsumerAll && p.Consumer != opts.Consumer {
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

// Delete removes the Percept with the given ID from whichever store contains it.
// Returns (true, nil) if deleted, (false, nil) if not found.
func (s *Store) Delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, p := range s.global.Percepts {
		if p.ID == id {
			s.global.Percepts = append(s.global.Percepts[:i], s.global.Percepts[i+1:]...)
			return true, s.saveFile(s.globalPath, s.global)
		}
	}
	if s.sessionPath != "" {
		for i, p := range s.session.Percepts {
			if p.ID == id {
				s.session.Percepts = append(s.session.Percepts[:i], s.session.Percepts[i+1:]...)
				return true, s.saveFile(s.sessionPath, s.session)
			}
		}
	}
	return false, nil
}

// FindByIDPrefix returns all Percepts whose ID starts with the given prefix (case-insensitive).
func (s *Store) FindByIDPrefix(prefix string) []Percept {
	s.mu.Lock()
	defer s.mu.Unlock()

	lower := strings.ToLower(prefix)
	var out []Percept
	for _, p := range s.allPercepts() {
		if strings.HasPrefix(strings.ToLower(p.ID), lower) {
			out = append(out, p)
		}
	}
	return out
}

// PruneGlobal removes lowest-weight non-core percepts from the global store
// until the total count is at most max. Does nothing when max <= 0.
func (s *Store) PruneGlobal(max int) error {
	if max <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	percepts := s.global.Percepts
	if len(percepts) <= max {
		return nil
	}

	// Stable-sort ascending by weight, keeping core percepts at the top so
	// they are never evicted by the slice trim below.
	sorted := make([]Percept, len(percepts))
	copy(sorted, percepts)

	// Separate core from non-core.
	var core, nonCore []Percept
	for _, p := range sorted {
		if p.Core {
			core = append(core, p)
		} else {
			nonCore = append(nonCore, p)
		}
	}

	// Sort non-core ascending by weight so lowest-weight are at the front.
	for i := 1; i < len(nonCore); i++ {
		for j := i; j > 0 && nonCore[j].W < nonCore[j-1].W; j-- {
			nonCore[j], nonCore[j-1] = nonCore[j-1], nonCore[j]
		}
	}

	// Drop from the low-weight end of nonCore until total fits.
	total := len(core) + len(nonCore)
	drop := total - max
	if drop > len(nonCore) {
		drop = len(nonCore)
	}
	if drop > 0 {
		nonCore = nonCore[drop:]
	}

	s.global.Percepts = append(core, nonCore...)
	return s.saveFile(s.globalPath, s.global)
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
