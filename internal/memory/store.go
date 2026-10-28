package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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

	params Params

	archivePath string
	// lastTick is when idle time was last accounted (store open, then every
	// consolidation). clock is time.Now except in tests.
	lastTick time.Time
	clock    func() time.Time
	// evidenceAwarded tracks which Percepts already earned their Evidence
	// point in this Store's lifetime (one milk run): corroboration counts at
	// most once per run, so Evidence >= 2 implies two distinct runs
	// (ADR-0090). In-memory only — the count persists, the set does not.
	evidenceAwarded map[string]struct{}
}

// NewStore opens (or creates) the global store and optional session store
// with the default weight-model parameters.
// baseDir is typically ~/.milk/memory. sessionID may be empty.
func NewStore(baseDir, sessionID string) (*Store, error) {
	return NewStoreParams(baseDir, sessionID, Params{})
}

// NewStoreParams is NewStore with explicit weight-model parameters (zero
// fields fall back to the package defaults — see Params.normalized).
func NewStoreParams(baseDir, sessionID string, p Params) (*Store, error) {
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		globalPath:  filepath.Join(baseDir, "global.json"),
		archivePath: filepath.Join(baseDir, "global-archive.jsonl"),
		sessionID:   sessionID,
		params:      p.normalized(),
		clock:       time.Now,
	}
	s.lastTick = s.now()
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

// RecordOutcome describes what a Record/RecordGlobal call did (ADR-0053).
type RecordOutcome struct {
	// ID is the percept's ID — the existing percept's when Reinforced.
	ID string
	// Reinforced is true when the content matched a near-duplicate and the
	// existing percept was strengthened instead of a new one being created.
	Reinforced bool
	// Similarity is the Jaccard overlap that triggered reinforcement.
	Similarity float64
	// Existing is the reinforced percept's content (empty for new inserts).
	Existing string
	// W is the percept's resulting weight (after reinforcement or at birth).
	W float64
}

