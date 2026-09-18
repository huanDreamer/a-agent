package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// lastSeen returns the most recent turn's history from the package's shared
// recordingModel, which is what a test reads to see what a turn was told.
func (m *recordingModel) lastSeen() []*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.seen) == 0 {
		return nil
	}
	return m.seen[len(m.seen)-1]
}

// seenCount is how many turns reached the model.
func (m *recordingModel) seenCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.seen)
}

// resumeHarness is a running server with the plan feature wired, plus one
// conversation.
func resumeHarness(t *testing.T, mdl *recordingModel) (*harness, string) {
	t.Helper()
	runner, err := chat.New(chat.Config{Model: mdl, MaxSteps: 4, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{
		Runner:       runner,
		PlanEnable:   true,
		PlanMaxTasks: defaultPlanMaxTasks,
	}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)

	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decode(t, h.postJSON(t, "/api/chat/sessions", map[string]string{"title": "resume"}), &created)
	if created.Session.ID == "" {
		t.Fatal("no session id")
	}
	return h, created.Session.ID
}

// seedFailedTurn stores the shape a died turn leaves behind: the user's goal, an
// assistant row carrying the steps it managed and the error that stopped it.
func seedFailedTurn(t *testing.T, st store.Store, sessionID, goal, runErr string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.AppendChatMessage(ctx, sessionID, store.ChatMessage{
		Role: store.RoleUser, Content: goal,
	}); err != nil {
		t.Fatalf("seed user message: %v", err)
	}
	steps, err := json.Marshal([]chat.Step{
		{
			Index: 1,
			Text:  "先读一下 runner.go",
			Tools: []chat.ToolRun{{
				ID: "c1", Name: "read_file", Args: `{"path":"internal/chat/runner.go"}`,
				Result: "package chat ...", Step: 1,
			}},
		},
		{
			Index: 2,
			Text:  "跑一下测试",
			Tools: []chat.ToolRun{{
				ID: "c2", Name: "bash", Args: `{"command":"go test ./internal/chat/"}`,
				Err: "exit status 1", Step: 2,
			}},
		},
	})
	if err != nil {
		t.Fatalf("encode steps: %v", err)
	}
	if _, err := st.AppendChatMessage(ctx, sessionID, store.ChatMessage{
		Role:      store.RoleAssistant,
		Content:   "我看到 runner.go 里…",
		Steps:     string(steps),
		Error:     runErr,
		TraceID:   "trace-1",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed assistant message: %v", err)
	}
}

func seedPlan(t *testing.T, st store.Store, sessionID string, tasks []tool.Task) {
	t.Helper()
	b, err := json.Marshal(tasks)
	if err != nil {
		t.Fatalf("encode tasks: %v", err)
	}
	if err := st.SetChatPlan(context.Background(), store.ChatPlanRow{
		SessionID: sessionID, Goal: "把续跑做出来", TasksJSON: string(b), Revision: 2,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
}

// messagesOf renders the history a turn was sent, for assertions that read like
// sentences.
func messagesOf(history []*schema.Message) string {
	var b strings.Builder
	for _, m := range history {
		b.WriteString("[" + string(m.Role) + "] " + m.Content + "\n")
	}
	return b.String()
}

func TestResume_InjectsThePreviousAttemptAndThePlan(t *testing.T) {
	mdl := &recordingModel{}
	h, sessionID := resumeHarness(t, mdl)
	seedFailedTurn(t, h.store, sessionID, "把 chat 的失败续跑做出来", "llm deepseek/deepseek-chat: status=503")
	seedPlan(t, h.store, sessionID, []tool.Task{
		{ID: "t1", Title: "读 runner 的循环", Status: tool.TaskDone, Note: "确认了 step 边界"},
		{ID: "t2", Title: "加步骤级重试", Status: tool.TaskInProgress},
		{ID: "t3", Title: "跑测试", Status: tool.TaskPending},
	})

	resp := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/resume", map[string]string{})
	if resp.StatusCode != http.StatusAccepted {
		body := readBody(t, resp)
		t.Fatalf("resume status = %d, want 202 (%s)", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	waitForCondition(t, 5*time.Second, "the resumed turn to reach the model", func() bool {
		return mdl.seenCount() > 0
	})
	waitForCondition(t, 5*time.Second, "the resumed turn to be stored", func() bool {
		msgs, err := h.store.ListChatMessages(context.Background(), sessionID, 0)
		if err != nil {
			return false
		}
		return len(msgs) > 0 && msgs[len(msgs)-1].Role == store.RoleAssistant
	})

	history := messagesOf(mdl.lastSeen())
	for _, want := range []string{
		"[接续执行]",
		"把 chat 的失败续跑做出来", // the goal
		"status=503",      // why it stopped
		"读 runner 的循环",    // the plan, with real task titles
		"read_file",       // what was already run
		"跑一下测试",           // and what ran and failed
		"exit status 1",   // including the failure
		"不要重做",            // the instruction that makes it a resume
		"继续执行（还有 2 项未完成）", // the visible user turn, last
	} {
		if !strings.Contains(history, want) {
			t.Errorf("the resumed turn was not told %q\n--- history ---\n%s", want, history)
		}
	}
	// The last message must still be the user's, or the model's instruction is
	// buried in the middle of its context.
	seen := mdl.lastSeen()
	last := seen[len(seen)-1]
	if last.Role != schema.User || !strings.Contains(last.Content, "继续执行") {
		t.Fatalf("last message = [%s] %q, want the user's 继续执行", last.Role, last.Content)
	}
	// And the transcript shows one short line, not the briefing.
	msgs, err := h.store.ListChatMessages(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	var sawContinue bool
	for _, m := range msgs {
		if m.Role == store.RoleUser && strings.Contains(m.Content, "继续执行") {
			sawContinue = true
			if strings.Contains(m.Content, "[接续执行]") {
				t.Fatal("the briefing was stored as the user's own message")
			}
		}
	}
	if !sawContinue {
		t.Fatal("the resume left no visible user message")
	}
}

func TestResume_PlanReadEndToEnd(t *testing.T) {
	// The board reads the plan from the conversation, so a reloaded page shows it
	// without waiting for the next turn to touch it.
	mdl := &recordingModel{}
	h, sessionID := resumeHarness(t, mdl)
	seedPlan(t, h.store, sessionID, []tool.Task{
		{ID: "t1", Title: "一件", Status: tool.TaskDone},
		{ID: "t2", Title: "两件", Status: tool.TaskPending},
	})

	var body struct {
		Plan *tool.Plan `json:"plan"`
	}
	decode(t, h.get(t, "/api/chat/sessions/"+sessionID), &body)
	if body.Plan == nil {
		t.Fatal("GET session returned no plan although one is stored")
	}
	if len(body.Plan.Tasks) != 2 || body.Plan.Tasks[0].Status != tool.TaskDone {
		t.Fatalf("plan = %+v, want the stored tasks", body.Plan)
	}
	if body.Plan.Revision != 2 {
		t.Fatalf("revision = %d, want 2", body.Plan.Revision)
	}
}

func TestResume_WithoutATaskToContinueIsRefused(t *testing.T) {
	mdl := &recordingModel{}
	h, sessionID := resumeHarness(t, mdl)
	ctx := context.Background()
	if _, err := h.store.AppendChatMessage(ctx, sessionID, store.ChatMessage{Role: store.RoleUser, Content: "你好"}); err != nil {
		t.Fatalf("seed user message: %v", err)
	}
	if _, err := h.store.AppendChatMessage(ctx, sessionID, store.ChatMessage{Role: store.RoleAssistant, Content: "你好，有什么可以帮你的？"}); err != nil {
		t.Fatalf("seed assistant message: %v", err)
	}

	resp := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/resume", map[string]string{})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("resume status = %d, want 400", resp.StatusCode)
	}
	if mdl.seenCount() != 0 {
		t.Fatal("a refused resume still called the model")
	}
}

func TestResume_WhileATurnRunsIsRefused(t *testing.T) {
	h, sessionID := turnHarness(t, &pieceModel{pieces: 40, size: 64, delay: 20 * time.Millisecond})
	seedPlan(t, h.store, sessionID, []tool.Task{{ID: "t1", Title: "一件", Status: tool.TaskPending}})

	resp := startTurn(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "跑一个长任务"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202", resp.StatusCode)
	}

	resume := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/resume", map[string]string{})
	defer func() { _ = resume.Body.Close() }()
	if resume.StatusCode != http.StatusConflict {
		t.Fatalf("resume while running = %d, want 409", resume.StatusCode)
	}

	// The refusal must not leave a stray "继续执行" in the transcript.
	msgs, err := h.store.ListChatMessages(context.Background(), sessionID, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	for _, m := range msgs {
		if m.Role == store.RoleUser && strings.Contains(m.Content, "继续执行") {
			t.Fatal("a refused resume stored a user message anyway")
		}
	}
}

func TestResume_UnknownSessionIs404(t *testing.T) {
	mdl := &recordingModel{}
	h, _ := resumeHarness(t, mdl)
	resp := h.postJSON(t, "/api/chat/sessions/does-not-exist/resume", map[string]string{})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("resume of an unknown session = %d, want 404", resp.StatusCode)
	}
}

func TestBuildResumeBrief(t *testing.T) {
	plan := &tool.Plan{
		Goal: "重构 chat",
		Tasks: []tool.Task{
			{ID: "t1", Title: "读代码", Status: tool.TaskDone, Note: "已读完"},
			{ID: "t2", Title: "改 runner", Status: tool.TaskInProgress},
		},
	}
	steps, err := json.Marshal([]chat.Step{{
		Index: 1,
		Text:  "先看配置\n顺便看看文档",
		Tools: []chat.ToolRun{{ID: "c1", Name: "read_file", Args: `{"path":"a.go"}`, Result: "line1\nline2", Step: 1}},
	}})
	if err != nil {
		t.Fatalf("encode steps: %v", err)
	}
	brief := buildResumeBrief(resumeBriefInput{
		goal:      "把坏事变成好事",
		plan:      plan,
		prev:      &store.ChatMessage{Content: "进行到一半", Error: "boom", Steps: string(steps)},
		workspace: "a-agent",
	})
	// The sections the model needs, in the order it needs them.
	order := []string{"【原始目标】", "【中断原因】", "【计划现状】", "【上一轮已经做过的步骤】", "【工作区】"}
	at := -1
	for _, section := range order {
		idx := strings.Index(brief, section)
		if idx < 0 {
			t.Fatalf("brief is missing %s\n%s", section, brief)
		}
		if idx < at {
			t.Fatalf("brief has %s out of order\n%s", section, brief)
		}
		at = idx
	}
	for _, want := range []string{
		"把坏事变成好事", "boom", "[>] t2 改 runner", "[x] t1 读代码", "a-agent",
		"read_file", "line1 line2", // a tool result is folded onto one line
		"plan_read", "不要重放", // what to do about it
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief does not mention %q\n%s", want, brief)
		}
	}
	// A step's multi-line narration is reduced to its first line: the brief is a
	// prompt, not a transcript.
	if strings.Contains(brief, "顺便看看文档") {
		t.Error("the brief quoted a whole step's narration instead of its first line")
	}
}

func TestBuildResumeBrief_DegradesWithoutAPlanOrARecord(t *testing.T) {
	brief := buildResumeBrief(resumeBriefInput{goal: "", prev: nil})
	if !strings.Contains(brief, "还没有计划") {
		t.Errorf("a planless brief does not say so:\n%s", brief)
	}
	if !strings.Contains(brief, "【中断原因】") {
		t.Errorf("a recordless brief has no reason section:\n%s", brief)
	}
	if strings.Contains(brief, "【原始目标】") {
		t.Error("an empty goal still produced a heading")
	}
}

func TestBuildResumeBrief_TruncatesLongToolOutput(t *testing.T) {
	long := strings.Repeat("字", 5000)
	steps, err := json.Marshal([]chat.Step{{
		Index: 1,
		Tools: []chat.ToolRun{{ID: "c1", Name: "bash", Result: long, Step: 1}},
	}})
	if err != nil {
		t.Fatalf("encode steps: %v", err)
	}
	brief := buildResumeBrief(resumeBriefInput{
		goal: "g",
		prev: &store.ChatMessage{Content: long, Steps: string(steps)},
	})
	if n := len([]rune(brief)); n > resumeBriefMaxRunes {
		t.Fatalf("brief is %d runes, over the %d cap", n, resumeBriefMaxRunes)
	}
	// Truncation is stated, not silent: a model that believes it has the whole
	// output will not re-read what it needs.
	if !strings.Contains(brief, "已截断") {
		t.Error("the brief was truncated without saying so")
	}
}

func TestResumable(t *testing.T) {
	unfinished := &tool.Plan{Tasks: []tool.Task{{ID: "t1", Status: tool.TaskPending}}}
	finished := &tool.Plan{Tasks: []tool.Task{{ID: "t1", Status: tool.TaskDone}}}
	skipped := &tool.Plan{Tasks: []tool.Task{{ID: "t1", Status: tool.TaskSkipped}}}
	cases := []struct {
		name string
		prev *store.ChatMessage
		plan *tool.Plan
		want bool
	}{
		{"a failed turn", &store.ChatMessage{Error: "boom"}, nil, true},
		{"a budget stop", &store.ChatMessage{StopReason: "steps"}, nil, true},
		{"an unfinished plan", &store.ChatMessage{}, unfinished, true},
		{"a failed turn with a finished plan", &store.ChatMessage{Error: "boom"}, finished, true},
		{"nothing at all", &store.ChatMessage{Content: "答完了"}, finished, false},
		{"no record and no plan", nil, nil, false},
		{"only skipped work left", &store.ChatMessage{Content: "答完了"}, skipped, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumable(tc.prev, tc.plan); got != tc.want {
				t.Fatalf("resumable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// readBody drains a response body, for a failure message that shows what the
// server said.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return b.String()
}
