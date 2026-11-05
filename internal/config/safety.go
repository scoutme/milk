package config

// Config write safety (#404): milk must not be able to destroy its own
// configuration by accident. Four properties, implemented here:
//
//  1. Persist guard — a config derived from a FAILED load (defaults() after
//     an unparseable file with no usable backup) is never persisted by an
//     ordinary save. The 2026-11-05 incident (issue #404) was exactly this:
//     a startup update-check write-back saved the in-memory defaults over a
//     good config and wiped the agents array. Explicit recovery paths (the
//     init wizard) save through SaveForced; config_consent: "open" restores
//     the legacy permissive behavior.
//  2. Atomic writes — config.json is replaced via temp-file + rename, so a
//     concurrent reader sees the old bytes or the new bytes, never a
//     truncate-in-place half. Writers serialize on an advisory lock (see
//     lock_*.go), including read-modify-write patches.
//  3. Backup = previous content — rotateConfigBackup snapshots the bytes
//     that are ABOUT to be replaced into config.json.bak (previous good copy
//     → .bak, old .bak → .bak.1), only when the change is effective (never
//     for bookkeeping-only diffs, #405) and never from an unparseable file.
//     Loads never touch the backup: refreshing it from a wiped-but-valid
//     file would replace the last-known-good copy at the very next start.
//  4. Consent by construction + visibility — after the bookkeeping-write-back
//     migration (updateCheck → PatchBookkeeping, a re-read-single-key
//     surgery), every full-config writer in milk is reachable only from an
//     explicit user action (slash command, wizard, CLI config command, ACP
//     set_config_option). Background processes may only patch whitelisted
//     bookkeeping keys onto a file they just re-read and parsed. Every write
//     and every refusal is reported through SetSaveObserver (wired to obs by
//     cmd/milk), so the next incident is attributable instead of lost to
//     overwritten mtimes. An interactive ask-gate (ADR-0052 machinery) was
//     deliberately not built: with no non-explicit full-config writer left it
//     would be unreachable UI; the hook point is this observer.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrPersistRefused is returned by the save functions when the process last
// failed to load a real config (see LoadFailed) and the caller did not force
// the write. The message is user-facing: it is surfaced by TUI/ACP/CLI save
// paths verbatim.
var ErrPersistRefused = errors.New("refusing to persist a config derived from a failed load — fix or re-run /config init (or set config_consent: \"open\" to restore legacy behavior)")

var persistGuard struct {
	mu      sync.Mutex
	corrupt bool
}

// setPersistGuard records the outcome of the most recent load attempt.
// Only an unparseable file with no usable backup sets it; a successful parse
// or backup recovery clears it; read errors (notably ENOENT for an absent
// overlay file — a normal state) leave it unchanged so a greenfield read can
// never accidentally clear a genuine corruption flag.
func setPersistGuard(corrupt bool) {
	persistGuard.mu.Lock()
	persistGuard.corrupt = corrupt
	persistGuard.mu.Unlock()
}

// LoadFailed reports whether the process's last config load ended in an
// unrecoverable parse failure (the in-memory config may be defaults()).
// While true, non-forced saves are refused unless config_consent is "open".
func LoadFailed() bool {
	persistGuard.mu.Lock()
	defer persistGuard.mu.Unlock()
	return persistGuard.corrupt
}

// ResetLoadFailedState clears the persist guard. Production paths clear it
// through a successful load or a forced save; tests use this to avoid
// carrying a guard trip between test cases in one binary.
func ResetLoadFailedState() { setPersistGuard(false) }

// ConfigConsentOpen reports whether config_consent opts the user out of the
// persist guard ("open" = legacy permissive behavior; unset/"strict"/anything
// else = guarded). Empty default keeps the safe behavior for existing configs.
func (c Config) ConfigConsentOpen() bool {
	return strings.EqualFold(strings.TrimSpace(c.ConfigConsent), "open")
}

// persistAllowed is the guard check shared by every non-forced writer.
func persistAllowed(cfg Config, force bool) error {
	if force || !LoadFailed() || cfg.ConfigConsentOpen() {
		return nil
	}
	return ErrPersistRefused
}

// --- write observer -------------------------------------------------------

// SaveEvent describes one config write attempt: Kind is "saved" (full config
// written), "patched" (bookkeeping keys re-read and patched) or "refused"
// (guard, lock, or parse refusal — Err says why). SetSaveObserver wires one
// in at startup; cmd/milk maps it to obs so writes land in the milk log.
type SaveEvent struct {
	Kind  string
	Path  string
	Bytes int
	Err   error
}

var saveObserverMu sync.Mutex
var saveObserver func(SaveEvent)

// SetSaveObserver installs the write observer (nil disables). Safe to call
// from init; the observer runs synchronously on the writing goroutine and
// must not deadlock with config writes.
func SetSaveObserver(fn func(SaveEvent)) {
	saveObserverMu.Lock()
	saveObserver = fn
	saveObserverMu.Unlock()
}

