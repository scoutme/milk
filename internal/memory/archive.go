package memory

// Stale global Percepts are archived, not deleted. Global Percepts only ever
// sank in rank (decay is floored), so a store that is used for months kept
// every fact it ever promoted. A Percept is stale when it has decayed to the
// prune threshold AND has gone unused for GlobalArchiveAfterDays of milk
// running time — summed over sessions, so one 20-day session counts like ten
// 2-day ones. "Used" means re-stated (reinforced), returned by get_memory, or
// injected into a prompt (MarkUsed).
//
// Archiving moves the Percept (and the edges touching it) to
// global-archive.jsonl, one JSON object per line, with when and why; nothing
// is lost and /memory restore brings it back. Core Percepts never go stale.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// ArchivedPercept is one line of the archive file.
type ArchivedPercept struct {
	ArchivedAt time.Time `json:"archived_at"`
	Reason     string    `json:"reason"`
	IdleDays   float64   `json:"idle_days"`
	Percept    Percept   `json:"percept"`
	Edges      []Edge    `json:"edges,omitempty"`
}

func (s *Store) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

func perceptIDs(ps []Percept) []string {
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.ID
	}
	return ids
}

// MarkUsed records that the global Percepts with these IDs were put to use
// (injected into a prompt), which restarts their idle clock.
func (s *Store) MarkUsed(ids []string) {
	if len(ids) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markUsedLocked(ids, s.now())
}

func (s *Store) markUsedLocked(ids []string, now time.Time) {
	if len(ids) == 0 {
		return
	}
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	for i := range s.global.Percepts {
		if _, ok := set[s.global.Percepts[i].ID]; ok {
			s.global.Percepts[i].LastUsedAt = now
			s.global.Percepts[i].IdleSeconds = 0
			s.global.Percepts[i].UseCount++
		}
	}
}

// accrueAndArchiveLocked adds the running time since the previous pass to
// every global Percept that was not used meanwhile, then archives the stale
// ones. Returns how many were archived. Caller holds s.mu. A failed archive
// write leaves the Percepts in the store: nothing is removed unless it was
// safely written first.
func (s *Store) accrueAndArchiveLocked() int {
	now := s.now()
	prev := s.lastTick
	if prev.IsZero() {
		prev = now // a Store built without NewStore has no previous pass
	}
	if now.Before(prev) {
		prev = now // clock went backwards: count nothing
	}
	s.lastTick = now

	for i := range s.global.Percepts {
		p := &s.global.Percepts[i]
		if p.Core {
			continue
		}
		// A use already reset IdleSeconds; only the running time after the
		// later of the last pass and the last use is idle time.
		from := prev
		if p.LastUsedAt.After(from) {
			from = p.LastUsedAt
		}
		p.IdleSeconds += now.Sub(from).Seconds()
	}

	if s.params.GlobalArchiveAfterDays < 0 {
		return 0
	}
	limit := s.params.GlobalArchiveAfterDays * 24 * 3600
	var keep []Percept
	var gone []ArchivedPercept
	for _, p := range s.global.Percepts {
		if !p.Core && p.W <= s.params.PruneThreshold+1e-9 && p.IdleSeconds >= limit {
			gone = append(gone, ArchivedPercept{
				ArchivedAt: now,
				Reason:     fmt.Sprintf("unused for %.1f days of session time at the prune weight", p.IdleSeconds/86400),
				IdleDays:   p.IdleSeconds / 86400,
				Percept:    p,
				Edges:      s.edgesTouching(p.ID),
			})
			continue
		}
		keep = append(keep, p)
	}
	if len(gone) == 0 {
		return 0
	}
	if err := s.appendArchive(gone); err != nil {
		return 0
	}
	s.global.Percepts = keep
	archivedIDs := make(map[string]struct{}, len(gone))
	for _, a := range gone {
		archivedIDs[a.Percept.ID] = struct{}{}
	}
	var edges []Edge
	for _, e := range s.global.Edges {
		_, f := archivedIDs[e.From]
		_, t := archivedIDs[e.To]
		if !f && !t {
			edges = append(edges, e)
		}
	}
	s.global.Edges = edges
	for i := range s.global.Engrams {
		var ids []string
		for _, id := range s.global.Engrams[i].PerceptIDs {
			if _, drop := archivedIDs[id]; !drop {
				ids = append(ids, id)
			}
		}
		s.global.Engrams[i].PerceptIDs = ids
	}
	return len(gone)
}

func (s *Store) edgesTouching(id string) []Edge {
	var out []Edge
	for _, e := range s.global.Edges {
		if e.From == id || e.To == id {
			out = append(out, e)
		}
	}
	return out
}

