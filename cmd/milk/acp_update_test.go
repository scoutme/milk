package main

// Tests for /update over ACP (check|status|install|skip) and the one-shot
// "update available" notice. The release feed is stubbed through
// updater.ReleasesURL (the fetch itself is covered by internal/updater's own
// tests); the install path is stubbed through updateBinaryPath/updateApply so
// no test ever resolves or replaces the running go-test binary.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/transport/acp"
	"github.com/scoutme/milk/internal/updater"
)

// testAssetName mirrors updater's unexported per-platform asset name — the
// stub feed must advertise exactly the name CheckLatest matches against.
func testAssetName() string {
	if runtime.GOOS == "windows" {
		return "milk-windows-amd64.exe"
	}
	return fmt.Sprintf("milk-%s-%s", runtime.GOOS, runtime.GOARCH)
}

// stubReleaseFeed serves a one-release GitHub feed whose binary and checksum
// assets self-host on the same server, and points updater.ReleasesURL at it.
// hits, when non-nil, counts every request the feed receives.
func stubReleaseFeed(t *testing.T, tag string, hits *atomic.Int64) {
	t.Helper()
	asset := testAssetName()
	binary := []byte("fake milk binary")
	sum := sha256.Sum256(binary)
	sumHex := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		base := "http://" + r.Host
		switch {
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			fmt.Fprintf(w, "%s  %s\n", sumHex, asset)
		case strings.Contains(r.URL.Path, "/assets/"):
			_, _ = w.Write(binary)
		default:
			fmt.Fprintf(w, `[{"tag_name":%q,"name":"rel","html_url":%q,"prerelease":false,"assets":[{"name":%q,"browser_download_url":%q},{"name":%q,"browser_download_url":%q}]}]`,
				tag, base+"/releases/"+tag,
				asset, base+"/assets/"+asset,
				asset+".sha256", base+"/assets/"+asset+".sha256")
		}
	}))
	t.Cleanup(srv.Close)
	old := updater.ReleasesURL
	updater.ReleasesURL = srv.URL + "/releases"
	t.Cleanup(func() { updater.ReleasesURL = old })
}

// stubUpdateApply replaces the install seams (update_core.go) and counts
// apply calls. Restored via t.Cleanup.
func stubUpdateApply(t *testing.T, applyErr error) *int {
	t.Helper()
	calls := new(int)
	oldPath, oldApply := updateBinaryPath, updateApply
	updateBinaryPath = func() (string, error) { return "/tmp/fake-milk-bin", nil }
	updateApply = func(_ context.Context, _ *updater.Release, dest string, _ func(done, total int64)) error {
		*calls++
		if dest != "/tmp/fake-milk-bin" {
			t.Errorf("apply dest = %q, want the stubbed binary path", dest)
		}
		return applyErr
	}
	t.Cleanup(func() { updateBinaryPath, updateApply = oldPath, oldApply })
	return calls
}

// chunksContaining returns the agent_message_chunk texts sent so far that
// contain sub.
func chunksContaining(conn *fakeACPConn, sub string) []string {
	var out []string
	for _, c := range acpChunks(conn) {
		if strings.Contains(c, sub) {
			out = append(out, c)
		}
	}
	return out
}

func TestACPUpdate_CheckReportsAvailable(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	stubReleaseFeed(t, "v9.9.9", nil)
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/update check")

	found := chunksContaining(conn, "update available")
	if len(found) == 0 {
		t.Fatalf("no reply chunk reports the update; chunks = %q", acpChunks(conn))
	}
	if !strings.Contains(found[0], "v9.9.9") {
		t.Errorf("reply = %q, want the release tag", found[0])
	}
	if rel := server.updateReleaseCached(); rel == nil {
		t.Error("release not cached on the server — later /update install would re-fetch")
	}
}

