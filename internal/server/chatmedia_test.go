package server

import (
	"net/http"
	"testing"

	"github.com/huan/huan-agent/internal/media"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tool/builtin"
	"github.com/huan/huan-agent/internal/workspace"
)

// TestChatModels_ListsMediaTools proves the last link in the availability
// chain: GET /api/chat/models lists the tools array straight from the registry,
// so a media tool that was registered because its capability is bound shows up
// in the UI, and one that was withheld (an unbound capability, or in this case
// its absence) does not.
//
// The tools here are the real ones, built by the real constructors from a
// resolved media.Target: what is under test is the wiring between registration
// and the endpoint, not the tools themselves.
func TestChatModels_ListsMediaTools(t *testing.T) {
	ws, err := workspace.New(t.TempDir(), workspace.Options{})
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	target := media.Target{
		ProviderID: "test-provider",
		ModelID:    "test-model",
		BaseURL:    "http://127.0.0.1:9/v1",
		Kind:       media.KindOpenAI,
	}

	reg := tool.NewRegistry()
	describe, err := builtin.NewDescribeImageTool(ws, target)
	if err != nil {
		t.Fatalf("describe_image: %v", err)
	}
	if err := reg.Register(tool.WithCapability(describe, tool.CapRead)); err != nil {
		t.Fatalf("register describe_image: %v", err)
	}
	transcribe, err := builtin.NewTranscribeAudioTool(ws, target)
	if err != nil {
		t.Fatalf("transcribe_audio: %v", err)
	}
	if err := reg.Register(tool.WithCapability(transcribe, tool.CapRead)); err != nil {
		t.Fatalf("register transcribe_audio: %v", err)
	}

	h := newChatHarness(t, nil, reg)
	h.login(t)

	var got struct {
		Tools []string `json:"tools"`
	}
	h.getJSON(t, "/api/chat/models", http.StatusOK, &got)

	listed := map[string]bool{}
	for _, name := range got.Tools {
		listed[name] = true
	}
	for _, want := range []string{"describe_image", "transcribe_audio"} {
		if !listed[want] {
			t.Errorf("GET /api/chat/models does not list %s: %v", want, got.Tools)
		}
	}
	// A capability that is not registered stays invisible, which is the whole
	// contract: the model must not be told about a tool that cannot work.
	if listed["generate_image"] {
		t.Errorf("generate_image should not be listed: %v", got.Tools)
	}
}
