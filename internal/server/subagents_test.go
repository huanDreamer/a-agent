package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/subagent"
	"github.com/huan/huan-agent/internal/tool"
)

// The console's view of delegated subagents, over real HTTP.
//
// It mirrors the background-jobs surface because it answers the same question for
// the header, and the tests mirror that: the chip needs a count and a liveness
// figure, the drawer needs the runs themselves, and a deployment with no subagents
// needs the endpoint absent rather than permanently empty.

func TestSubagentEndpointIsAbsentWhenDisabled(t *testing.T) {
	h := newHarness(t, nil)
	h.login(t)

	resp := h.get(t, "/api/chat/sessions/whatever/subagents")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when the deployment has no subagents", resp.StatusCode)
	}
}

func TestSubagentEndpointReportsRuns(t *testing.T) {
	tracker := subagent.NewTracker(10)
	h, sessionID := newSubagentHarness(t, tracker)
	h.login(t)

	// Nothing delegated yet.
	resp := h.get(t, "/api/chat/sessions/"+sessionID+"/subagents")
	var empty struct {
		Subagents []subagent.Run `json:"subagents"`
		Running   int            `json:"running"`
		Total     int            `json:"total"`
	}
	decodeBody(t, resp, &empty)
	if empty.Total != 0 || empty.Running != 0 || len(empty.Subagents) != 0 {
		t.Errorf("an idle conversation reports %+v", empty)
	}

	// One running, one finished.
	live := tracker.Start(sessionID, "auth-survey", "调研鉴权", "call-1")
	done := tracker.Start(sessionID, "db-survey", "调研数据库", "call-2")
	tracker.Finish(done.ID, subagent.Finish{Steps: 5, Tokens: 900})

	resp2 := h.get(t, "/api/chat/sessions/"+sessionID+"/subagents")
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp2.StatusCode)
	}
	var got struct {
		Subagents  []subagent.Run `json:"subagents"`
		Running    int            `json:"running"`
		Total      int            `json:"total"`
		MaxConc    int            `json:"max_concurrent"`
		RunningAll int            `json:"running_all"`
		Tokens     int            `json:"tokens"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Total != 2 || got.Running != 1 {
		t.Errorf("total/running = %d/%d, want 2/1", got.Total, got.Running)
	}
	if len(got.Subagents) != 2 {
		t.Fatalf("subagents = %+v", got.Subagents)
	}
	// Newest first, so a header reads the live one before the history.
	if got.Subagents[0].ID != done.ID || got.Subagents[1].ID != live.ID {
		t.Errorf("order = %s, %s; want newest first", got.Subagents[0].Name, got.Subagents[1].Name)
	}
	if got.Subagents[1].Status != subagent.StatusRunning {
		t.Errorf("the live run is %q", got.Subagents[1].Status)
	}
	if got.Subagents[0].Steps != 5 || got.Tokens != 900 {
		t.Errorf("the finished run's detail is missing: %+v (tokens %d)", got.Subagents[0], got.Tokens)
	}
	if got.MaxConc != 3 {
		t.Errorf("max_concurrent = %d, want what the harness configured", got.MaxConc)
	}
}

// TestSubagentEndpointIsPerConversation: another conversation's delegations must not
// appear under this one's header.
func TestSubagentEndpointIsPerConversation(t *testing.T) {
	tracker := subagent.NewTracker(10)
	h, mine := newSubagentHarness(t, tracker)
	h.login(t)
	tracker.Start("someone-elses-conversation", "theirs", "x", "")

	resp := h.get(t, "/api/chat/sessions/"+mine+"/subagents")
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		Total int `json:"total"`
	}
	decodeBody(t, resp, &got)
	if got.Total != 0 {
		t.Errorf("another conversation's run leaked into this header: %+v", got)
	}
}

// TestSubagentEndpointChecksTheSession: an unknown id is a 404 rather than an empty
// list, so a client bug does not look like "nothing delegated".
func TestSubagentEndpointChecksTheSession(t *testing.T) {
	tracker := subagent.NewTracker(10)
	h, _ := newSubagentHarness(t, tracker)
	h.login(t)

	resp := h.get(t, "/api/chat/sessions/no-such-session/subagents")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// newSubagentHarness builds a console with a subagent tracker and one conversation.
//
// It needs a runner: the conversation routes only exist on a chat-enabled server, so
// a harness without one answers 404 to every session URL and would prove nothing
// about this endpoint.
func newSubagentHarness(t *testing.T, tracker *subagent.Tracker) (*harness, string) {
	t.Helper()
	runner, err := chat.New(chat.Config{
		Model:    &scriptedModel{turns: [][]*schema.Message{{{Role: schema.Assistant, Content: "ok"}}}},
		Tools:    tool.NewRegistry(),
		MaxSteps: 2,
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	srv, st := buildServerWith(t, buildOpts{
		chat: ChatDeps{
			Runner:                runner,
			Tools:                 tool.NewRegistry(),
			Subagents:             tracker,
			SubagentMaxConcurrent: 3,
		},
	})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)
	return h, createSession(t, h)
}