func (s *Store) appendArchive(entries []ArchivedPercept) error {
	f, err := os.OpenFile(s.archivePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var werr error
	for _, a := range entries {
		b, err := json.Marshal(a)
		if err != nil {
			werr = err
			break
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			werr = err
			break
		}
	}
	if err := f.Close(); werr == nil {
		werr = err
	}
	return werr
}

// readArchive returns the archive's entries oldest first; a missing file is an
// empty archive. Unreadable lines are skipped rather than failing the listing.
func (s *Store) readArchive() ([]ArchivedPercept, error) {
	f, err := os.Open(s.archivePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []ArchivedPercept
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		var a ArchivedPercept
		if json.Unmarshal(sc.Bytes(), &a) == nil && a.Percept.ID != "" {
			out = append(out, a)
		}
	}
	return out, sc.Err()
}

// Archived lists the archived global Percepts, most recently archived first.
func (s *Store) Archived() ([]ArchivedPercept, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readArchive()
	sort.SliceStable(all, func(i, j int) bool { return all[i].ArchivedAt.After(all[j].ArchivedAt) })
	return all, err
}

// Restore moves the archived Percept whose ID starts with prefix back into the
// global store, with its idle clock reset so it is not archived again at once.
// Its edges come back when the other end still exists. The prefix must match
// exactly one archived Percept.
func (s *Store) Restore(prefix string) (Percept, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return Percept{}, errors.New("an id (or unambiguous prefix) is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.readArchive()
	if err != nil {
		return Percept{}, err
	}
	match := -1
	for i, a := range all {
		if strings.HasPrefix(strings.ToLower(a.Percept.ID), prefix) {
			if match >= 0 {
				return Percept{}, fmt.Errorf("prefix %q matches more than one archived percept", prefix)
			}
			match = i
		}
	}
	if match < 0 {
		return Percept{}, fmt.Errorf("no archived percept matches %q", prefix)
	}
	a := all[match]
	if dup := s.findSimilarLocked(a.Percept.Content, DuplicateSimilarityThreshold); dup != nil {
		return Percept{}, fmt.Errorf("an equivalent percept is already stored (#%.8s)", dup.ID)
	}

	p := a.Percept
	now := s.now()
	p.LastUsedAt, p.IdleSeconds = now, 0
	if p.W < s.params.PruneThreshold {
		p.W = s.params.PruneThreshold
	}
	s.global.Percepts = append(s.global.Percepts, p)
	have := make(map[string]struct{}, len(s.global.Percepts))
	for _, q := range s.global.Percepts {
		have[q.ID] = struct{}{}
	}
	for _, e := range a.Edges {
		_, f := have[e.From]
		_, t := have[e.To]
		if f && t {
			s.global.Edges = append(s.global.Edges, e)
		}
	}
	if err := s.saveFile(s.globalPath, s.global); err != nil {
		return Percept{}, err
	}
	if err := s.rewriteArchiveWithout(all, match); err != nil {
		return p, fmt.Errorf("restored, but could not update the archive file: %w", err)
	}
	return p, nil
}

func (s *Store) rewriteArchiveWithout(all []ArchivedPercept, skip int) error {
	tmp := s.archivePath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for i, a := range all {
		if i == skip {
			continue
		}
		b, err := json.Marshal(a)
		if err == nil {
			_, err = f.Write(append(b, '\n'))
		}
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, s.archivePath)
}

// StaleEntry describes how close a global Percept is to being archived.
type StaleEntry struct {
	Percept  Percept
	IdleDays float64
	// DaysLeft is the running time still needed before the idle limit is
	// reached; 0 means the limit is reached (it is archived once it also sits
	// at the prune weight).
	DaysLeft float64
	// AtFloor reports whether the weight has decayed to the prune threshold.
	AtFloor bool
}

// Stale lists the non-core global Percepts by idle time, longest first,
// including the time since the last accounting pass. Nil when archiving is
// disabled.
func (s *Store) Stale() []StaleEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.params.GlobalArchiveAfterDays < 0 {
		return nil
	}
	now := s.now()
	limit := s.params.GlobalArchiveAfterDays * 24 * 3600
	var out []StaleEntry
	for _, p := range s.global.Percepts {
		if p.Core {
			continue
		}
		from := s.lastTick
		if p.LastUsedAt.After(from) {
			from = p.LastUsedAt
		}
		idle := p.IdleSeconds + now.Sub(from).Seconds()
		left := limit - idle
		if left < 0 {
			left = 0
		}
		out = append(out, StaleEntry{Percept: p, IdleDays: idle / 86400, DaysLeft: left / 86400,
			AtFloor: p.W <= s.params.PruneThreshold+1e-9})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].IdleDays > out[j].IdleDays })
	return out
}
