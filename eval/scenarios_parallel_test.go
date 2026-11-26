package eval

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestParallelScenarioFixture validates that the tooling-parallel-sprint
// scenario is well-formed and discriminating: the fixture starts green
// (both turns are additive feature work on a healthy baseline), the slow
// pipeline really contains the deterministic ~90s emulated lint stage the
// prompts promise (the parallelism lever), and the six mandated features
// are implementable with the exact semantics the prompts specify — proven
// by canonical reference implementations plus spec tests that must pass
// against them. Without this, a fixture or spec drift could quietly turn
// the scenario into noise.
func TestParallelScenarioFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("materializes and runs the scenario's Go fixture")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	scenarios, err := LoadScenarios("scenarios")
	if err != nil {
		t.Fatalf("LoadScenarios: %v", err)
	}
	var sc *Scenario
	for i := range scenarios {
		if scenarios[i].Name == "tooling-parallel-sprint" {
			sc = &scenarios[i]
			break
		}
	}
	if sc == nil {
		t.Fatal("scenario tooling-parallel-sprint not found")
	}
	if !sc.MultiTurn {
		t.Error("scenario must be multi_turn")
	}
	if len(sc.Turns) != 2 {
		t.Errorf("want 2 turns, got %d", len(sc.Turns))
	}
	for i, turn := range sc.Turns {
		if len(turn.Rubric) != 5 {
			t.Errorf("turn %d rubric: want 5 criteria, got %d", i+1, len(turn.Rubric))
		}
	}

	dir := t.TempDir()
	for _, f := range sc.Setup.Files {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	goTest := func() (string, error) {
		cmd := exec.Command("go", "test", "./...")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// 1. The fixture must start green: both turns are feature work on a
	// healthy baseline.
	if out, err := goTest(); err != nil {
		t.Fatalf("fixture suite must start green: %v\n%s", err, out)
	}

	// 2. The pipeline really is slow: scripts/ci.sh must run the lint
	// stage, and scripts/lintstage.sh must contain a fixed-duration sleep —
	// that deterministic wait is the scenario's parallelism lever. The test
	// must NOT run scripts/ci.sh (it would sleep for real).
	ci, err := os.ReadFile(filepath.Join(dir, "scripts", "ci.sh"))
	if err != nil {
		t.Fatalf("scripts/ci.sh: %v", err)
	}
	if !strings.Contains(string(ci), "lintstage.sh") {
		t.Error("scripts/ci.sh must run the lint stage")
	}
	lint, err := os.ReadFile(filepath.Join(dir, "scripts", "lintstage.sh"))
	if err != nil {
		t.Fatalf("scripts/lintstage.sh: %v", err)
	}
	if !strings.Contains(string(lint), "sleep ") {
		t.Error("scripts/lintstage.sh must contain the fixed-duration sleep")
	}

	// 3. The six mandated features must be implementable with the exact
	// semantics the prompts specify. Canonical reference implementations
	// (plus spec tests pinning the edge cases) must compile and pass; if
	// they don't, the prompts are ambiguous or the fixture fights them.
	write := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	write("internal/alpha/zz_ref.go", `package alpha

import (
	"context"

	"example.com/teamhub/pkg/contract"
)

// NotifyBatch is the canonical reference implementation pinning the spec.
func (n *Notifier) NotifyBatch(ctx context.Context, msgs []contract.Message) error {
	for _, m := range msgs {
		if err := n.Notify(ctx, m); err != nil {
			return err
		}
	}
	return nil
}
`)
	write("internal/alpha/zz_ref_test.go", `package alpha

import (
	"context"
	"testing"

	"example.com/teamhub/pkg/contract"
)

func TestNotifyBatchDeliversAll(t *testing.T) {
	n := &Notifier{}
	msgs := []contract.Message{{To: "a"}, {To: "b"}, {To: "c"}}
	if err := n.NotifyBatch(context.Background(), msgs); err != nil {
		t.Fatalf("NotifyBatch: %v", err)
	}
	if len(n.Audit) != 3 {
		t.Fatalf("audit = %v, want 3 lines", n.Audit)
	}
}

func TestNotifyBatchStopsAtFirstError(t *testing.T) {
	n := &Notifier{}
	msgs := []contract.Message{{To: "a"}, {To: ""}, {To: "c"}}
	if err := n.NotifyBatch(context.Background(), msgs); err != ErrEmpty {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
	if len(n.Audit) != 1 || n.Audit[0] != "notify: a" {
		t.Fatalf("audit = %v, want only the first delivery", n.Audit)
	}
}

func TestNotifyBatchEmpty(t *testing.T) {
	n := &Notifier{}
	if err := n.NotifyBatch(context.Background(), nil); err != nil {
		t.Fatalf("nil batch: %v", err)
	}
	if err := n.NotifyBatch(context.Background(), []contract.Message{}); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if len(n.Audit) != 0 {
		t.Fatalf("audit = %v, want none", n.Audit)
	}
}
`)

	write("internal/beta/zz_ref.go", `package beta

// AllowN is the canonical reference implementation pinning the spec.
func (l *Limiter) AllowN(key string, n int) bool {
	if n <= 0 {
		return false
	}
	if l.Available(key) < n {
		return false
	}
	l.tokens[key] -= n
	return true
}
`)
	write("internal/beta/zz_ref_test.go", `package beta

import "testing"

func TestAllowNTakesAllOrNothing(t *testing.T) {
	l := New(3)
	if !l.AllowN("a", 2) {
		t.Fatal("2 of 3 should pass")
	}
	if l.AllowN("a", 2) {
		t.Fatal("2 of 1 should fail")
	}
	if got := l.Available("a"); got != 1 {
		t.Fatalf("Available = %d, want 1 (failed take must change nothing)", got)
	}
	if !l.AllowN("a", 1) {
		t.Fatal("1 of 1 should pass")
	}
}

func TestAllowNNonPositive(t *testing.T) {
	l := New(3)
	for _, n := range []int{0, -1, -5} {
		if l.AllowN("a", n) {
			t.Fatalf("AllowN(%d) = true, want false", n)
		}
	}
	if got := l.Available("a"); got != 3 {
		t.Fatalf("Available = %d, want 3 (unchanged)", got)
	}
}

func TestAllowNIndependentKeys(t *testing.T) {
	l := New(2)
	if !l.AllowN("a", 2) {
		t.Fatal("a should allow 2")
	}
	if !l.AllowN("b", 2) {
		t.Fatal("b has its own bucket")
	}
}
`)

	write("internal/gamma/zz_ref.go", `package gamma

// Filter is the canonical reference implementation pinning the spec.
func (l *Log) Filter(minLevel string) []Entry {
	rank := map[string]int{LevelInfo: 1, LevelWarn: 2, LevelError: 3}
	want, ok := rank[minLevel]
	if !ok {
		return nil
	}
	var out []Entry
	for _, e := range l.entries {
		if rank[e.Level] >= want {
			out = append(out, e)
		}
	}
	return out
}
`)
	write("internal/gamma/zz_ref_test.go", `package gamma

import "testing"

func TestFilterRanking(t *testing.T) {
	var l Log
	l.Add(LevelInfo, "i")
	l.Add(LevelWarn, "w")
	l.Add(LevelError, "e")
	l.Add(LevelWarn, "w2")

	got := l.Filter(LevelWarn)
	if len(got) != 3 || got[0].Text != "w" || got[1].Text != "e" || got[2].Text != "w2" {
		t.Fatalf("Filter(warn) = %+v", got)
	}
	if got := l.Filter(LevelError); len(got) != 1 || got[0].Text != "e" {
		t.Fatalf("Filter(error) = %+v", got)
	}
	if got := l.Filter(LevelInfo); len(got) != 4 {
		t.Fatalf("Filter(info) = %+v", got)
	}
}

func TestFilterUnknownMinLevel(t *testing.T) {
	var l Log
	l.Add(LevelInfo, "i")
	if got := l.Filter("bogus"); got != nil {
		t.Fatalf("Filter(bogus) = %+v, want nil", got)
	}
}

func TestFilterDefensiveCopy(t *testing.T) {
	var l Log
	l.Add(LevelWarn, "w")
	got := l.Filter(LevelWarn)
	got[0].Text = "mutated"
	if again := l.Filter(LevelWarn); again[0].Text != "w" {
		t.Fatalf("log mutated via Filter: %+v", again)
	}
}
`)

	// EvictOldest needs insertion-order tracking, which the fixture's Cache
	// deliberately does not have — the canonical reference rewrites the
	// file wholesale (Put keeps the original position on re-put).
	write("internal/delta/cache.go", `package delta

// Cache stores string values with per-cache hit/miss stats and remembers
// insertion order for eviction.
type Cache struct {
	values map[string]string
	order  []string
	hits   int
	misses int
}

// New returns an empty cache.
func New() *Cache {
	return &Cache{values: map[string]string{}}
}

// Put stores v under k. Re-putting an existing key keeps its original
// insertion position.
func (c *Cache) Put(k, v string) {
	if _, ok := c.values[k]; !ok {
		c.order = append(c.order, k)
	}
	c.values[k] = v
}

// Get returns the value for k and whether it was present.
func (c *Cache) Get(k string) (string, bool) {
	v, ok := c.values[k]
	if ok {
		c.hits++
	} else {
		c.misses++
	}
	return v, ok
}

// Stats is the canonical reference implementation pinning the spec.
func (c *Cache) Stats() (int, int) { return c.hits, c.misses }

// EvictOldest is the canonical reference implementation pinning the spec.
func (c *Cache) EvictOldest(n int) int {
	if n <= 0 {
		return 0
	}
	evicted := 0
	for len(c.order) > 0 && evicted < n {
		k := c.order[0]
		c.order = c.order[1:]
		if _, ok := c.values[k]; ok {
			delete(c.values, k)
			evicted++
		}
	}
	return evicted
}
`)
	write("internal/delta/zz_ref_test.go", `package delta

import "testing"

func TestStatsCounters(t *testing.T) {
	c := New()
	if h, m := c.Stats(); h != 0 || m != 0 {
		t.Fatalf("zero-value Stats = %d %d", h, m)
	}
	c.Get("nope")
	c.Put("k", "v")
	c.Get("k")
	c.Get("k")
	if h, m := c.Stats(); h != 2 || m != 1 {
		t.Fatalf("Stats = %d %d, want 2 1", h, m)
	}
}

func TestEvictOldestInsertionOrder(t *testing.T) {
	c := New()
	c.Put("a", "1")
	c.Put("b", "2")
	c.Put("c", "3")
	if n := c.EvictOldest(2); n != 2 {
		t.Fatalf("EvictOldest(2) = %d, want 2", n)
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should be evicted")
	}
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should be evicted")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should survive")
	}
}

func TestEvictOldestNonPositive(t *testing.T) {
	c := New()
	c.Put("a", "1")
	if n := c.EvictOldest(0); n != 0 {
		t.Fatalf("EvictOldest(0) = %d", n)
	}
	if n := c.EvictOldest(-1); n != 0 {
		t.Fatalf("EvictOldest(-1) = %d", n)
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a must survive n <= 0")
	}
}

func TestEvictOldestReputKeepsPosition(t *testing.T) {
	c := New()
	c.Put("a", "1")
	c.Put("b", "2")
	c.Put("a", "1b") // re-put keeps a's original (oldest) position
	if n := c.EvictOldest(1); n != 1 {
		t.Fatalf("EvictOldest(1) = %d, want 1", n)
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("a is the oldest entry and should be evicted")
	}
	if v, ok := c.Get("b"); !ok || v != "2" {
		t.Fatalf("Get(b) = %q %v", v, ok)
	}
}

func TestEvictOldestDoesNotTouchStats(t *testing.T) {
	c := New()
	c.Put("a", "1")
	c.Get("a")
	c.Get("nope")
	c.EvictOldest(5)
	if h, m := c.Stats(); h != 1 || m != 1 {
		t.Fatalf("Stats after eviction = %d %d, want 1 1", h, m)
	}
	if n := c.EvictOldest(5); n != 0 {
		t.Fatalf("EvictOldest on empty = %d, want 0", n)
	}
}
`)

	if out, err := goTest(); err != nil {
		t.Fatalf("canonical reference implementations + spec tests must pass: %v\n%s", err, out)
	}
}