// Record adds a new Percept to the session store (or global if no session).
// Returns the new Percept's ID.
//
// If the incoming content is too similar to an existing Percept (Jaccard
// overlap ≥ DuplicateSimilarityThreshold), Record does NOT insert a new
// Percept: it reinforces the existing one (W += ReconfirmBoost capped at
// ReconfirmCap, UpdatedAt touched — a re-stated fact is corroborated, not
// rejected) and reports Reinforced in the outcome.
func (s *Store) Record(ctx context.Context, content string, producer Producer, consumer Consumer, roles Roles, core bool) (RecordOutcome, error) {
	ctx, end := traceRecord(ctx, producer)
	defer func() { end(nil) }()

	s.mu.Lock()
	defer s.mu.Unlock()

	if dup := s.findSimilarLocked(content, DuplicateSimilarityThreshold); dup != nil {
		return s.reinforceLocked(ctx, dup.ID, jaccardSimilarity(tokenize(content), tokenize(dup.Content)))
	}

	p := Percept{
		ID:        uuid.New().String(),
		Content:   content,
		Producer:  producer,
		Consumer:  consumer,
		W:         initialWeight(producer, core),
		Roles:     roles,
		Core:      core,
		CreatedAt: s.now(),
		UpdatedAt: s.now(),
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
	return RecordOutcome{ID: p.ID, W: p.W}, saveErr
}

// reinforceLocked boosts the existing Percept with the given ID in whichever
// store holds it (W += ReconfirmBoost capped at ReconfirmCap, UpdatedAt
// touched, Evidence +1 at most once per Store lifetime) and persists that
// store. Caller must hold s.mu.
func (s *Store) reinforceLocked(ctx context.Context, id string, similarity float64) (RecordOutcome, error) {
	now := s.now()
	for i := range s.global.Percepts {
		if s.global.Percepts[i].ID == id {
			existing := s.global.Percepts[i].Content
			s.global.Percepts[i].W = reinforceW(s.global.Percepts[i].W, s.params)
			s.global.Percepts[i].UpdatedAt = now
			s.global.Percepts[i].LastUsedAt = now
			s.global.Percepts[i].IdleSeconds = 0
			if s.awardEvidenceLocked(id) {
				s.global.Percepts[i].Evidence++
			}
			metricsReinforce(ctx)
			return RecordOutcome{ID: id, Reinforced: true, Similarity: similarity, Existing: existing, W: s.global.Percepts[i].W},
				s.saveFile(s.globalPath, s.global)
		}
	}
	for i := range s.session.Percepts {
		if s.session.Percepts[i].ID == id {
			existing := s.session.Percepts[i].Content
			s.session.Percepts[i].W = reinforceW(s.session.Percepts[i].W, s.params)
			s.session.Percepts[i].UpdatedAt = now
			if s.awardEvidenceLocked(id) {
				s.session.Percepts[i].Evidence++
			}
			metricsReinforce(ctx)
			var saveErr error
			if s.sessionPath != "" {
				saveErr = s.saveFile(s.sessionPath, s.session)
			}
			return RecordOutcome{ID: id, Reinforced: true, Similarity: similarity, Existing: existing, W: s.session.Percepts[i].W}, saveErr
		}
	}
	// The percept vanished between lookup and reinforcement (concurrent
	// forget); report the ID without a reinforcement side effect.
	return RecordOutcome{ID: id, Similarity: similarity}, nil
}

// awardEvidenceLocked grants Percept id one corroboration event unless it
// already earned one in this Store's lifetime, and reports whether it was
// granted now. Caller must hold s.mu.
func (s *Store) awardEvidenceLocked(id string) bool {
	if _, ok := s.evidenceAwarded[id]; ok {
		return false
	}
	if s.evidenceAwarded == nil {
		s.evidenceAwarded = make(map[string]struct{})
	}
	s.evidenceAwarded[id] = struct{}{}
	return true
}

func initialWeight(producer Producer, core bool) float64 {
	if core {
		return 1.0
	}
	switch producer {
	case ProducerUser:
		return 0.9 // promotion fast path: graduates on the first consolidation pass
	case ProducerSystem:
		return 0.4 // low-confidence hint; decays to the prune threshold in a few running days
	default: // ProducerLocal, ProducerEscalation
		return 0.7 // inference; promoted via corroboration in a second run (Evidence 2) or positive edges
	}
}

// RecordGlobal writes a Percept directly to the global store regardless of
// session scope. Used by /learn.
//
// Like Record, a near-duplicate reinforces the existing Percept instead of
// inserting a new one (see RecordOutcome.Reinforced).
func (s *Store) RecordGlobal(ctx context.Context, content string, producer Producer, consumer Consumer, roles Roles) (RecordOutcome, error) {
	ctx, end := traceRecord(ctx, producer)
	defer func() { end(nil) }()

	s.mu.Lock()
	defer s.mu.Unlock()

	if dup := s.findSimilarLocked(content, DuplicateSimilarityThreshold); dup != nil {
		return s.reinforceLocked(ctx, dup.ID, jaccardSimilarity(tokenize(content), tokenize(dup.Content)))
	}

	p := Percept{
		ID:        uuid.New().String(),
		Content:   content,
		Producer:  producer,
		Consumer:  consumer,
		W:         1.0,
		Core:      true,
		Roles:     roles,
		CreatedAt: s.now(),
		UpdatedAt: s.now(),
	}
	s.global.Percepts = append(s.global.Percepts, p)

	logPercept(ctx, p, s.sessionID)
	metricsRecord(ctx, producer, "global")
	return RecordOutcome{ID: p.ID, W: p.W}, s.saveFile(s.globalPath, s.global)
}

// Query returns Percepts matching a keyword query, ordered by token-overlap
// count desc, then rank score (W scaled by the recency and evidence factors —
// see rankScore; a recently updated, corroborated percept outranks an equally
// relevant stale one) desc, then raw W desc. maxResults <= 0 means no limit.
// minConfidence filters by raw W (not the rank score, so old facts are never
// hidden by age alone). caller restricts results to percepts visible to that
// agent (ConsumerAll = no restriction).
//
// Matching is token-based (tokenize, shared with FindSimilar/FilterByRelevance)
// rather than literal substring — a query like "dog name" must still match a
// percept phrased "the user's dog's name is Biscuit", where the possessive
// apostrophe means "dog's" and "name" never literally contain "dog name" as a
// substring. A query with no content-bearing tokens (empty, or stop-words
// only) matches everything, same as FilterByRelevance.
func (s *Store) Query(ctx context.Context, query string, minConfidence float64, maxResults int, caller Consumer) []Percept {
	ctx, end := traceRecall(ctx, query)

	s.mu.Lock()
	defer s.mu.Unlock()

	all := s.allPercepts()
	queryTokens := tokenize(query)
	now := s.now()

	type scored struct {
		p     Percept
		hits  int
		score float64
	}
	var candidates []scored
	for _, p := range all {
		if p.W < minConfidence {
			continue
		}
		if caller != ConsumerAll && p.Consumer != ConsumerAll && p.Consumer != caller {
			continue
		}
		if len(queryTokens) == 0 {
			candidates = append(candidates, scored{p: p, score: s.rankScore(p, now)})
			continue
		}
		hits := 0
		for t := range tokenize(p.Content) {
			if _, ok := queryTokens[t]; ok {
				hits++
			}
		}
		if hits > 0 {
			candidates = append(candidates, scored{p: p, hits: hits, score: s.rankScore(p, now)})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].hits != candidates[j].hits {
			return candidates[i].hits > candidates[j].hits
		}
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].p.W > candidates[j].p.W
	})

	if maxResults > 0 && len(candidates) > maxResults {
		candidates = candidates[:maxResults]
	}

	out := make([]Percept, len(candidates))
	for i, c := range candidates {
		out[i] = c.p
	}
	s.markUsedLocked(perceptIDs(out), now)

	end(len(out), nil)
	logRecall(ctx, query, len(out), s.sessionID)
	metricsRecall(ctx)
	return out
}

