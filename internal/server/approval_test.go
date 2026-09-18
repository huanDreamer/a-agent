package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/tool"
)

// approvalRequest is the request every test in this file puts to the user.
func approvalRequest() tool.Request {
	return tool.Request{
		Tool:       "bash",
		Capability: tool.CapExec,
		Summary:    "执行: git push origin main",
		Preview: []tool.PreviewLine{
			{Kind: tool.PreviewMeta, Text: "完整命令：git push origin main"},
		},
		Default: tool.DecisionDeny,
	}
}

// approvalRecorder collects what a turnApprover emitted and remembers the id of
// the request it announced — which is what a test decides, exactly as the browser
// does.
type approvalRecorder struct {
	mu       sync.Mutex
	events   []chat.Event
	asked    string
	settled  chat.Event
	hasFinal bool
}

func (r *approvalRecorder) emit(e chat.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	if e.Approval != nil {
		r.asked = e.Approval.ID
	}
	if e.ApprovalID != "" {
		r.settled = e
		r.hasFinal = true
	}
}

func (r *approvalRecorder) announced() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asked
}

func (r *approvalRecorder) final() (chat.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settled, r.hasFinal
}

func collectApprover(hub *approvalHub, timeout time.Duration) (*turnApprover, *approvalRecorder) {
	rec := &approvalRecorder{}
	a := &turnApprover{
		hub:     hub,
		session: "sess-1",
		timeout: timeout,
		logger:  zap.NewNop(),
		emit:    rec.emit,
	}
	return a, rec
}

// TestApprovalHubResolveDeliversOnce: of two simultaneous decisions exactly one
// wins and the other is told the request is gone.
func TestApprovalHubResolveDeliversOnce(t *testing.T) {
	hub := newApprovalHub()
	req := approvalRequest()
	req.ID = "ap-1"
	hub.register("sess-1", req)

	if err := hub.resolve("ap-1", "sess-1", tool.Decision{Kind: tool.DecisionAllowOnce}); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if err := hub.resolve("ap-1", "sess-1", tool.Decision{Kind: tool.DecisionDeny}); !errors.Is(err, errApprovalGone) {
		t.Errorf("second resolve = %v, want errApprovalGone", err)
	}
	if got := hub.count(); got != 0 {
		t.Errorf("hub holds %d requests after a decision, want 0", got)
	}
}

// TestApprovalHubIsScopedToItsSession: an id from another conversation must not be
// decidable here, which is what stops a guessed id from approving a stranger's
// command.
func TestApprovalHubIsScopedToItsSession(t *testing.T) {
	hub := newApprovalHub()
	req := approvalRequest()
	req.ID = "ap-2"
	hub.register("sess-1", req)

	if _, ok := hub.peek("ap-2", "sess-2"); ok {
		t.Error("another session must not be able to see this request")
	}
	if err := hub.resolve("ap-2", "sess-2", tool.Decision{Kind: tool.DecisionAllowOnce}); !errors.Is(err, errApprovalGone) {
		t.Errorf("resolve from another session = %v, want errApprovalGone", err)
	}
}

