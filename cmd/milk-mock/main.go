// milk-mock is a scriptable mock for milk integration testing.
// It provides two subcommands:
//
//	milk-mock server  — OpenAI-compatible SSE chat completions server
//	milk-mock claude  — drop-in claude --print --output-format stream-json mock
package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:          "milk-mock",
	Short:        "Scriptable mock provider for milk integration testing",
	SilenceUsage: true,
}

func init() {
	rootCmd.AddCommand(serverCmd)
	rootCmd.AddCommand(claudeCmd)
}
