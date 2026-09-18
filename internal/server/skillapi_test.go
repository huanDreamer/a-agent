package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/prompt"
	"github.com/huan/huan-agent/internal/skill"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// newSkillHarness starts a server with a skills directory containing the given
// files, a fresh state file, and a real tool registry.
func newSkillHarness(t *testing.T, files map[string]string, tools *tool.Registry) (*harness, string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := writeFile(filepath.Join(dir, name), content); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{Tools: tools}})
	srv.skillState = newSkillState(dir, filepath.Join(t.TempDir(), "skills.json"))
	if err := srv.registerSkillTool(); err != nil {
		t.Fatalf("registerSkillTool: %v", err)
	}
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)
	return h, dir
}

const sampleSkill = `---
name: code-review
description: 审查代码风格与潜在缺陷
tools:
  - read_file
---

# 代码审查

1. 用 read_file 读取目标文件。
2. 按清单检查。
`

func TestSkillsAPI_ListReportsFiles(t *testing.T) {
	h, _ := newSkillHarness(t, map[string]string{"code-review.md": sampleSkill}, nil)

	var out struct {
		Skills []skillInfo `json:"skills"`
	}
	h.getJSON(t, "/api/skills", http.StatusOK, &out)
	if len(out.Skills) != 1 {
		t.Fatalf("skills = %+v", out.Skills)
	}
	got := out.Skills[0]
	if got.Name != "code-review" || !got.Enabled {
		t.Errorf("skill = %+v", got)
	}
	if got.File != "code-review.md" || got.Bytes == 0 {
		t.Errorf("file/bytes not reported: %+v", got)
	}
	if len(got.Tools) != 1 || got.Tools[0] != "read_file" {
		t.Errorf("tools = %v", got.Tools)
	}
}

func TestSkillsAPI_DetailReturnsTheBody(t *testing.T) {
	h, _ := newSkillHarness(t, map[string]string{"code-review.md": sampleSkill}, nil)

	var out struct {
		Skill skillDetail `json:"skill"`
	}
	h.getJSON(t, "/api/skills/code-review", http.StatusOK, &out)
	if !strings.Contains(out.Skill.Body, "代码审查") {
		t.Errorf("body = %q", out.Skill.Body)
	}
	if out.Skill.Description != "审查代码风格与潜在缺陷" {
		t.Errorf("description = %q", out.Skill.Description)
	}
	requireStatus(t, h.get(t, "/api/skills/nope"), http.StatusNotFound)
}

