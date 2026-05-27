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
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[2m"
	ansiItalic  = "\033[3m"
	ansiRed     = "\033[31m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiBlue    = "\033[34m"
	ansiMagenta = "\033[35m"
	ansiCyan    = "\033[36m"
)

func colorize(s, code string) string {
	if !isTTY {
		return s
	}
	return code + s + ansiReset
}

func green(s string) string      { return colorize(s, ansiGreen) }
func blue(s string) string       { return colorize(s, ansiBlue) }
func yellow(s string) string     { return colorize(s, ansiYellow) }
func red(s string) string        { return colorize(s, ansiRed) }
func dim(s string) string        { return colorize(s, ansiDim) }
func bold(s string) string       { return colorize(s, ansiBold) }
func boldYellow(s string) string { return colorize(s, "\033[1;33m") }

// milkTag returns the dimmed [milk] system prefix.
func milkTag() string { return dim("[milk]") }

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// pulseColors is a 16-step cosine-eased yellow breathing gradient using truecolor.
// Cycles from near-black yellow (dim) to bright yellow (peak) and back, giving a
// smooth sine-wave brightness curve that ANSI dim/bold levels cannot approximate.
// Advances every spinner tick (80 ms/step → ~1.28 s per full breath).
var pulseColors = [16]string{
	"\033[38;2;60;50;0m",
	"\033[38;2;67;58;1m",
	"\033[38;2;81;73;4m",
	"\033[38;2;110;101;9m",
	"\033[38;2;157;135;15m",
	"\033[38;2;204;170;20m",
	"\033[38;2;232;197;25m",
	"\033[38;2;251;215;28m",
	"\033[38;2;255;220;30m",
	"\033[38;2;251;215;28m",
	"\033[38;2;232;197;25m",
	"\033[38;2;204;170;20m",
	"\033[38;2;157;135;15m",
	"\033[38;2;110;101;9m",
	"\033[38;2;81;73;4m",
	"\033[38;2;67;58;1m",
}

// pulse applies a cosine-eased breathing color effect keyed to the spinner frame counter.
func pulse(s string, frame int) string {
	if !isTTY {
		return s
	}
	return pulseColors[frame%len(pulseColors)] + s + ansiReset
}

// logoMark is the diamond glyph used in the header.
const logoMark = "◈"

// headerLogo returns the styled logo string for the persistent header bar.
// The ◈ mark pulses through the breathing gradient keyed to frame.
// Use frame=8 (peak brightness) for a static appearance when idle.
// Layout: pulsing ◈ · bold "milk" in bright gold
func headerLogo(frame int) string {
	if !isTTY {
		return logoMark + " milk"
	}
	mark := pulseColors[frame%len(pulseColors)] + logoMark + ansiReset
	// "milk" rendered in bold bright-gold (#FFD060) to match peak pulse color
	name := "\033[1;38;2;255;208;60m" + "milk" + ansiReset
	return mark + " " + name
}

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
				fmt.Fprintf(a.w, "\033[u%s", yellow(spinnerFrames[a.frame%len(spinnerFrames)]))
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
