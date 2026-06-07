package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/obs"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/usage"
)

var (
	chatProvider string
	chatModel    string
	chatSystem   string
	chatCmd      = &cobra.Command{
		Use:   "chat",
		Short: "Interactive chat REPL with the configured LLM (Phase 1)",
		Long: `Start an interactive REPL that streams responses from the configured LLM.

Examples:
  # Use the default provider/model from config
  huan-agent chat

  # Override provider and model
  huan-agent chat --provider qwen --model qwen-turbo

  # Inject a system prompt
  huan-agent chat --system "You are a helpful assistant. Answer in Chinese."

Commands inside the REPL:
  /quit, /exit, Ctrl+D   exit the chat
  /reset                  clear conversation history
  /provider               print current provider/model
`,
		RunE: runChat,
	}
)

func init() {
	chatCmd.Flags().StringVar(&chatProvider, "provider", "", "override default provider")
	chatCmd.Flags().StringVar(&chatModel, "model", "", "override default model")
	chatCmd.Flags().StringVar(&chatSystem, "system", "", "system prompt")
	rootCmd.AddCommand(chatCmd)
}

func runChat(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger, err := obs.NewLogger(cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	// Open SQLite + start the recorder.
	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0o755); err != nil {
		return fmt.Errorf("mkdir database dir: %w", err)
	}
	st, err := store.Open(cmd.Context(), cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	recorder := usage.NewRecorder(st, logger, 256)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = recorder.Close(ctx)
	}()

	// Build the registry and resolve the model.
	providers := make(map[string]llm.Provider, len(cfg.LLM.Providers))
	for name, p := range cfg.LLM.Providers {
		providers[name] = llm.Provider{
			Name:    name,
			BaseURL: p.BaseURL,
			APIKey:  p.APIKey,
			Model:   p.Model,
		}
	}
	registry := llm.NewRegistry(providers, cfg.LLM.DefaultProvider)

	chosen := chatProvider
	if chosen == "" {
		chosen = cfg.LLM.DefaultProvider
	}
	if chosen == "" {
		return fmt.Errorf("no provider configured (set llm.default_provider in config or use --provider)")
	}

	cm, err := registry.Get(chosen)
	if err != nil {
		return err
	}
	if chatModel != "" {
		// Override model on the resolved model: we rebuild it via the
		// registry to honour the new model name.
		prov, _ := registry.Provider(chosen)
		prov.Model = chatModel
		cm, err = llm.New(prov)
		if err != nil {
			return err
		}
	}

	sessionID := uuid.NewString()
	logger.Info("chat session starting",
		zap.String("session_id", sessionID),
		zap.String("provider", chosen),
		zap.String("model", effectiveModel(registry, chosen, chatModel)))

	fmt.Println(banner(chosen, effectiveModel(registry, chosen, chatModel)))
	fmt.Println()

	// Build initial message history.
	var history []*schema.Message
	if chatSystem != "" {
		history = append(history, &schema.Message{Role: schema.System, Content: chatSystem})
	}

	// Signal handler: cancels the in-flight stream when the user hits Ctrl+C.
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64*1024), 1024*1024)

	for {
		fmt.Print("you> ")
		if !in.Scan() {
			if err := in.Err(); err != nil && !errors.Is(err, io.EOF) {
				fmt.Fprintln(os.Stderr, "read error:", err)
			}
			fmt.Println()
			return nil
		}
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		switch strings.ToLower(line) {
		case "/quit", "/exit":
			return nil
		case "/reset":
			history = history[:0]
			if chatSystem != "" {
				history = append(history, &schema.Message{Role: schema.System, Content: chatSystem})
			}
			fmt.Println("(conversation reset)")
			continue
		case "/provider":
			fmt.Printf("provider=%s model=%s\n", chosen, effectiveModel(registry, chosen, chatModel))
			continue
		}

		history = append(history, &schema.Message{Role: schema.User, Content: line})

		if err := streamOnce(ctx, cm, registry, chosen, chatModel, history, sessionID, recorder, logger); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			// drop the last user message so the conversation stays sane
			history = history[:len(history)-1]
			if errors.Is(err, context.Canceled) {
				return nil
			}
		}
		fmt.Println()
	}
}

func streamOnce(
	ctx context.Context,
	cm model.BaseChatModel,
	reg *llm.Registry,
	providerName string,
	modelOverride string,
	history []*schema.Message,
	sessionID string,
	rec *usage.Recorder,
	logger *zap.Logger,
) error {
	start := time.Now()
	stream, err := cm.Stream(ctx, history)
	if err != nil {
		return err
	}
	defer stream.Close()

	var (
		assistant strings.Builder
		finish    string
	)
	fmt.Print("ai> ")
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return recvErr
		}
		if chunk == nil {
			continue
		}
		if chunk.Content != "" {
			assistant.WriteString(chunk.Content)
			fmt.Print(chunk.Content)
		}
		if chunk.ResponseMeta != nil && chunk.ResponseMeta.FinishReason != "" {
			finish = chunk.ResponseMeta.FinishReason
		}
	}
	fmt.Println()

	// Append assistant reply to history.
	history = append(history, &schema.Message{
		Role:    schema.Assistant,
		Content: assistant.String(),
		ResponseMeta: &schema.ResponseMeta{
			FinishReason: finish,
		},
	})

	// We don't always have token counts in streamed responses; emit 0s and let
	// future phases (or the provider itself) populate them. Recorder tolerates 0s.
	dur := time.Since(start)
	recErr := rec.Record(usage.Event{
		SessionID:  sessionID,
		Provider:   providerName,
		Model:      effectiveModel(reg, providerName, modelOverride),
		DurationMs: dur.Milliseconds(),
	})
	if recErr != nil {
		logger.Warn("usage record failed", zap.Error(recErr))
	}
	return nil
}

func effectiveModel(reg *llm.Registry, name, override string) string {
	if override != "" {
		return override
	}
	if p, ok := reg.Provider(name); ok {
		return p.Model
	}
	return "<unknown>"
}

func banner(provider, model string) string {
	return fmt.Sprintf("huan-agent chat (provider=%s model=%s)\nType /quit to exit, /reset to clear, /provider to inspect.", provider, model)
}
