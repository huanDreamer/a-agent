package main

// Tests for the surface gating of the web-only tools.
//
// ask_user is the one tool a surface can be unable to honour: it parks the turn
// until a person answers, and only the web console can render the card that
// answer comes from. Offering it to Feishu or the CLI would produce a tool call
// that can only fail, which the repository treats as worse than a tool that was
// never on the menu.

import (
	"testing"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tool/builtin"
)

func TestRegisterBuiltinTools_AskUserIsWebOnly(t *testing.T) {
	for _, surface := range []string{"web", "feishu", "cli", ""} {
		t.Run("surface="+surface, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Tools.Workspace = t.TempDir()
			cfg.Tools.EnableBash = true

			reg := tool.NewRegistry()
			if err := registerBuiltinTools(reg, cfg, newMediaTestStore(t), zap.NewNop(), nil,
				toolSetOptions{Surface: surface}); err != nil {
				t.Fatalf("registerBuiltinTools: %v", err)
			}

			_, registered := reg.Get(builtin.AskUserToolName)
			if want := surface == "web"; registered != want {
				t.Errorf("ask_user registered = %v on surface %q, want %v (tools: %v)",
					registered, surface, want, reg.Names())
			}
			// The always-on tools are unaffected, so the gate is specific rather
			// than a whole tool set that failed to build.
			if _, ok := reg.Get("time"); !ok {
				t.Errorf("the basics are missing on surface %q: %v", surface, reg.Names())
			}
		})
	}
}
