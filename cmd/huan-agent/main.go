// Command huan-agent is the entry point of the huan-agent binary.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/huan/huan-agent/internal/version"
)

var (
	configPath string
	rootCmd    = &cobra.Command{
		Use:           "huan-agent",
		Short:         "Personal AI Agent platform (Eino-based)",
		Long:          "huan-agent is a personal AI Agent platform that integrates with IM platforms (Feishu), supports MCP tools and Skills, and manages LLM context and usage.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.String(),
	}
	versionCmd = &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Println(version.String())
		},
	}
)

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "", "path to config file (env: HUAN_CONFIG)")
	rootCmd.AddCommand(versionCmd)
}

func main() {
	err := rootCmd.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
	}

	// Language servers are child processes. Stopping them here rather than in
	// each command means no subcommand can forget: one left behind holds locks on
	// the module cache, and the next run pays for it.
	closeLanguageServers()

	if err != nil {
		// A command that knows which exit code its failure means says so through
		// exitCodeFor; everything else is a plain failure. `run` is the reason
		// this exists: a script has to be able to tell "the model failed" from
		// "the budget ran out", and both from "it worked".
		os.Exit(exitCodeFor(err))
	}
}
