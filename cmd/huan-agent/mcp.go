package main

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/mcp"
	"github.com/huan/huan-agent/internal/tool"
)

// This file holds the one conversion from a configured MCP server to the
// runtime's own spec type, plus the one loop that connects them.
//
// It exists because the conversion used to be written out by hand at every call
// site, and a hand-written field list is a field list that can be wrong. The CLI
// and Feishu loops both copied only Name/Command/Args/Env, which dropped
// Transport, URL and Headers. That silently turned every http server declared in
// the config file into a stdio server with no command — and the resulting error,
// "command is required for a stdio server", points at the config file, which is
// the one place the problem was not.
//
// The console never had the bug because internal/server/mcp.go spells all eight
// fields out. The lesson is not "remember three more fields", it is "there is one
// conversion". Do not inline it again.

// mcpSpecFromConfig converts one configured MCP server into a runtime spec.
//
// The transport is parsed rather than copied so an unparseable value fails here,
// with the server's name attached, instead of falling back to stdio and failing
// later with a message about a missing command. The ID matches
// internal/server.SyncConfigServers, so a server has the same identity whether it
// was reached through the console or through this path.
func mcpSpecFromConfig(s config.MCPServer) (mcp.ServerSpec, error) {
	name := strings.TrimSpace(s.Name)

	transport, err := mcp.ParseTransport(s.Transport)
	if err != nil {
		return mcp.ServerSpec{}, fmt.Errorf("mcp server %q: %w", name, err)
	}

	return mcp.ServerSpec{
		ID:        mcp.ServerID(name),
		Name:      name,
		Transport: transport,
		Command:   s.Command,
		Args:      s.Args,
		Env:       s.Env,
		URL:       s.URL,
		Headers:   s.Headers,
	}, nil
}

// connectConfiguredMCP connects every enabled MCP server declared in the config
// file and registers its tools into reg, returning the clients so the caller
// keeps owning their lifetime.
//
// On failure it closes everything it already opened: a half-connected MCP setup
// is worse than none, because the caller is about to abort and the orphaned
// clients (each a subprocess, for stdio) would outlive it.
//
// surface is only for the log line; it is what makes "the CLI connected this"
// and "the bot connected this" distinguishable in one log stream.
func connectConfiguredMCP(ctx context.Context, reg *tool.Registry, cfg *config.Config,
	logger *zap.Logger, surface string) ([]*mcp.Client, error) {

	if cfg == nil || reg == nil {
		return nil, nil
	}

	var clients []*mcp.Client
	closeAll := func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}

	for _, s := range cfg.MCP.Servers {
		// A disabled entry is skipped, not connected-and-ignored. The feishu
		// path always did this and the CLI path did not, which is the second
		// half of the same "two hand-written loops drifted apart" problem.
		if !s.IsEnabled() {
			logger.Info("mcp server is disabled, skipping",
				zap.String("name", strings.TrimSpace(s.Name)),
				zap.String("surface", surface))
			continue
		}

		spec, err := mcpSpecFromConfig(s)
		if err != nil {
			closeAll()
			return nil, err
		}

		c, err := mcp.Connect(ctx, spec)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("connect mcp %s (%s): %w", spec.Name, spec.ResolvedTransport(), err)
		}
		clients = append(clients, c)

		// A tool whose name is already taken is skipped by the bridge, not
		// treated as a failure to connect: the server is still usable, and (for
		// stdio servers) a client we rolled back here would be a subprocess the
		// caller then abandons. The bridge logs one warn per skipped tool, so the
		// reason is not repeated here; the count is reported below.
		names, skipped, rerr := mcp.RegisterMCPTools(ctx, reg, c, logger)
		if rerr != nil {
			closeAll()
			return nil, fmt.Errorf("register mcp tools %s (%s): %w", spec.Name, spec.ResolvedTransport(), rerr)
		}

		logger.Info("mcp server connected",
			zap.String("name", spec.Name),
			zap.String("transport", string(spec.ResolvedTransport())),
			zap.String("surface", surface),
			zap.Int("tools", len(names)),
			zap.Int("skipped", len(skipped)))
	}

	return clients, nil
}
