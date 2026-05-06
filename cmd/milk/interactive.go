package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chzyer/readline"

	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
)

var slashCommands = []string{
	"/escalate", "/local", "/new", "/drop", "/list", "/help", "/exit", "/quit",
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

// milkPainter colorizes /commands and @paths as the user types.
type milkPainter struct{}

func (p *milkPainter) Paint(line []rune, _ int) []rune {
	if !isTTY {
		return line
	}
	s := string(line)
	var out strings.Builder
	for _, token := range strings.Fields(s) {
		switch {
		case strings.HasPrefix(token, "/"):
			out.WriteString(ansiYellow + token + ansiReset)
		case strings.HasPrefix(token, "@"):
			out.WriteString(ansiDim + token + ansiReset)
		default:
			out.WriteString(token)
		}
		out.WriteByte(' ')
	}
	result := strings.TrimRight(out.String(), " ")
	// Preserve trailing space if the original line had one
	if len(s) > 0 && s[len(s)-1] == ' ' {
		result += " "
	}
	return []rune(result)
}

// milkCompleter implements readline.AutoCompleter for / commands and @ file paths.
type milkCompleter struct {
	cwd string
}

func (c *milkCompleter) Do(line []rune, pos int) ([][]rune, int) {
	word := lastWord(string(line[:pos]))
	switch {
	case strings.HasPrefix(word, "/"):
		return completeSlash(word), len([]rune(word))
	case strings.HasPrefix(word, "@"):
		return c.completePath(word), len([]rune(word))
	}
	return nil, 0
}

func completeSlash(word string) [][]rune {
	var out [][]rune
	for _, cmd := range slashCommands {
		if strings.HasPrefix(cmd, word) {
			out = append(out, []rune(cmd[len(word):]))
		}
	}
	return out
}

func (c *milkCompleter) completePath(word string) [][]rune {
	partial := word[1:]
	dir, prefix := filepath.Split(partial)
	entries, err := os.ReadDir(filepath.Join(c.cwd, dir))
	if err != nil {
		return nil
	}
	var out [][]rune
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		candidate := "@" + dir + name
		if e.IsDir() {
			candidate += "/"
		}
		out = append(out, []rune(candidate[len(word):]))
	}
	return out
}

// lastWord returns the last whitespace-delimited token in s.
func lastWord(s string) string {
	if i := strings.LastIndexAny(s, " \t"); i >= 0 {
		return s[i+1:]
	}
	return s
}

const interactiveHelp = `Slash commands:
  /escalate   force next turn to Claude
  /local      force next turn to local model
  /new        start a fresh session
  /drop       delete current session
  /list       list sessions for current directory
  /help       show this help
  /exit       quit

@ prefix:
  @path       reference a file path (Tab-completes from cwd)`

const errFmt = "error: %v\n"

// interactiveState holds mutable state for the interactive loop.
type interactiveState struct {
	sess          *session.Session
	forceEscalate bool
	forceLocal    bool
	cwd           string
}

