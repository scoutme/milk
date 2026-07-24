package config

import (
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