func TestACPUpdate_CheckUpToDateStillRecordsLastCheck(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	// v0.0.0 is not newer than the test binary's "dev" (== 0.0.0): up to date.
	stubReleaseFeed(t, "v0.0.0", nil)
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/update check")

	if got := chunksContaining(conn, "already up to date"); len(got) == 0 {
		t.Errorf("chunks = %q, want the up-to-date reply", acpChunks(conn))
	}
	raw, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".milk", "config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	// Regression pin: the last-check write-back happens on the boring
	// up-to-date outcome too — otherwise the 24h debounce never holds.
	if v, _ := saved["update_last_check"].(string); v == "" {
		t.Error("up-to-date check did not record update_last_check")
	}
}

func TestACPUpdate_StatusKnownReleaseThenInstalled(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	stubReleaseFeed(t, "v9.9.9", nil)
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/update status")
	if got := chunksContaining(conn, "no update information"); len(got) == 0 {
		t.Errorf("fresh status chunks = %q, want the 'no information' reply", acpChunks(conn))
	}

	acpPromptText(t, server, id, "/update check")
	acpPromptText(t, server, id, "/update status")
	if got := chunksContaining(conn, "v9.9.9 available"); len(got) == 0 {
		t.Errorf("after check, status chunks = %q, want the available reply", acpChunks(conn))
	}

	stubUpdateApply(t, nil)
	acpPromptText(t, server, id, "/update install")
	acpPromptText(t, server, id, "/update status")
	if got := chunksContaining(conn, "v9.9.9 installed (running"); len(got) == 0 {
		t.Errorf("after install, status chunks = %q, want the pending-restart reply", acpChunks(conn))
	}
}

func TestACPUpdate_InstallAppliesAndRequestsRestart(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	stubReleaseFeed(t, "v9.9.9", nil)
	id := acpNewSession(t, server)
	acpPromptText(t, server, id, "/update check") // caches the release

	calls := stubUpdateApply(t, nil)
	acpPromptText(t, server, id, "/update install")

	if *calls != 1 {
		t.Errorf("apply calls = %d, want 1", *calls)
	}
	applied := chunksContaining(conn, "applied v9.9.9")
	if len(applied) == 0 {
		t.Fatalf("chunks = %q, want the applied reply", acpChunks(conn))
	}
	// Honesty: the running process still executes the old binary — the reply
	// must say to restart the agent (a new session would not pick it up).
	if !strings.Contains(applied[0], "restart the milk agent") {
		t.Errorf("reply = %q, want the restart instruction", applied[0])
	}
}

func TestACPUpdate_InstallWindowsManualPath(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	stubReleaseFeed(t, "v9.9.9", nil)
	id := acpNewSession(t, server)
	acpPromptText(t, server, id, "/update check")

	stubUpdateApply(t, fmt.Errorf("%w: %s", updater.ErrWindowsManual, "/tmp/milk-update-x"))
	acpPromptText(t, server, id, "/update install")

	failed := chunksContaining(conn, "/tmp/milk-update-x")
	if len(failed) == 0 {
		t.Fatalf("chunks = %q, want the manual-update reply carrying the temp path", acpChunks(conn))
	}
	// Honesty: never claim success when the binary was not replaced.
	if !strings.Contains(failed[0], "manually") {
		t.Errorf("reply = %q, want the manual-replacement instruction", failed[0])
	}
	if got := chunksContaining(conn, "applied "); len(got) != 0 {
		t.Errorf("windows-manual outcome claimed success: %q", got)
	}
}

func TestACPUpdate_InstallInFlightGuard(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	stubReleaseFeed(t, "v9.9.9", nil)
	id := acpNewSession(t, server)
	acpPromptText(t, server, id, "/update check")

	if !server.beginUpdateInstall() {
		t.Fatal("beginUpdateInstall on an idle server = false")
	}
	stubUpdateApply(t, nil)
	acpPromptText(t, server, id, "/update install")
	server.endUpdateInstall()

	if got := chunksContaining(conn, "update already in progress"); len(got) == 0 {
		t.Errorf("chunks = %q, want the in-flight reply", acpChunks(conn))
	}
}

