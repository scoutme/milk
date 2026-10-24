package main

import (
	"fmt"
	"io"
	"os"
	"strings"
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

// colorize wraps s in code/ansiReset, closing the reset before any embedded
// newline rather than after it — an SGR reset placed after a newline byte
// leaves the color "open" for the terminal until some later, unrelated
// escape happens to cancel it. Blank lines stay unstyled.
func colorize(s, code string) string { return paintLines(s, code, false) }

// paintLines is colorize's implementation: every line of s becomes its own
// self-contained code…ansiReset span. styleBlank decides whether an empty line
// gets a code+reset pair too — the thinking surface needs it (a blank
// reasoning line is a row of the surface and must carry its background),
// plain colouring does not.
func paintLines(s, code string, styleBlank bool) string {
	if !isTTY || s == "" {
		return s
	}
	var out strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		body, hasNL := strings.CutSuffix(line, "\n")
		if body != "" || (hasNL && styleBlank) {
			out.WriteString(code)
			out.WriteString(body)
			out.WriteString(ansiReset)
		}
		if hasNL {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func green(s string) string      { return colorize(s, ansiGreen) }
func blue(s string) string       { return colorize(s, ansiBlue) }
func yellow(s string) string     { return colorize(s, ansiYellow) }
func red(s string) string        { return colorize(s, ansiRed) }
func dim(s string) string        { return colorize(s, dimCode()) }
func bold(s string) string       { return colorize(s, ansiBold) }
func boldYellow(s string) string { return colorize(s, "\033[1;33m") }
func boldGold(s string) string   { return colorize(s, accentStyle()) }

func dimLines(s string) string { return colorize(s, dimCode()) }

// milkTag returns the dimmed [milk] system prefix.
func milkTag() string { return dim("[milk]") }

// legacyAnnotationPrefix is the pre-#270 tool-activity manifest header —
// still present in sessions persisted before the [milk] standard landed.
const legacyAnnotationPrefix = "[Tool activity in that turn:"

// dimMilkAnnotationRows gives platform-annotation lines that arrive as PLAIN
// text the standard tag treatment. Milk writes its own [milk] annotations
// with the tag already dimmed (milkTag), but a model can echo the
// tool-activity manifest format into a reply — it sits in the model's
// replayed context (local.formatOwnTurn folds it onto old turns), so it
// reproduces the shape — and that echo lands in the transcript with a bright
// tag, glaring next to every real [milk] line. Rows here are the
// already-wrapped render (called from tintFullRows): a row that literally
// starts with an annotation gets its tag wrapped in the exact dim run
// milkTag emits, so the two are indistinguishable; rows carrying any other
// styling — including already-dim milkTag rows — are left untouched. Render
// only: the transcript buffer, /export and model context never see these
// escapes, and transcriptPlainLines strips them for selection and copy.
func dimMilkAnnotationRows(content string) string {
	if !strings.Contains(content, "[milk] ") && !strings.Contains(content, legacyAnnotationPrefix) {
		return content
	}
	rows := strings.Split(content, "\n")
	for i, row := range rows {
		switch {
		case strings.HasPrefix(row, "[milk] "):
			rows[i] = milkTag() + row[len("[milk]"):]
		case strings.HasPrefix(row, legacyAnnotationPrefix):
			rows[i] = dim(legacyAnnotationPrefix) + row[len(legacyAnnotationPrefix):]
		}
	}
	return strings.Join(rows, "\n")
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// pulseColors is the 16-step breathing gradient (theme.go: buildPulse); the
// original yellow ramp unless a theme is active. Peak at index 8.
var pulseColors = originalPulse

// pulse applies a cosine-eased breathing color effect keyed to the spinner frame counter.
func pulse(s string, frame int) string {
	if !isTTY {
		return s
	}
	return pulseColors[frame%len(pulseColors)] + s + ansiReset
}

// staleContentColor returns a truecolor ANSI code that grades from bright
// near-white (ratio=0, fresh) through amber (ratio≈0.5) to a muted orange-red
// (ratio≥1, fully stale). ratio is clamped to [0,1].
// No dim modifier is used — brightness is encoded in the RGB values so fresh
// bricks appear at full terminal brightness and age visibly as turns accumulate.
func staleContentColor(ratio float64) string {
	if !isTTY {
		return ansiDim
	}
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	if th.modern {
		// grey → dark grey: readable on a light ground, staleness reads as
		// the text settling into the page
		g := 150 + int(float64(58-150)*ratio)
		return fmt.Sprintf("\033[0;38;2;%d;%d;%dm", g, g-2, g-6)
	}
	// Truecolor gradient, no \033[2m dim modifier:
	//   ratio=0   → (220, 218, 215)  bright near-white  (full brightness, fresh)
	//   ratio=0.5 → (195, 130, 55)   amber              (warming, mid-age)
	//   ratio=1   → (175, 65,  40)   muted orange-red   (clearly stale)
	lerp := func(a, b int, t float64) int {
		return a + int(float64(b-a)*t)
	}
	var r, g, b int
	if ratio <= 0.5 {
		t := ratio * 2
		r = lerp(220, 195, t)
		g = lerp(218, 130, t)
		b = lerp(215, 55, t)
	} else {
		t := (ratio - 0.5) * 2
		r = lerp(195, 175, t)
		g = lerp(130, 65, t)
		b = lerp(55, 40, t)
	}
	return fmt.Sprintf("\033[0;38;2;%d;%d;%dm", r, g, b)
}

// stalenessLegend returns a single line showing "fresh" on the left and "stale"
// on the right, with every character coloured by its position in the gradient.
// The line is padded/trimmed to exactly inner display columns.
func stalenessLegend(inner int) string {
	if !isTTY || inner <= 0 {
		return ""
	}
	const left = "fresh"
	const right = "stale"
	// Fill: "fresh" + spaces + "stale", total = inner chars.
	fill := max(inner-len(left)-len(right), 0)
	runes := []rune(left + strings.Repeat("·", fill) + right)
	if len(runes) > inner {
		runes = runes[:inner]
	}
	var out strings.Builder
	for i, ch := range runes {
		ratio := float64(i) / float64(len(runes)-1)
		out.WriteString(colorize(string(ch), staleContentColor(ratio)))
	}
	return out.String()
}

func headerLogo(_ int) string {
	if !isTTY {
		return "milk"
	}
	return boldGold("milk")
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
