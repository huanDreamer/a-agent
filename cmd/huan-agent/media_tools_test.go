package main

import (
	"context"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspace"
)

// These tests cover the registration contract that matters most: a media tool
// whose model capability is unbound must not exist in the registry at all. They
// run against a real SQLite catalog, because the decision is made from catalog
// rows and a fake store would only test the fake.

// newMediaTestStore opens a throwaway catalog.
func newMediaTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newMediaTestWorkspace returns a workspace rooted in a fresh directory.
func newMediaTestWorkspace(t *testing.T, opts workspace.Options) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.New(t.TempDir(), opts)
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	return ws
}

// seedProviderAndModels writes an enabled provider with the given models.
func seedProviderAndModels(t *testing.T, st store.Store, providerID string, models ...store.Model) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertProvider(ctx, store.Provider{
		ID:      providerID,
		Name:    providerID,
		BaseURL: "http://127.0.0.1:9/v1",
		Kind:    "openai",
		Enabled: true,
	}); err != nil {
		t.Fatalf("upsert provider: %v", err)
	}
	for _, m := range models {
		m.ProviderID = providerID
		if err := st.UpsertModel(ctx, m); err != nil {
			t.Fatalf("upsert model %s: %v", m.ModelID, err)
		}
	}
}

// mediaToolNamesIn returns the media tools present in a registry.
func mediaToolNamesIn(reg *tool.Registry) []string {
	var out []string
	for _, c := range mediaToolCandidates {
		if _, ok := reg.Get(c.name); ok {
			out = append(out, c.name)
		}
	}
	return out
}

func TestRegisterMediaTools_RegisterNothingWhenNothingIsBound(t *testing.T) {
	// The fresh-install case: a catalog with no provider at all must still
	// produce a working agent, and must not advertise tools it cannot run.
	st := newMediaTestStore(t)
	ws := newMediaTestWorkspace(t, workspace.Options{})
	reg := tool.NewRegistry()

	registerMediaTools(reg, &config.Config{}, st, ws, zap.NewNop())

	if got := mediaToolNamesIn(reg); len(got) != 0 {
		t.Errorf("registered %v, want no media tools", got)
	}
	if len(reg.Names()) != 0 {
		t.Errorf("registry is not empty: %v", reg.Names())
	}
}

func TestRegisterMediaTools_RegistersTheBoundCapabilitiesOnly(t *testing.T) {
	st := newMediaTestStore(t)
	seedProviderAndModels(t, st,
		"vision-provider",
		store.Model{ModelID: "vision-model", Capabilities: store.Capabilities{store.CapVision}, Enabled: true},
		store.Model{ModelID: "whisper-model", Capabilities: store.Capabilities{store.CapAudioTranscribe}, Enabled: true},
		store.Model{ModelID: "chat-model", Capabilities: store.Capabilities{store.CapChat}, Enabled: true},
	)
	ws := newMediaTestWorkspace(t, workspace.Options{})
	reg := tool.NewRegistry()

	registerMediaTools(reg, &config.Config{}, st, ws, zap.NewNop())

	// The two capabilities with a single candidate resolve; image generation has
	// no model at all, so the tool must not exist.
	for _, want := range []string{"describe_image", "transcribe_audio"} {
		if _, ok := reg.Get(want); !ok {
			t.Errorf("%s is missing from the registry: %v", want, reg.Names())
		}
	}
	if _, ok := reg.Get("generate_image"); ok {
		t.Error("generate_image was registered with no image model configured")
	}
	// Reading tools are tagged read, so a read-only chat surface keeps them.
	if got := tool.CapabilityOf(mustTool(t, reg, "describe_image")); got != tool.CapRead {
		t.Errorf("describe_image capability = %q, want read", got)
	}
	if got := tool.CapabilityOf(mustTool(t, reg, "transcribe_audio")); got != tool.CapRead {
		t.Errorf("transcribe_audio capability = %q, want read", got)
	}
}

func TestRegisterMediaTools_GenerateImageIsAToolWrite(t *testing.T) {
	st := newMediaTestStore(t)
	seedProviderAndModels(t, st, "image-provider",
		store.Model{ModelID: "image-model", Capabilities: store.Capabilities{store.CapImageGen}, Enabled: true},
	)
	ws := newMediaTestWorkspace(t, workspace.Options{})
	reg := tool.NewRegistry()

	registerMediaTools(reg, &config.Config{}, st, ws, zap.NewNop())

	// It writes a file, so it must be tagged write rather than read.
	if got := tool.CapabilityOf(mustTool(t, reg, "generate_image")); got != tool.CapWrite {
		t.Errorf("generate_image capability = %q, want write", got)
	}
}

