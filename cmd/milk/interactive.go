package main

import (
	"fmt"
	"strings"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
)

const cmdEscalate = "/escalate"
const cmdLocal = "/local"
const cmdPaste = "/paste"

var slashCommands = []string{
	cmdEscalate, cmdLocal, cmdPaste, "/new", "/drop", "/list", "/help", "/exit", "/quit",
}

func promptLabel(sess *session.Session, forceEscalate, forceLocal bool) string {
	if forceEscalate {
		return blue("[claude]") + " > "
	}
	if forceLocal {
		return green("[local]") + " > "
	}
	switch sess.State {
	case session.StateClaudeWaiting:
		return yellow("[claude:waiting]") + " > "
	case session.StateClaude:
		return blue("[claude]") + " > "
	default:
		return green("[local]") + " > "
	}
}

const interactiveHelp = `Slash commands:
  /escalate   force next turn to Claude
  /local      force next turn to local model
  /new        start a fresh session
  /drop       delete current session
  /list       list sessions for current directory
  /help       show this help
  /exit       quit

Multi-line input:
  Shift+Enter   insert a newline (compose multi-line prompts)
  Paste         multi-line pastes are sent as a single block automatically

@ prefix:
  @path       reference a file path`

const errFmt = "error: %v\n"

// interactiveState holds mutable state for the interactive loop.
type interactiveState struct {
	sess          *session.Session
	forceEscalate bool
	forceLocal    bool
	cwd           string
	cfg           config.Config
}

// extractSlashCommand scans input for a known slash command token anywhere in
// the line. Returns the command, the remaining text with the token stripped,
// and whether a command was found.
func extractSlashCommand(input string) (cmd, rest string, found bool) {
	words := strings.Fields(input)
	var keep []string
	for _, w := range words {
		if !found && strings.HasPrefix(w, "/") {
			for _, known := range slashCommands {
				if w == known {
					cmd = w
					found = true
					break
				}
			}
			if !found {
				keep = append(keep, w)
			}
		} else {
			keep = append(keep, w)
		}
	}
	return cmd, strings.Join(keep, " "), found
}

// promptFriendly is the set of slash commands that can be combined with a prompt.
var promptFriendly = map[string]bool{
	cmdEscalate: true,
	cmdLocal:    true,
}

// handleSlashCommand processes a slash command with optional surrounding prompt text.
// Returns (exit, prompt-to-dispatch, output): exit=true means quit the loop,
// prompt is non-empty when the command should be followed by an immediate dispatch,
// output is text to print via tea.Println.
func handleSlashCommand(cmd, prompt string, st *interactiveState) (exit bool, dispatch, output string) {
	switch cmd {
	case "/exit", "/quit":
		return true, "", ""
	case "/help", "/new", "/drop", "/list", cmdPaste:
		output = execNonPromptCmd(cmd, prompt, st)
	case cmdEscalate:
		st.forceEscalate = true
		st.forceLocal = false
		if prompt == "" {
			output = milkTag() + " next turn: " + blue("Claude")
		}
		return false, prompt, output
	case cmdLocal:
		st.forceLocal = true
		st.forceEscalate = false
		if prompt == "" {
			output = milkTag() + " next turn: " + green("local model")
		}
		return false, prompt, output
	default:
		output = fmt.Sprintf("unknown command %q — type /help", cmd)
	}
	return false, "", output
}

// execNonPromptCmd runs a command that has no prompt semantics.
// Returns any output to be printed. Warns if the user included extra text.
func execNonPromptCmd(cmd, prompt string, st *interactiveState) string {
	var out strings.Builder
	if prompt != "" && !promptFriendly[cmd] {
		fmt.Fprintf(&out, "%s %s does not accept a prompt — text ignored\n", milkTag(), cmd)
	}
	switch cmd {
	case "/help":
		fmt.Fprint(&out, interactiveHelp)
	case "/new":
		var err error
		st.sess, err = session.New(st.cwd, "")
		if err != nil {
			fmt.Fprintf(&out, errFmt, err)
			return out.String()
		}
		fmt.Fprintf(&out, "%s new session %s", milkTag(), st.sess.ID[:8])
	case "/drop":
		if err := dropAndNewSession(st, &out); err != nil {
			fmt.Fprintf(&out, red("error: ")+"%v", err)
		}
	case "/list":
		if err := listSessions(st.cwd, &out); err != nil {
			fmt.Fprintf(&out, errFmt, err)
		}
	case cmdPaste:
		fmt.Fprint(&out, milkTag()+" hint: paste multi-line text directly, or use Shift+Enter to compose multi-line input")
	}
	return out.String()
}

// dropAndNewSession drops the current session, creates a fresh one, and writes output to w.
func dropAndNewSession(st *interactiveState, w *strings.Builder) error {
	id := st.sess.ID
	if err := session.Drop(id, st.cwd); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s dropped session %s\n", milkTag(), id[:8])
	var err error
	st.sess, err = session.New(st.cwd, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s new session %s", milkTag(), st.sess.ID[:8])
	return nil
}

// listSessions writes the session list for cwd to w.
func listSessions(cwd string, w *strings.Builder) error {
	entries, err := session.List(cwd)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprint(w, "no sessions found")
		return nil
	}
	for dir, list := range entries {
		fmt.Fprintf(w, "%s\n", dir)
		for _, e := range list {
			name := e.Name
			if name == "" {
				name = "(unnamed)"
			}
			fmt.Fprintf(w, "  %s  %-20s  %s", e.ID[:8], name, e.LastUsed.Format("2006-01-02 15:04"))
		}
	}
	return nil
}

func loadSession(cwd string, flagNew bool, flagSession string) (*session.Session, error) {
	if flagNew {
		return session.New(cwd, flagSession)
	}
	return session.Resume(cwd, flagSession)
}
