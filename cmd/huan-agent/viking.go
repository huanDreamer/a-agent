package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/documents"
	"github.com/huan/huan-agent/internal/memory"
	"github.com/huan/huan-agent/internal/obs"
	"github.com/huan/huan-agent/internal/server"
	"github.com/huan/huan-agent/internal/viking"
)

// newVikingService builds the OpenViking integration, or returns nil when it is
// switched off.
//
// A nil service is the normal case for an install that does not use OpenViking,
// so every caller treats "nil" as "feature off" rather than as an error — and a
// service that cannot be built (a bad URL) is reported once, at startup, rather
// than at the first memory write.
func newVikingService(cfg *config.Config, logger *zap.Logger) (*viking.Service, error) {
	if !cfg.OpenViking.Enabled() {
		return nil, nil
	}
	svc, err := viking.New(cfg.OpenViking, cfg.Tools, logger)
	if err != nil {
		if errors.Is(err, viking.ErrDisabled) {
			return nil, nil
		}
		return nil, fmt.Errorf("init openviking: %w", err)
	}
	return svc, nil
}

// buildMemoryStore opens the long-term memory store and wraps it with the
// OpenViking mirror when that is configured.
//
// The returned closer is owned by the process, not by a session: the mirror
// batches turns across sessions, so closing it per session would cut batches in
// half.
func buildMemoryStore(cfg *config.Config, svc *viking.Service, logger *zap.Logger) (memory.Store, func(), error) {
	if !cfg.Memory.Enable || cfg.Memory.Dir == "" {
		return nil, func() {}, nil
	}
	local, err := memory.NewStore(cfg.Memory.Dir)
	if err != nil {
		return nil, nil, fmt.Errorf("open memory store: %w", err)
	}
	if svc == nil {
		return local, func() { _ = local.Close() }, nil
	}
	store := svc.WrapStore(local)
	return store, func() { _ = store.Close() }, nil
}

// startWorkspaceSyncLoop runs an incremental workspace sync every interval
// until ctx is cancelled.
//
// The first sync waits one interval rather than running immediately, so a
// process restart loop cannot turn into a sync storm, and a failing sync is
// logged and retried on the next tick instead of stopping the loop.
func startWorkspaceSyncLoop(ctx context.Context, svc *viking.Service, interval time.Duration, logger *zap.Logger) {
	logger.Info("openviking workspace sync loop started",
		zap.Duration("interval", interval), zap.String("workspace", svc.WorkspaceDir()))
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				syncCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
				rep, err := svc.SyncWorkspace(syncCtx, false)
				cancel()
				if err != nil {
					logger.Warn("openviking workspace sync failed", zap.Error(err))
					continue
				}
				if rep.Uploaded > 0 || rep.Failed > 0 {
					logger.Info("openviking workspace sync",
						zap.Int("uploaded", rep.Uploaded),
						zap.Int("unchanged", rep.Unchanged),
						zap.Int("failed", rep.Failed))
				}
			}
		}
	}()
}

// syncWorkspaceOnExit publishes the workspace when an interactive chat session
// ends, so the files a conversation produced are stored without anyone asking.
//
// It runs incrementally and reports a failure as a warning: the session is over
// and the user is waiting for their shell back, which is not the moment to fail
// with an error they cannot act on.
func syncWorkspaceOnExit(svc *viking.Service, logger *zap.Logger) {
	if svc == nil || svc.Documents() == nil || svc.WorkspaceDir() == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fmt.Println("syncing workspace to OpenViking ...")
	rep, err := svc.SyncWorkspace(ctx, false)
	if err != nil {
		logger.Warn("openviking workspace sync on exit failed", zap.Error(err))
		fmt.Fprintln(os.Stderr, "workspace sync:", err)
		return
	}
	fmt.Printf("workspace sync: uploaded=%d unchanged=%d skipped=%d failed=%d\n",
		rep.Uploaded, rep.Unchanged, rep.Skipped, rep.Failed)
}

// vikingConsole adapts the service to the console's interface, preserving nil.
//
// The conversion is deliberate rather than implicit: a nil *viking.Service
// stored in the interface would satisfy it and then panic on the first call,
// while a nil interface is the "not configured" state the routes expect.
func vikingConsole(svc *viking.Service) server.OpenVikingConsole {
	if svc == nil {
		return nil
	}
	return svc
}