// handleSlashCommand processes a /command input. Returns (exit, err).
func handleSlashCommand(input string, st *interactiveState) (exit bool, err error) {
	cmd, _, _ := strings.Cut(input, " ")
	switch cmd {
	case "/exit", "/quit":
		return true, nil
	case "/help":
		fmt.Println(interactiveHelp)
	case "/new":
		st.sess, err = session.New(st.cwd, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, errFmt, err)
			return false, nil
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
	case "/escalate":
		st.forceEscalate = true
		st.forceLocal = false
		fmt.Println(milkTag() + " next turn: " + blue("Claude"))
	case "/local":
		st.forceLocal = true
		st.forceEscalate = false
		fmt.Println(milkTag() + " next turn: " + green("local model"))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q — type /help\n", cmd)
	}
	return false, nil
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

// dispatchAgents holds the agents and their availability for dispatchTurn.
type dispatchAgents struct {
	local       *local.Agent
	claude      *claude.Agent
	localAvail  bool
	claudeAvail bool
}

// dispatchTurn routes a regular prompt to the appropriate agent.
func dispatchTurn(
	ctx context.Context,
	st *interactiveState,
	rtr *router.Router,
	agents dispatchAgents,
	input string,
) {
	localAgent := agents.local
	claudeAgent := agents.claude
	localAvail := agents.localAvail
	claudeAvail := agents.claudeAvail
	turnCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	decision, routeErr := rtr.Route(turnCtx, st.sess, input, st.forceEscalate, st.forceLocal)
	if routeErr != nil {
		fmt.Fprintf(os.Stderr, "routing error: %v\n", routeErr)
		return
	}
	st.forceEscalate = false
	st.forceLocal = false

	target := decision.Target
	if target == router.TargetLocal && !localAvail {
		target = router.TargetClaude
	}
	if target == router.TargetClaude && !claudeAvail {
		target = router.TargetLocal
	}

	switch target {
	case router.TargetLocal:
		if err := runLocal(turnCtx, st.sess, localAgent, input); err != nil {
			fmt.Fprintf(os.Stderr, errFmt, err)
		}
	case router.TargetClaude:
		if err := runClaude(turnCtx, st.sess, claudeAgent, input); err != nil {
			fmt.Fprintf(os.Stderr, errFmt, err)
		}
	}
	fmt.Println()
}

func loadSession(cwd string, flagNew bool, flagSession string) (*session.Session, error) {
	if flagNew {
		return session.New(cwd, flagSession)
	}
	return session.Resume(cwd, flagSession)
}

func handlePromptError(err error, st *interactiveState) (stop bool) {
	if err == readline.ErrInterrupt {
		if st.forceEscalate || st.forceLocal {
			st.forceEscalate = false
			st.forceLocal = false
			fmt.Println(milkTag() + " mode cleared")
			return false
		}
		fmt.Println()
		return true
	}
	// io.EOF = Ctrl-D, or any other error: exit cleanly
	fmt.Println()
	return true
}

func runInteractive(cfg config.Config, cwd string, initialFlagNew bool, initialFlagSession string) error {
	sess, err := loadSession(cwd, initialFlagNew, initialFlagSession)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}

	localAgent := local.New(cfg.LlamaURL, cfg.LlamaModel)
	claudeAgent := claude.New(cfg.ClaudeBin)

	ctx := context.Background()
	localAvail, claudeAvail, err := checkAgentAvailability(ctx, localAgent, claudeAgent)
	if err != nil {
		return err
	}

	var routeLocalAgent *local.Agent
	if localAvail {
		routeLocalAgent = localAgent
	}
	rtr := router.New(cfg, routeLocalAgent)

	fmt.Printf("%s interactive mode — session %s  (type /help for commands)\n", milkTag(), sess.ID[:8])

	st := &interactiveState{sess: sess, cwd: cwd}
	agents := dispatchAgents{localAgent, claudeAgent, localAvail, claudeAvail}

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          promptLabel(sess, false, false),
		AutoComplete:    &milkCompleter{cwd: cwd},
		Painter:         &milkPainter{},
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
		HistoryLimit:    500,
	})
	if err != nil {
		return fmt.Errorf("readline init: %w", err)
	}
	defer rl.Close()

	for {
		if stop := runInteractiveStep(ctx, rl, st, rtr, agents); stop {
			return nil
		}
	}
}

// runInteractiveStep handles a single prompt/response cycle.
// Returns true if the interactive loop should stop.
func runInteractiveStep(ctx context.Context, rl *readline.Instance, st *interactiveState, rtr *router.Router, agents dispatchAgents) bool {
	rl.SetPrompt(promptLabel(st.sess, st.forceEscalate, st.forceLocal))
	input, err := rl.Readline()
	fmt.Println()
	if err != nil {
		if err == io.EOF {
			fmt.Println()
			return true
		}
		return handlePromptError(err, st)
	}

	input = strings.TrimSpace(input)
	if input == "" {
		return false
	}

	if strings.HasPrefix(input, "/") {
		exit, _ := handleSlashCommand(input, st)
		return exit
	}

	dispatchTurn(ctx, st, rtr, agents, input)
	return false
}
