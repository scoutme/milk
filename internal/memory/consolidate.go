package memory

import (
	"context"
	"math"
	"time"
)

// Weight-model constants and their defaults (ADR-0091 "memory consolidation
// v4"; supersedes the weighing and reinforcement rules of ADR-0090, whose
// running-time decay, evidence gating and rank ordering stand). Reinforcement,
// typed edges and staleness-archiving stand.
//
// The NREM consolidation pipeline runs after every primary-agent turn and at
// exit. Its rules never count passes or sessions: every interval is Δt, the
// milk running time since the previous pass — the same "summed over sessions"
// clock idle_seconds and global_archive_after_days use (ADR-0056).
//
// Weighing — W is derived from persisted primitives, then decayed in time:
//
//	W = derivedW(p) = clamp(Prior + corrobBoost(Evidence)
//	                         − contraPenalty(Contradictions)
//	                         − supersedPenalty·Superseded)
//
// Every event (reinforcement, positive edge, contradicts edge) shifts W by
// exactly its marginal derivedW(after) − derivedW(before) — and re-affirmation
// (reinforcement) refreshes W to derivedW outright — so boosts telescope and
// consolidation is idempotent: re-running a pass over the same events changes
// nothing. One sitting can never mint a trusted fact: events are awarded at
// most once per Store lifetime, and the corroboration term saturates, so the
// tenth echo in one run counts as little as the second.
//
// The pass:
//
//  1. Decay — non-core W falls exponentially with a half-life in running
//     time: session W multiplies by 2^(−Δt/session_half_life_days); global W
//     decays toward PruneThreshold with global_half_life_days. Core is exempt.
//     The half-life grows with UseCount (a used fact forgets slower, at most
//     2×) and halves while the Percept is superseded.
//  2. Edge events — the corroborated endpoint of a positive edge (target of
//     extends/derives, source of updates) gains one Evidence event; each
//     endpoint of contradicts gains one Contradictions event; live updates
//     edges mark their target superseded. Each event applies its marginal W
//     delta exactly once per Store lifetime.
//  3. Durability tracking — AboveThresholdSince is set at the first pass that
//     observes derivedW >= PromoteThreshold and cleared if it dips: promotion
//     wants an unbroken run above the gate, not a boost burst. Forgetting
//     (decay of W) does not clear it — forgetting is not falsifying.
//  4. Prune — session Percepts with W <= PruneThreshold are removed.
//  5. Archive — stale global Percepts move to the archive (ADR-0056, rules
//     unchanged: weight decayed to the prune floor and unused for
//     GlobalArchiveAfterDays of running time).
//  6. Promote — core and user Percepts with derivedW >= PromoteThreshold
//     graduate on the fast path (explicit assertions); everything else also
//     needs Evidence >= evidencePromote (corroborated in at least two distinct
//     Store lifetimes) and durability: derivedW at/above the threshold for
//     PromoteStableDays of running time. A live contradicts edge or an
//     updates supersession defers until resolved. Promotion is applied after
//     the global pass so fresh promotions are not decayed again in this run.
//
// The global store is never emptied by decay alone — only staleness-archiving
// or the size cap removes a promoted fact.
//
// Reinforcement (ADR-0053): re-stating a near-duplicate fact re-affirms the
// existing Percept — one corroboration event (Evidence +1, once per Store
// lifetime), UseCount +1, UpdatedAt touched, W refreshed to derivedW.
// Repetition within the same run adds nothing: echo is not corroboration.
//
// Ordering (recall, PruneGlobal eviction, the injection budget) uses the
// derived rank score — see rankScore — which is never stored. Visibility and
// death read raw W (min_confidence, prune_threshold) so age can never hide a
// fact and weak facts die; promotion reads derivedW (the undecayed strength)
// because forgetting is not falsifying — and hype can never mint a fact:
// derivedW is saturated, run-capped and durability-gated.
const (
	sessionHalfLifeDays = 3    // running-time half-life of non-core session Percepts
	globalHalfLifeDays  = 60   // running-time half-life of non-core global Percepts
	reconfirmCap        = 0.90 // non-core W ceiling: corroboration never mints perfect confidence
	promoteThreshold    = 0.80 // only genuinely high-confidence percepts graduate to global
	pruneThreshold      = 0.20 // cull weak percepts before they accumulate
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

	// Derived-weight terms (ADR-0091).
	promoteStableDays  = 1    // running days derivedW must sit at/above PromoteThreshold before non-fast-path promotion
	corrobMax          = 0.25 // asymptotic W gain from corroboration (corroboration(2) = 0.125: a twice-corroborated inference fact clears the gate)
	corrobHalfSat      = 2    // Evidence count at which the corroboration gain is half of corrobMax
	contraDelta        = 0.10 // W lost per contradiction event
	contraMax          = 0.30 // contradiction losses saturate here
	supersedPenalty    = 0.10 // W lost while a live updates edge supersedes the Percept
	supersedHalfLife   = 0.5  // half-life multiplier while superseded — the newer claim wins by faster forgetting
	useDecaySlowing    = 0.25 // half-life growth per use
	useDecaySlowingCap = 4    // max counted uses (half-life grows at most 2×)
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
	// ReconfirmCap caps corroboration-driven W (non-core). Default 0.90.
	ReconfirmCap float64 `json:"reconfirm_cap,omitempty"`
	// PromoteThreshold is the W at which session Percepts graduate to the
	// global store. Default 0.80.
	PromoteThreshold float64 `json:"promote_threshold,omitempty"`
	// PromoteStableDays is how long, in milk running time, a non-fast-path
	// Percept must have sat at/above PromoteThreshold before it graduates —
	// the durability gate: a boost burst cannot mint a fact. Default 1;
	// negative disables the gate.
	PromoteStableDays float64 `json:"promote_stable_days,omitempty"`
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
		ReconfirmCap:           reconfirmCap,
		PromoteThreshold:       promoteThreshold,
		PromoteStableDays:      promoteStableDays,
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
	if p.ReconfirmCap <= 0 {
		p.ReconfirmCap = d.ReconfirmCap
	}
	if p.PromoteThreshold <= 0 {
		p.PromoteThreshold = d.PromoteThreshold
	}
	if p.PromoteStableDays == 0 {
		p.PromoteStableDays = d.PromoteStableDays // negative = gate disabled, kept as is
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

// Consolidate runs the NREM pass: running-time decay → edge events →
// durability tracking → prune → staleness-archive → evidence-gated promotion
// (see the pipeline comment above). Session-scoped Percepts that pass the
// promotion gate are moved to the global store. Both files are saved.
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

	s.normalizeLocked()

	var before, decayed, pruned, promoted, deferred int
	var promoteLater []Percept

	if s.sessionPath != "" {
		percepts := s.sessionPercepts()
		before = len(percepts)

		// 1. Decay — session W multiplies by 2^(−Δt/half-life) (floor 0).
		decayed = decayPercepts(percepts, now, s.lastTick, p.SessionHalfLifeDays, 0)

		// 2. Edge events — evidence/conflict/supersession counts and their
		// marginal W deltas, each applied at most once per Store lifetime.
		s.applyEdgeEvents(percepts, s.session.Edges)

		// 3. Durability tracking — the promotion gate wants an unbroken run
		// above the threshold, not a boost burst.
		trackDurability(percepts, now, p.PromoteThreshold)

		// 4. Prune — remove Percepts with W <= PruneThreshold.
		percepts, pruned = prunePerceptsCount(percepts, p.PruneThreshold)

		// 6 (staged) — the promotion gate. Applied below, after the global
		// pass, so freshly promoted percepts are not decayed again in this run.
		contradicted := contradictedIDs(s.session.Edges)
		remaining := percepts[:0]
		for _, q := range percepts {
			if promote, hold := promotionGate(q, contradicted[q.ID], now, p.PromoteThreshold, p.PromoteStableDays); promote {
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
	// nothing is deleted by decay alone, then edge events.
	globalDecayed := decayPercepts(s.global.Percepts, now, s.lastTick, p.GlobalHalfLifeDays, p.PruneThreshold)
	s.applyEdgeEvents(s.global.Percepts, s.global.Edges)

	// 5. Staleness archiving (ADR-0056 rules, unchanged) — also accrues the
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

// --- derived weight (ADR-0091) ---

// corrobBoost is the saturating W gain for c corroboration events:
// corrobMax·c/(c + corrobHalfSat). The second corroboration matters far more
// than the tenth — repetition is not corroboration.
func corrobBoost(c int) float64 {
	if c <= 0 {
		return 0
	}
	return corrobMax * float64(c) / float64(c+corrobHalfSat)
}

// contraPenalty is the W loss for k contradiction events, saturating at
// contraMax so a persistent conflict can never grind a fact below its prior
// by penalty alone — pruning decides when a conflicted fact is truly dead.
func contraPenalty(k int) float64 {
	if k <= 0 {
		return 0
	}
	p := contraDelta * float64(k)
	if p > contraMax {
		p = contraMax
	}
	return p
}

// derivedW is the primitive-derived strength of a Percept (the boost side of
// W — time decay is layered on top at consolidation, see decayPercepts):
//
//	W = clamp(Prior + corrobBoost(Evidence) − contraPenalty(Contradictions)
//	          − supersedPenalty·Superseded)
//
// Non-core W is capped at ReconfirmCap: corroboration never mints perfect
// confidence (Core is exempt). Because it reads only persisted primitives,
// the boost side of W is idempotent and order-independent.
func derivedW(p Percept) float64 {
	w := p.Prior + corrobBoost(p.Evidence) - contraPenalty(p.Contradictions)
	if p.Superseded > 0 {
		w -= supersedPenalty
	}
	if !p.Core && w > reconfirmCap {
		w = reconfirmCap
	}
	return clampW(w)
}

// applyEvent moves the primitive counts to (e, k, s) and shifts W by exactly
// derivedW(after) − derivedW(before). Events telescope: however many passes
// they arrive in, the accumulated boost equals the full derivation, and
// decayed weight is never restored by a mere event.
func applyEvent(p *Percept, e, k, s int) {
	before := derivedW(*p)
	p.Evidence, p.Contradictions, p.Superseded = e, k, s
	p.W = clampW(p.W + (derivedW(*p) - before))
}

// useFactor is the forgetting slowdown for a fact that keeps being put to
// use (spaced repetition): a used fact forgets up to 2× slower. Repetition
// buys persistence, never truth — W is untouched.
func useFactor(useCount int) float64 {
	if useCount > useDecaySlowingCap {
		useCount = useDecaySlowingCap
	}
	return 1 + useDecaySlowing*float64(useCount)
}

// normalizePriors seeds the provenance prior of pre-v4 Percepts (Prior == 0)
// once: Prior := W with the count-derived boosts backed out (W − corrobBoost
// + contraPenalty + supersession), so deriving them again cannot
// double-count them. Boosts the older models baked flat into W stay in Prior
// — a one-time, monotone-safe approximation. No existing fact is promoted by
// migration itself: AboveThresholdSince starts fresh, so the durability gate
// defers every non-fast-path promotion to a later pass. Called on every
// mutation path (hand-built Percepts included).
func normalizePriors(percepts []Percept) {
	for i := range percepts {
		if percepts[i].Prior != 0 {
			continue
		}
		prior := percepts[i].W - corrobBoost(percepts[i].Evidence) + contraPenalty(percepts[i].Contradictions)
		if percepts[i].Superseded > 0 {
			prior += supersedPenalty
		}
		if prior < 0 {
			prior = 0
		}
		if prior > maxW {
			prior = maxW
		}
		percepts[i].Prior = prior
	}
}

// normalizeLocked runs the pre-v4 Prior migration over both stores.
// Caller must hold s.mu.
func (s *Store) normalizeLocked() {
	normalizePriors(s.global.Percepts)
	normalizePriors(s.session.Percepts)
}

// trackDurability maintains AboveThresholdSince: set at the first pass that
// observes derivedW at/above the threshold, cleared when derivedW dips —
// durability means an unbroken run above the gate. Only events move derivedW
// (forgetting decays W, not the derived strength), so a decayed-but-unchallenged
// fact stays durable.
func trackDurability(percepts []Percept, now time.Time, threshold float64) {
	for i := range percepts {
		if derivedW(percepts[i]) >= threshold {
			if percepts[i].AboveThresholdSince.IsZero() {
				percepts[i].AboveThresholdSince = now
			}
		} else {
			percepts[i].AboveThresholdSince = time.Time{}
		}
	}
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
//
// The half-life is modulated: it grows with UseCount (useFactor — a used fact
// forgets slower) and halves while the Percept is superseded by a live
// updates edge.
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
		h := halfLifeDays * useFactor(percepts[i].UseCount)
		if percepts[i].Superseded > 0 {
			h *= supersedHalfLife
		}
		factor := math.Pow(0.5, dt.Hours()/(h*24))
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

// applyEdgeEvents folds this pass's edge activity into the primitives and
// applies each event's marginal W delta exactly once per Store lifetime:
//
//   - positive edges (extends/derives → target, updates → source — the newer
//     claim that supersedes) award one Evidence event (shared per-run cap with
//     reinforcement: one corroboration per percept per run);
//   - contradicts awards one Contradictions event per endpoint (its own
//     per-run cap) and never counts as corroboration;
//   - live updates edges mark their target superseded (refreshed every pass,
//     so removing the edge lifts the penalty again).
//
// Re-running a pass over the same edges changes nothing. Caller must hold s.mu.
func (s *Store) applyEdgeEvents(percepts []Percept, edges []Edge) {
	for i := range percepts {
		id := percepts[i].ID
		e, k, sup := percepts[i].Evidence, percepts[i].Contradictions, 0
		for _, ed := range edges {
			switch ed.Relation {
			case RelationExtends, RelationDerives:
				if ed.To == id && s.awardEvidenceLocked(id) {
					e++
				}
			case RelationUpdates:
				if ed.From == id && s.awardEvidenceLocked(id) {
					e++
				}
				if ed.To == id {
					sup = 1
				}
			case RelationContradicts:
				if (ed.From == id || ed.To == id) && s.awardConflictLocked(id) {
					k++
				}
			}
		}
		if e != percepts[i].Evidence || k != percepts[i].Contradictions || sup != percepts[i].Superseded {
			applyEvent(&percepts[i], e, k, sup)
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
// promotion_deferred). The threshold check reads derivedW — the undecayed
// strength — because forgetting is not falsifying: a fact affirmed strongly
// yesterday and a fact affirmed strongly this minute are equally true. Core
// and user facts take the fast path — they are explicit assertions. Everything
// else must also be corroborated in at least two distinct runs (Evidence >=
// evidencePromote) and durable: derivedW at/above the threshold for
// PromoteStableDays of running time — a boost burst cannot graduate on
// momentum. A live contradicts edge or an updates supersession defers until
// the conflict is resolved.
func promotionGate(p Percept, contradicted bool, now time.Time, threshold, stableDays float64) (promote, deferred bool) {
	if derivedW(p) < threshold {
		return false, false
	}
	if contradicted || p.Superseded > 0 {
		return false, true
	}
	if p.Core || p.Producer == ProducerUser {
		return true, false
	}
	if p.Evidence < evidencePromote {
		return false, true
	}
	if stableDays > 0 && (p.AboveThresholdSince.IsZero() ||
		now.Sub(p.AboveThresholdSince) < time.Duration(stableDays*24)*time.Hour) {
		return false, true
	}
	return true, false
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