func emitSaveEvent(ev SaveEvent) {
	saveObserverMu.Lock()
	fn := saveObserver
	saveObserverMu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

// --- atomic write + advisory lock -----------------------------------------

// writeFileAtomic replaces path with data via a same-directory temp file and
// rename: readers see the previous bytes or the new bytes, never a partial
// truncate-in-place write (the torn-read class behind #404's amplifier).
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// lockConfig takes the advisory cross-process writer lock for path (a stable
// sidecar file — the config file itself is rename-replaced beneath writers,
// so it cannot be the lock target). The returned release unlocks and closes;
// it is idempotent-safe to defer. Implemented per-OS in lock_*.go; the
// fallback build has no locking (writes remain atomic).
func lockConfig(path string) (release func(), err error) { return lockSidecar(path + ".lock") }

// writeConfigFileLocked assumes the caller holds the lock for path: when
// withBackup, rotates the backup from the current bytes (global scope —
// local overlays deliberately keep no backup, matching SaveLocalIn's
// contract), then atomically replaces the file and reports the event. Do NOT
// call without the lock held — and never call lockConfig for the same path
// twice in one goroutine (flock on a second descriptor blocks against the
// first).
func writeConfigFileLocked(path string, data []byte, kind string, withBackup bool) error {
	prev, _ := os.ReadFile(path)
	if withBackup {
		rotateConfigBackup(path, prev, data)
	}
	if err := writeFileAtomic(path, data); err != nil {
		emitSaveEvent(SaveEvent{Kind: "refused", Path: path, Bytes: len(data), Err: err})
		return err
	}
	emitSaveEvent(SaveEvent{Kind: kind, Path: path, Bytes: len(data)})
	return nil
}

// writeConfigFile locks, writes, unlocks.
func writeConfigFile(path string, data []byte, kind string, withBackup bool) error {
	release, err := lockConfig(path)
	if err != nil {
		emitSaveEvent(SaveEvent{Kind: "refused", Path: path, Bytes: len(data), Err: err})
		return err
	}
	defer release()
	return writeConfigFileLocked(path, data, kind, withBackup)
}

// configBackupRotationSuffix names the single kept predecessor of .bak
// (config.json.bak.1). N=1: enough to outlive one effective wipe, small
// enough to never become a graveyard.
const configBackupRotationSuffix = ".1"

// rotateConfigBackup snapshots prev — the bytes currently on disk that data
// is about to replace — into path's backup slot (#404: backup = previous
// content, never the new bytes). Skips: no previous bytes (greenfield),
// unparseable prev (never rotate corrupt bytes over the last-known-good
// copy), or an effectively-equal change (#405: bookkeeping-only diffs must
// not churn the backup).
func rotateConfigBackup(path string, prev, new []byte) {
	if len(prev) == 0 {
		return
	}
	var probe map[string]any
	if json.Unmarshal(prev, &probe) != nil || probe == nil {
		return
	}
	if configsEquivalent(prev, new) {
		return
	}
	bakPath := path + configBackupSuffix
	if _, err := os.Stat(bakPath); err == nil {
		_ = os.Rename(bakPath, bakPath+configBackupRotationSuffix)
	}
	_ = os.WriteFile(bakPath, prev, 0o600)
}

// --- bookkeeping patch -----------------------------------------------------

// bookkeepingPatchableKeys are the ONLY keys background write-backs may
// touch. Both are stamps the update check maintains on behalf of the user;
// neither changes what milk does. Everything else reaching disk goes through
// a full save from an explicit user action (#404 consent rule).
var bookkeepingPatchableKeys = map[string]bool{
	"update_last_check":      true,
	"update_skipped_version": true,
}

// PatchBookkeeping re-reads the config file in dir's active scope, sets the
// given whitelisted bookkeeping keys, and writes only if something actually
// changed — the surgical replacement for the snapshot write-back that raced
// every concurrent config edit (#404 "additional race"): a patch can only
// carry the file it just parsed, so an edit made during a long update check
// is never overwritten, and a corrupt or absent file means no write at all.
//
// Scope mirrors saveLocalOrGlobal: the project-local overlay when one exists
// (HasLocalConfigIn(dir)), otherwise the state root's config.json. Returns
// nil without writing when the file is absent (greenfield) or when the
// patch is a no-op.
func PatchBookkeeping(dir string, kv map[string]string) error {
	for k := range kv {
		if !bookkeepingPatchableKeys[k] {
			return fmt.Errorf("config: %q is not a bookkeeping key (patchable: update_last_check, update_skipped_version)", k)
		}
	}
	if len(kv) == 0 {
		return nil
	}
	path := ""
	localScope := false
	if HasLocalConfigIn(dir) {
		p, err := LocalConfigPathIn(dir)
		if err != nil {
			return err
		}
		path = p
		localScope = true
	} else {
		d, err := Dir()
		if err != nil {
			return err
		}
		path = filepath.Join(d, "config.json")
	}
	// Existence first, without the lock: a greenfield root has no directory
	// to lock in (and no file to damage). Writers replace the file via
	// rename, so the unlocked stat can never see a torn file — the locked
	// re-read below is the authoritative one.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil // greenfield: nothing to stamp, nothing to damage
	} else if err != nil {
		emitSaveEvent(SaveEvent{Kind: "refused", Path: path, Err: err})
		return err
	}
	release, err := lockConfig(path)
	if err != nil {
		emitSaveEvent(SaveEvent{Kind: "refused", Path: path, Err: err})
		return err
	}
	defer release()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil // vanished under us: greenfield now
	}
	if err != nil {
		emitSaveEvent(SaveEvent{Kind: "refused", Path: path, Err: err})
		return err
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil || m == nil {
		// Never write a patch over a file we cannot parse — a patch must not
		// become a backdoor around the persist guard.
		err := fmt.Errorf("config: refusing bookkeeping patch over unparsable %s", path)
		emitSaveEvent(SaveEvent{Kind: "refused", Path: path, Err: err})
		return err
	}
	changed := false
	for k, v := range kv {
		if cur, ok := m[k].(string); !ok || cur != v {
			m[k] = v
			changed = true
		}
	}
	if !changed {
		return nil
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeConfigFileLocked(path, out, "patched", !localScope)
}
