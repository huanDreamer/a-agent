package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/obs"
	"github.com/huan/huan-agent/internal/store"
)

var (
	usageLimit  int
	usageByProv bool
	usageCmd    = &cobra.Command{
		Use:   "usage",
		Short: "Show recent LLM usage from the local store (Phase 1 helper)",
		RunE:  runUsage,
	}
)

func init() {
	usageCmd.Flags().IntVar(&usageLimit, "limit", 20, "max number of records to show")
	rootCmd.AddCommand(usageCmd)
}

func runUsage(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger, err := obs.NewLogger(cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	st, err := store.Open(cmd.Context(), cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	recs, err := st.QueryUsage(context.Background(), store.UsageFilter{Limit: usageLimit})
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}

	if len(recs) == 0 {
		fmt.Println("no usage records yet")
		return nil
	}

	fmt.Printf("%-4s  %-19s  %-10s  %-15s  %6s %6s %6s  %7sms\n",
		"id", "time", "provider", "model", "prompt", "compl", "total", "dur")
	for _, r := range recs {
		fmt.Printf("%-4d  %-19s  %-10s  %-15s  %6d %6d %6d  %7d\n",
			r.ID, r.CreatedAt, r.Provider, r.Model,
			r.PromptTokens, r.CompletionTokens, r.TotalTokens, r.DurationMs)
	}
	logger.Debug("usage query complete", zap.Int("records", len(recs)))
	return nil
}
