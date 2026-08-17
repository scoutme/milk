package eval

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// progressHeartbeat is how often a non-TTY writer (redirected to a file, CI
// logs) gets a "still working" line during a single long phase. TTY output
// instead redraws a live spinner every tick — see run().
const progressHeartbeat = 15 * time.Second

var progressSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// progressReporter renders what the harness is doing right now — starting an
// adapter, waiting on an agent turn, judging a response — so a long phase
// (adapter startup up to 30s, a turn up to 5 minutes, an LLM judge call) never
// sits silent long enough to look stalled. Renders a live spinner + elapsed
// time on a TTY; falls back to periodic heartbeat log lines otherwise. Nil-safe:
// a nil *progressReporter no-ops so callers that don't want progress output
// (e.g. tests) can pass one in without a conditional at every call site.
type progressReporter struct {
	w     io.Writer
	isTTY bool

	mu         sync.Mutex
	label      string
	phaseStart time.Time
	nextBeat   time.Time

	// writeMu serializes all writes to w — run()'s ticker and Log/Logf calls
	// from the caller's goroutine race otherwise, corrupting the spinner line.
	writeMu       sync.Mutex
	lastRenderLen int

	stop    chan struct{}
	stopped chan struct{}
}

// newProgressReporter starts the reporter's render loop against w.
func newProgressReporter(w io.Writer) *progressReporter {
	p := &progressReporter{w: w, stop: make(chan struct{}), stopped: make(chan struct{})}
	if f, ok := w.(*os.File); ok {
		p.isTTY = term.IsTerminal(int(f.Fd()))
	}
	go p.run()
	return p
}

// Set announces a phase transition. Printed immediately on non-TTY writers so
// redirected logs see every transition without waiting for a heartbeat; on a
// TTY the next render tick picks it up.
func (p *progressReporter) Set(label string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.label = label
	p.phaseStart = time.Now()
	p.nextBeat = p.phaseStart.Add(progressHeartbeat)
	p.mu.Unlock()

	if !p.isTTY {
		p.writeMu.Lock()
		fmt.Fprintf(p.w, "  ... %s\n", label)
		p.writeMu.Unlock()
	}
}

// Setf is Set with fmt.Sprintf formatting.
func (p *progressReporter) Setf(format string, args ...any) {
	if p == nil {
		return
	}
	p.Set(fmt.Sprintf(format, args...))
}

// Stop halts the render loop and clears the current line (TTY only). Safe to
// call on a nil reporter.
func (p *progressReporter) Stop() {
	if p == nil {
		return
	}
	close(p.stop)
	<-p.stopped
}

func (p *progressReporter) run() {
	defer close(p.stopped)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	frame := 0
	for {
		select {
		case <-p.stop:
			p.writeMu.Lock()
			p.clearLine()
			p.writeMu.Unlock()
			return
		case <-tick.C:
			p.mu.Lock()
			label := p.label
			elapsed := time.Since(p.phaseStart)
			due := !p.nextBeat.IsZero() && time.Now().After(p.nextBeat)
			if due {
				p.nextBeat = time.Now().Add(progressHeartbeat)
			}
			p.mu.Unlock()

			if label == "" {
				continue
			}
			if p.isTTY {
				line := fmt.Sprintf("  %s %s (%s)", progressSpinnerFrames[frame%len(progressSpinnerFrames)], label, elapsed.Round(time.Second))
				p.writeMu.Lock()
				p.renderLine(line)
				p.writeMu.Unlock()
				frame++
			} else if due {
				p.writeMu.Lock()
				fmt.Fprintf(p.w, "  ... still on: %s (%s elapsed)\n", label, elapsed.Round(time.Second))
				p.writeMu.Unlock()
			}
		}
	}
}

// renderLine redraws the current line in place, padding over any leftover
// characters from a longer previous line. Caller holds writeMu.
func (p *progressReporter) renderLine(line string) {
	pad := ""
	if len(line) < p.lastRenderLen {
		pad = strings.Repeat(" ", p.lastRenderLen-len(line))
	}
	fmt.Fprintf(p.w, "\r%s%s", line, pad)
	p.lastRenderLen = len(line)
}

// clearLine blanks out the current spinner line. Caller holds writeMu.
func (p *progressReporter) clearLine() {
	if p.isTTY && p.lastRenderLen > 0 {
		fmt.Fprintf(p.w, "\r%s\r", strings.Repeat(" ", p.lastRenderLen))
		p.lastRenderLen = 0
	}
}

// Log prints a standalone line (e.g. "Running scenario: X", an error), first
// clearing any in-progress spinner rendering so the two don't interleave.
// Safe to call on a nil reporter — falls through to a plain stderr line so
// call sites don't need a conditional when progress reporting is disabled.
func (p *progressReporter) Log(line string) {
	if p == nil {
		fmt.Fprintln(os.Stderr, line)
		return
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.clearLine()
	fmt.Fprintln(p.w, line)
}

// Logf is Log with fmt.Sprintf formatting.
func (p *progressReporter) Logf(format string, args ...any) {
	if p == nil {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		return
	}
	p.Log(fmt.Sprintf(format, args...))
}
