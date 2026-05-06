package claude

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unsafe"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// runPTY runs Claude fully interactively with a PTY, but interposes a VT100
// emulator between the PTY output and the user's terminal. The Ink TUI chrome
// (cursor positioning, alternate screen, menus) is absorbed by the emulator;
// only clean text lines are forwarded to the user.
//
// Approval prompts are detected by scanning the virtual screen for known
// patterns and presenting a simple y/n prompt to the user.
func (a *Agent) runPTY(ctx context.Context, args []string) (ParseResult, error) {
	cmd := newCmd(ctx, a.bin, args)

	cols, rows := termSize()
	if cols > 0 {
		cmd.Env = append(os.Environ(), fmt.Sprintf("COLUMNS=%d", cols))
	}

	ptmx, err := pty.StartWithAttrs(cmd, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	}, setsidSysProcAttr())
	if err != nil {
		return ParseResult{}, err
	}
	defer ptmx.Close()

	// Disable echo on the PTY master so keystrokes aren't echoed twice
	// (our forwardInputWithEcho already handles local echo).
	disablePTYEcho(ptmx)

	// Raw mode: keystrokes pass through unmodified.
	fd := int(os.Stdin.Fd())
	if oldState, err := term.MakeRaw(fd); err == nil {
		defer term.Restore(fd, oldState) //nolint:errcheck
	}

	// Speaker label — matches milk's readline prompt style.
	os.Stdout.Write([]byte("\033[34m[claude]\033[0m > ")) //nolint:errcheck

	// Forward terminal resize to the PTY.
	winchC := make(chan os.Signal, 1)
	signal.Notify(winchC, syscall.SIGWINCH)
	defer signal.Stop(winchC)
	go func() {
		for range winchC {
			pty.InheritSize(os.Stdin, ptmx) //nolint:errcheck
		}
	}()

	// VT filter absorbs TUI chrome, emits plain text lines to stdout.
	vt := newVTFilter(cols, rows, os.Stdout)

	// PTY output → VT filter.
	go func() { io.Copy(vt, ptmx) }() //nolint:errcheck

	// User keystrokes → PTY, with local echo since raw mode disables terminal echo.
	go forwardInputWithEcho(ptmx, os.Stdin)

	cmd.Wait() //nolint:errcheck

	// Flush any remaining visible lines.
	vt.flush()

	// Collect final visible text for EndsWithQ / session text.
	lines := vt.Lines()
	text := strings.Join(lines, "\n")
	return ParseResult{
		Text:      text,
		EndsWithQ: strings.HasSuffix(strings.TrimSpace(text), "?"),
	}, nil
}

// forwardInputWithEcho reads from r byte by byte, writes each byte to ptmx,
// and echoes printable characters and backspace to stdout so the user can see
// what they're typing (raw mode disables terminal echo).
// Escape sequences (ESC + anything) are forwarded to ptmx but not echoed.
func forwardInputWithEcho(ptmx io.Writer, r io.Reader) {
	buf := make([]byte, 1)
	inEscape := false
	for {
		n, err := r.Read(buf)
		if n > 0 {
			ptmx.Write(buf[:n]) //nolint:errcheck
			inEscape = echoInputByte(buf[0], inEscape)
		}
		if err != nil {
			return
		}
	}
}

// echoInputByte echoes a single input byte to stdout if appropriate and
// returns the updated escape-sequence state. Escape sequences are forwarded
// to the PTY but suppressed from local echo to prevent raw codes appearing.
func echoInputByte(b byte, inEscape bool) bool {
	if b == 0x1b {
		return true // entering escape sequence
	}
	if inEscape {
		// End of sequence: final byte >= 0x40 that isn't a CSI/OSC introducer.
		return b < 0x40 || b == '[' || b == ']'
	}
	switch {
	case b == '\r' || b == '\n':
		os.Stdout.Write([]byte("\r\n")) //nolint:errcheck
	case b == 127 || b == 8:
		os.Stdout.Write([]byte("\b \b")) //nolint:errcheck
	case b >= 32 && b < 127:
		os.Stdout.Write([]byte{b}) //nolint:errcheck
	}
	return false
}

// disablePTYEcho turns off ECHO on the PTY master so keystrokes forwarded by
// forwardInputWithEcho aren't echoed a second time by the line discipline.
func disablePTYEcho(f *os.File) {
	var termios syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&termios))); errno != 0 {
		return
	}
	termios.Lflag &^= syscall.ECHO | syscall.ECHOE | syscall.ECHOK | syscall.ECHONL
	syscall.Syscall(syscall.SYS_IOCTL, //nolint:errcheck
		f.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&termios)))
}

// termSize returns the current terminal columns and rows.
func termSize() (cols, rows int) {
	r, c, err := pty.Getsize(os.Stdin)
	if err != nil {
		return 220, 50
	}
	return c, r
}