func TestACPUpdate_UsageLine(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/update bogus")

	if got := chunksContaining(conn, "usage: /update [check|status|install|skip]"); len(got) == 0 {
		t.Errorf("chunks = %q, want the usage line", acpChunks(conn))
	}
}

func TestACPUpdate_SkipPersistsAndStopsTheNag(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	stubReleaseFeed(t, "v9.9.9", nil)
	id := acpNewSession(t, server)
	acpPromptText(t, server, id, "/update check")

	acpPromptText(t, server, id, "/update skip")
	if got := chunksContaining(conn, "skipped — it will not be announced again"); len(got) == 0 {
		t.Errorf("chunks = %q, want the skipped reply", acpChunks(conn))
	}

	raw, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".milk", "config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if v, _ := saved["update_skipped_version"].(string); v != "v9.9.9" {
		t.Errorf("update_skipped_version = %q, want v9.9.9 (skip must persist across restarts)", v)
	}

	// The skip filter: the skipped release never comes back from a check.
	acpPromptText(t, server, id, "/update check")
	if got := chunksContaining(conn, "already up to date"); len(got) == 0 {
		t.Errorf("chunks = %q, want the filtered up-to-date reply", acpChunks(conn))
	}
	if rel := server.updateReleaseCached(); rel != nil {
		t.Errorf("skipped release re-cached: %+v", rel)
	}
}

func TestACPStartupCheck_DebouncedNoNetwork(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	var hits atomic.Int64
	stubReleaseFeed(t, "v9.9.9", &hits)
	acpNewSession(t, server)

	server.cfg.UpdateLastCheck = time.Now().UTC().Format(time.RFC3339)
	server.backgroundUpdateCheck(context.Background())

	if hits.Load() != 0 {
		t.Errorf("feed hits = %d, want 0 — the 24h debounce must gate the fetch", hits.Load())
	}
	if got := chunksContaining(conn, "available"); len(got) != 0 {
		t.Errorf("debounced check announced anyway: %q", got)
	}
}

func TestACPUpdateNotice_AnnouncedOncePerOpenSession(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	stubReleaseFeed(t, "v9.9.9", nil)
	acpNewSession(t, server)
	acpNewSession(t, server)

	server.backgroundUpdateCheck(context.Background())

	if got := chunksContaining(conn, "available"); len(got) != 2 {
		t.Fatalf("update notices = %d (%q), want exactly one per session", len(got), got)
	}
	// Re-announcing the same release adds nothing — once per session.
	rel := server.updateReleaseCached()
	if rel == nil {
		t.Fatal("release not cached after the background check")
	}
	server.announceUpdate(rel)
	if got := chunksContaining(conn, "available"); len(got) != 2 {
		t.Errorf("after re-announce, notices = %d, want still 2", len(got))
	}
}

func TestACPUpdateNotice_SessionCreatedAfterCheck(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	stubReleaseFeed(t, "v9.9.9", nil)

	// The check completes with no open session (the late-completion case).
	server.backgroundUpdateCheck(context.Background())

	// A session created afterwards hears the notice via the session/new
	// hook (AfterResponse is the transport's post-response step — drive it
	// explicitly like the real wire path does).
	for i := 1; i <= 2; i++ {
		id := acpNewSession(t, server)
		server.AfterResponse("session/new", acp.NewSessionResponse{SessionID: id})
		if got := chunksContaining(conn, "available"); len(got) != i {
			t.Fatalf("after session %d: notices = %d (%q), want %d", i, len(got), got, i)
		}
	}
}

func TestACPStartupCheck_SkippedReleaseNotAnnounced(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	var hits atomic.Int64
	stubReleaseFeed(t, "v9.9.9", &hits)
	acpNewSession(t, server)

	server.cfg.UpdateSkippedVersion = "v9.9.9"
	server.backgroundUpdateCheck(context.Background())

	if hits.Load() == 0 {
		t.Error("feed never hit — the check itself must run and let the skip filter drop the release")
	}
	if got := chunksContaining(conn, "available"); len(got) != 0 {
		t.Errorf("skipped release announced: %q", got)
	}
}
