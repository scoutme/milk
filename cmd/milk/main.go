package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/scoutme/milk/internal/agent/claude"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
)

var (
	flagEscalate   bool
	flagLocal      bool
	flagNew        bool
	flagSession    string
	flagContinue   bool
	flagList       bool
	flagListAll    bool
	flagDrop       bool
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "milk [flags] [prompt]",
	Short: "Local-first agentic orchestrator",
	Long: `milk routes prompts between a local LLM (Qwen2.5 via llama.cpp) and
Claude Code CLI, maintaining session state and supporting context promotion.`,
	Args:         cobra.ArbitraryArgs,
	SilenceUsage: true,
	RunE:         run,
}

func init() {
	rootCmd.Flags().BoolVar(&flagEscalate, "escalate", false, "Force route to Claude for this turn")
	rootCmd.Flags().BoolVar(&flagLocal, "local", false, "Force route to local model for this turn")
	rootCmd.Flags().BoolVar(&flagNew, "new", false, "Start a new session")
	rootCmd.Flags().StringVar(&flagSession, "session", "", "Target session by name")
	rootCmd.Flags().BoolVarP(&flagContinue, "continue", "c", false, "Resume current session (default behavior, explicit alias)")
	rootCmd.Flags().BoolVar(&flagList, "list", false, "List sessions for current cwd")
	rootCmd.Flags().BoolVar(&flagListAll, "all", false, "With --list: show all sessions across all directories")
	rootCmd.Flags().BoolVar(&flagDrop, "drop", false, "Delete the current session")

	rootCmd.AddCommand(configCmd)
}

func run(cmd *cobra.Command, args []string) error {
	if flagList {
		return runList(flagListAll)
	}
	if flagDrop {
		return runDrop()
	}

	prompt := strings.TrimSpace(strings.Join(args, " "))
	if prompt == "" {
		return fmt.Errorf("prompt required (interactive mode is not yet implemented)")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting cwd: %w", err)
	}

	var sess *session.Session
	if flagNew {
		sess, err = session.New(cwd, flagSession)
	} else {
		sess, err = session.Resume(cwd, flagSession)
	}
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	localAgent := local.New(cfg.LlamaURL, cfg.LlamaModel)
	claudeAgent := claude.New(cfg.ClaudeBin)

	// Probe availability once per invocation
	localAvail := localAgent.Ping(ctx) == nil
	claudeAvail := claudeAgent.Ping() == nil

	if !localAvail && !claudeAvail {
		return fmt.Errorf("neither llama.cpp nor claude CLI is available")
	}
	if !localAvail {
		fmt.Fprintln(os.Stderr, "[milk] warning: llama.cpp unreachable — routing all to Claude")
	}
	if !claudeAvail {
		fmt.Fprintln(os.Stderr, "[milk] warning: claude CLI unavailable — local only")
	}

	// Router uses nil localAgent when llama.cpp is down (skips classifier)
	var routeLocalAgent *local.Agent
	if localAvail {
		routeLocalAgent = localAgent
	}
	rtr := router.New(cfg, routeLocalAgent)

	decision, err := rtr.Route(ctx, sess, prompt, flagEscalate, flagLocal)
	if err != nil {
		return fmt.Errorf("routing: %w", err)
	}

	// Override target when an agent is unavailable
	target := decision.Target
	if target == router.TargetLocal && !localAvail {
		target = router.TargetClaude
	}
	if target == router.TargetClaude && !claudeAvail {
		target = router.TargetLocal
	}

	switch target {
	case router.TargetLocal:
		return runLocal(ctx, sess, localAgent, prompt)
	case router.TargetClaude:
		return runClaude(ctx, sess, claudeAgent, prompt)
	default:
		return fmt.Errorf("unknown routing target: %s", target)
	}
}

