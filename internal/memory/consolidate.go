package memory

import (
	"context"
	"math"
	"time"
)

// Weight-model constants and their defaults (ADR-0090 "memory consolidation
// v3"; supersedes the decay, recall-ordering and promotion rules of ADR-0025
// and ADR-0053 — reinforcement, typed edges and staleness-archiving stand).
//
// The NREM consolidation pipeline runs after every primary-agent turn and at
// exit. Its rules never count passes or sessions: every interval is Δt, the
// milk running time since the previous pass — the same "summed over sessions"
// clock idle_seconds and global_archive_after_days use (ADR-0056):
//
//  1. Decay — non-core W falls exponentially with a half-life in running
//     time: session W multiplies by 2^(−Δt/session_half_life_days); global W
//     decays toward PruneThreshold with global_half_life_days. Core is exempt.
//  2. Edge propagation — the target of extends/updates/derives gains
//     EdgeBoost and one Evidence point (at most once per Store lifetime);
//     both endpoints of contradicts lose EdgeNegativeDelta.
//  3. Prune — session Percepts with W <= PruneThreshold are removed.
//  4. Archive — stale global Percepts move to the archive (ADR-0056, rules
//     unchanged: weight decayed to the prune floor and unused for
//     GlobalArchiveAfterDays of running time).
//  5. Promote — core and user Percepts with W >= PromoteThreshold graduate on
//     the fast path (explicit assertions); everything else also needs
//     Evidence >= evidencePromote, i.e. corroborated in at least two distinct
//     Store lifetimes. A live contradicts edge blocks both endpoints until
//     the conflict is resolved. Promotion is applied after the global pass so
//     fresh promotions are not decayed again in this run.
//
// The global store is never emptied by decay alone — only staleness-archiving
// or the size cap removes a promoted fact.
//
// Reinforcement (ADR-0053): re-stating a near-duplicate fact boosts the
// existing Percept by ReconfirmBoost, capped at ReconfirmCap, touches
// UpdatedAt, and is one corroboration event (Evidence +1, once per Store
// lifetime). Weights never decrease through reinforcement, and a single
// sitting can never mint a trusted fact: one run yields at most Evidence 1.
//
// Ordering (recall, PruneGlobal eviction, the injection budget) uses the
// derived rank score — see rankScore — which is never stored. Every gate
// (min_confidence, promote_threshold, prune_threshold) reads raw W, so age or
// hype can never hide or mint a fact.
const (
	sessionHalfLifeDays = 3    // running-time half-life of non-core session Percepts
	globalHalfLifeDays  = 60   // running-time half-life of non-core global Percepts
	reconfirmBoost      = 0.05 // W gained per reinforcement
	reconfirmCap        = 0.90 // reinforcement cannot mint perfect confidence
	promoteThreshold    = 0.80 // only genuinely high-confidence percepts graduate to global
	pruneThreshold      = 0.20 // cull weak percepts before they accumulate
	edgeBoost           = 0.05 // positive edge (extends/updates/derives) boosts the target
	edgeNegativeDelta   = 0.10 // contradicts penalizes both endpoints
	recencyBoost        = 0.10 // max rank-score recency factor for a just-updated percept
	recencyHalfLifeDays = 30   // age at which the recency factor has halved
	globalArchiveAfter  = 14   // days of milk running time a global percept may sit unused before it is archived
	// floorSnapBand is how close to the prune floor a decayed global W must
	// come to count as "at the floor". Exponential decay is asymptotic, so
	// without the snap the ADR-0056 "decayed to prune_threshold" archive rule
	// would never fire.
	floorSnapBand      = 0.05
	rankEvidenceFactor = 0.05 // rank-score evidence factor per Evidence level
	rankEvidenceCap    = 4    // Evidence levels beyond which the rank factor stops growing
	evidencePromote    = 2    // distinct-run corroborations required off the promotion fast path
	maxW               = 1.0
)