// rankScore is the derived ordering score (ADR-0090), never stored:
//
//	rank = W · (1 + r) · (1 + e)
//	r = RecencyBoost · 0.5^(age_days / RecencyHalfLifeDays)   // since UpdatedAt
//	e = rankEvidenceFactor · min(Evidence, rankEvidenceCap)
//
// It orders recall, PruneGlobal eviction and the injection budget. Every
// *gate* (min_confidence, promote_threshold, prune_threshold) reads raw W, so
// recency and corroboration can never hide or mint a fact.
func (s *Store) rankScore(p Percept, now time.Time) float64 {
	ageDays := now.Sub(p.UpdatedAt).Hours() / 24
	if ageDays < 0 {
		ageDays = 0
	}
	r := s.params.RecencyBoost * math.Pow(0.5, ageDays/s.params.RecencyHalfLifeDays)
	e := rankEvidenceFactor * math.Min(float64(p.Evidence), rankEvidenceCap)
	return p.W * (1 + r) * (1 + e)
}

// SortByRank orders percepts in place by descending rank score (highest
// first) — the order the injection budget drops from when over budget.
func (s *Store) SortByRank(percepts []Percept) {
	now := s.now()
	sort.SliceStable(percepts, func(i, j int) bool {
		return s.rankScore(percepts[i], now) > s.rankScore(percepts[j], now)
	})
}

// AddEdge records a typed directed relation between two Percepts (ADR-0053):
// extends/updates/derives corroborate the target (To), contradicts weakens
// both endpoints — all applied at consolidation. The edge is stored in the
// same store file as the "from" Percept and only affects endpoints present in
// that file at consolidation time. Both endpoints must already exist;
// identical (From, To, Relation) triples are stored once.
func (s *Store) AddEdge(fromID, toID string, rel Relation) error {
	switch rel {
	case RelationExtends, RelationUpdates, RelationDerives, RelationContradicts:
	default:
		return fmt.Errorf("unknown relation %q", rel)
	}
	if fromID == toID {
		return fmt.Errorf("self-referential edge on %s", fromID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	fromGlobal, fromOK := s.findPerceptLocked(fromID)
	_, toOK := s.findPerceptLocked(toID)
	if !fromOK {
		return fmt.Errorf("percept %s not found (edge source)", fromID)
	}
	if !toOK {
		return fmt.Errorf("percept %s not found (edge target)", toID)
	}

	e := Edge{From: fromID, To: toID, Relation: rel}
	var saveErr error
	if fromGlobal {
		for _, ex := range s.global.Edges {
			if ex == e {
				return nil // already recorded
			}
		}
		s.global.Edges = append(s.global.Edges, e)
		saveErr = s.saveFile(s.globalPath, s.global)
	} else {
		for _, ex := range s.session.Edges {
			if ex == e {
				return nil
			}
		}
		s.session.Edges = append(s.session.Edges, e)
		if s.sessionPath != "" {
			saveErr = s.saveFile(s.sessionPath, s.session)
		}
	}
	return saveErr
}

// findPerceptLocked reports whether id exists and, if so, whether it lives in
// the global store (true) or the session store (false). Caller must hold s.mu.
func (s *Store) findPerceptLocked(id string) (inGlobal bool, ok bool) {
	for _, p := range s.global.Percepts {
		if p.ID == id {
			return true, true
		}
	}
	for _, p := range s.session.Percepts {
		if p.ID == id {
			return false, true
		}
	}
	return false, false
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

// PruneGlobal removes lowest-ranked non-core percepts from the global store
// until the total count is at most max (eviction order is ascending rank
// score — see rankScore; core percepts are never evicted). Does nothing when
// max <= 0.
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

	// Stable-sort ascending by rank score, keeping core percepts at the top so
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

	// Sort non-core ascending by rank score so the lowest-ranked are at the front.
	now := s.now()
	sort.SliceStable(nonCore, func(i, j int) bool {
		return s.rankScore(nonCore[i], now) < s.rankScore(nonCore[j], now)
	})

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
	// Reaching the promotion weight took active use: the idle clock starts now.
	now := s.now()
	for i := range percepts {
		percepts[i].LastUsedAt = now
		percepts[i].IdleSeconds = 0
	}
	s.global.Percepts = append(s.global.Percepts, percepts...)
}