func TestRegisterMediaTools_ReadOnlyWorkspaceWithholdsTheWriteTool(t *testing.T) {
	st := newMediaTestStore(t)
	seedProviderAndModels(t, st, "image-provider",
		store.Model{ModelID: "image-model", Capabilities: store.Capabilities{store.CapImageGen}, Enabled: true},
		store.Model{ModelID: "vision-model", Capabilities: store.Capabilities{store.CapVision}, Enabled: true},
	)
	ws := newMediaTestWorkspace(t, workspace.Options{ReadOnly: true})
	reg := tool.NewRegistry()

	registerMediaTools(reg, &config.Config{Tools: config.ToolsConfig{ReadOnly: true}}, st, ws, zap.NewNop())

	if _, ok := reg.Get("generate_image"); ok {
		t.Error("generate_image must not be offered by a read-only workspace")
	}
	// Reading the workspace is still allowed.
	if _, ok := reg.Get("describe_image"); !ok {
		t.Errorf("describe_image should survive a read-only workspace: %v", reg.Names())
	}
}

func TestRegisterMediaTools_NilWorkspaceOrStoreRegistersNothing(t *testing.T) {
	st := newMediaTestStore(t)
	seedProviderAndModels(t, st, "vision-provider",
		store.Model{ModelID: "vision-model", Capabilities: store.Capabilities{store.CapVision}, Enabled: true},
	)

	t.Run("no workspace", func(t *testing.T) {
		reg := tool.NewRegistry()
		registerMediaTools(reg, &config.Config{}, st, nil, zap.NewNop())
		if len(reg.Names()) != 0 {
			t.Errorf("registered %v with no workspace, want nothing", reg.Names())
		}
	})
	t.Run("no store", func(t *testing.T) {
		reg := tool.NewRegistry()
		registerMediaTools(reg, &config.Config{}, nil, newMediaTestWorkspace(t, workspace.Options{}), zap.NewNop())
		if len(reg.Names()) != 0 {
			t.Errorf("registered %v with no catalog, want nothing", reg.Names())
		}
	})
}

func TestRegisterBuiltinTools_AddsMediaToolsAlongsideTheFileTools(t *testing.T) {
	st := newMediaTestStore(t)
	seedProviderAndModels(t, st, "vision-provider",
		store.Model{ModelID: "vision-model", Capabilities: store.Capabilities{store.CapVision}, Enabled: true},
	)
	cfg := &config.Config{}
	cfg.Tools.Workspace = t.TempDir()

	reg := tool.NewRegistry()
	if err := registerBuiltinTools(reg, cfg, st, zap.NewNop()); err != nil {
		t.Fatalf("registerBuiltinTools: %v", err)
	}

	for _, want := range []string{"read_file", "write_file", "describe_image"} {
		if _, ok := reg.Get(want); !ok {
			t.Errorf("%s is missing: %v", want, reg.Names())
		}
	}
	// Unbound media capabilities stay invisible even though other tools exist.
	if _, ok := reg.Get("generate_image"); ok {
		t.Error("generate_image was registered with no image model configured")
	}
}

func TestBuildChatDeps_ExposesMediaToolsToTheChatModelsEndpoint(t *testing.T) {
	// GET /api/chat/models reports the tools array straight from this registry,
	// so a media tool that is missing here is a tool the UI cannot show.
	st := newMediaTestStore(t)
	seedProviderAndModels(t, st, "vision-provider",
		store.Model{ModelID: "vision-model", Capabilities: store.Capabilities{store.CapVision}, Enabled: true},
	)

	cfg := &config.Config{}
	cfg.Chat.Enable = true
	cfg.LLM.DefaultProvider = "fake"
	cfg.LLM.Providers = map[string]config.LLMProvider{
		"fake": {BaseURL: "http://127.0.0.1:9/v1", APIKey: "sk-not-used", Model: "fake-model"},
	}
	cfg.Tools.Workspace = t.TempDir()

	deps, _, _ := buildChatDeps(cfg, nil, st, nil, zap.NewNop())
	if deps.Tools == nil {
		t.Fatal("web chat has no tool registry")
	}
	if _, ok := deps.Tools.Get("describe_image"); !ok {
		t.Errorf("describe_image is not exposed to the web chat: %v", deps.Tools.Names())
	}
}

func TestBuildChatDeps_NoProvidersStillBuildsWithoutMediaTools(t *testing.T) {
	// The fresh-install path: no provider is configured, so the web chat is
	// disabled and nothing panics on the way there.
	cfg := &config.Config{}
	cfg.Chat.Enable = true
	deps, _, _ := buildChatDeps(cfg, nil, newMediaTestStore(t), nil, zap.NewNop())
	if deps.Tools != nil {
		t.Errorf("expected no chat deps, got tools %v", deps.Tools.Names())
	}
}

func mustTool(t *testing.T, reg *tool.Registry, name string) tool.Tool {
	t.Helper()
	tl, ok := reg.Get(name)
	if !ok {
		t.Fatalf("%s is not registered: %v", name, reg.Names())
	}
	return tl
}