// Params holds the tunable weight-model knobs. The zero value falls back to
// the package defaults above; use DefaultParams to get an explicit copy.
// Exposed via config ("memory" block, see Config.MemoryParams) so thresholds
// can be tuned without a rebuild — values are validated/clamped at use.
type Params struct {
	// SessionHalfLifeDays is the half-life, in milk running time, of a
	// non-core session Percept's weight: one half-life of running time halves
	// it. Default 3.
	SessionHalfLifeDays float64 `json:"session_half_life_days,omitempty"`
	// GlobalHalfLifeDays is the half-life, in milk running time, over which a
	// non-core global Percept's weight sinks toward PruneThreshold.
	// Default 60.
	GlobalHalfLifeDays float64 `json:"global_half_life_days,omitempty"`
	// ReconfirmBoost is added to a Percept's W when a near-duplicate fact is
	// re-stated (reinforcement). Default 0.05.
	ReconfirmBoost float64 `json:"reconfirm_boost,omitempty"`
	// ReconfirmCap caps reinforcement-driven W. Default 0.90.
	ReconfirmCap float64 `json:"reconfirm_cap,omitempty"`
	// PromoteThreshold is the W at which session Percepts graduate to the
	// global store. Default 0.80.
	PromoteThreshold float64 `json:"promote_threshold,omitempty"`
	// PruneThreshold is the W at or below which session Percepts are culled
	// (and below which global decay stops). Default 0.20.
	PruneThreshold float64 `json:"prune_threshold,omitempty"`
	// RecencyBoost is the maximum rank-score recency factor for a Percept
	// that was updated just now (ordering only — never gates). Default 0.10.
	RecencyBoost float64 `json:"recency_boost,omitempty"`
	// RecencyHalfLifeDays is the age at which the recency factor has decayed
	// to half of RecencyBoost. Default 30.
	RecencyHalfLifeDays float64 `json:"recency_half_life_days,omitempty"`
	// GlobalArchiveAfterDays is how many days of cumulative milk running time
	// (summed over sessions) a non-core global Percept may go unused — never
	// re-stated, recalled or injected — once decayed to PruneThreshold before
	// it is moved to the archive. Default 14; negative disables archiving.
	GlobalArchiveAfterDays float64 `json:"global_archive_after_days,omitempty"`
}

// DefaultParams returns the built-in weight-model parameters.
func DefaultParams() Params {
	return Params{
		SessionHalfLifeDays:    sessionHalfLifeDays,
		GlobalHalfLifeDays:     globalHalfLifeDays,
		ReconfirmBoost:         reconfirmBoost,
		ReconfirmCap:           reconfirmCap,
		PromoteThreshold:       promoteThreshold,
		PruneThreshold:         pruneThreshold,
		RecencyBoost:           recencyBoost,
		RecencyHalfLifeDays:    recencyHalfLifeDays,
		GlobalArchiveAfterDays: globalArchiveAfter,
	}
}

// normalized replaces zero/negative fields with the package defaults so a
// partial Params (or config block) can never disable the model by omission.
func (p Params) normalized() Params {
	d := DefaultParams()
	if p.SessionHalfLifeDays <= 0 {
		p.SessionHalfLifeDays = d.SessionHalfLifeDays
	}
	if p.GlobalHalfLifeDays <= 0 {
		p.GlobalHalfLifeDays = d.GlobalHalfLifeDays
	}
	if p.ReconfirmBoost <= 0 {
		p.ReconfirmBoost = d.ReconfirmBoost
	}
	if p.ReconfirmCap <= 0 {
		p.ReconfirmCap = d.ReconfirmCap
	}
	if p.PromoteThreshold <= 0 {
		p.PromoteThreshold = d.PromoteThreshold
	}
	if p.PruneThreshold <= 0 {
		p.PruneThreshold = d.PruneThreshold
	}
	if p.RecencyBoost <= 0 {
		p.RecencyBoost = d.RecencyBoost
	}
	if p.RecencyHalfLifeDays <= 0 {
		p.RecencyHalfLifeDays = d.RecencyHalfLifeDays
	}
	if p.GlobalArchiveAfterDays == 0 {
		p.GlobalArchiveAfterDays = d.GlobalArchiveAfterDays // negative = disabled, kept as is
	}
	return p
}