func runLocal(ctx context.Context, sess *session.Session, agent *local.Agent, prompt string) error {
	// Convert session history to local agent message format
	history := sessionToMessages(sess)

	sess.ForceState(session.StateLocal)
	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentLocal, Content: prompt})

	updatedHistory, err := agent.Run(ctx, history, prompt, os.Stdout)
	if err != nil {
		if esc, ok := err.(*local.EscalationSignal); ok {
			fmt.Fprintf(os.Stderr, "\n[milk] local model requested escalation: %s\n", esc.Reason)
			sess.ForceState(session.StateRouting)
			session.Save(sess) //nolint:errcheck
			// Re-run via Claude with existing context
			claudeAgent := claude.New("")
			return runClaude(ctx, sess, claudeAgent, prompt)
		}
		return err
	}

	// Record assistant response
	if len(updatedHistory) > 0 {
		last := updatedHistory[len(updatedHistory)-1]
		if last.Role == "assistant" {
			sess.AddTurn(session.Turn{
				Role:    session.RoleAssistant,
				Agent:   session.AgentLocal,
				Content: last.Content,
			})
		}
	}

	sess.ForceState(session.StateRouting)
	return session.Save(sess)
}

func runClaude(ctx context.Context, sess *session.Session, agent *claude.Agent, prompt string) error {
	// Capture state before we mutate it: only resume an existing Claude session
	// when we were explicitly waiting for user input mid-conversation.
	resuming := sess.State == session.StateClaudeWaiting && sess.ClaudeSessionID != ""

	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentClaude, Content: prompt})
	sess.ForceState(session.StateClaude)

	var res claude.ParseResult
	var err error

	if resuming {
		res, err = agent.RunResume(ctx, sess.ClaudeSessionID, prompt, os.Stdout)
		if err != nil {
			return err
		}
	} else {
		// New escalation: build context from full session history and start a new Claude session.
		// Always opens a new Claude session even if ClaudeSessionID is already set
		// (the old one may belong to a previous escalation chain).
		sysContext := escalation.BuildContext(sess)
		var claudeSessionID string
		claudeSessionID, res, err = agent.RunFirst(ctx, sysContext, prompt, os.Stdout)
		if err != nil {
			return err
		}
		sess.ClaudeSessionID = claudeSessionID
	}

	sess.AddTurn(session.Turn{
		Role:    session.RoleAssistant,
		Agent:   session.AgentClaude,
		Content: res.Text,
	})

	if res.EndsWithQ {
		sess.ForceState(session.StateClaudeWaiting)
	} else {
		sess.ForceState(session.StateRouting)
	}

	return session.Save(sess)
}

// sessionToMessages converts local-agent session turns to the local agent's Message format.
// Claude turns are excluded: the local model should only see its own prior conversation.
func sessionToMessages(sess *session.Session) []local.Message {
	msgs := []local.Message{}
	for _, t := range sess.History {
		if t.Agent != session.AgentLocal {
			continue
		}
		switch t.Role {
		case session.RoleUser:
			msgs = append(msgs, local.Message{Role: "user", Content: t.Content})
		case session.RoleAssistant:
			msgs = append(msgs, local.Message{Role: "assistant", Content: t.Content})
		case session.RoleToolResult:
			msgs = append(msgs, local.Message{Role: "tool", Content: t.Content})
		}
	}
	return msgs
}

func runList(all bool) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting cwd: %w", err)
	}
	target := cwd
	if all {
		target = ""
	}
	entries, err := session.List(target)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("no sessions found")
		return nil
	}
	for dir, list := range entries {
		fmt.Printf("%s\n", dir)
		for _, e := range list {
			name := e.Name
			if name == "" {
				name = "(unnamed)"
			}
			fmt.Printf("  %s  %-20s  %s\n", e.ID[:8], name, e.LastUsed.Format("2006-01-02 15:04"))
		}
	}
	return nil
}

func runDrop() error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting cwd: %w", err)
	}
	sess, err := session.Resume(cwd, flagSession)
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
	if err := session.Drop(sess.ID, cwd); err != nil {
		return err
	}
	fmt.Printf("dropped session %s\n", sess.ID[:8])
	return nil
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Print effective configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		fmt.Printf("llama_url:      %s\n", cfg.LlamaURL)
		fmt.Printf("llama_model:    %s\n", cfg.LlamaModel)
		fmt.Printf("claude_bin:     %s\n", cfg.ClaudeBin)
		fmt.Printf("default_route:  %s\n", cfg.DefaultRoute)
		fmt.Printf("escalate_above_tokens: %d\n", cfg.Rules.EscalateAboveTokens)
		fmt.Printf("escalate_keywords:     %v\n", cfg.Rules.EscalateKeywords)
		return nil
	},
}
