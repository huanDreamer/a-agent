package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodSkill = `---
name: daily-summary
description: 总结过去 24 小时的 commit
tools: [time, calc]
---

# Daily Summary

You are a daily-summary assistant. Be concise.
`

const noFrontmatter = `# Bare skill

Just a body, no frontmatter.
`

const badFrontmatter = `---
this is not: valid: yaml: at all
---

body
`

func TestLoadFile_Good(t *testing.T) {
	s, err := LoadFile("daily-summary", goodSkill)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if s.Frontmatter.Name != "daily-summary" {
		t.Errorf("name = %q", s.Frontmatter.Name)
	}
	if !strings.Contains(s.Frontmatter.Description, "总结") {
		t.Errorf("description missing: %q", s.Frontmatter.Description)
	}
	if len(s.Frontmatter.Tools) != 2 {
		t.Errorf("tools = %v", s.Frontmatter.Tools)
	}
	if !strings.Contains(s.Body, "Daily Summary") {
		t.Errorf("body missing header: %q", s.Body)
	}
}

func TestLoadFile_NoFrontmatter(t *testing.T) {
	s, err := LoadFile("bare", noFrontmatter)
	if err != nil {
		t.Fatal(err)
	}
	if s.Frontmatter.Name != "bare" {
		t.Errorf("name = %q, want bare", s.Frontmatter.Name)
	}
	if !strings.Contains(s.Body, "Bare skill") {
		t.Errorf("body wrong: %q", s.Body)
	}
}

func TestLoadFile_NoName(t *testing.T) {
	if _, err := LoadFile("", noFrontmatter); err == nil {
		t.Error("expected error for missing name")
	}
}

func TestLoadFile_BadFrontmatter(t *testing.T) {
	if _, err := LoadFile("x", badFrontmatter); err == nil {
		t.Error("expected error for bad YAML")
	}
}

func TestLoadFile_UnclosedFrontmatter(t *testing.T) {
	const c = "---\nname: x\nno closing\n"
	if _, err := LoadFile("x", c); err == nil {
		t.Error("expected error for unclosed frontmatter")
	}
}

func TestSystemPrompt(t *testing.T) {
	s, _ := LoadFile("daily-summary", goodSkill)
	got := s.SystemPrompt()
	if !strings.HasPrefix(got, "总结") {
		t.Errorf("expected description first, got prefix %q", got[:20])
	}
	if !strings.Contains(got, "Daily Summary") {
		t.Error("body missing from system prompt")
	}
}

func TestLoader_EmptyDir(t *testing.T) {
	l := NewLoader(t.TempDir())
	got, err := l.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestLoader_MissingDir(t *testing.T) {
	l := NewLoader(filepath.Join(t.TempDir(), "nope"))
	got, err := l.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestLoader_LoadAndGet(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "alpha.md"), goodSkill)
	mustWrite(t, filepath.Join(dir, "beta.md"), noFrontmatter)
	mustWrite(t, filepath.Join(dir, "ignore.txt"), "ignore me")

	l := NewLoader(dir)
	all, err := l.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("len = %d, want 2 (alpha + beta)", len(all))
	}

	if s, ok := l.Get("daily-summary"); !ok || s == nil {
		t.Error("missing daily-summary")
	}
	if s, ok := l.Get("beta"); !ok || s == nil {
		t.Error("missing beta")
	}
	if _, ok := l.Get("missing"); ok {
		t.Error("found missing skill")
	}
}

func TestLoader_DuplicateSkillName(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.md"), goodSkill)
	mustWrite(t, filepath.Join(dir, "b.md"), goodSkill) // same name "daily-summary"

	l := NewLoader(dir)
	_, err := l.LoadAll()
	if err == nil {
		t.Error("expected error for duplicate skill name")
	}
}

func TestLoader_PartialFailure(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "good.md"), goodSkill)
	mustWrite(t, filepath.Join(dir, "bad.md"), badFrontmatter)

	l := NewLoader(dir)
	all, err := l.LoadAll()
	if err == nil {
		t.Error("expected error")
	}
	if len(all) != 1 {
		t.Errorf("len = %d, want 1 (good still loaded)", len(all))
	}
}

func TestLoader_EmptyDirString(t *testing.T) {
	l := NewLoader("")
	if _, err := l.LoadAll(); err != nil {
		t.Errorf("empty dir should not error: %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
