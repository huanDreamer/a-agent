package main

// The fetch_url wiring: one fetcher per process, built from the configuration.

import (
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	agenttool "github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tool/builtin"
	"github.com/huan/huan-agent/internal/web"
)

// webFetcher is the process's fetcher. One, not one per surface: the in-process
// cache is what makes "read the next window of that page" cheap, and a per-surface
// cache would make the same page fetched twice.
var (
	webOnce    sync.Once
	webFetcher *web.Fetcher
)

// fetcherFor returns the process's fetcher, building it on first use.
func fetcherFor(cfg *config.Config, logger *zap.Logger) *web.Fetcher {
	if cfg == nil || !cfg.Tools.Web.Enable {
		return nil
	}
	webOnce.Do(func() {
		allowPrivate := cfg.Tools.Web.AllowPrivate
		if allowPrivate && logger != nil {
			// The one thing an operator must be able to see: this deployment will
			// fetch its own local network, including the agent's own console.
			logger.Warn("fetch_url 允许内网地址（tools.web.allow_private = true）：" +
				"云元数据地址（169.254.169.254）与本机服务因此都可被模型读取")
		}
		webFetcher = web.NewFetcher(web.Options{
			Guard:     web.Guard{AllowPrivate: allowPrivate, Logger: webLogger{logger}},
			Timeout:   cfg.Tools.Web.Timeout(),
			MaxBytes:  cfg.Tools.Web.MaxBytes(),
			UserAgent: cfg.Tools.Web.UserAgent,
			CacheTTL:  cfg.Tools.Web.CacheTTL(),
			Logger:    webLogger{logger},
		})
	})
	return webFetcher
}

// registerFetchURLTool adds fetch_url to a registry.
func registerFetchURLTool(reg *agenttool.Registry, cfg *config.Config, logger *zap.Logger) error {
	f := fetcherFor(cfg, logger)
	if f == nil {
		return nil
	}
	t, err := builtin.NewFetchURLTool(f, cfg.Tools.Web.MaxCharsOr())
	if err != nil {
		return fmt.Errorf("build fetch_url tool: %w", err)
	}
	// CapRead: it reads the internet, not the workspace. ParallelSafe: reading
	// several pages at once is the common case, and the per-host bound lives in
	// the tool's own scheduler rather than in this declaration.
	if err := reg.Register(agenttool.WithConcurrency(
		agenttool.WithCapability(t, agenttool.CapRead), agenttool.ParallelSafe)); err != nil {
		return err
	}
	if logger != nil {
		logger.Info("fetch_url registered",
			zap.Bool("allow_private", cfg.Tools.Web.AllowPrivate),
			zap.Int("max_chars", cfg.Tools.Web.MaxCharsOr()),
			zap.Duration("timeout", cfg.Tools.Web.Timeout()))
	}
	return nil
}

// webLogger adapts *zap.Logger to the web package's interface.
type webLogger struct{ l *zap.Logger }

func (w webLogger) Debug(msg string, kv ...any) {
	if w.l != nil {
		w.l.Sugar().Debugw(msg, kv...)
	}
}

func (w webLogger) Warn(msg string, kv ...any) {
	if w.l != nil {
		w.l.Sugar().Warnw(msg, kv...)
	}
}
