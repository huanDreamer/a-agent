// Command huan-agent is the entry point of the huan-agent binary.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/obs"
	"github.com/huan/huan-agent/internal/version"
)

const shutdownTimeout = 5 * time.Second

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
	serveCmd = &cobra.Command{
		Use:   "serve",
		Short: "Start the agent service (Phase 0 placeholder)",
		RunE:  runServe,
	}
)

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "", "path to config file (env: HUAN_CONFIG)")
	rootCmd.AddCommand(versionCmd, serveCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func runServe(cmd *cobra.Command, _ []string) error {
	// Honor HUAN_CONFIG env var if --config flag not set.
	if configPath == "" {
		configPath = os.Getenv("HUAN_CONFIG")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger, err := obs.NewLogger(cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	logger.Info("huan-agent starting",
		zap.String("version", version.Version),
		zap.String("commit", version.Commit),
		zap.String("build_time", version.BuildTime),
		zap.String("config_path", effectiveConfigPath()),
		zap.String("log_level", cfg.Logging.Level),
		zap.String("log_format", cfg.Logging.Format),
	)

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Phase 0 placeholder: no real services yet. We just wait for a signal.
	<-ctx.Done()
	logger.Info("shutdown signal received, cleaning up")

	// Run cleanup with a hard deadline. Future phases will register their own
	// cleanup functions in this block; for now, there's nothing to do.
	cleanupDone := make(chan struct{})
	go func() {
		// Placeholder for future cleanup work.
		close(cleanupDone)
	}()

	select {
	case <-cleanupDone:
		logger.Info("huan-agent stopped cleanly")
		return nil
	case <-time.After(shutdownTimeout):
		logger.Warn("cleanup exceeded shutdown timeout; forcing exit")
		return fmt.Errorf("cleanup timeout after %s", shutdownTimeout)
	}
}

func effectiveConfigPath() string {
	if configPath != "" {
		return configPath
	}
	return "<defaults>"
}
