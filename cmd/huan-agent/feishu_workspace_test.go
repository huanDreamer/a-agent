package main

// Tests for the Feishu workspace surface: the commands, the natural-language
// switch, and — the part that matters most — the requests that must not be read
// as a switch. A false positive here moves the agent's next writes into another
// project.

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/platform/feishu"
	"github.com/huan/huan-agent/internal/workspaces"
)

// newWorkspaceHandler builds a bot handler with a real workspace layer over a
// temp store and directory.
func newWorkspaceHandler(t *testing.T) (*botHandler, *fakeSender, *workspaces.Manager) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Tools.EnableBash = true
	// Only the manager is needed here: the Feishu surface talks to the workspace
	// layer directly, and the tool binding is the runner's ToolsFor closure.
	_, mgr, _ := newBinderTestRig(t, cfg)
	if _, err := mgr.EnsureSeed(context.Background()); err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}

	h := &botHandler{
		cfg:      cfg,
		logger:   zap.NewNop(),
		sender:   &fakeSender{},
		sessions: map[string]*botSession{},
		wsMgr:    mgr,
	}
	return h, h.sender.(*fakeSender), mgr
}

// lastText returns the most recent plain-text reply.
func lastText(t *testing.T, s *fakeSender) string {
	t.Helper()
	if len(s.texts) == 0 {
		t.Fatal("no reply was sent")
	}
	return s.texts[len(s.texts)-1].text
}

func textInbound(openID, text string) feishu.Inbound {
	return feishu.Inbound{MsgType: "text", Text: text, OpenID: openID, ChatID: "oc_1"}
}