var (
	vikingFull   bool
	vikingJSON   bool
	vikingTitle  string
	vikingFile   string
	vikingBody   string
	vikingTags   string
	vikingSource string
	vikingLimit  int

	vikingCmd = &cobra.Command{
		Use:   "viking",
		Short: "Talk to the OpenViking context database directly",
		Long: `Inspect and write to the OpenViking server huan-agent is configured to use.

Examples:
  # Connection, memory and document state
  huan-agent viking status

  # Synchronize the configured workspace into OpenViking
  huan-agent viking sync

  # Save a document from a file
  huan-agent viking save --title "Design notes" --file notes.md --tags design

  # Record a durable fact
  huan-agent viking remember "The deployment owner is Alice"

  # Semantic search over everything this agent has stored
  huan-agent viking search "deployment owner"`,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	vikingStatusCmd = &cobra.Command{
		Use:   "status",
		Short: "Show the OpenViking connection and what has been stored",
		RunE:  runVikingStatus,
	}

	vikingSyncCmd = &cobra.Command{
		Use:   "sync",
		Short: "Synchronize the workspace directory into OpenViking",
		RunE:  runVikingSync,
	}

	vikingSaveCmd = &cobra.Command{
		Use:   "save",
		Short: "Save a document into OpenViking",
		RunE:  runVikingSave,
	}

	vikingRememberCmd = &cobra.Command{
		Use:   "remember <text>",
		Short: "Record a durable fact in OpenViking memory",
		Args:  cobra.MinimumNArgs(1),
		RunE:  runVikingRemember,
	}

	vikingSearchCmd = &cobra.Command{
		Use:   "search <query>",
		Short: "Semantic search over the agent's subtree",
		Args:  cobra.MinimumNArgs(1),
		RunE:  runVikingSearch,
	}
)

func init() {
	vikingSyncCmd.Flags().BoolVar(&vikingFull, "full", false, "ignore the local state file and re-upload every file")
	vikingStatusCmd.Flags().BoolVar(&vikingJSON, "json", false, "print machine-readable JSON")
	vikingSaveCmd.Flags().StringVar(&vikingTitle, "title", "", "document title (required)")
	vikingSaveCmd.Flags().StringVar(&vikingFile, "file", "", "read the content from this file")
	vikingSaveCmd.Flags().StringVar(&vikingBody, "content", "", "use this content instead of --file")
	vikingSaveCmd.Flags().StringVar(&vikingTags, "tags", "", "comma-separated tags")
	vikingSaveCmd.Flags().StringVar(&vikingSource, "source", "cli", "where the document came from")
	vikingSearchCmd.Flags().IntVar(&vikingLimit, "limit", 10, "maximum number of hits")

	vikingCmd.AddCommand(vikingStatusCmd, vikingSyncCmd, vikingSaveCmd, vikingRememberCmd, vikingSearchCmd)
	rootCmd.AddCommand(vikingCmd)
}

// vikingRuntime loads config, builds a logger and the service, and refuses to
// continue when the integration is off — with the reason, so an operator knows
// which switch to flip.
func vikingRuntime() (*config.Config, *viking.Service, *zap.Logger, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load config: %w", err)
	}
	logger, err := obs.NewLogger(cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("init logger: %w", err)
	}
	if !cfg.OpenViking.Enabled() {
		return nil, nil, nil, errors.New(
			"openviking is not enabled: set openviking.enable: true (and openviking.base_url) in the config, " +
				"or export HUAN_OPENVIKING_ENABLE=true")
	}
	svc, err := newVikingService(cfg, logger)
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, svc, logger, nil
}

func runVikingStatus(cmd *cobra.Command, _ []string) error {
	_, svc, _, err := vikingRuntime()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
	defer cancel()
	st := svc.Status(ctx)
	if vikingJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	printVikingStatus(st)
	return nil
}

