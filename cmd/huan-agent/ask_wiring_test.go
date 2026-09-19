package main

// Tests for the ask_user wiring in the web console.
//
// The tool itself is covered in internal/tool/builtin and the transport in
// internal/server. What is only checkable here is the wiring: that the web chat
// is handed the tool and the configured wait limit. (That no *other* surface
// gets the tool is covered in ask_tool_test.go, where the surface gate lives.)

import (
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/server"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool/builtin"
)

// webChatDeps builds the chat wiring for a minimal but real deployment: one
// provider, one chat model, a workspace.
func webChatDeps(t *testing.T, cfg *config.Config) server.ChatDeps {
	t.Helper()

	st := newMediaTestStore(t)
	seedProviderAndModels(t, st, "fake-provider",
		store.Model{ModelID: "fake-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
	)
	cfg.Chat.Enable = true
	cfg.LLM.DefaultProvider = "fake"
	cfg.LLM.Providers = map[string]config.LLMProvider{
		"fake": {BaseURL: "http://127.0.0.1:9/v1", APIKey: "sk-not-used", Model: "fake-model"},
	}
	cfg.Tools.Workspace = t.TempDir()

	deps, _, _ := buildChatDeps(cfg, nil, st, nil, zap.NewNop(), nil, nil, nil)
	return deps
}

func TestBuildChatDeps_ExposesAskUserToTheWebChat(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chat.AskUserTimeoutSeconds = 90

	deps := webChatDeps(t, cfg)
	if deps.Tools == nil {
		t.Fatal("web chat has no tool registry")
	}
	if _, ok := deps.Tools.Get(builtin.AskUserToolName); !ok {
		t.Errorf("ask_user is not exposed to the web chat: %v", deps.Tools.Names())
	}
	if deps.AskTimeout != 90*time.Second {
		t.Errorf("AskTimeout = %s, want the configured 90s", deps.AskTimeout)
	}
}

func TestBuildChatDeps_AskTimeoutDefaultsWhenUnset(t *testing.T) {
	// An unset value must still bound the wait: zero would mean "no limit", which
	// parks a goroutine — and a half-finished tool call — for as long as the tab
	// stays open.
	deps := webChatDeps(t, &config.Config{})

	if deps.AskTimeout != time.Duration(config.DefaultChatAskUserTimeoutSeconds)*time.Second {
		t.Errorf("AskTimeout = %s, want the default %ds",
			deps.AskTimeout, config.DefaultChatAskUserTimeoutSeconds)
	}
}
