package config

import "testing"

// Issue #375 (verdict proposal 6): percept_store_max must default to a real
// backstop — the global store previously had no bound at all (unset = 0 =
// unlimited), and staleness-archiving alone never removes an actively-used
// store's growth. Unset/zero now falls back to DefaultPerceptStoreMax; only
// an explicitly negative value disables the cap.
func TestPerceptStoreSizeLimit_DefaultBackstop(t *testing.T) {
	for _, tc := range []struct {
		name string
		max  int
		want int
	}{
		{"unset (zero value) falls back to the backstop", 0, DefaultPerceptStoreMax},
		{"negative disables the cap", -1, 0},
		{"explicit positive value honored", 37, 37},
		{"large explicit value honored", 1000, 1000},
	} {
		got := (Config{PerceptStoreMax: tc.max}).PerceptStoreSizeLimit()
		if got != tc.want {
			t.Errorf("%s: PerceptStoreSizeLimit() = %d, want %d", tc.name, got, tc.want)
		}
	}
	if DefaultPerceptStoreMax <= 0 {
		t.Errorf("backstop must be > 0, got %d", DefaultPerceptStoreMax)
	}
}
