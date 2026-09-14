package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

const watcherPollInterval = 200 * time.Millisecond

// Watcher monitors a config file for changes and calls onChange whenever the
// file's modification time advances. It uses polling so it works on all
// platforms without a filesystem-event dependency.
//
// Usage:
//
//	w, err := config.NewWatcher(path, func(cfg config.Config, err error) { … })
//	// … later …
//	w.Close()
type Watcher struct {
	path        string
	onChange    func(Config, error)
	stop        chan struct{}
	once        sync.Once
	initialTime time.Time // mod time at creation; first poll skips times ≤ this
}

// NewWatcher starts watching path and calls onChange only when the file changes
// *after* the watcher is created. Changes that existed before NewWatcher was
// called are ignored. Close must be called to release resources.
func NewWatcher(path string, onChange func(Config, error)) (*Watcher, error) {
	// Prime with the current mod time so the first poll does not fire a
	// spurious "changed" event for a file that was already there at startup.
	var initialModTime time.Time
	if info, err := os.Stat(path); err == nil {
		initialModTime = info.ModTime()
	}
	w := &Watcher{
		path:        path,
		onChange:    onChange,
		stop:        make(chan struct{}),
		initialTime: initialModTime,
	}
	go w.poll()
	return w, nil
}

// poll is the background goroutine that detects file changes via os.Stat.
func (w *Watcher) poll() {
	lastModTime := w.initialTime

	ticker := time.NewTicker(watcherPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			info, err := os.Stat(w.path)
			if err != nil {
				if os.IsNotExist(err) {
					// File removed or not yet created — reset mod time so we detect
					// re-creation as a change.
					if !lastModTime.IsZero() {
						lastModTime = time.Time{}
					}
				}
				continue
			}
			if info.ModTime().After(lastModTime) {
				lastModTime = info.ModTime()
				cfg, parseErr := LoadFrom(w.path)
				w.onChange(cfg, parseErr)
			}
		}
	}
}

// Close stops the watcher. It is safe to call multiple times.
func (w *Watcher) Close() {
	w.once.Do(func() {
		close(w.stop)
	})
}

// DualWatcher monitors two config files (global and local) and calls onChange
// whenever either file's modification time advances. It re-merges both configs
// and passes the merged result to onChange. This is the primary watcher used by
// the TUI to support live-reload of both config sources.
type DualWatcher struct {
	globalPath string
	localPath  string
	onChange   func(Config, error)
	stop       chan struct{}
	once       sync.Once
}

// NewDualWatcher starts watching both globalPath and localPath. On any change
// to either file, it loads both, deep-merges (local over global), and calls
// onChange with the merged result. If localPath doesn't exist, the merge
// degenerates to global-only. Close must be called to release resources.
func NewDualWatcher(globalPath, localPath string, onChange func(Config, error)) (*DualWatcher, error) {
	dw := &DualWatcher{
		globalPath: globalPath,
		localPath:  localPath,
		onChange:   onChange,
		stop:       make(chan struct{}),
	}
	go dw.poll()
	return dw, nil
}

func (dw *DualWatcher) poll() {
	var globalMod, localMod time.Time

	// Prime initial mod times.
	if info, err := os.Stat(dw.globalPath); err == nil {
		globalMod = info.ModTime()
	}
	if info, err := os.Stat(dw.localPath); err == nil {
		localMod = info.ModTime()
	}

	ticker := time.NewTicker(watcherPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-dw.stop:
			return
		case <-ticker.C:
			changed := false

			if info, err := os.Stat(dw.globalPath); err == nil {
				if info.ModTime().After(globalMod) {
					globalMod = info.ModTime()
					changed = true
				}
			}
			if info, err := os.Stat(dw.localPath); err == nil {
				if info.ModTime().After(localMod) {
					localMod = info.ModTime()
					changed = true
				}
			} else if !localMod.IsZero() {
				// Local file was removed — reset and trigger reload so the
				// merged config falls back to global-only.
				localMod = time.Time{}
				changed = true
			}

			if changed {
				// Load both and merge, same as LoadWithLocal.
				cfg, parseErr := LoadFrom(dw.globalPath)
				if parseErr != nil {
					dw.onChange(cfg, parseErr)
					continue
				}
				if localData, err := os.ReadFile(dw.localPath); err == nil {
					local := defaults()
					if parseErr := json.Unmarshal(localData, &local); parseErr != nil {
						dw.onChange(cfg, fmt.Errorf("parsing local config %s: %w", dw.localPath, parseErr))
						continue
					}
					cfg = DeepMerge(cfg, local)
				}
				dw.onChange(cfg, nil)
			}
		}
	}
}

// Close stops the dual watcher. It is safe to call multiple times.
func (dw *DualWatcher) Close() {
	dw.once.Do(func() {
		close(dw.stop)
	})
}
