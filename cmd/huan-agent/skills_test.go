package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/skill"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// newSkillTestRegistry builds the registry a CLI session runs with, over a real
// (empty) store and a real workspace directory, so the shipped skills are
// validated against the tools they would actually be offered.
func newSkillTestRegistry(t *testing.T) *tool.Registry {
	t.Helper()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "skills.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{}
	cfg.Tools.Workspace = t.TempDir()

	reg := tool.NewRegistry()
	if err := registerBuiltinTools(reg, cfg, st, zap.NewNop(), nil,
		toolSetOptions{Surface: "cli"}); err != nil {
		t.Fatalf("registerBuiltinTools: %v", err)
	}
	return reg
}

// shippedSkills loads every skill the repo ships.
func shippedSkills(t *testing.T) map[string]*skill.Skill {
	t.Helper()

	paths, err := filepath.Glob("../../configs/skills/*.md")
	if err != nil {
		t.Fatalf("glob skills: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no skills found under configs/skills; the test is looking in the wrong place")
	}

	out := make(map[string]*skill.Skill, len(paths))
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		name := strings.TrimSuffix(filepath.Base(p), ".md")
		sk, err := skill.LoadFile(name, string(body))
		if err != nil {
			t.Errorf("load %s: %v", p, err)
			continue
		}
		out[name] = sk
	}
	return out
}

// TestShippedSkillsDeclareRegisteredTools guards the failure mode a skill's
// `tools` list has: it is an allow-list, and the loader only refuses *unknown*
// names. A list that is valid but useless — `[calc, echo]` on a code-review
// skill — loads without complaint and then leaves the model unable to read a
// single file, which is how it shipped for as long as it did.
func TestShippedSkillsDeclareRegisteredTools(t *testing.T) {
	reg := newSkillTestRegistry(t)
	skills := shippedSkills(t)

	for name, sk := range skills {
		t.Run(name, func(t *testing.T) {
			declared := sk.Frontmatter.Tools
			if len(declared) == 0 {
				t.Fatalf("skill %q declares no tools, so it is subject to cfg.Agent.AllowedTools; "+
					"declare the set it needs explicitly", name)
			}

			// SetAllowList refuses an unknown name, so this is the registry check.
			if err := reg.SetAllowList(declared); err != nil {
				t.Fatalf("skill %q lists a tool the registry does not have: %v", name, err)
			}

			allowed := reg.AllowList()
			if len(allowed) == 0 {
				t.Fatalf("skill %q narrows the registry to nothing", name)
			}
			// Every declared name must survive, in case a name was dropped.
			for _, want := range declared {
				if !contains(allowed, want) {
					t.Errorf("skill %q declared %q but it is not in the resulting allow-list %v",
						name, want, allowed)
				}
			}
		})
	}
}

// TestCodeReviewSkillCanReadFiles pins the specific regression: a code-review
// skill that cannot read a file cannot review anything. Everything else in its
// prompt — "引用代码时使用 path/to/file.go:line 格式", "写明文件/函数/行号" — is
// unreachable without read access.
func TestCodeReviewSkillCanReadFiles(t *testing.T) {
	reg := newSkillTestRegistry(t)
	skills := shippedSkills(t)

	sk, ok := skills["code-review"]
	if !ok {
		t.Fatal("configs/skills/code-review.md is missing")
	}
	if err := reg.SetAllowList(sk.Frontmatter.Tools); err != nil {
		t.Fatalf("apply allow-list: %v", err)
	}

	allowed := reg.AllowList()
	for _, want := range []string{"read_file", "grep"} {
		if !contains(allowed, want) {
			t.Errorf("code-review cannot %s; allow-list = %v", want, allowed)
		}
	}
	// It is a review skill: it must not be able to change what it reviews.
	for _, unwanted := range []string{"write_file", "edit_file", "bash"} {
		if contains(allowed, unwanted) {
			t.Errorf("code-review has write/exec access via %q; it should be read-only", unwanted)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
