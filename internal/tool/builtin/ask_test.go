package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/huan/huan-agent/internal/tool"
)

// stubAsker is a person who answers instantly, records the question they were
// asked, and can pretend to have walked away.
type stubAsker struct {
	asked  tool.Question
	answer tool.Answer
	err    error
	// wait, when set, is how long the "person" takes. It is what lets a test
	// exercise the note a timeout produces without waiting for one.
	wait time.Duration
}

func (s *stubAsker) Ask(ctx context.Context, q tool.Question) (tool.Answer, error) {
	s.asked = q
	if s.wait > 0 {
		select {
		case <-time.After(s.wait):
		case <-ctx.Done():
			return tool.Answer{Status: tool.AnswerCancelled}, nil
		}
	}
	if s.err != nil {
		return tool.Answer{}, s.err
	}
	return s.answer, nil
}

// askTool invokes ask_user with a raw argument object, the way the model does.
func askTool(t *testing.T, ctx context.Context, args map[string]any) (AskUserOutput, error) {
	t.Helper()
	tl, err := NewAskUserTool()
	if err != nil {
		t.Fatalf("NewAskUserTool: %v", err)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	out, err := tl.InvokableRun(ctx, string(raw))
	if err != nil {
		return AskUserOutput{}, err
	}
	var parsed AskUserOutput
	if uerr := json.Unmarshal([]byte(out), &parsed); uerr != nil {
		t.Fatalf("decode tool output %q: %v", out, uerr)
	}
	return parsed, nil
}

func TestAskUserTool_ReturnsTheChosenOption(t *testing.T) {
	asker := &stubAsker{answer: tool.Answer{Status: tool.AnswerAnswered, Selected: []string{"Postgres"}}}
	ctx := tool.WithAsker(context.Background(), asker)

	out, err := askTool(t, ctx, map[string]any{
		"question": "新服务用哪个数据库？",
		"header":   "数据库选型",
		"options": []map[string]string{
			{"label": "Postgres", "description": "已有实例，迁移成本低"},
			{"label": "SQLite"},
		},
	})
	if err != nil {
		t.Fatalf("ask_user: %v", err)
	}
	if out.Status != string(tool.AnswerAnswered) {
		t.Errorf("status = %q, want answered", out.Status)
	}
	if out.Answer != "Postgres" {
		t.Errorf("answer = %q, want Postgres", out.Answer)
	}
	if out.Note != "" {
		t.Errorf("note = %q, want empty for a real answer", out.Note)
	}
	if asker.asked.Header != "数据库选型" || asker.asked.Text != "新服务用哪个数据库？" {
		t.Errorf("question reached the asker as %+v", asker.asked)
	}
	if len(asker.asked.Options) != 2 || asker.asked.Options[0].Description != "已有实例，迁移成本低" {
		t.Errorf("options reached the asker as %+v", asker.asked.Options)
	}
	// allow_custom is omitted, and omitting it means "the person may type their
	// own answer" — the opposite would silently remove the input box.
	if !asker.asked.AllowCustom {
		t.Error("allow_custom defaulted to false when the model omitted it")
	}
}

func TestAskUserTool_MultiSelectWithCustomTextReadsAsOneAnswer(t *testing.T) {
	asker := &stubAsker{answer: tool.Answer{
		Status:   tool.AnswerAnswered,
		Selected: []string{"登录", "支付"},
		Text:     "先做登录",
	}}
	ctx := tool.WithAsker(context.Background(), asker)

	out, err := askTool(t, ctx, map[string]any{
		"question":     "这次先做哪几个模块？",
		"multi_select": true,
		"options":      []map[string]string{{"label": "登录"}, {"label": "支付"}, {"label": "报表"}},
	})
	if err != nil {
		t.Fatalf("ask_user: %v", err)
	}
	if out.Answer != "登录、支付（补充：先做登录）" {
		t.Errorf("answer = %q, want the selection plus the note in one line", out.Answer)
	}
	if !asker.asked.MultiSelect {
		t.Error("multi_select did not reach the asker")
	}
}

func TestAskUserTool_CustomTextAloneIsTheAnswer(t *testing.T) {
	asker := &stubAsker{answer: tool.Answer{Status: tool.AnswerAnswered, Text: " 用 MySQL "}}
	ctx := tool.WithAsker(context.Background(), asker)

	out, err := askTool(t, ctx, map[string]any{"question": "用哪个数据库？"})
	if err != nil {
		t.Fatalf("ask_user: %v", err)
	}
	if out.Answer != "用 MySQL" {
		t.Errorf("answer = %q, want the trimmed custom text", out.Answer)
	}
}

func TestAskUserTool_ACancelledWaitIsNotAFailure(t *testing.T) {
	// The turn is cancelled while the person is still deciding (stop button, tab
	// closed, deadline). The tool must report the outcome, not hand the model an
	// error it cannot act on.
	asker := &stubAsker{
		answer: tool.Answer{Status: tool.AnswerAnswered, Selected: []string{"A"}},
		wait:   30 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(tool.WithAsker(context.Background(), asker), time.Millisecond)
	defer cancel()
	// The stub mirrors a real Asker: ctx expiry becomes an AnswerCancelled.

	out, err := askTool(t, ctx, map[string]any{"question": "选一个？", "options": []map[string]string{{"label": "A"}}})
	if err != nil {
		t.Fatalf("a cancelled wait must not be an error: %v", err)
	}
	if out.Status != string(tool.AnswerCancelled) {
		t.Errorf("status = %q, want cancelled", out.Status)
	}
	if out.Answer != "" {
		t.Errorf("answer = %q, want empty when nobody answered", out.Answer)
	}
	if !strings.Contains(out.Note, "中断") {
		t.Errorf("note = %q, want it to explain the interruption", out.Note)
	}
}

func TestRenderAskAnswer_TimeoutNamesTheWaitAndTheNextMove(t *testing.T) {
	answer, note := renderAskAnswer(AskUserOutput{Status: string(tool.AnswerTimeout)}, 90*time.Second)
	if answer != "" {
		t.Errorf("answer = %q, want empty", answer)
	}
	for _, want := range []string{"1m30s", "没有回答", "假设", "不要重复问"} {
		if !strings.Contains(note, want) {
			t.Errorf("timeout note %q is missing %q", note, want)
		}
	}
}

func TestAskUserTool_WithoutAnAskerSaysSo(t *testing.T) {
	// No asker on the context: the surface has no cards (Feishu, the CLI), so the
	// tool must tell the model to answer in text instead of asking into the void.
	_, err := askTool(t, context.Background(), map[string]any{"question": "选一个？"})
	if err == nil {
		t.Fatal("want an error when no asker is installed")
	}
	if !strings.Contains(err.Error(), "无法向用户提问") {
		t.Errorf("error = %q, want it to say this channel cannot ask", err)
	}
}

func TestAskUserTool_RefusesUnusableQuestions(t *testing.T) {
	options := func(n int) []map[string]string {
		out := make([]map[string]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, map[string]string{"label": string(rune('A' + i))})
		}
		return out
	}

	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "empty question",
			args: map[string]any{"question": "   "},
			want: "question 不能为空",
		},
		{
			name: "too many options",
			args: map[string]any{"question": "选一个", "options": options(maxAskOptions + 1)},
			want: "选项最多",
		},
		{
			name: "blank label",
			args: map[string]any{"question": "选一个", "options": []map[string]string{{"label": "  "}}},
			want: "label 为空",
		},
		{
			name: "duplicate labels",
			args: map[string]any{"question": "选一个", "options": []map[string]string{{"label": "A"}, {"label": " A "}}},
			want: "重复",
		},
		{
			// The one shape that leaves a person with nothing to click and
			// nothing to type.
			name: "no options and no free text",
			args: map[string]any{"question": "选一个", "allow_custom": false},
			want: "无法回答",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			asker := &stubAsker{answer: tool.Answer{Status: tool.AnswerAnswered, Text: "x"}}
			ctx := tool.WithAsker(context.Background(), asker)
			_, err := askTool(t, ctx, tc.args)
			if err == nil {
				t.Fatalf("want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			// A refused question must never reach a person: the model gets to fix
			// the call first.
			if asker.asked.Text != "" {
				t.Errorf("a rejected question was still asked: %+v", asker.asked)
			}
		})
	}
}

func TestAskUserTool_AllowCustomFalseReachesTheAsker(t *testing.T) {
	asker := &stubAsker{answer: tool.Answer{Status: tool.AnswerAnswered, Selected: []string{"A"}}}
	ctx := tool.WithAsker(context.Background(), asker)

	if _, err := askTool(t, ctx, map[string]any{
		"question":     "选一个",
		"allow_custom": false,
		"options":      []map[string]string{{"label": "A"}},
	}); err != nil {
		t.Fatalf("ask_user: %v", err)
	}
	if asker.asked.AllowCustom {
		t.Error("allow_custom=false was not passed through")
	}
}
