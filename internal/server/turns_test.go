package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/store"
)

// pieceModel streams a fixed answer in pieces, slowly enough that a reader can
// walk away in the middle of it. It stands in for a model that takes its time,
// which is the situation every one of these tests is about.
type pieceModel struct {
	pieces int
	size   int
	delay  time.Duration
}

func (m *pieceModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *pieceModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return &schema.Message{Role: schema.Assistant, Content: strings.Repeat("x", m.pieces*m.size)}, nil
}

func (m *pieceModel) Stream(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	sr, sw := schema.Pipe[*schema.Message](1)
	chunk := strings.Repeat("x", m.size)
	go func() {
		defer sw.Close()
		for i := 0; i < m.pieces; i++ {
			if sw.Send(&schema.Message{Role: schema.Assistant, Content: chunk}, nil) {
				return
			}
			select {
			case <-time.After(m.delay):
			case <-ctx.Done():
				return
			}
		}
	}()
	return sr, nil
}

// turnHarness is a server whose model answers slowly, plus one conversation.
func turnHarness(t *testing.T, mdl model.ToolCallingChatModel) (*harness, string) {
	t.Helper()

	runner, err := chat.New(chat.Config{Model: mdl, MaxSteps: 4, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{Runner: runner}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)

	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decode(t, h.postJSON(t, "/api/chat/sessions", map[string]string{"title": "turn"}), &created)
	if created.Session.ID == "" {
		t.Fatal("no session id")
	}
	return h, created.Session.ID
}

// attach opens an attach stream (body left open for the caller).
func attach(t *testing.T, h *harness, sessionID string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.base+"/api/chat/sessions/"+sessionID+"/turn", nil)
	if err != nil {
		t.Fatalf("new attach request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("attach status = %d, want 200", resp.StatusCode)
	}
	return resp
}

// answerText is every text_delta a reader received, concatenated: what the
// reader would have on screen.
func answerText(events []sseEvent) string {
	var b strings.Builder
	for _, e := range events {
		if kind, _ := e["type"].(string); kind == "text_delta" {
			text, _ := e["text"].(string)
			b.WriteString(text)
		}
	}
	return b.String()
}

// readSomeFrames reads up to n data frames and returns, leaving the body open:
// a reader who watches for a moment and then leaves.
func readSomeFrames(t *testing.T, body io.Reader, n int) []sseEvent {
	t.Helper()

	events := make([]sseEvent, 0, n)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode SSE frame %q: %v", line, err)
		}
		events = append(events, ev)
		if len(events) >= n {
			return events
		}
	}
	return events
}

// TestTurn_ReaderReattachSeesTheAnswerFromTheStart is the reported bug, end to
// end: a reader who walks away mid-answer — another page, another conversation,
// a reloaded browser — attaches again and sees the whole thing, not just the
// part produced after coming back.
func TestTurn_ReaderReattachSeesTheAnswerFromTheStart(t *testing.T) {
	const pieces, size = 60, 512
	want := pieces * size

	h, sessionID := turnHarness(t, &pieceModel{pieces: pieces, size: size, delay: 40 * time.Millisecond})

	startResp := startTurn(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "写一段很长的话"})
	_ = startResp.Body.Close()
	if startResp.StatusCode != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202", startResp.StatusCode)
	}

	// First reader: watches for a moment, then leaves — the page switch.
	first := attach(t, h, sessionID)
	if seen := readSomeFrames(t, first.Body, 2); len(seen) == 0 {
		t.Fatal("the first reader saw nothing")
	}
	_ = first.Body.Close()
	time.Sleep(300 * time.Millisecond) // let the model get on with it unobserved

	// Second reader: attaching again must replay what already happened.
	second := attach(t, h, sessionID)
	events := scanSSE(t, second.Body)
	_ = second.Body.Close()

	got := answerText(events)
	if len(got) != want {
		t.Fatalf("reattached reader saw %d bytes, want %d (the turn was not replayed from its start)", len(got), want)
	}
	if kind := eventTypes(events); len(kind) == 0 || kind[len(kind)-1] != "stream_end" {
		t.Errorf("event types end with %v, want stream_end", kind)
	}

	// And the stored message is the whole answer.
	msgs, err := h.store.ListChatMessages(context.Background(), sessionID, 50)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	last := msgs[len(msgs)-1]
	if last.Role != store.RoleAssistant || len(last.Content) != want {
		t.Errorf("stored %s of %d bytes, want the whole %d", last.Role, len(last.Content), want)
	}
}

// TestTurn_DetachedReaderDoesNotCostTheAnswer keeps the earlier guarantee: the
// conversation keeps its answer even when nobody is watching.
func TestTurn_DetachedReaderDoesNotCostTheAnswer(t *testing.T) {
	const pieces, size = 40, 512
	want := pieces * size

	h, sessionID := turnHarness(t, &pieceModel{pieces: pieces, size: size, delay: 5 * time.Millisecond})

	_ = startTurn(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "hi"}).Body.Close()

	// The turn runs with no reader at all from here on.
	var assistantLen int
	waitForCondition(t, 20*time.Second, "the turn to be stored", func() bool {
		msgs, err := h.store.ListChatMessages(context.Background(), sessionID, 50)
		if err != nil {
			t.Fatalf("list messages: %v", err)
		}
		for _, m := range msgs {
			if m.Role == store.RoleAssistant {
				assistantLen = len(m.Content)
			}
		}
		return assistantLen >= want
	})
}