// TestTurnApproverDeliversADecision: the happy path, end to end through the hub.
func TestTurnApproverDeliversADecision(t *testing.T) {
	hub := newApprovalHub()
	a, rec := collectApprover(hub, 5*time.Second)

	done := make(chan tool.Decision, 1)
	go func() {
		d, err := a.Approve(context.Background(), approvalRequest())
		if err != nil {
			t.Errorf("Approve: %v", err)
		}
		done <- d
	}()

	// Wait until the request is announced, the way a browser would.
	deadline := time.Now().Add(2 * time.Second)
	for rec.announced() == "" && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	id := rec.announced()
	if id == "" {
		t.Fatal("no request was announced")
	}
	if err := hub.resolve(id, "sess-1", tool.Decision{
		Kind: tool.DecisionAllowOnce, Reason: "ignored", Source: tool.SourceHuman,
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	select {
	case d := <-done:
		if !d.Allowed() {
			t.Errorf("decision = %+v, want it allowed", d)
		}
		if d.Source != tool.SourceHuman {
			t.Errorf("source = %q, want human", d.Source)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Approve never returned")
	}

	final, ok := rec.final()
	if !ok {
		t.Fatal("the request was never settled on the stream; the card would sit there offering buttons")
	}
	if final.ApprovalDecision != string(tool.DecisionAllowOnce) {
		t.Errorf("final event = %+v", final)
	}
	if final.ApprovalID != id {
		t.Errorf("the final event names %q, want %q", final.ApprovalID, id)
	}
	if got := hub.count(); got != 0 {
		t.Errorf("hub holds %d requests after the turn moved on, want 0", got)
	}
}

// TestTurnApproverTimeoutIsARefusal is the one behaviour that must not be got
// wrong, and the one place this deliberately differs from ask_user.
func TestTurnApproverTimeoutIsARefusal(t *testing.T) {
	hub := newApprovalHub()
	a, rec := collectApprover(hub, 40*time.Millisecond)

	start := time.Now()
	d, err := a.Approve(context.Background(), approvalRequest())
	if err != nil {
		t.Fatalf("a timeout is not an error: %v", err)
	}
	if d.Allowed() {
		t.Fatal("a timed-out approval must be a refusal: nobody was watching, which is when the gate matters")
	}
	if d.Kind != tool.DecisionDeny {
		t.Errorf("kind = %q, want deny", d.Kind)
	}
	if d.Source != tool.SourceTimeout {
		t.Errorf("source = %q, want timeout so the audit can tell it from a person's refusal", d.Source)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waited %v for a 40ms timeout", elapsed)
	}

	final, ok := rec.final()
	if !ok {
		t.Fatal("a timeout must still settle the card")
	}
	if final.ApprovalDecision != string(tool.DecisionDeny) || final.ApprovalSource != tool.SourceTimeout {
		t.Errorf("final event = %+v", final)
	}
	if got := hub.count(); got != 0 {
		t.Errorf("hub holds %d requests after a timeout, want 0 (a waiter that forgets nothing leaks)", got)
	}
}

// TestTurnApproverCancelledTurnRefuses: a turn that was stopped has nobody to
// answer for it, and an undecided request must not become an allowed one.
func TestTurnApproverCancelledTurnRefuses(t *testing.T) {
	hub := newApprovalHub()
	a, _ := collectApprover(hub, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	d, err := a.Approve(ctx, approvalRequest())
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if d.Allowed() {
		t.Fatal("a cancelled turn must not approve anything")
	}
	if d.Source != tool.SourceTimeout {
		t.Errorf("source = %q; a cancelled turn is 'nobody decided', not a person", d.Source)
	}
	if got := hub.count(); got != 0 {
		t.Errorf("hub holds %d requests, want 0", got)
	}
}

// TestTurnApproverHonoursTheRequestTimeout: a request may carry its own limit, so
// a tool can ask for less than the surface's default.
func TestTurnApproverHonoursTheRequestTimeout(t *testing.T) {
	hub := newApprovalHub()
	a, _ := collectApprover(hub, time.Minute)

	req := approvalRequest()
	req.Timeout = 30 // ms
	start := time.Now()
	if _, err := a.Approve(context.Background(), req); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waited %v for a 30ms request timeout", elapsed)
	}
}

// TestTurnApproverWithoutAHubFails: a Server built without New has no registry to
// park a request in. A sentence beats registering into a nil map.
func TestTurnApproverWithoutAHubFails(t *testing.T) {
	a := &turnApprover{session: "sess-1", logger: zap.NewNop(), emit: func(chat.Event) {}}
	if _, err := a.Approve(context.Background(), approvalRequest()); err == nil {
		t.Error("expected an error rather than a panic")
	}
}

// newApprovalHarness builds a server whose chat runs on a scripted model with a
// gate in front of one tool, so a whole turn — ask, decide, act — can be driven
// end to end.
func newApprovalHarness(t *testing.T, turns [][]*schema.Message, tools *tool.Registry,
	approvalTimeout time.Duration) (*harness, *gateRecorder) {

	t.Helper()
	rec := &gateRecorder{}
	wrapped := tool.NewRegistry()
	specs, err := tools.List(context.Background())
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, spec := range specs {
		tl, ok := tools.Get(spec.Name)
		if !ok {
			continue
		}
		gated := tool.Gate(tl, tool.GateOptions{
			Policy: tool.ApprovalPolicy{Mode: tool.ApprovalWritesExec},
		})
		rec.wrapped = append(rec.wrapped, spec.Name)
		if err := wrapped.Register(gated); err != nil {
			t.Fatalf("register gated %s: %v", spec.Name, err)
		}
	}

	runner, err := chat.New(chat.Config{
		Model:    &scriptedModel{turns: turns},
		Tools:    wrapped,
		MaxSteps: 4,
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}
	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{
		Runner:          runner,
		Tools:           wrapped,
		ApprovalTimeout: approvalTimeout,
	}})
	startHarness(t, srv)
	return &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}, rec
}

// gateRecorder remembers what the harness gated.
type gateRecorder struct {
	wrapped []string
}

// recordingWriteTool is the tool the scripted model calls, and the record of
// whether the action happened. That is the only question the gate exists to
// answer.
type recordingWriteTool struct {
	name string
	cap  tool.Capability
	mark *int32
}

func (r *recordingWriteTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: r.name, Desc: "records that it ran"}, nil
}

