package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/plugin"
)

func parseAddRefForTest(ref string) (plugin.Source, error) {
	return plugin.ParseAddRef(ref)
}

// TestMarketplaceDirName (review W5): the snapshot directory must be unique
// per SOURCE, not per basename. The old name (filepath.Base of repo-or-URL)
// collapsed "a/tools" and "b/tools" onto one dir — and silently RemoveAll'd
// the loser — and gave URL sources directory names like "plugin.zip".
func TestMarketplaceDirName(t *testing.T) {
	for _, tc := range []struct{ ref, want string }{
		{"lliu56/minicut-skills", "lliu56-minicut-skills"},
		{"ccplugins/awesome-claude-code-plugins", "ccplugins-awesome-claude-code-plugins"},
		{"https://example.com/x/plugin.zip", "example.com-plugin"},
		{"https://example.com/x/mkt.tar.gz", "example.com-mkt"},
		{"npm:@scope/pkg", "scope-pkg"},
	} {
		src, err := parseAddRefForTest(tc.ref)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.ref, err)
		}
		if got := marketplaceDirName(src); got != tc.want {
			t.Errorf("marketplaceDirName(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
	// Two same-basename repos must not collide.
	a, _ := parseAddRefForTest("a/tools")
	b, _ := parseAddRefForTest("b/tools")
	if marketplaceDirName(a) == marketplaceDirName(b) {
		t.Error("same-basename repos must get distinct snapshot directories")
	}
}

// TestMarketplaceAdd_CollisionRefusedNotDestroyed (review W5): re-registering
// over an existing snapshot must FAIL LOUDLY and leave the existing catalog
// untouched. The old flow RemoveAll'd the destination before even fetching —
// so a name collision, or simply a failed fetch, destroyed the previous
// registration. The add target is a never-answering local URL so the test
// never reaches the network: with the fix the collision is refused before
// any fetch starts.
func TestMarketplaceAdd_CollisionRefusedNotDestroyed(t *testing.T) {
	resetPluginState(t)
	mktJSON := `{"name":"preexisting","owner":{"name":"t"},"plugins":[]}`
	writeStateFile(t, "marketplaces/127.0.0.1-1-plugin/marketplace.json", mktJSON)
	path := filepath.Join(paths().marketplaces, "127.0.0.1-1-plugin", "marketplace.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = pluginsActionRun([]string{"marketplace", "add", "https://127.0.0.1:1/x/plugin.zip"}, pluginsActionOpts{})
	if err == nil {
		t.Fatal("re-registering an existing marketplace must be refused")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("expected 'already registered', got: %v", err)
	}
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("existing catalog destroyed by the refused add: %v", rerr)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("existing catalog content changed by the refused add")
	}
}
