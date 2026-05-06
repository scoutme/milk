package claude

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// approvalPatterns are substrings that appear in Claude's tool-approval prompt lines.
// Claude's Ink TUI renders these as styled menu items; the VT emulator extracts plain text.
var approvalPatterns = []string{
	"Allow once",
	"Allow always",
	"Allow for this session",
	"Deny",
	"bash",
	"Execute this command",
	"Edit file",
	"Write file",
	"Would you like to proceed",
	"approval required",
}

// looksLikeApprovalPrompt returns true if any visible screen line contains a
// known approval UI pattern.
func looksLikeApprovalPrompt(lines []string) bool {
	for _, line := range lines {
		for _, pat := range approvalPatterns {
			if strings.Contains(line, pat) {
				return true
			}
		}
	}
	return false
}

// waitForApprovalPrompt polls the vtFilter until an approval prompt appears
// (or the context deadline fires). When found it prints the relevant lines and
// asks the user for y/n via stdin. It writes the chosen byte to ptmxW.
//
// This is a best-effort mechanism: if the pattern doesn't match, we fall
// through and the user sees the raw lines anyway (they were already flushed
// by the vtFilter).
func waitForApprovalPrompt(vt *vtFilter, ptmxW io.Writer, userIn io.Reader) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		lines := vt.Lines()
		if looksLikeApprovalPrompt(lines) {
			printApprovalContext(lines)
			askUserApproval(ptmxW, userIn)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// printApprovalContext prints the approval-related lines to stdout so the
// user can see what Claude is asking permission for.
func printApprovalContext(lines []string) {
	fmt.Print("\r\n")
	for _, line := range lines {
		for _, pat := range approvalPatterns {
			if strings.Contains(line, pat) {
				fmt.Printf("  %s\r\n", line)
				break
			}
		}
	}
}

// askUserApproval prompts for y/n and sends the corresponding keypress to
// the PTY. Claude's menu is navigated with arrow keys + enter or direct y/n.
func askUserApproval(ptmxW io.Writer, userIn io.Reader) {
	fmt.Print("\r\napprove? [y/n] ")
	buf := make([]byte, 1)
	for {
		n, err := userIn.Read(buf)
		if err != nil || n == 0 {
			return
		}
		switch buf[0] {
		case 'y', 'Y':
			// Send Enter — selects the highlighted (first = Allow once) option.
			ptmxW.Write([]byte{'\r'}) //nolint:errcheck
			fmt.Print("y\r\n")
			return
		case 'n', 'N':
			// Send 'd' then Enter — moves to Deny and confirms.
			ptmxW.Write([]byte{'d', '\r'}) //nolint:errcheck
			fmt.Print("n\r\n")
			return
		}
	}
}
