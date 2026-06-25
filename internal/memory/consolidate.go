package memory

import (
	"context"

	"github.com/scoutme/milk/internal/obs"
)

const (
	decayPerSession   = 0.10 // −10% per session end; Local/Claude percepts prune after ~5 sessions
	promoteThreshold  = 0.80 // only genuinely high-confidence percepts graduate to global
	pruneThreshold    = 0.20 // cull weak percepts before they accumulate
	edgePositiveDelta = 0.05
	edgeNegativeDelta = 0.10
	maxW              = 1.0
)

// Consolidate runs end-of-session NREM: decay → edge propagation → prune → promote.
// Session-scoped Percepts with W >= promoteThreshold are moved to the global store.
// The session file is saved (pruned) and the global file is saved (with promoted items).
func (s *Store) Consolidate() error {
	return s.ConsolidateCtx(context.Background())
}

// ConsolidateCtx is the context-aware consolidation path used internally and by tests.
func (s *Store) ConsolidateCtx(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sessionPath == "" {
		return nil
	}

	percepts := s.sessionPercepts()
	before := len(percepts)

	// 1. Decay — unconditional Model A: −0.03 for all non-core session Percepts.
	percepts, decayed := applyDecayCount(percepts)

	// 2. Edge propagation — adjust weights based on typed edges.
	percepts = applyEdges(percepts, s.session.Edges)

	// 3. Prune — remove Percepts with W <= 0.
	percepts, pruned := prunePerceptsCount(percepts)

	// 4. Promote — move high-W non-core Percepts to global.
	remaining, promoted := partitionByPromotion(percepts)

	s.replaceSessionPercepts(remaining)
	if len(promoted) > 0 {
		s.promoteToGlobal(promoted)
	}

	// Persist both files.
	var saveErr error
	if err := s.saveFile(s.globalPath, s.global); err != nil {
		saveErr = err
	}
	if err := s.saveFile(s.sessionPath, s.session); err != nil && saveErr == nil {
		saveErr = err
	}

	globalTotal := len(s.global.Percepts)
	ctx, endSpan := traceConsolidation(ctx)
	endSpan(decayed, pruned, len(promoted), saveErr)
	logConsolidation(ctx, s.sessionID, before, decayed, pruned, len(promoted), globalTotal)
	metricsConsolidation(ctx, decayed, pruned, len(promoted), globalTotal)
	obs.Debug("consolidation run", "session", s.sessionID, "before", before, "decayed", decayed, "pruned", pruned, "promoted", len(promoted), "global_total", globalTotal)

	return saveErr
}

func applyDecayCount(percepts []Percept) ([]Percept, int) {
	n := 0
	for i := range percepts {
		if percepts[i].Core {
			continue
		}
		percepts[i].W -= decayPerSession
		if percepts[i].W < 0 {
			percepts[i].W = 0
		}
		n++
	}
	return percepts, n
}

func applyEdges(percepts []Percept, edges []Edge) []Percept {
	idx := make(map[string]int, len(percepts))
	for i, p := range percepts {
		idx[p.ID] = i
	}
	for _, e := range edges {
		switch e.Relation {
		case RelationExtends, RelationUpdates, RelationDerives:
			if i, ok := idx[e.To]; ok {
				percepts[i].W = clampW(percepts[i].W + edgePositiveDelta)
			}
		case RelationContradicts:
			if i, ok := idx[e.From]; ok {
				percepts[i].W = clampW(percepts[i].W - edgeNegativeDelta)
			}
			if i, ok := idx[e.To]; ok {
				percepts[i].W = clampW(percepts[i].W - edgeNegativeDelta)
			}
		}
	}
	return percepts
}

func prunePerceptsCount(percepts []Percept) ([]Percept, int) {
	out := percepts[:0]
	pruned := 0
	for _, p := range percepts {
		if p.W > pruneThreshold {
			out = append(out, p)
		} else {
			pruned++
		}
	}
	return out, pruned
}

func partitionByPromotion(percepts []Percept) (remaining, promoted []Percept) {
	for _, p := range percepts {
		if !p.Core && p.W >= promoteThreshold {
			promoted = append(promoted, p)
		} else {
			remaining = append(remaining, p)
		}
	}
	return remaining, promoted
}

func clampW(w float64) float64 {
	if w > maxW {
		return maxW
	}
	if w < 0 {
		return 0
	}
	return w
}