// Consolidate runs the NREM pass: running-time decay → edge propagation →
// prune → staleness-archive → evidence-gated promotion (see the pipeline
// comment above). Session-scoped Percepts that pass the promotion gate are
// moved to the global store. Both files are saved.
func (s *Store) Consolidate() error {
	return s.ConsolidateCtx(context.Background())
}

// ConsolidateCtx is the context-aware consolidation path used internally and by tests.
func (s *Store) ConsolidateCtx(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p := s.params
	now := s.now()
	// Δt is the milk running time since the previous pass — the same interval
	// accrueAndArchiveLocked accrues into idle_seconds. A zero lastTick (a
	// hand-built Store) or a backwards clock counts nothing.

	var before, decayed, pruned, promoted, deferred int
	var promoteLater []Percept

	if s.sessionPath != "" {
		percepts := s.sessionPercepts()
		before = len(percepts)

		// 1. Decay — session W multiplies by 2^(−Δt/half-life) (floor 0).
		decayed = decayPercepts(percepts, now, s.lastTick, p.SessionHalfLifeDays, 0)

		// 2. Edge propagation, then Evidence for positive-edge boosts.
		percepts = applyEdges(percepts, s.session.Edges)
		s.awardEdgeEvidence(percepts, s.session.Edges)

		// 3. Prune — remove Percepts with W <= PruneThreshold.
		percepts, pruned = prunePerceptsCount(percepts, p.PruneThreshold)

		// 5 (staged) — the promotion gate. Applied below, after the global
		// pass, so freshly promoted percepts are not decayed again in this run.
		contradicted := contradictedIDs(s.session.Edges)
		remaining := percepts[:0]
		for _, q := range percepts {
			if promote, hold := promotionGate(q, contradicted[q.ID], p.PromoteThreshold); promote {
				promoted++
				promoteLater = append(promoteLater, q)
			} else {
				if hold {
					deferred++
				}
				remaining = append(remaining, q)
			}
		}
		s.replaceSessionPercepts(remaining)
	}

	// Global pass — the same time-based decay, floored at PruneThreshold so
	// nothing is deleted by decay alone, then edges and their Evidence.
	globalDecayed := decayPercepts(s.global.Percepts, now, s.lastTick, p.GlobalHalfLifeDays, p.PruneThreshold)
	s.global.Percepts = applyEdges(s.global.Percepts, s.global.Edges)
	s.awardEdgeEvidence(s.global.Percepts, s.global.Edges)

	// 4. Staleness archiving (ADR-0056 rules, unchanged) — also accrues the
	// idle_seconds of globals not used since the previous pass.
	archived := s.accrueAndArchiveLocked()

	if promoted > 0 {
		s.promoteToGlobal(promoteLater)
	}

	// 7. Persist both files.
	var saveErr error
	if err := s.saveFile(s.globalPath, s.global); err != nil {
		saveErr = err
	}
	if s.sessionPath != "" {
		if err := s.saveFile(s.sessionPath, s.session); err != nil && saveErr == nil {
			saveErr = err
		}
	}

	globalTotal := len(s.global.Percepts)
	ctx, endSpan := traceConsolidation(ctx)
	endSpan(decayed, pruned, promoted, deferred, saveErr)
	logConsolidation(ctx, s.sessionID, before, decayed, pruned, promoted, deferred, globalTotal)
	metricsConsolidation(ctx, decayed, pruned, promoted, deferred, globalTotal)
	if globalDecayed > 0 {
		metricsGlobalDecay(ctx, globalDecayed)
	}
	if archived > 0 {
		metricsGlobalArchive(ctx, archived)
	}

	return saveErr
}