func (r *recordingWriteTool) Capability() tool.Capability { return r.cap }

func (r *recordingWriteTool) InvokableRun(context.Context, string, ...einotool.Option) (string, error) {
	atomic.AddInt32(r.mark, 1)
	return `{"ok":true}`, nil
}

// TestDecideApprovalOverHTTP drives the endpoint the way the browser does: a real
// request against a real server, with the request parked in the hub the way a
// running turn would leave it.
func TestDecideApprovalOverHTTP(t *testing.T) {
	h, _ := newApprovalHarness(t, nil, tool.NewRegistry(), 5*time.Second)
	h.login(t)
	hub := h.srv.approvals
	if hub == nil {
		t.Fatal("the server has no approval hub")
	}

	req := approvalRequest()
	req.ID = "ap-http-1"
	hub.register("sess-1", req)

	resp := h.postJSON(t, "/api/chat/sessions/sess-1/approvals/ap-http-1",
		map[string]any{"decision": "deny", "reason": "不要动主分支"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if hub.count() != 0 {
		t.Errorf("the decision did not consume the request: %d left", hub.count())
	}
}

// TestDecideApprovalOverHTTPValidation covers the endpoint's own refusals, over
// real HTTP, including that a rejected submission does not consume the request.
func TestDecideApprovalOverHTTPValidation(t *testing.T) {
	cases := []struct {
		name    string
		session string
		id      string
		body    any
		want    int
	}{
		{"unknown decision kind", "sess-1", "ap-1", map[string]any{"decision": "maybe"}, http.StatusBadRequest},
		{"missing decision", "sess-1", "ap-1", map[string]any{}, http.StatusBadRequest},
		{"unknown id", "sess-1", "nope", map[string]any{"decision": "allow_once"}, http.StatusNotFound},
		{"another session", "sess-2", "ap-1", map[string]any{"decision": "allow_once"}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newApprovalHarness(t, nil, tool.NewRegistry(), 5*time.Second)
			h.login(t)
			req := approvalRequest()
			req.ID = "ap-1"
			h.srv.approvals.register("sess-1", req)

			resp := h.postJSON(t, "/api/chat/sessions/"+tc.session+"/approvals/"+tc.id, tc.body)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want != http.StatusOK && h.srv.approvals.count() != 1 {
				t.Error("a refused submission consumed the request; the card would stop working")
			}
		})
	}
}

// TestDecideApprovalTwiceIsRefusedOverHTTP: a decision already made cannot be
// flipped, which is what stops a late click from changing an outcome.
func TestDecideApprovalTwiceIsRefusedOverHTTP(t *testing.T) {
	h, _ := newApprovalHarness(t, nil, tool.NewRegistry(), 5*time.Second)
	h.login(t)
	req := approvalRequest()
	req.ID = "ap-2"
	h.srv.approvals.register("sess-1", req)

	first := h.postJSON(t, "/api/chat/sessions/sess-1/approvals/ap-2", map[string]any{"decision": "allow_once"})
	defer func() { _ = first.Body.Close() }()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first = %d", first.StatusCode)
	}
	second := h.postJSON(t, "/api/chat/sessions/sess-1/approvals/ap-2", map[string]any{"decision": "deny"})
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusNotFound {
		t.Errorf("second = %d, want 404", second.StatusCode)
	}
}