func TestSkillsAPI_SaveThenReload(t *testing.T) {
	reg := tool.NewRegistry()
	for _, name := range []string{"read_file", "write_file"} {
		if err := reg.Register(&stubTool{name: name, desc: name, run: func(context.Context, string) (string, error) {
			return "", nil
		}}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	h, dir := newSkillHarness(t, nil, reg)

	body := map[string]any{
		"description": "生成日报",
		"tools":       []string{"read_file", "write_file"},
		"body":        "# 日报\n\n1. 汇总今天的改动。\n",
	}
	resp := h.putJSON(t, "/api/skills/daily-summary", body)
	requireStatus(t, resp, http.StatusOK)

	// The file exists on disk in the shape internal/skill parses back.
	raw, err := os.ReadFile(filepath.Join(dir, "daily-summary.md"))
	if err != nil {
		t.Fatalf("read saved skill: %v", err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, "---\n") || !strings.Contains(text, "name: daily-summary") {
		t.Fatalf("file =\n%s", text)
	}

	// …and the API reads back what was written.
	var out struct {
		Skill skillDetail `json:"skill"`
	}
	h.getJSON(t, "/api/skills/daily-summary", http.StatusOK, &out)
	if out.Skill.Description != "生成日报" || !strings.Contains(out.Skill.Body, "汇总今天的改动") {
		t.Errorf("read back = %+v", out.Skill)
	}
	if len(out.Skill.Tools) != 2 {
		t.Errorf("tools = %v", out.Skill.Tools)
	}
}

func TestSkillsAPI_SaveReplacesInPlace(t *testing.T) {
	h, dir := newSkillHarness(t, map[string]string{"code-review.md": sampleSkill}, nil)

	// A skill whose file name and frontmatter name agree is edited in place, and
	// nothing extra is created.
	resp := h.putJSON(t, "/api/skills/code-review", map[string]any{
		"description": "改过的描述", "body": "# 新的正文\n",
	})
	requireStatus(t, resp, http.StatusOK)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir has %d files, want 1: %v", len(entries), entries)
	}
	var out struct {
		Skill skillDetail `json:"skill"`
	}
	h.getJSON(t, "/api/skills/code-review", http.StatusOK, &out)
	if out.Skill.Description != "改过的描述" || len(out.Skill.Tools) != 0 {
		t.Errorf("skill = %+v", out.Skill)
	}
}

func TestSkillsAPI_SaveValidates(t *testing.T) {
	h, _ := newSkillHarness(t, nil, nil)
	cases := []struct {
		name string
		path string
		body map[string]any
		want string
	}{
		{"empty body", "/api/skills/x", map[string]any{"body": "  "}, "body is required"},
		{"bad name", "/api/skills/Bad Name", map[string]any{"body": "hi"}, "slug"},
		{"dot prefix", "/api/skills/..escape", map[string]any{"body": "hi"}, "slug"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.putJSON(t, tc.path, tc.body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var out map[string]string
			_ = json.NewDecoder(resp.Body).Decode(&out)
			if !strings.Contains(out["error"], tc.want) {
				t.Errorf("error = %q, want %q", out["error"], tc.want)
			}
		})
	}
}

// TestSkillsAPI_TraversalIsRefused covers the one name that must never become a
// file path. Hertz answers 404 for a path it will not route (../ cannot appear
// in a matched segment) and the handler answers 400 for a name that reaches it,
// so the test accepts either — what matters is that nothing is written.
func TestSkillsAPI_TraversalIsRefused(t *testing.T) {
	h, dir := newSkillHarness(t, nil, nil)
	for _, path := range []string{"/api/skills/..%2Fescape", "/api/skills/" + url.PathEscape("../escape.md")} {
		resp := h.putJSON(t, path, map[string]any{"body": "hi"})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 400 or 404", path, resp.StatusCode)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("dir = %v, want nothing written", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.md")); !os.IsNotExist(err) {
		t.Errorf("a file escaped the skills directory: %v", err)
	}
}

func TestSkillsAPI_SaveDropsUnknownTools(t *testing.T) {
	reg := tool.NewRegistry()
	if err := reg.Register(&stubTool{name: "read_file", desc: "read", run: func(context.Context, string) (string, error) {
		return "", nil
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	h, _ := newSkillHarness(t, nil, reg)

	var out struct {
		Skill        skillDetail `json:"skill"`
		DroppedTools []string    `json:"dropped_tools"`
	}
	resp := h.putJSON(t, "/api/skills/mixed", map[string]any{
		"body":  "正文",
		"tools": []string{"read_file", "does_not_exist"},
	})
	decode(t, resp, &out)

	// A tool name this build does not have would make the skill's allow-list
	// unusable, so it is removed and reported.
	if len(out.DroppedTools) != 1 || out.DroppedTools[0] != "does_not_exist" {
		t.Errorf("dropped = %v", out.DroppedTools)
	}
	if len(out.Skill.Tools) != 1 || out.Skill.Tools[0] != "read_file" {
		t.Errorf("tools = %v", out.Skill.Tools)
	}
}

func TestSkillsAPI_DeleteRemovesTheFile(t *testing.T) {
	h, dir := newSkillHarness(t, map[string]string{"code-review.md": sampleSkill}, nil)

	requireStatus(t, h.deleteJSON(t, "/api/skills/code-review"), http.StatusOK)
	if _, err := os.Stat(filepath.Join(dir, "code-review.md")); !os.IsNotExist(err) {
		t.Errorf("file still exists: %v", err)
	}
	requireStatus(t, h.deleteJSON(t, "/api/skills/code-review"), http.StatusNotFound)
}

func TestSkillsAPI_SaveClearsADisabledOverride(t *testing.T) {
	h, _ := newSkillHarness(t, map[string]string{"code-review.md": sampleSkill}, nil)

	requireStatus(t, h.postJSON(t, "/api/skills/code-review", map[string]any{"enabled": false}), http.StatusOK)
	var list struct {
		Skills []skillInfo `json:"skills"`
	}
	h.getJSON(t, "/api/skills", http.StatusOK, &list)
	if list.Skills[0].Enabled {
		t.Fatal("skill should be disabled")
	}

	// Rewriting a disabled skill re-enables it: a save is an act of bringing it
	// back, and leaving it disabled would silently discard the edit.
	requireStatus(t, h.putJSON(t, "/api/skills/code-review", map[string]any{"body": "新正文"}), http.StatusOK)
	h.getJSON(t, "/api/skills", http.StatusOK, &list)
	if !list.Skills[0].Enabled {
		t.Error("skill should be enabled after a save")
	}
}

func TestSkillsAPI_SkillToolLoadsABody(t *testing.T) {
	reg := tool.NewRegistry()
	h, _ := newSkillHarness(t, map[string]string{"code-review.md": sampleSkill}, reg)

	// The skill tool is registered into the chat's registry, so the model can
	// load a skill's instructions on demand.
	got, ok := reg.Get(skillToolName)
	if !ok {
		t.Fatalf("registry = %v, want the %s tool", reg.Names(), skillToolName)
	}
	out, err := got.InvokableRun(context.Background(), `{"name":"code-review"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if !strings.Contains(out, "代码审查") {
		t.Errorf("result = %q, want the skill body", out)
	}
	if !strings.Contains(out, "read_file") {
		t.Errorf("result = %q, want the declared tools noted", out)
	}

	// An unknown name fails with the available names, which is what lets the
	// model correct itself.
	if _, err := got.InvokableRun(context.Background(), `{"name":"nope"}`); err == nil {
		t.Error("expected an error for an unknown skill")
	}
	_ = h
}

func TestSkillsAPI_DisabledSkillIsNotOffered(t *testing.T) {
	reg := tool.NewRegistry()
	h, _ := newSkillHarness(t, map[string]string{"code-review.md": sampleSkill}, reg)

	requireStatus(t, h.postJSON(t, "/api/skills/code-review", map[string]any{"enabled": false}), http.StatusOK)

	got, _ := reg.Get(skillToolName)
	if _, err := got.InvokableRun(context.Background(), `{"name":"code-review"}`); err == nil {
		t.Error("a disabled skill must not be loadable")
	}
	if section := h.srv.skillsPromptSection(); section != "" {
		t.Errorf("prompt section = %q, want empty when nothing is enabled", section)
	}
}

func TestSkillPromptSectionListsEnabledSkills(t *testing.T) {
	h, _ := newSkillHarness(t, map[string]string{
		"code-review.md":   sampleSkill,
		"daily-summary.md": "---\nname: daily-summary\ndescription: 生成日报\n---\n\n正文\n",
	}, nil)

	section := h.srv.skillsPromptSection()
	if !strings.Contains(section, "code-review") || !strings.Contains(section, "daily-summary") {
		t.Fatalf("section = %q", section)
	}
	if !strings.Contains(section, "审查代码风格与潜在缺陷") {
		t.Errorf("section = %q, want descriptions", section)
	}
	if !strings.Contains(section, skillToolName) {
		t.Errorf("section = %q, want it to name the tool to call", section)
	}
	// The instructions themselves stay out of the prompt: that is the whole
	// point of loading them through the tool.
	if strings.Contains(section, "按清单检查") {
		t.Errorf("section = %q, want bodies left out", section)
	}
	// The full prompt is the console's default plus this section.
	full := h.srv.systemPrompt()
	if !strings.Contains(full, section) || !strings.Contains(full, prompt.For(prompt.SurfaceWeb)) {
		t.Errorf("systemPrompt = %q", full)
	}
}

func TestSkillToolIsNotRegisteredWithoutADirectory(t *testing.T) {
	reg := tool.NewRegistry()
	srv, _ := buildServerWith(t, buildOpts{chat: ChatDeps{Tools: reg}})
	srv.skillState = newSkillState("", "")
	if err := srv.registerSkillTool(); err != nil {
		t.Fatalf("registerSkillTool: %v", err)
	}
	if _, ok := reg.Get(skillToolName); ok {
		t.Error("no skills directory means no skill tool")
	}
}

func TestRenderSkillFileRoundTrips(t *testing.T) {
	content, err := renderSkillFile("demo", skillDraft{
		Description: "带: 冒号 \"引号\" 的描述",
		Tools:       []string{"read_file"},
		Body:        "# 标题\n\n正文\n",
	})
	if err != nil {
		t.Fatalf("renderSkillFile: %v", err)
	}
	// The rendered file must parse back into exactly what was written: the
	// console writes files the agent then loads with internal/skill.
	loaded := loadSkillFile(t, "demo", string(content))
	if loaded.Frontmatter.Description != "带: 冒号 \"引号\" 的描述" {
		t.Errorf("description = %q", loaded.Frontmatter.Description)
	}
	if len(loaded.Frontmatter.Tools) != 1 || loaded.Frontmatter.Tools[0] != "read_file" {
		t.Errorf("tools = %v", loaded.Frontmatter.Tools)
	}
	if strings.TrimSpace(loaded.Body) != "# 标题\n\n正文" {
		t.Errorf("body = %q", loaded.Body)
	}
}

func TestSkillsAPI_DraftWritesASkillFromTheModel(t *testing.T) {
	model := &jsonModel{answer: `{
	  "name": "release-notes",
	  "summary": "根据 git 记录写发布说明",
	  "description": "当用户要求整理发布说明时使用",
	  "tools": ["read_file", "not_a_tool"],
	  "body": "# 发布说明\n\n1. 用 read_file 读取 CHANGELOG。\n"
	}`}
	reg := tool.NewRegistry()
	if err := reg.Register(&stubTool{name: "read_file", desc: "read", run: func(context.Context, string) (string, error) {
		return "", nil
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	dir := t.TempDir()
	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{
		Tools:   reg,
		Builder: &staticBuilder{provider: "deepseek", model: "deepseek-chat", cm: model},
	}})
	srv.skillState = newSkillState(dir, filepath.Join(t.TempDir(), "skills.json"))
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)

	var out struct {
		Draft struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Tools       []string `json:"tools"`
			Body        string   `json:"body"`
		} `json:"draft"`
		Warnings []string          `json:"warnings"`
		Model    map[string]string `json:"model"`
	}
	resp := h.postJSON(t, "/api/skills-draft", map[string]any{"description": "帮我写一个整理发布说明的技能"})
	decode(t, resp, &out)

	if out.Draft.Name != "release-notes" || !strings.Contains(out.Draft.Body, "CHANGELOG") {
		t.Fatalf("draft = %+v", out.Draft)
	}
	// A tool this build does not have is dropped from the draft and reported.
	if len(out.Draft.Tools) != 1 || out.Draft.Tools[0] != "read_file" {
		t.Errorf("tools = %v", out.Draft.Tools)
	}
	if !strings.Contains(strings.Join(out.Warnings, " | "), "not_a_tool") {
		t.Errorf("warnings = %v", out.Warnings)
	}
	if out.Model["provider"] != "deepseek" {
		t.Errorf("model = %v", out.Model)
	}

	// Nothing was written: a draft is reviewed first.
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("dir = %v, want nothing saved by a draft", entries)
	}

	// …and saving the reviewed draft works.
	requireStatus(t, h.putJSON(t, "/api/skills/release-notes", map[string]any{
		"description": out.Draft.Description, "tools": out.Draft.Tools, "body": out.Draft.Body,
	}), http.StatusOK)
	h.getJSON(t, "/api/skills", http.StatusOK, &struct {
		Skills []skillInfo `json:"skills"`
	}{})
}

func TestSkillsAPI_DraftHonoursAnExplicitName(t *testing.T) {
	model := &jsonModel{answer: `{"name":"ignored","description":"d","tools":[],"body":"# x"}`}
	dir := t.TempDir()
	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{
		Tools:   tool.NewRegistry(),
		Builder: &staticBuilder{provider: "p", model: "m", cm: model},
	}})
	srv.skillState = newSkillState(dir, filepath.Join(t.TempDir(), "skills.json"))
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)

	var out struct {
		Draft struct {
			Name string `json:"name"`
		} `json:"draft"`
	}
	resp := h.postJSON(t, "/api/skills-draft", map[string]any{"description": "x", "name": "chosen-name"})
	decode(t, resp, &out)
	if out.Draft.Name != "chosen-name" {
		t.Errorf("name = %q, want the operator's choice to win", out.Draft.Name)
	}
}

func TestSkillsAPI_DraftWarnsAboutAnUnusableName(t *testing.T) {
	model := &jsonModel{answer: `{"name":"中文名","description":"d","tools":[],"body":"# x"}`}
	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{
		Tools:   tool.NewRegistry(),
		Builder: &staticBuilder{provider: "p", model: "m", cm: model},
	}})
	srv.skillState = newSkillState(t.TempDir(), filepath.Join(t.TempDir(), "skills.json"))
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)

	var out struct {
		Warnings []string `json:"warnings"`
	}
	resp := h.postJSON(t, "/api/skills-draft", map[string]any{"description": "x"})
	decode(t, resp, &out)
	if !strings.Contains(strings.Join(out.Warnings, " | "), "文件名") {
		t.Errorf("warnings = %v, want a note that the name cannot be a file name", out.Warnings)
	}
}

func TestSkillsAPI_DraftNeedsADescription(t *testing.T) {
	model := &jsonModel{answer: `{}`}
	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{
		Tools:   tool.NewRegistry(),
		Builder: &staticBuilder{provider: "p", model: "m", cm: model},
	}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)
	requireStatus(t, h.postJSON(t, "/api/skills-draft", map[string]any{}), http.StatusBadRequest)
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"plain", `{"a":1}`, true},
		{"fenced", "```json\n{\"a\":1}\n```", true},
		{"prose around", "好的： {\"a\":1} 就这样", true},
		{"brace in string", `{"body":"a { b } c","n":2}`, true},
		{"escaped quote", `{"body":"say \"hi\"","n":2}`, true},
		{"nested", `{"a":{"b":{"c":1}}}`, true},
		{"no object", "我不知道", false},
		{"unterminated", `{"a":1`, false},
		{"invalid json", `{"a": }`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractJSONObject(tc.in)
			if !tc.ok {
				if err == nil {
					t.Fatalf("got %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractJSONObject: %v", err)
			}
			if got == nil {
				t.Fatal("got nil object")
			}
		})
	}
}

func TestExtractJSONObjectKeepsNestedContent(t *testing.T) {
	got, err := extractJSONObject("prose {\"body\":\"a { b } c\",\"n\":2} more prose")
	if err != nil {
		t.Fatalf("extractJSONObject: %v", err)
	}
	if got["body"] != "a { b } c" || got["n"].(float64) != 2 {
		t.Errorf("got = %v", got)
	}
}

func TestStringFieldCoercions(t *testing.T) {
	fields := map[string]any{"s": " x ", "n": float64(3000), "b": true, "obj": map[string]any{}}
	if got := stringField(fields, "s"); got != "x" {
		t.Errorf("string = %q", got)
	}
	if got := stringField(fields, "n"); got != "3000" {
		t.Errorf("number = %q", got)
	}
	if got := stringField(fields, "b"); got != "true" {
		t.Errorf("bool = %q", got)
	}
	if got := stringField(fields, "obj"); got != "" {
		t.Errorf("object = %q, want empty", got)
	}
	if got := stringField(fields, "missing"); got != "" {
		t.Errorf("missing = %q", got)
	}
}

func TestStringListFieldCoercions(t *testing.T) {
	fields := map[string]any{
		"single": "one",
		"list":   []any{"a", 1.0, "", "b"},
		"bad":    map[string]any{},
	}
	if got := stringListField(fields, "single"); len(got) != 1 || got[0] != "one" {
		t.Errorf("single = %v", got)
	}
	if got := stringListField(fields, "list"); len(got) != 3 || got[0] != "a" || got[2] != "b" {
		t.Errorf("list = %v", got)
	}
	if got := stringListField(fields, "bad"); got != nil {
		t.Errorf("bad = %v", got)
	}
}

func TestSyncConfigSkillsNothingToDo(t *testing.T) {
	// A store with no mcp rows and no config entries is the common case; it must
	// be a no-op rather than an error.
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	n, err := SyncConfigServers(context.Background(), st, nil, zap.NewNop())
	if err != nil || n != 0 {
		t.Fatalf("SyncConfigServers = %d, %v", n, err)
	}
}

// loadSkillFile parses a rendered skill with the real loader, so a change in
// the rendering is caught here rather than in production.
func loadSkillFile(t *testing.T, name, content string) *skill.Skill {
	t.Helper()
	parsed, err := skill.LoadFile(name, content)
	if err != nil {
		t.Fatalf("skill.LoadFile: %v", err)
	}
	return parsed
}