// TestTurn_SecondMessageIsRefusedWhileOneRuns: one turn per conversation.
func TestTurn_SecondMessageIsRefusedWhileOneRuns(t *testing.T) {
	h, sessionID := turnHarness(t, &pieceModel{pieces: 60, size: 64, delay: 20 * time.Millisecond})

	_ = startTurn(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "first"}).Body.Close()

	resp := startTurn(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "second"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second message status = %d, want 409", resp.StatusCode)
	}

	// Stopping clears the way, which is what makes the refusal a queue and not a
	// trap.
	stopResp := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/turn/stop", map[string]string{})
	_ = stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, want 200", stopResp.StatusCode)
	}
	waitForCondition(t, 10*time.Second, "the turn to end", func() bool {
		return !h.srv.turns.running(sessionID)
	})

	resp2 := startTurn(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "third"})
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("message after stop status = %d, want 202", resp2.StatusCode)
	}
}

// TestTurn_StopKeepsWhatWasProduced: stopping is a real stop, and the part that
// arrived is stored — marked, so the next reader does not take it for the whole
// answer.
func TestTurn_StopKeepsWhatWasProduced(t *testing.T) {
	const pieces, size = 200, 512
	h, sessionID := turnHarness(t, &pieceModel{pieces: pieces, size: size, delay: 20 * time.Millisecond})

	_ = startTurn(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "hi"}).Body.Close()

	events := make([]sseEvent, 0, 32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		events = scanSSE(t, attach(t, h, sessionID).Body)
	}()

	// Let a little of the answer arrive, then stop it.
	time.Sleep(400 * time.Millisecond)
	stopResp := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/turn/stop", map[string]string{})
	_ = stopResp.Body.Close()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the attach stream never ended after a stop")
	}

	if !strings.Contains(answerText(events), "x") {
		t.Error("the stopped turn's stream carried no answer text")
	}
	waitForCondition(t, 10*time.Second, "the stopped turn to be stored", func() bool {
		msgs, err := h.store.ListChatMessages(context.Background(), sessionID, 50)
		if err != nil {
			t.Fatalf("list messages: %v", err)
		}
		last := msgs[len(msgs)-1]
		if last.Role != store.RoleAssistant {
			return false
		}
		if last.Error != turnStoppedText {
			t.Fatalf("stored error = %q, want %q", last.Error, turnStoppedText)
		}
		if len(last.Content) == 0 {
			t.Error("the stopped turn stored an empty answer")
		}
		if len(last.Content) >= pieces*size {
			t.Error("the stopped turn stored the whole answer; it was not stopped")
		}
		return true
	})
}

// TestTurn_SessionsReportWhichOneIsGenerating is what lets the sidebar mark a
// conversation the reader is not looking at — and what tells a reloaded page
// which conversation to attach to.
func TestTurn_SessionsReportWhichOneIsGenerating(t *testing.T) {
	h, sessionID := turnHarness(t, &pieceModel{pieces: 80, size: 64, delay: 20 * time.Millisecond})

	var other struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decode(t, h.postJSON(t, "/api/chat/sessions", map[string]string{"title": "other"}), &other)

	_ = startTurn(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
		map[string]string{"content": "hi"}).Body.Close()

	streamingOf := func() map[string]bool {
		t.Helper()
		var list struct {
			Sessions []struct {
				ID        string `json:"id"`
				Streaming bool   `json:"streaming"`
			} `json:"sessions"`
		}
		h.getJSON(t, "/api/chat/sessions", http.StatusOK, &list)
		out := map[string]bool{}
		for _, s := range list.Sessions {
			out[s.ID] = s.Streaming
		}
		return out
	}

	states := streamingOf()
	if !states[sessionID] {
		t.Error("the conversation with a running turn is not marked as streaming")
	}
	if states[other.Session.ID] {
		t.Error("an idle conversation is marked as streaming")
	}

	// The same flag on the conversation itself, which is what a reloaded page
	// reads before deciding to attach.
	var one struct {
		Session struct {
			Streaming bool `json:"streaming"`
		} `json:"session"`
	}
	h.getJSON(t, "/api/chat/sessions/"+sessionID, http.StatusOK, &one)
	if !one.Session.Streaming {
		t.Error("GET of a running conversation does not report streaming")
	}

	stopResp := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/turn/stop", map[string]string{})
	_ = stopResp.Body.Close()
	waitForCondition(t, 10*time.Second, "the turn to end", func() bool {
		return !streamingOf()[sessionID]
	})
}

// TestTurn_AttachIsIdleWithoutATurn: attaching to a quiet conversation answers
// at once instead of holding a connection open.
func TestTurn_AttachIsIdleWithoutATurn(t *testing.T) {
	h, sessionID := turnHarness(t, &pieceModel{pieces: 2, size: 8, delay: time.Millisecond})

	var body struct {
		Streaming bool `json:"streaming"`
	}
	h.getJSON(t, "/api/chat/sessions/"+sessionID+"/turn", http.StatusOK, &body)
	if body.Streaming {
		t.Error("an idle conversation reported a running turn")
	}
}