// TestDecideApprovalDropsAReasonOnAllow: "rejected because X" and "allowed, and
// here is X" are different facts, and the model reads the reason as an
// instruction — so a reason submitted with an allowance must not be handed over.
func TestDecideApprovalDropsAReasonOnAllow(t *testing.T) {
	h, _ := newApprovalHarness(t, nil, tool.NewRegistry(), 5*time.Second)
	h.login(t)
	a, rec := collectApprover(h.srv.approvals, 3*time.Second)

	decided := make(chan tool.Decision, 1)
	go func() {
		d, _ := a.Approve(context.Background(), approvalRequest())
		decided <- d
	}()
	deadline := time.Now().Add(2 * time.Second)
	for rec.announced() == "" && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	id := rec.announced()
	if id == "" {
		t.Fatal("no request was announced")
	}

	resp := h.postJSON(t, "/api/chat/sessions/sess-1/approvals/"+id,
		map[string]any{"decision": "allow_once", "reason": "whatever was typed"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	d := <-decided
	if !d.Allowed() {
		t.Fatalf("decision = %+v", d)
	}
	if d.Reason != "" {
		t.Errorf("reason = %q; an allowance must not carry one", d.Reason)
	}
}

// TestApprovalTimeoutDefaultIsShorterThanAQuestion pins the relationship: an
// approval blocks an action, so leaving it hanging is worse than leaving a
// question hanging.
func TestApprovalTimeoutDefaultIsShorterThanAQuestion(t *testing.T) {
	if DefaultApprovalTimeout >= DefaultAskTimeout {
		t.Errorf("DefaultApprovalTimeout = %v, DefaultAskTimeout = %v; an approval should not wait as long as a question",
			DefaultApprovalTimeout, DefaultAskTimeout)
	}
	s := &Server{logger: zap.NewNop()}
	if got := s.approvalTimeout(); got != DefaultApprovalTimeout {
		t.Errorf("approvalTimeout with no config = %v, want %v", got, DefaultApprovalTimeout)
	}
	s.chat.ApprovalTimeout = 0
	if got := s.approvalTimeout(); got != DefaultApprovalTimeout {
		t.Errorf("approvalTimeout with an unset value = %v, want the default", got)
	}
}

// approvalTurn is a model turn that calls one gated tool.
func approvalTurn(toolName string) []*schema.Message {
	return []*schema.Message{{
		Role:    schema.Assistant,
		Content: "我需要写一个文件。",
		ToolCalls: []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      toolName,
				Arguments: `{"command":"git push origin main"}`,
			},
		}},
	}}
}

