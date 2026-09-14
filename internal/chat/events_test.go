package chat

import (
	"encoding/json"
	"testing"

	"github.com/huan/huan-agent/internal/tool"
)

// decodeEvent marshals an event the way the SSE writer does and reads it back as
// a plain map, so the assertions are about the wire shape the browser parses
// rather than about the Go struct.
func decodeEvent(t *testing.T, e Event) map[string]any {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal event %s: %v", b, err)
	}
	return out
}

func TestEventAsk_AnnouncementShape(t *testing.T) {
	e := Event{
		Type: EventAsk,
		Ask: &tool.Question{
			ID:          "q1",
			Header:      "数据库选型",
			Text:        "用哪个？",
			Options:     []tool.Option{{Label: "Postgres", Description: "已有实例"}},
			MultiSelect: false,
			AllowCustom: true,
		},
		AskStatus: AskPending,
	}

	got := decodeEvent(t, e)
	if got["type"] != "ask_user" {
		t.Errorf("type = %v, want ask_user (the client switches on it)", got["type"])
	}
	if got["ask_status"] != "pending" {
		t.Errorf("ask_status = %v, want pending", got["ask_status"])
	}
	ask, ok := got["ask"].(map[string]any)
	if !ok {
		t.Fatalf("ask = %#v, want an object", got["ask"])
	}
	for _, key := range []string{"id", "header", "text", "options", "allow_custom"} {
		if _, ok := ask[key]; !ok {
			t.Errorf("ask is missing %q: %#v", key, ask)
		}
	}
	// multi_select is omitted when false; the client reads absence as false, and
	// allow_custom travels explicitly so a card never has to guess whether the
	// missing input box was intended.
	if _, present := ask["multi_select"]; present {
		t.Errorf("multi_select = %v, want it omitted when false", ask["multi_select"])
	}
	if ask["allow_custom"] != true {
		t.Errorf("allow_custom = %v, want true", ask["allow_custom"])
	}
	if _, present := got["ask_id"]; present {
		t.Errorf("ask_id = %v, want it absent before the question settles", got["ask_id"])
	}
}

func TestEventAsk_OutcomeShapes(t *testing.T) {
	answered := decodeEvent(t, Event{
		Type:      EventAsk,
		AskID:     "q1",
		AskStatus: string(tool.AnswerAnswered),
		AskAnswer: &tool.Answer{Status: tool.AnswerAnswered, Selected: []string{"SQLite"}, Text: "也可以"},
	})
	if answered["ask_id"] != "q1" || answered["ask_status"] != "answered" {
		t.Errorf("outcome = %#v, want the id and the status", answered)
	}
	answer, ok := answered["ask_answer"].(map[string]any)
	if !ok {
		t.Fatalf("ask_answer = %#v, want an object", answered["ask_answer"])
	}
	if answer["status"] != "answered" {
		t.Errorf("answer status = %v, want answered", answer["status"])
	}
	if _, present := answered["ask"]; present {
		t.Errorf("ask = %v, want it absent on an outcome", answered["ask"])
	}

	// A timeout has no answer: an empty one would read as "the user submitted
	// nothing", which is a different fact.
	timeout := decodeEvent(t, Event{Type: EventAsk, AskID: "q1", AskStatus: string(tool.AnswerTimeout)})
	if _, present := timeout["ask_answer"]; present {
		t.Errorf("ask_answer = %v, want it absent on a timeout", timeout["ask_answer"])
	}
	if timeout["ask_status"] != "timeout" {
		t.Errorf("ask_status = %v, want timeout", timeout["ask_status"])
	}
}
