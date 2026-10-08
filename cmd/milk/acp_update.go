package main

// /update over ACP: check|status|install|skip plus the one-shot
// "update available" notice. Runs as a turn-aware command (runTurn) because
// check and install do network I/O — t.ctx makes them cancellable via
// session/cancel, and say() streams the install banner so the client isn't
// staring at a frozen prompt during the download. The release cache and the
// in-flight guard live on acpServer (per serve process, not per session);
// the shared check/install plumbing is update_core.go.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/transport/acp"
	"github.com/scoutme/milk/internal/updater"
)

// acpUpdate implements /update [check|status|install|skip] over ACP.
func acpUpdate(t *acpTurn, rest string) (string, string) {
	as := t.as
	switch sub := strings.ToLower(strings.TrimSpace(rest)); sub {
	case "", "check":
		ctx, cancel := context.WithTimeout(t.ctx, 15*time.Second)
		defer cancel()
		rel, err := updateCheck(ctx, &as.st.cfg)
		if err != nil {
			return milkTag() + " update check failed: " + err.Error(), ""
		}
		as.srv.setUpdateRelease(rel)
		if rel == nil {
			return fmt.Sprintf("%s already up to date (%s)", milkTag(), version), ""
		}
		return fmt.Sprintf("%s update available: %s  %s — /update install (or /update skip)", milkTag(), rel.Tag, rel.HTMLURL), ""

	case "status":
		return as.updateStatusText(), ""

	case "install":
		return acpUpdateInstall(t)

	case "skip":
		rel := as.srv.updateReleaseCached()
		if rel == nil {
			ctx, cancel := context.WithTimeout(t.ctx, 15*time.Second)
			var err error
			rel, err = updateCheck(ctx, &as.st.cfg)
			cancel()
			if err != nil {
				return milkTag() + " update check failed: " + err.Error(), ""
			}
			as.srv.setUpdateRelease(rel)
		}
		if rel == nil {
			return fmt.Sprintf("%s no update available (%s)", milkTag(), version), ""
		}
		skipUpdateRelease(&as.st.cfg, rel.Tag)
		as.srv.setUpdateRelease(nil)
		return fmt.Sprintf("%s update %s skipped — it will not be announced again", milkTag(), rel.Tag), ""
	}
	return "usage: /update [check|status|install|skip]", ""
}

// acpUpdateInstall downloads and applies a release. The cached release is
// preferred; with no cache this does a fresh raw check (CLI parity for
// `milk update install`: an explicit install is explicit intent — it fetches
// and applies the latest release regardless of any skipped tag).
func acpUpdateInstall(t *acpTurn) (string, string) {
	as := t.as
	if !as.srv.beginUpdateInstall() {
		return milkTag() + " update already in progress", ""
	}
	defer as.srv.endUpdateInstall()

	rel := as.srv.updateReleaseCached()
	if rel == nil {
		ctx, cancel := context.WithTimeout(t.ctx, 15*time.Second)
		var err error
		rel, err = updater.CheckLatest(ctx, version, as.st.cfg.UpdateCheckIncludePrerelease())
		cancel()
		if err != nil {
			return milkTag() + " update check failed: " + err.Error(), ""
		}
	}
	if rel == nil {
		return fmt.Sprintf("%s no update available (%s)", milkTag(), version), ""
	}

	t.say(stripANSI(milkTag()) + " installing " + rel.Tag + "…")
	ctx, cancel := context.WithTimeout(t.ctx, 5*time.Minute)
	defer cancel()
	err := updateInstall(ctx, rel, nil)
	switch {
	case errors.Is(err, updater.ErrWindowsManual):
		// Never claim success: the download is saved but the running
		// binary could not be replaced (the error carries the temp path).
		return fmt.Sprintf("%s %s downloaded but %v — replace the binary manually, then restart the milk agent", milkTag(), rel.Tag, err), ""
	case err != nil:
		return milkTag() + " update failed: " + err.Error(), ""
	}
	as.srv.setInstalledTag(rel.Tag)
	as.srv.setUpdateRelease(nil)
	return fmt.Sprintf("%s applied %s — restart the milk agent (the running serve process) to use it", milkTag(), rel.Tag), ""
}

