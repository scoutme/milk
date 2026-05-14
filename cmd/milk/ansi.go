package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

var isTTY = term.IsTerminal(int(os.Stdout.Fd()))

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBlue   = "\033[34m"
)

func colorize(s, code string) string {
	if !isTTY {
		return s
	}
	return code + s + ansiReset
}

func green(s string) string  { return colorize(s, ansiGreen) }
func blue(s string) string   { return colorize(s, ansiBlue) }
func yellow(s string) string { return colorize(s, ansiYellow) }
func red(s string) string    { return colorize(s, ansiRed) }
func dim(s string) string    { return colorize(s, ansiDim) }
func bold(s string) string   { return colorize(s, ansiBold) }

// milkTag returns the dimmed [milk] system prefix.
func milkTag() string { return dim("[milk]") }

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner prints an animated spinner on the current line until Stop is called.
// Designed to run after a label has been printed with no trailing newline.
// Stop is idempotent and safe to call multiple times.
type Spinner struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

const spinnerIdleThreshold = 300 * time.Millisecond
const ansiSpinnerErase = "\033[u " // restore cursor + overwrite spinner char

// activityWriter wraps a writer and drives a spinner: the spinner appears when
// no bytes have been written for spinnerIdleThreshold, and disappears the moment
// output resumes. All stdout access is serialised under mu. Call Done() when done.
type activityWriter struct {
	w         io.Writer
	mu        sync.Mutex
	lastWrite time.Time
	spinning  bool
	frame     int
	done      chan struct{}
	stopped   chan struct{}
}

func newActivityWriter(w io.Writer) *activityWriter {
	a := &activityWriter{
		w:       w,
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	// Only run the spinner goroutine when writing to a real terminal.
	f, isFile := w.(*os.File)
	if isFile && term.IsTerminal(int(f.Fd())) {
		go a.run()
	} else {
		close(a.stopped)
	}
	return a
}

func (a *activityWriter) run() {
	defer close(a.stopped)
	tick := time.NewTicker(80 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-a.done:
			a.mu.Lock()
			if a.spinning {
				fmt.Fprint(a.w, ansiSpinnerErase)
				a.spinning = false
			}
			a.mu.Unlock()
			return
		case <-tick.C:
			a.mu.Lock()
			idle := time.Since(a.lastWrite) >= spinnerIdleThreshold
			if idle && !a.spinning {
				fmt.Fprint(a.w, "\033[s") // save cursor
				a.spinning = true
			}
			if a.spinning {
				fmt.Fprintf(a.w, "\033[u%s", dim(spinnerFrames[a.frame%len(spinnerFrames)]))
				a.frame++
			}
			a.mu.Unlock()
		}
	}
}

func (a *activityWriter) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.spinning {
		fmt.Fprint(a.w, ansiSpinnerErase) // erase before output bytes land
		a.spinning = false
	}
	a.lastWrite = time.Now()
	return a.w.Write(p)
}

func (a *activityWriter) Done() {
	close(a.done)
	<-a.stopped
}