// TestApprovalGateInARealTurn is the whole feature end to end: the model asks to
// run a command, the card goes out on the stream, the decision arrives on a second
// request, and the turn continues — with the action either taken or not, which is
// the only question the gate exists to answer.
func TestApprovalGateInARealTurn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision string
		reason   string
		wantRan  int32
	}{
		{name: "allowed", decision: "allow_once", wantRan: 1},
		{name: "refused", decision: "deny", reason: "不要动主分支", wantRan: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ran int32
			reg := tool.NewRegistry()
			if err := reg.Register(tool.WithCapability(
				&recordingWriteTool{name: "bash", cap: tool.CapExec, mark: &ran}, tool.CapExec)); err != nil {
				t.Fatal(err)
			}

			h, _ := newApprovalHarness(t, [][]*schema.Message{
				approvalTurn("bash"),
				{{Role: schema.Assistant, Content: "好，按你说的办。"}},
			}, reg, 10*time.Second)
			h.login(t)
			sessionID := createSession(t, h)

			startResp := startTurn(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages",
				map[string]string{"content": "把改动推上去"})
			_ = startResp.Body.Close()
			if startResp.StatusCode != http.StatusAccepted {
				t.Fatalf("start status = %d, want 202", startResp.StatusCode)
			}

			attachReq, err := http.NewRequest(http.MethodGet, h.base+"/api/chat/sessions/"+sessionID+"/turn", nil)
			if err != nil {
				t.Fatal(err)
			}
			attachReq.Header.Set("Accept", "text/event-stream")
			resp, err := h.client.Do(attachReq)
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("attach status = %d", resp.StatusCode)
			}

			// Read the stream the way the browser does: decide the moment the card
			// arrives, while the response is still open.
			var (
				announced  sseEvent
				decided    sseEvent
				submitted  bool
				toolResult string
			)
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
			for scanner.Scan() {
				line := scanner.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var ev sseEvent
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
					t.Fatalf("decode frame %q: %v", line, err)
				}
				switch ev["type"] {
				case "approval":
					if req, ok := ev["approval"].(map[string]any); ok {
						id, _ := req["id"].(string)
						if id == "" || submitted {
							break
						}
						if summary, _ := req["summary"].(string); !strings.Contains(summary, "git push") {
							t.Errorf("the card does not describe the action: %q", summary)
						}
						submitted = true
						announced = ev
						body := map[string]any{"decision": tc.decision}
						if tc.reason != "" {
							body["reason"] = tc.reason
						}
						decResp := h.postJSON(t, "/api/chat/sessions/"+sessionID+"/approvals/"+id, body)
						_ = decResp.Body.Close()
						if decResp.StatusCode != http.StatusOK {
							t.Errorf("decision status = %d, want 200", decResp.StatusCode)
						}
						continue
					}
					if id, _ := ev["approval_id"].(string); id != "" {
						decided = ev
					}
				case "tool_result":
					if s, _ := ev["tool_result"].(string); s != "" {
						toolResult = s
					}
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatalf("read stream: %v", err)
			}
			if !submitted {
				t.Fatal("no approval was announced on the stream; the card would never appear")
			}
			if announced == nil {
				t.Fatal("the announcement carried no request")
			}
			if decided == nil {
				t.Fatal("the request was never settled on the stream; the card would sit there offering buttons")
			}
			if got, _ := decided["approval_decision"].(string); got != tc.decision {
				t.Errorf("settled decision = %q, want %q", got, tc.decision)
			}
			if tc.decision == "deny" {
				if got, _ := decided["approval_reason"].(string); got != tc.reason {
					t.Errorf("settled reason = %q, want %q", got, tc.reason)
				}
			}

			// The only question that matters.
			if got := atomic.LoadInt32(&ran); got != tc.wantRan {
				t.Errorf("the tool ran %d times, want %d", got, tc.wantRan)
			}
			if tc.wantRan == 0 {
				if !strings.Contains(toolResult, "已被拒绝") {
					t.Errorf("the model was not told it was refused: %q", toolResult)
				}
				if !strings.Contains(toolResult, tc.reason) {
					t.Errorf("the reason did not reach the model: %q", toolResult)
				}
			}
		})
	}
}