// decayPercepts applies the running-time decay accrued between since (the
// previous pass) and now to every non-core Percept in place, and reports how
// many weights changed. Each Percept decays only over the part of the interval
// after its own UpdatedAt: re-confirmation restarts the decay clock — the
// forgetting curve runs from the last time the fact was affirmed — mirroring
// how accrueAndArchiveLocked clips idle time at LastUsedAt.
//
// floor is 0 for session Percepts (W multiplies by 2^(−Δt/half-life)) and
// PruneThreshold for global ones (W decays toward the floor and snaps into it
// once within floorSnapBand).
func decayPercepts(percepts []Percept, now, since time.Time, halfLifeDays, floor float64) int {
	if since.IsZero() || !now.After(since) {
		return 0
	}
	n := 0
	for i := range percepts {
		if percepts[i].Core {
			continue
		}
		from := since
		if percepts[i].UpdatedAt.After(from) {
			from = percepts[i].UpdatedAt
		}
		dt := now.Sub(from)
		if dt <= 0 {
			continue
		}
		factor := math.Pow(0.5, dt.Hours()/(halfLifeDays*24))
		w := floor + (percepts[i].W-floor)*factor
		if floor > 0 && w-floor < floorSnapBand {
			w = floor
		}
		if w < 0 {
			w = 0
		}
		if w != percepts[i].W {
			n++
		}
		percepts[i].W = w
	}
	return n
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
				percepts[i].W = clampW(percepts[i].W + edgeBoost)
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

// awardEdgeEvidence grants one Evidence point per Percept targeted by an
// extends/updates/derives edge at this consolidation — the edge's boost is a
// corroboration event — at most once per Store lifetime (ADR-0090). Caller
// must hold s.mu.
func (s *Store) awardEdgeEvidence(percepts []Percept, edges []Edge) {
	idx := make(map[string]int, len(percepts))
	for i, p := range percepts {
		idx[p.ID] = i
	}
	for _, e := range edges {
		switch e.Relation {
		case RelationExtends, RelationUpdates, RelationDerives:
			if i, ok := idx[e.To]; ok && s.awardEvidenceLocked(e.To) {
				percepts[i].Evidence++
			}
		}
	}
}

// contradictedIDs returns the IDs of Percepts with a live contradicts edge on
// either endpoint. Such a Percept cannot be promoted until the conflict is
// resolved (/forget of one side, or removal of the edge).
func contradictedIDs(edges []Edge) map[string]bool {
	out := make(map[string]bool)
	for _, e := range edges {
		if e.Relation == RelationContradicts {
			out[e.From] = true
			out[e.To] = true
		}
	}
	return out
}

// promotionGate reports whether p graduates to the global store at this pass
// and, if not, whether it was a would-be promotion held back (counted as
// promotion_deferred). Core and user facts take the fast path at
// W >= threshold — they are explicit assertions; everything else also needs
// Evidence >= evidencePromote, corroborated in at least two distinct runs.
func promotionGate(p Percept, contradicted bool, threshold float64) (promote, deferred bool) {
	if p.W < threshold {
		return false, false
	}
	if contradicted {
		return false, true
	}
	if p.Core || p.Producer == ProducerUser {
		return true, false
	}
	if p.Evidence >= evidencePromote {
		return true, false
	}
	return false, true
}

func prunePerceptsCount(percepts []Percept, threshold float64) ([]Percept, int) {
	out := percepts[:0]
	pruned := 0
	for _, p := range percepts {
		if p.W > threshold {
			out = append(out, p)
		} else {
			pruned++
		}
	}
	return out, pruned
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

// reinforceW applies a re-confirmation boost to an existing Percept's weight:
// W gains ReconfirmBoost, capped at ReconfirmCap — and never decreases (a
// percept already above the cap keeps its weight). Returns the result.
func reinforceW(w float64, p Params) float64 {
	if w >= p.ReconfirmCap {
		return w
	}
	w += p.ReconfirmBoost
	if w > p.ReconfirmCap {
		w = p.ReconfirmCap
	}
	return w
}