// printVikingStatus renders the status for a human.
func printVikingStatus(st viking.Status) {
	fmt.Printf("OpenViking  %s  (account=%s user=%s)\n", st.BaseURL, st.Account, st.User)
	if st.Connected {
		fmt.Printf("  connection      ok (version %s, auth %s)\n", st.Version, st.AuthMode)
	} else {
		fmt.Printf("  connection      FAILED: %s\n", st.HealthError)
	}
	fmt.Printf("  subtree         %s\n", st.Subtree)

	ms := st.Memory
	if !ms.Enable {
		fmt.Println("  memory          disabled (openviking.memory.enable = false)")
	} else {
		stats := ms.Stats
		fmt.Printf("  memory          submitted=%d facts=%d pending=%d failures=%d\n",
			stats.Submitted, stats.Facts, stats.Pending, stats.Failures)
		if stats.LastError != "" {
			fmt.Printf("  memory last err %s\n", stats.LastError)
			if stats.LastErrorAt != nil {
				fmt.Printf("                  at %s\n", stats.LastErrorAt.Format(time.RFC3339))
			}
		}
	}

	ds := st.Documents
	if !ds.Enable {
		fmt.Println("  documents       disabled (openviking.documents.enable = false)")
	} else {
		fmt.Printf("  documents       root=%s tracked=%d\n", ds.RootURI, ds.Tracked)
		if ds.WorkspaceDir == "" {
			fmt.Println("  workspace       not configured (openviking.documents.workspace_dir / tools.workspace)")
		} else {
			fmt.Printf("  workspace       %s\n", ds.WorkspaceDir)
		}
		if ds.LastSync != nil {
			r := ds.LastSync
			fmt.Printf("  last sync       %s uploaded=%d unchanged=%d skipped=%d failed=%d\n",
				ds.LastSyncAt.Format(time.RFC3339), r.Uploaded, r.Unchanged, r.Skipped, r.Failed)
			for _, e := range r.Errors {
				fmt.Printf("                  ! %s\n", e)
			}
		}
	}
	if st.MCP.Registered {
		fmt.Printf("  mcp server      %s (registered in mcp.servers)\n", st.MCP.Name)
	}
}

func runVikingSync(cmd *cobra.Command, _ []string) error {
	_, svc, _, err := vikingRuntime()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
	defer cancel()
	fmt.Printf("syncing %s ...\n", svc.WorkspaceDir())
	rep, syncErr := svc.SyncWorkspace(ctx, vikingFull)
	printSyncReport(rep)
	return syncErr
}

// printSyncReport renders one sync's outcome.
func printSyncReport(rep documents.Report) {
	fmt.Printf("scanned=%d uploaded=%d unchanged=%d skipped=%d failed=%d\n",
		rep.Scanned, rep.Uploaded, rep.Unchanged, rep.Skipped, rep.Failed)
	fmt.Printf("under %s\n", rep.RootURI)
	if rep.Uploaded > 0 {
		// OpenViking indexes asynchronously, so a hit right after a sync can
		// lag; saying so prevents a "it didn't work" that is really "wait".
		fmt.Println("note: OpenViking indexes in the background; search may lag a few seconds")
	}
	for _, e := range rep.Errors {
		fmt.Printf("  ! %s\n", e)
	}
}

func runVikingSave(cmd *cobra.Command, _ []string) error {
	_, svc, _, err := vikingRuntime()
	if err != nil {
		return err
	}
	title := strings.TrimSpace(vikingTitle)
	if title == "" {
		return errors.New("--title is required")
	}
	content := vikingBody
	if vikingFile != "" {
		raw, rerr := os.ReadFile(vikingFile)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", vikingFile, rerr)
		}
		content = string(raw)
	}
	if strings.TrimSpace(content) == "" {
		return errors.New("provide the document body with --file or --content")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()
	uri, err := svc.SaveDocument(ctx, documents.Document{
		Title:   title,
		Content: content,
		Tags:    splitTags(vikingTags),
		Source:  vikingSource,
	})
	if err != nil {
		return err
	}
	fmt.Println(uri)
	return nil
}

func runVikingRemember(cmd *cobra.Command, args []string) error {
	cfg, svc, logger, err := vikingRuntime()
	if err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(args, " "))
	if text == "" {
		return errors.New("nothing to remember")
	}
	local, err := memory.NewStore(cfg.Memory.Dir)
	if err != nil {
		return fmt.Errorf("open memory store: %w", err)
	}
	store := svc.WrapStore(local)
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	if err := store.AddFact(ctx, "cli", memory.Fact{Value: text}); err != nil {
		return err
	}
	if err := store.Close(); err != nil {
		return err
	}
	logger.Debug("fact recorded", zap.String("text", text))
	fmt.Println("recorded")
	return nil
}

func runVikingSearch(cmd *cobra.Command, args []string) error {
	_, svc, _, err := vikingRuntime()
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(args, " "))
	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()
	res, err := svc.Search(ctx, query, vikingLimit)
	if err != nil {
		return err
	}
	hits := res.All()
	if len(hits) == 0 {
		fmt.Println("(no results)")
		return nil
	}
	for _, h := range hits {
		summary := strings.TrimSpace(h.Abstract)
		if summary == "" {
			summary = strings.TrimSpace(h.Content)
		}
		if len(summary) > 160 {
			summary = summary[:160] + "…"
		}
		fmt.Printf("%.3f  %-9s  %s\n", h.Score, h.ContextType, h.URI)
		if summary != "" {
			fmt.Printf("       %s\n", strings.ReplaceAll(summary, "\n", " "))
		}
	}
	return nil
}

// splitTags parses a comma-separated tag list, dropping blanks.
func splitTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
