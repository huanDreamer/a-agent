package server

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/store"
)

// sessionStats is what one conversation has cost and done so far, as the header
// of that conversation reports it.
//
// Every number is derived from what is stored, not from a counter kept in
// memory, so it survives a page reload, a model switch and a process restart,
// and it is the same answer for the console and for any other client reading the
// API. Two definitions are worth stating because a reader will otherwise guess
// them:
//
//   - LlmCalls counts assistant messages, because one assistant message is one
//     completed model call. A turn with ten steps contributes ten.
//   - ToolCalls counts the tool calls the model asked for, read from the
//     assistant messages themselves. That is the same list the transcript
//     renders, so the number in the header cannot disagree with the number of
//     tool cards on screen.
//
// Durations are what the provider reported for a call and what the audit log
// measured for a tool, and neither is the wall-clock length of the conversation:
// they exclude queueing, thinking between steps, and the user's own time. The
// header's tooltip says so, since a total that looks too small is otherwise read
// as a bug.
type sessionStats struct {
	// Messages is every stored message, user and assistant and tool.
	Messages int `json:"messages"`
	// Turns counts the user messages, i.e. the requests that were made.
	Turns int `json:"turns"`
	// LlmCalls counts completed model calls (one per assistant message).
	LlmCalls int `json:"llm_calls"`
	// LlmDurationMs sums the provider-reported duration of those calls.
	LlmDurationMs int `json:"llm_duration_ms"`
	// ToolCalls counts the tool calls the model asked for.
	ToolCalls int `json:"tool_calls"`
	// ToolDurationMs sums the measured duration of the tools that ran.
	ToolDurationMs int `json:"tool_duration_ms"`
	// PromptTokens, CompletionTokens and TotalTokens sum the provider-reported
	// usage. A provider that reports none leaves these at 0 rather than
	// estimating.
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// sessionStatsFor aggregates one conversation's messages.
//
// It reads what is already in hand — the messages the handler just loaded — and
// adds one indexed aggregate query for the tool timing, which is not stored on
// the messages: a tool result is the tool's output, and the measured time lives
// in the audit log. A failure to read that aggregate is not a failure of the
// page: the counts, the tokens and the model durations are all still right, so
// the tool duration is reported as 0 with a log line rather than a 500.
func (s *Server) sessionStatsFor(ctx context.Context, sessionID string, msgs []store.ChatMessage) sessionStats {
	var out sessionStats
	out.Messages = len(msgs)
	for i := range msgs {
		m := &msgs[i]
		switch m.Role {
		case store.RoleUser:
			out.Turns++
		case store.RoleAssistant:
			// An assistant message that only carries reasoning or tool calls is
			// still one model call: the call happened, whatever it returned.
			out.LlmCalls++
			out.ToolCalls += countToolCalls(m.ToolCalls)
			// parseUsage is the same reader the transcript path uses, so a
			// message's usage is decoded exactly once, in one place.
			u := parseUsage(m.UsageJSON)
			out.LlmDurationMs += u.DurationMs
			out.PromptTokens += u.PromptTokens
			out.CompletionTokens += u.CompletionTokens
			out.TotalTokens += u.TotalTokens
		}
	}

	totals, err := s.store.QueryInvocationTotals(ctx, sessionID)
	if err != nil {
		s.logger.Warn("chat: read tool invocation totals failed, reporting 0",
			zapString("session", sessionID), zapError(err))
		return out
	}
	out.ToolDurationMs = int(totals.DurationMs)
	// The audit log knows how many tools ran; the transcript knows how many were
	// asked for. They agree in practice, and when they do not the transcript is
	// what the reader can see, so the count stays the transcript's and only the
	// timing comes from the log. The mismatch is logged because it means an
	// invocation row was lost or a call never ran.
	if totals.Calls != out.ToolCalls {
		s.logger.Debug("chat: tool call count differs from the audit log",
			zapString("session", sessionID),
			zap.Int("transcript", out.ToolCalls),
			zap.Int("audit", totals.Calls))
	}
	return out
}

// countToolCalls counts the entries of one assistant message's tool_calls JSON.
// A row that does not parse counts as 0: the message is still a model call, and
// inventing a count from unreadable data would put a number on screen that
// nothing backs.
func countToolCalls(raw string) int {
	if raw == "" {
		return 0
	}
	var calls []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &calls); err != nil {
		return 0
	}
	return len(calls)
}