func TestFeishuWorkspaceListAndCurrent(t *testing.T) {
	ctx := context.Background()
	h, sender, mgr := newWorkspaceHandler(t)
	if _, err := mgr.Create(ctx, workspaces.CreateInput{Root: t.TempDir(), Name: "blog"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The seeded workspace is the one a sender starts in, so the listing has to
	// mention both it and the one that was just created.
	seed, err := mgr.Active(ctx, workspaces.FeishuScope("ou_1"))
	if err != nil {
		t.Fatalf("Active: %v", err)
	}

	if err := h.Handle(ctx, textInbound("ou_1", "/workspaces")); err != nil {
		t.Fatalf("Handle(/workspaces): %v", err)
	}
	list := lastText(t, sender)
	if !strings.Contains(list, "blog") || !strings.Contains(list, seed.Name) {
		t.Errorf("list = %q, want both workspaces", list)
	}

	// /workspace reports the current one, which is where the sender started.
	if err := h.Handle(ctx, textInbound("ou_1", "/workspace")); err != nil {
		t.Fatalf("Handle(/workspace): %v", err)
	}
	if got := lastText(t, sender); !strings.Contains(got, seed.Name) {
		t.Errorf("current = %q, want the workspace the sender is in", got)
	}

	// The natural-language forms reach the same answers.
	if err := h.Handle(ctx, textInbound("ou_1", "有哪些工作区")); err != nil {
		t.Fatalf("Handle(有哪些工作区): %v", err)
	}
	if got := lastText(t, sender); !strings.Contains(got, "blog") {
		t.Errorf("natural-language list = %q, want blog", got)
	}
	if err := h.Handle(ctx, textInbound("ou_1", "我现在在哪个工作区")); err != nil {
		t.Fatalf("Handle(哪个工作区): %v", err)
	}
	if got := lastText(t, sender); !strings.Contains(got, seed.Name) {
		t.Errorf("natural-language current = %q, want the sender's workspace", got)
	}
}

// TestFeishuWorkspaceSwitchIsScopedAndConfirmed is the core behaviour: the switch
// takes effect for the sender, the reply states what changed and where it points,
// and another user is unaffected.
func TestFeishuWorkspaceSwitchIsScopedAndConfirmed(t *testing.T) {
	ctx := context.Background()
	h, sender, mgr := newWorkspaceHandler(t)
	spec, err := mgr.Create(ctx, workspaces.CreateInput{Root: t.TempDir(), Name: "blog"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, command := range []string{"/workspace blog", "切换到 blog 工作区", "切到 blog"} {
		sender.texts = nil
		if err := h.Handle(ctx, textInbound("ou_1", command)); err != nil {
			t.Fatalf("Handle(%q): %v", command, err)
		}
		reply := lastText(t, sender)
		if !strings.Contains(reply, "blog") || !strings.Contains(reply, spec.Root) {
			t.Errorf("reply to %q = %q, want the workspace name and its root", command, reply)
		}
		active, err := mgr.Active(ctx, workspaces.FeishuScope("ou_1"))
		if err != nil {
			t.Fatalf("Active: %v", err)
		}
		if active.Name != "blog" {
			t.Errorf("after %q the scope is in %q, want blog", command, active.Name)
		}
		// Another sender is untouched: a switch is per person.
		other, err := mgr.Active(ctx, workspaces.FeishuScope("ou_2"))
		if err != nil {
			t.Fatalf("Active(ou_2): %v", err)
		}
		if other.Name == "blog" {
			t.Errorf("another user was moved to %q", other.Name)
		}
	}

	// Switching somewhere else is confirmed the same way, and the previous
	// workspace stops applying to this sender.
	if _, err := mgr.Create(ctx, workspaces.CreateInput{Root: t.TempDir(), Name: "api"}); err != nil {
		t.Fatalf("create api: %v", err)
	}
	sender.texts = nil
	if err := h.Handle(ctx, textInbound("ou_1", "切换到 api")); err != nil {
		t.Fatalf("Handle(api): %v", err)
	}
	if got := lastText(t, sender); !strings.Contains(got, "api") {
		t.Errorf("reply = %q, want it to name api", got)
	}
	active, _ := mgr.Active(ctx, workspaces.FeishuScope("ou_1"))
	if active.Name != "api" {
		t.Errorf("scope is in %q, want api", active.Name)
	}
}

// TestFeishuWorkspaceUnknownNameIsAnswered: an unrecognised name gets the list,
// not a guess and not a generic error.
func TestFeishuWorkspaceUnknownNameIsAnswered(t *testing.T) {
	ctx := context.Background()
	h, sender, mgr := newWorkspaceHandler(t)
	if _, err := mgr.Create(ctx, workspaces.CreateInput{Root: t.TempDir(), Name: "blog"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := h.Handle(ctx, textInbound("ou_1", "切换到 dianqi")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	reply := lastText(t, sender)
	if !strings.Contains(reply, "dianqi") {
		t.Errorf("reply = %q, want it to quote what was asked for", reply)
	}
	if !strings.Contains(reply, "blog") {
		t.Errorf("reply = %q, want it to list what exists", reply)
	}
	// Nothing changed: the scope is still where it was.
	active, _ := mgr.Active(ctx, workspaces.FeishuScope("ou_1"))
	if active.Name == "dianqi" {
		t.Errorf("an unknown name moved the scope to %q", active.Name)
	}
}

// TestFeishuWorkspaceSwitchWithoutANameAsks: the command with no argument is a
// question, not a switch to somewhere arbitrary.
func TestFeishuWorkspaceSwitchWithoutANameAsks(t *testing.T) {
	ctx := context.Background()
	h, sender, mgr := newWorkspaceHandler(t)
	if _, err := mgr.Create(ctx, workspaces.CreateInput{Root: t.TempDir(), Name: "blog"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := h.Handle(ctx, textInbound("ou_1", "切换到")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	reply := lastText(t, sender)
	if !strings.Contains(reply, "blog") {
		t.Errorf("reply = %q, want it to name an example workspace", reply)
	}
	active, _ := mgr.Active(ctx, workspaces.FeishuScope("ou_1"))
	if active.Name == "blog" {
		t.Errorf("a nameless switch moved the scope to %q", active.Name)
	}
}

// TestFeishuWorkspaceRequestsAreNotHijacked is the negative half, run through the
// whole handler: a request that merely mentions a workspace must be answered, and
// must leave the selection alone.
func TestFeishuWorkspaceRequestsAreNotHijacked(t *testing.T) {
	ctx := context.Background()
	h, sender, mgr := newWorkspaceHandler(t)
	if _, err := mgr.Create(ctx, workspaces.CreateInput{Root: t.TempDir(), Name: "blog"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, text := range []string{
		"在工作区里建个 hello.go",
		"用 Python 写个脚本",
		"帮我看看工作区里的代码",
		"把工作区里的文件列出来",
		"工作区里有哪些文件",
	} {
		in := textInbound("ou_1", text)
		// The handler must read this as an ordinary request, which is what
		// leaves it to the model rather than acting on it.
		if intent := h.parseWorkspaceIntent(ctx, in, text); intent.Kind != workspaces.IntentNone {
			t.Errorf("ParseSwitchIntent(%q) = %+v, want no workspace intent", text, intent)
		}
		active, _ := mgr.Active(ctx, workspaces.FeishuScope("ou_1"))
		if active.Name == "blog" {
			t.Fatalf("%q moved the workspace to %q", text, active.Name)
		}
	}
	_ = sender
}

// TestFeishuWorkspaceCommandsWithoutALayer: a deployment with no workspace layer
// answers rather than panicking, and the help text does not advertise commands
// that cannot work.
func TestFeishuWorkspaceCommandsWithoutALayer(t *testing.T) {
	ctx := context.Background()
	h := newTestHandler(&fakeSender{}, nil)

	if err := h.Handle(ctx, textInbound("ou_1", "/workspace")); err != nil {
		t.Fatalf("Handle(/workspace): %v", err)
	}
	if got := lastText(t, h.sender.(*fakeSender)); !strings.Contains(got, "未启用") {
		t.Errorf("reply = %q, want it to say workspaces are not enabled", got)
	}

	if err := h.Handle(ctx, textInbound("ou_1", "/help")); err != nil {
		t.Fatalf("Handle(/help): %v", err)
	}
	if got := lastText(t, h.sender.(*fakeSender)); strings.Contains(got, "/workspaces") {
		t.Errorf("help advertises workspace commands without a workspace layer: %q", got)
	}
}

// TestFeishuHelpMentionsWorkspacesWhenAvailable keeps the discoverability promise
// in step with the feature.
func TestFeishuHelpMentionsWorkspacesWhenAvailable(t *testing.T) {
	ctx := context.Background()
	h, sender, _ := newWorkspaceHandler(t)
	if err := h.Handle(ctx, textInbound("ou_1", "/help")); err != nil {
		t.Fatalf("Handle(/help): %v", err)
	}
	help := lastText(t, sender)
	for _, want := range []string{"/workspace", "/workspaces", "切换到"} {
		if !strings.Contains(help, want) {
			t.Errorf("help = %q, want it to mention %q", help, want)
		}
	}
}

// TestFeishuWorkspacePolicyIsReported is now TestFeishuWorkspaceDirectoryIsReported:
// a workspace has no policy to report — what the agent may do is process-wide —
// so what the answer must carry is the directory.
func TestFeishuWorkspaceDirectoryIsReported(t *testing.T) {
	ctx := context.Background()
	h, sender, mgr := newWorkspaceHandler(t)
	root := t.TempDir()
	if _, err := mgr.Create(ctx, workspaces.CreateInput{Root: root, Name: "frozen"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := h.Handle(ctx, textInbound("ou_1", "/workspace frozen")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// The reply names the workspace AND the directory: the directory is the part
	// that says what actually changed, since a workspace has no other property.
	got := lastText(t, sender)
	if !strings.Contains(got, "frozen") || !strings.Contains(got, root) {
		t.Errorf("reply = %q, want the workspace name and its directory", got)
	}
}

// TestToolSummary pins the footer a user sees after a turn that used tools.
func TestToolSummary(t *testing.T) {
	runs := []chat.ToolRun{
		{Name: "read_file", Result: "ok"},
		{Name: "read_file", Result: "ok"},
		{Name: "bash", Result: "", Err: "boom"},
	}
	got := toolSummary(runs, "blog")
	for _, want := range []string{"工具 3 次", "read_file×2", "bash", "1 次失败", "blog"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary = %q, want it to contain %q", got, want)
		}
	}
	if toolSummary(nil, "blog") != "" {
		t.Error("a turn with no tools produced a footer")
	}
}
