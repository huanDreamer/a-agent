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
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
