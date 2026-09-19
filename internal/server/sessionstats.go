package server

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/store"
)

// sessionStats is what one conversation has cost and done so far, as the header
// of that conversation reports it.
//
// Every number is derived from what is stored, not from a counter kept in
// memory, so it survives a page reload, a model switch and a process restart,
// and it is the same answer for the console and for any other client reading the
// API. Three definitions are worth stating because a reader will otherwise guess
// them:
//
//   - LlmCalls counts assistant messages, because one assistant message is one
//     completed model call. A turn with ten steps contributes ten.
//   - ToolCalls counts the tool calls written down with the turn — read out of
//     the stored steps, which is the same list the transcript renders, so the
//     number in the header cannot disagree with the number of tool rows on
//     screen. A turn stored before steps were kept carries the same calls in the
//     flat ToolCalls column instead, and that is the fallback.
//   - ToolDurationMs is what the calls themselves reported, again from the steps
//     (the per-turn line under each answer shows the same sum). Older turns whose
//     steps recorded no timing fall back to the audit log's aggregate.
//
// Durations are what the provider reported for a call and what the tool itself
// measured, and neither is the wall-clock length of the conversation: they
// exclude queueing, thinking between steps, and the user's own time. The
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
	// ToolCalls counts the tool calls the turn recorded.
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
// adds one indexed aggregate query for the tool timing of turns that do not
// carry their own. A failure to read that aggregate is not a failure of the
// page: the counts, the tokens and the model durations are all still right, so
// the tool duration falls back to what the steps recorded rather than to a 500.
func (s *Server) sessionStatsFor(ctx context.Context, sessionID string, msgs []store.ChatMessage) sessionStats {
	var out sessionStats
	out.Messages = len(msgs)
	var toolMs int
	var sawToolMs bool
	for i := range msgs {
		m := &msgs[i]
		switch m.Role {
		case store.RoleUser:
			out.Turns++
		case store.RoleAssistant:
			// An assistant message that only carries reasoning or tool calls is
			// still one model call: the call happened, whatever it returned.
			out.LlmCalls++
			calls, ms, saw := transcriptTools(m)
			out.ToolCalls += calls
			if saw {
				toolMs += ms
				sawToolMs = true
			}
			// parseUsage is the same reader the transcript path uses, so a
			// message's usage is decoded exactly once, in one place.
			u := parseUsage(m.UsageJSON)
			out.LlmDurationMs += u.DurationMs
			out.PromptTokens += u.PromptTokens
			out.CompletionTokens += u.CompletionTokens
			out.TotalTokens += u.TotalTokens
		}
	}
	if sawToolMs {
		out.ToolDurationMs = toolMs
	}

	totals, err := s.store.QueryInvocationTotals(ctx, sessionID)
	if err != nil {
		s.logger.Warn("chat: read tool invocation totals failed, reporting what the steps recorded",
			zapString("session", sessionID), zapError(err))
		return out
	}
	// The audit log is the fallback, not the source: it is the only place an old
	// turn's timing exists at all (a turn stored before the steps kept their own),
	// and where the steps do have timing they are what the transcript shows, so
	// the two surfaces agree by construction.
	if !sawToolMs {
		out.ToolDurationMs = int(totals.DurationMs)
	}
	// A count that disagrees with the audit log means an invocation row was lost
	// or a call never ran. It is logged only when the audit log has rows at all:
	// a conversation whose turns predate the audit log is not a mismatch, it is
	// simply older than the log.
	if totals.Calls > 0 && totals.Calls != out.ToolCalls {
		s.logger.Debug("chat: tool call count differs from the audit log",
			zapString("session", sessionID),
			zap.Int("transcript", out.ToolCalls),
			zap.Int("audit", totals.Calls))
	}
	return out
}

// transcriptTools reads one assistant message's tool calls out of the transcript.
//
// The steps are the source because they are what the console renders: each one
// names the calls that iteration asked for, with the timing the call reported. A
// turn stored before steps were kept has none, and its calls live in the flat
// ToolCalls column — which is also what an older client reads — so that column is
// the fallback rather than a second thing to add up. Reading both and summing
// them would double every call of a current turn.
//
// `sawMs` reports whether any call in this message carried a duration: a message
// whose steps are all silent is "no timing recorded", which is not the same as
// 0ms and lets the caller fall back to the audit log.
func transcriptTools(m *store.ChatMessage) (calls, durationMs int, sawMs bool) {
	if m.Steps != "" {
		var steps []chat.Step
		if err := json.Unmarshal([]byte(m.Steps), &steps); err == nil {
			for _, step := range steps {
				for _, call := range step.Tools {
					calls++
					if call.DurationMs > 0 {
						durationMs += int(call.DurationMs)
						sawMs = true
					}
				}
			}
		}
	}
	if calls == 0 {
		// Either the turn kept no steps (an older row) or it kept steps that
		// called nothing: the flat list is the only other account of it.
		calls = countToolCalls(m.ToolCalls)
	}
	return calls, durationMs, sawMs
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
