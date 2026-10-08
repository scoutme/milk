package main

// Host-independent self-update plumbing shared by milk's three surfaces: the
// TUI (/update), the CLI (milk update …) and ACP (/update over
// session/prompt). One check path — with the skipped-release filter and the
// last-check write-back — one install path, one notice wording. This is what
// makes the 24h debounce (config.ShouldCheckUpdate) actually hold across
// surfaces and a skipped release stop being announced everywhere at once.
//
// The TUI's bubbletea message plumbing (updateAvailableMsg, …) stays in
// repl.go; this file owns everything below it.

import (
	"context"
	"fmt"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/updater"
)

// Install seams: tests stub these so /update install never resolves or
// replaces the running go-test binary. Production wires the real updater
// functions.
var (
	updateBinaryPath = updater.CurrentBinaryPath
	updateApply      = updater.Apply
)

// updateCheck fetches the newest applicable release and records the check.
// Compared to the raw updater call it applies milk's skip filter (a release
// the user chose to skip never comes back) and writes UpdateLastCheck after
// every successful check — up to date included, which is the whole point:
// without the write-back on the boring path, every surface would re-hit the
// release feed on every start while up to date and the shared 24h debounce
// would never hold. cfg is mutated and saved to the auto-detected scope.
func updateCheck(ctx context.Context, cfg *config.Config) (*updater.Release, error) {
	rel, err := updater.CheckLatest(ctx, version, cfg.UpdateCheckIncludePrerelease())
	if err != nil {
		return nil, err
	}
	cfg.UpdateLastCheck = time.Now().UTC().Format(time.RFC3339)
	_ = saveLocalOrGlobal(*cfg)
	if rel != nil && rel.Tag == cfg.UpdateSkippedVersion {
		return nil, nil
	}
	return rel, nil
}

// updateInstall downloads rel over the running binary. On Windows the
// running binary cannot be replaced: the download is saved and the returned
// error wraps updater.ErrWindowsManual with the temp path appended — callers
// must surface it and never claim success. On success the running process is
// still executing the old binary, so callers must tell the user to restart
// milk itself (the process/agent — a new session would NOT pick the new
// version up).
func updateInstall(ctx context.Context, rel *updater.Release, progress func(done, total int64)) error {
	dest, err := updateBinaryPath()
	if err != nil {
		return fmt.Errorf("resolving binary path: %w", err)
	}
	return updateApply(ctx, rel, dest, progress)
}

// skipUpdateRelease records tag as the skipped release in config. The skip
// is honored by every check path afterwards (updateCheck's filter) until a
// newer release appears.
func skipUpdateRelease(cfg *config.Config, tag string) {
	cfg.UpdateSkippedVersion = tag
	_ = saveLocalOrGlobal(*cfg)
}

// updateAvailableNotice is the out-of-band "new release" line, used by the
// ACP channel's startup announcement (the TUI shows its status-bar hint
// instead). Actionable because it is only ever sent after a completed check.
func updateAvailableNotice(rel *updater.Release) string {
	return fmt.Sprintf("%s %s available — /update install (or /update skip)", milkTag(), rel.Tag)
}
