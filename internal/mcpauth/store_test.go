package mcpauth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempMilkHome points config.Dir() (and thus storeDir()) at a temp dir
// for the duration of the test by overriding $HOME.
func withTempMilkHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
}

func TestLoadToken_Missing(t *testing.T) {
	withTempMilkHome(t)
	ts, err := LoadToken("nonexistent")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if ts != nil {
		t.Errorf("expected nil TokenSet for missing file, got %+v", ts)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withTempMilkHome(t)
	want := &TokenSet{
		AccessToken:           "at",
		RefreshToken:          "rt",
		TokenType:             "Bearer",
		ExpiresAt:             time.Now().Add(time.Hour).Truncate(time.Second),
		Scope:                 "read write",
		Issuer:                "https://auth.example.com",
		AuthorizationEndpoint: "https://auth.example.com/authorize",
		TokenEndpoint:         "https://auth.example.com/token",
		ClientID:              "client-123",
	}
	if err := SaveToken("my-server", want); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	got, err := LoadToken("my-server")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if got == nil {
		t.Fatal("LoadToken returned nil after Save")
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken ||
		got.ClientID != want.ClientID || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestSaveToken_AtomicWriteNoTmpLeftover(t *testing.T) {
	withTempMilkHome(t)
	if err := SaveToken("srv", &TokenSet{AccessToken: "x"}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	dir, err := storeDir()
	if err != nil {
		t.Fatalf("storeDir: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "srv.json"))
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token file perm = %o, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "srv.json.tmp")); !os.IsNotExist(err) {
		t.Errorf("expected tmp file to be gone after rename, stat err = %v", err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("store dir perm = %o, want 0700", dirInfo.Mode().Perm())
	}
}

func TestTokenFileName_Sanitizes(t *testing.T) {
	if got := tokenFileName("../../etc/passwd"); filepath.Base(got) != got {
		t.Errorf("tokenFileName(%q) = %q contains path separators", "../../etc/passwd", got)
	}
}

func TestDeleteToken(t *testing.T) {
	withTempMilkHome(t)
	if err := SaveToken("gone-soon", &TokenSet{AccessToken: "x"}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if err := DeleteToken("gone-soon"); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	ts, err := LoadToken("gone-soon")
	if err != nil {
		t.Fatalf("LoadToken after delete: %v", err)
	}
	if ts != nil {
		t.Errorf("expected nil after delete, got %+v", ts)
	}
	// Deleting again is a no-op, not an error.
	if err := DeleteToken("gone-soon"); err != nil {
		t.Errorf("DeleteToken (again): %v", err)
	}
}
