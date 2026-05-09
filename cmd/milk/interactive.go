package main

import (
	"fmt"
	"os"
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
// Returns (exit, prompt-to-dispatch): exit=true means quit the loop,
// prompt is non-empty when the command should be followed by an immediate dispatch.
func handleSlashCommand(cmd, prompt string, st *interactiveState) (exit bool, dispatch string) {
	switch cmd {
	case "/exit", "/quit":
		return true, ""
	case "/help", "/new", "/drop", "/list", cmdPaste:
		execNonPromptCmd(cmd, prompt, st)
	case cmdEscalate:
		st.forceEscalate = true
		st.forceLocal = false
		if prompt == "" {
			fmt.Println(milkTag() + " next turn: " + blue("Claude"))
		}
		return false, prompt
	case cmdLocal:
		st.forceLocal = true
		st.forceEscalate = false
		if prompt == "" {
			fmt.Println(milkTag() + " next turn: " + green("local model"))
		}
		return false, prompt
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q — type /help\n", cmd)
	}
	return false, ""
}

// execNonPromptCmd runs a command that has no prompt semantics.
// Warns if the user included extra text alongside the command.
func execNonPromptCmd(cmd, prompt string, st *interactiveState) {
	if prompt != "" && !promptFriendly[cmd] {
		fmt.Fprintf(os.Stderr, "%s %s does not accept a prompt — text ignored\n", milkTag(), cmd)
	}
	switch cmd {
	case "/help":
		fmt.Println(interactiveHelp)
	case "/new":
		var err error
		st.sess, err = session.New(st.cwd, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, errFmt, err)
			return
		}
		fmt.Printf("%s new session %s\n", milkTag(), st.sess.ID[:8])
	case "/drop":
		if err := dropAndNewSession(st); err != nil {
			fmt.Fprintf(os.Stderr, red("error: ")+"%v\n", err)
		}
	case "/list":
		if err := runList(false); err != nil {
			fmt.Fprintf(os.Stderr, errFmt, err)
		}
	case cmdPaste:
		fmt.Println(milkTag() + " hint: paste multi-line text directly, or use Shift+Enter to compose multi-line input")
	}
}

// dropAndNewSession drops the current session and creates a fresh one.
func dropAndNewSession(st *interactiveState) error {
	id := st.sess.ID
	if err := session.Drop(id, st.cwd); err != nil {
		return err
	}
	fmt.Printf("%s dropped session %s\n", milkTag(), id[:8])
	var err error
	st.sess, err = session.New(st.cwd, "")
	if err != nil {
		return err
	}
	fmt.Printf("%s new session %s\n", milkTag(), st.sess.ID[:8])
	return nil
}

func loadSession(cwd string, flagNew bool, flagSession string) (*session.Session, error) {
	if flagNew {
		return session.New(cwd, flagSession)
	}
	return session.Resume(cwd, flagSession)
}