// updateStatusText renders /update status from what is already known — no
// network round trip. An installed-pending-restart tag comes first (it is the
// most action-relevant), then a known release, then the skip note.
func (as *acpSession) updateStatusText() string {
	if tag := as.srv.installedTagValue(); tag != "" {
		return fmt.Sprintf("%s %s installed (running %s) — restart the milk agent (the running serve process) to use it", milkTag(), tag, version)
	}
	if rel := as.srv.updateReleaseCached(); rel != nil {
		return fmt.Sprintf("%s %s available — /update install (or /update skip)", milkTag(), rel.Tag)
	}
	if skip := as.st.cfg.UpdateSkippedVersion; skip != "" {
		return fmt.Sprintf("%s %s skipped — /update install installs it after all", milkTag(), skip)
	}
	return fmt.Sprintf("%s no update information yet — /update check", milkTag())
}

// --- acpServer update state (all guarded by s.mu) ---

func (s *acpServer) setUpdateRelease(rel *updater.Release) {
	s.mu.Lock()
	s.updateRelease = rel
	s.mu.Unlock()
}

func (s *acpServer) updateReleaseCached() *updater.Release {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateRelease
}

func (s *acpServer) beginUpdateInstall() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateInstalling {
		return false
	}
	s.updateInstalling = true
	return true
}

func (s *acpServer) endUpdateInstall() {
	s.mu.Lock()
	s.updateInstalling = false
	s.mu.Unlock()
}

func (s *acpServer) setInstalledTag(tag string) {
	s.mu.Lock()
	s.updateInstalled = tag
	s.mu.Unlock()
}

func (s *acpServer) installedTagValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateInstalled
}

// backgroundUpdateCheck is the serve-time release check: debounced by
// config.ShouldCheckUpdate (shared with the TUI through UpdateLastCheck, which
// updateCheck writes on every successful check), silent on error or up-to-date,
// announcing at most one notice per session — including sessions created
// before the check finished (announceUpdate) and after it (announceUpdateTo).
// Launched once from runServe, never from newACPServer, so tests don't spawn
// network checks.
func (s *acpServer) backgroundUpdateCheck(ctx context.Context) {
	cfg := s.cfg
	if !cfg.ShouldCheckUpdate() {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rel, err := updateCheck(cctx, &cfg)
	if err != nil || rel == nil {
		return
	}
	s.announceUpdate(rel)
}

// announceUpdate stores rel and notifies every open session that has not
// heard about it yet. The flag flip happens under s.mu but the notifications
// go out after the unlock — conn.Notify under the server lock would add a
// lock-order edge to every other notify path.
func (s *acpServer) announceUpdate(rel *updater.Release) {
	s.mu.Lock()
	s.updateRelease = rel
	var targets []*acpSession
	for _, as := range s.sessions {
		if !as.updateAnnounced {
			as.updateAnnounced = true
			targets = append(targets, as)
		}
	}
	s.mu.Unlock()
	for _, as := range targets {
		as.notify(acp.AgentMessageChunk(
			acp.MessageID(fmt.Sprintf("update-%d", as.msgCounter.Add(1))),
			stripANSI(updateAvailableNotice(rel))))
	}
}

// announceUpdateTo tells one session about a release the process already
// knows (the session/new hook: a session created after the check completed
// must hear about it too). No-op when there is nothing to announce or the
// session has been announced already — the notice is once per session.
func (s *acpServer) announceUpdateTo(as *acpSession) {
	s.mu.Lock()
	rel := s.updateRelease
	if rel == nil || as.updateAnnounced {
		s.mu.Unlock()
		return
	}
	as.updateAnnounced = true
	s.mu.Unlock()
	as.notify(acp.AgentMessageChunk(
		acp.MessageID(fmt.Sprintf("update-%d", as.msgCounter.Add(1))),
		stripANSI(updateAvailableNotice(rel))))
}
