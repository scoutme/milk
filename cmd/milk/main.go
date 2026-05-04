package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/scoutme/milk/internal/config"
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
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	_ = cfg // will be threaded through in subsequent steps

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

	_ = sess // will be threaded through in feat/state-machine
	fmt.Fprintf(os.Stderr, "milk: routing not yet implemented (session: %s, prompt: %q)\n", sess.ID, prompt)
	return nil
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
