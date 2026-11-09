package main

// The `milk plugins …` cobra face (ADR-0104 Part II): flag parsing and
// output only — every action dispatches into pluginsActionRun in
// plugins_cmd.go, the same layer the TUI /plugins suite uses.

import (
	"fmt"
	"github.com/spf13/cobra"
)

func pluginsUsage(format string, args ...any) error {
	return fmt.Errorf("usage: "+format, args...)
}

// pluginsActionRun executes one distribution subcommand and returns its
// report text (no trailing newline), whether the installed set changed
// (the TUI reloads after that), and the first error. Text may be non-empty
// alongside an error (a multi-name update that partially succeeded).

func newPluginsCmd() *cobra.Command {
	var ref, sha string
	opts := func() pluginsActionOpts { return pluginsActionOpts{ref: ref, sha: sha} }
	run := func(prefix []string) func(*cobra.Command, []string) error {
		return func(_ *cobra.Command, args []string) error {
			text, _, err := pluginsActionRun(append(append([]string{}, prefix...), args...), opts())
			if text != "" {
				fmt.Println(text)
			}
			return err
		}
	}

	cmd := &cobra.Command{Use: "plugins", Short: "Install and manage Agent Plugins"}

	add := &cobra.Command{
		Use:   "add <owner/repo | https://… | npm:pkg>",
		Short: "Install a plugin by reference (no clone)",
		RunE:  run([]string{"add"}),
	}
	add.Flags().StringVar(&ref, "ref", "", "branch or tag to fetch")
	add.Flags().StringVar(&sha, "sha", "", "commit pin to fetch")

	marketplace := &cobra.Command{Use: "marketplace", Short: "Plugin catalogs (marketplace.json interchange)"}
	mkAdd := &cobra.Command{
		Use:   "add <owner/repo | https://…>",
		Short: "Register a marketplace catalog",
		RunE:  run([]string{"marketplace", "add"}),
	}
	mkAdd.Flags().StringVar(&ref, "ref", "", "branch or tag to fetch")
	mkAdd.Flags().StringVar(&sha, "sha", "", "commit pin to fetch")
	marketplace.AddCommand(mkAdd, &cobra.Command{
		Use:   "list",
		Short: "List registered marketplaces",
		RunE:  run([]string{"marketplace", "list"}),
	})

	cmd.AddCommand(add, marketplace)
	cmd.AddCommand(
		&cobra.Command{
			Use:   "install <name>@<marketplace>",
			Short: "Install a plugin from a registered marketplace",
			RunE:  run([]string{"install"}),
		},
		&cobra.Command{
			Use:   "list",
			Short: "List installed plugins (bookkeeping + hand-installed)",
			RunE:  run([]string{"list"}),
		},
		&cobra.Command{
			Use:   "details <name>",
			Short: "Show an installed plugin's record and loaded components",
			RunE:  run([]string{"details"}),
		},
		&cobra.Command{
			Use:   "update [name]",
			Short: "Refetch installed plugins from their recorded sources",
			RunE:  run([]string{"update"}),
		},
		&cobra.Command{
			Use:   "remove <name>",
			Short: "Uninstall a recorded plugin (PLUGIN_DATA is preserved)",
			RunE:  run([]string{"remove"}),
		},
		&cobra.Command{
			Use:   "shim <dir>",
			Short: "Write a portable manifest into a .claude-plugin/ package (create-only)",
			RunE:  run([]string{"shim"}),
		},
	)
	return cmd
}

// pluginPaths are the state-root locations the distribution layer owns.
