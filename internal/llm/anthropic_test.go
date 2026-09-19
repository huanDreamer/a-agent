package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/retry"
)

// The tests below read the requests the adapter actually put on the wire — as
// raw JSON, not through the adapter's own request types, so that a mistake in
// those types cannot make a broken request look correct. Every one of them goes
// through llm.New with Kind set, which means the kind dispatch is exercised by
// all of them rather than by one test alone.

// capturedRequest records the last request a server double received.
//
// The mutex is not decoration: a streaming test reads this while the handler
// that wrote it may still be returning, so an unguarded read would be a race.
type capturedRequest struct {
	mu    sync.Mutex
	calls int
	path  string
	head  http.Header
	raw   []byte
}

func (c *capturedRequest) record(r *http.Request, raw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.path = r.URL.Path
	c.head = r.Header.Clone()
	c.raw = raw
}

// count is how many requests arrived, which is what a retrying test asserts on.
func (c *capturedRequest) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// body decodes the captured request as a JSON object.
func (c *capturedRequest) body(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out map[string]any
	if err := json.Unmarshal(c.raw, &out); err != nil {
		t.Fatalf("the request body is not a JSON object: %v (%s)", err, c.raw)
	}
	return out
}

// hasRaw reports whether the captured request contained a literal string, which
// is how a test proves something was *not* sent.
func (c *capturedRequest) hasRaw(s string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Contains(string(c.raw), s)
}

func (c *capturedRequest) header(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.head.Get(key)
}

func (c *capturedRequest) pathOf() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path
}

// anthropicServer starts a server double. respond is called with the 1-based
// call number and writes whatever the test wants to answer with; a nil respond
// answers an empty 200.
func anthropicServer(t *testing.T, respond func(w http.ResponseWriter, call int)) (*httptest.Server, *capturedRequest) {
	t.Helper()
	rec := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.record(r, raw)
		if respond != nil {
			respond(w, rec.count())
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// sendAnthropicJSON writes one canned reply.
func sendAnthropicJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// anthropicOK is the body a test answers with when it only cares about the
// request: one text block, one stop reason and usable usage.
const anthropicOK = `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
	`"usage":{"input_tokens":1,"output_tokens":1}}`

// anthropicModelFor builds the adapter the way production does — through
// llm.New and the provider's kind — pointed at srv.
func anthropicModelFor(t *testing.T, srv *httptest.Server, mutate func(*Provider)) model.BaseChatModel {
	t.Helper()
	p := Provider{
		Name:    "mock",
		BaseURL: srv.URL,
		APIKey:  "test-key",
		Model:   "mock-model",
		Kind:    KindAnthropicMessages,
	}
	if mutate != nil {
		mutate(&p)
	}
	m, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// turns reads the request's messages.
func turns(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages is %T, want an array: %v", body["messages"], body["messages"])
	}
	out := make([]map[string]any, 0, len(raw))
	for i, m := range raw {
		msg, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("messages[%d] is %T, want an object", i, m)
		}
		out = append(out, msg)
	}
	return out
}

// blocks reads one turn's content blocks.
func blocks(t *testing.T, msg map[string]any) []map[string]any {
	t.Helper()
	raw, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content is %T, want an array: %v", msg["content"], msg["content"])
	}
	out := make([]map[string]any, 0, len(raw))
	for i, b := range raw {
		blk, ok := b.(map[string]any)
		if !ok {
			t.Fatalf("content[%d] is %T, want an object", i, b)
		}
		out = append(out, blk)
	}
	return out
}

// assertBlock compares one content block with its exact expected shape. It is a
// whole-map comparison because the failure mode worth catching is a *stray*
// field: an extra key is how a correctly-shaped block starts being rejected.
func assertBlock(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("block =\n%#v\nwant\n%#v", got, want)
	}
}

// drain reads a stream to its end, failing on any error: every streaming test
// here expects a clean stream.
func drain(t *testing.T, sr *schema.StreamReader[*schema.Message]) []*schema.Message {
	t.Helper()
	defer sr.Close()
	var out []*schema.Message
	for {
		msg, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if msg != nil {
			out = append(out, msg)
		}
	}
}

// mustConcat merges the chunks the way the agent loop does, which is the shape a
// caller actually consumes.
func mustConcat(t *testing.T, chunks []*schema.Message) *schema.Message {
	t.Helper()
	merged, err := schema.ConcatMessages(chunks)
	if err != nil {
		t.Fatalf("ConcatMessages: %v", err)
	}
	return merged
}

// TestAnthropic_EndpointAndHeaders pins the request line and the credential
// header, which is the whole reason the provider carries AuthStyle: an endpoint
// that wants a bearer token rejects x-api-key, and vice versa.
func TestAnthropic_EndpointAndHeaders(t *testing.T) {
	cases := []struct {
		name string
		// suffix is appended to the server URL to spell the configured BaseURL.
		suffix    string
		authStyle string
		apiKey    string
		wantPath  string
		wantAuth  map[string]string
		// absent is the credential header that must NOT be set: sending both
		// leaves a gateway to pick whichever it checks first.
		absent string
	}{
		{
			name:   "base url without a version segment",
			apiKey: "key", wantPath: "/v1/messages",
			wantAuth: map[string]string{"x-api-key": "key"},
			absent:   "authorization",
		},
		{
			name:   "base url already carrying /v1",
			suffix: "/v1", authStyle: AuthStyleAPIKey, apiKey: "key", wantPath: "/v1/messages",
			wantAuth: map[string]string{"x-api-key": "key"},
			absent:   "authorization",
		},
		{
			// Claude Code's shape: ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic
			// with ANTHROPIC_AUTH_TOKEN sent as a bearer token.
			name:   "anthropic shaped gateway with a bearer token",
			suffix: "/anthropic", authStyle: AuthStyleBearer, apiKey: "sk-token",
			wantPath: "/anthropic/v1/messages",
			wantAuth: map[string]string{"authorization": "Bearer sk-token"},
			absent:   "x-api-key",
		},
		{
			name:   "auth style api-key spelled out",
			suffix: "/anthropic", authStyle: AuthStyleAPIKey, apiKey: "sk-ant",
			wantPath: "/anthropic/v1/messages",
			wantAuth: map[string]string{"x-api-key": "sk-ant"},
			absent:   "authorization",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := anthropicServer(t, func(w http.ResponseWriter, _ int) {
				sendAnthropicJSON(w, anthropicOK)
			})
			m := anthropicModelFor(t, srv, func(p *Provider) {
				p.BaseURL = srv.URL + tc.suffix
				p.AuthStyle = tc.authStyle
				p.APIKey = tc.apiKey
			})
			if _, err := m.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hello"}}); err != nil {
				t.Fatalf("Generate: %v", err)
			}

			if got := rec.pathOf(); got != tc.wantPath {
				t.Errorf("path = %q, want %q (base url = %q)", got, tc.wantPath, srv.URL+tc.suffix)
			}
			for key, want := range tc.wantAuth {
				if got := rec.header(key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if got := rec.header(tc.absent); got != "" {
				t.Errorf("%s = %q, want it unset: the endpoint must see exactly one credential", tc.absent, got)
			}
			if got := rec.header("anthropic-version"); got != DefaultAnthropicVersion {
				t.Errorf("anthropic-version = %q, want %q", got, DefaultAnthropicVersion)
			}
			if got := rec.header("content-type"); got != "application/json" {
				t.Errorf("content-type = %q, want application/json", got)
			}
			if got := rec.header("accept"); got != "application/json" {
				t.Errorf("accept = %q, want application/json on a non-streamed call", got)
			}
		})
	}
}

// TestAnthropic_VersionHeader covers the override: an endpoint pinned to an
// older protocol version answers a newer header with an error.
func TestAnthropic_VersionHeader(t *testing.T) {
	srv, rec := anthropicServer(t, func(w http.ResponseWriter, _ int) {
		sendAnthropicJSON(w, anthropicOK)
	})
	m := anthropicModelFor(t, srv, func(p *Provider) { p.AnthropicVersion = "2024-10-22" })
	if _, err := m.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := rec.header("anthropic-version"); got != "2024-10-22" {
		t.Errorf("anthropic-version = %q, want the configured 2024-10-22", got)
	}
}

// TestAnthropic_SystemHoistedAndTurnsConverted is the core protocol test: the
// system prompt leaves the message list, an assistant turn becomes text plus
// tool_use blocks, and consecutive tool results become ONE user turn — the API
// rejects a tool_result that does not follow its tool_use immediately, and
// rejects a user turn that does not alternate with an assistant one.
func TestAnthropic_SystemHoistedAndTurnsConverted(t *testing.T) {
	srv, rec := anthropicServer(t, func(w http.ResponseWriter, _ int) {
		sendAnthropicJSON(w, anthropicOK)
	})
	m := anthropicModelFor(t, srv, nil)

	history := []*schema.Message{
		{Role: schema.System, Content: "be brief"},
		{Role: schema.System, Content: "answer in Chinese"},
		{Role: schema.User, Content: "what time is it?"},
		{Role: schema.Assistant,
			Content:          "let me check",
			ReasoningContent: "the user wants the time",
			ToolCalls: []schema.ToolCall{
				{ID: "toolu_1", Type: "function", Function: schema.FunctionCall{Name: "clock", Arguments: `{"tz":"UTC"}`}},
				// A call with no arguments at all: the API requires an object.
				{ID: "toolu_2", Type: "function", Function: schema.FunctionCall{Name: "clock", Arguments: ""}},
			}},
		{Role: schema.Tool, Content: "12:00", ToolCallID: "toolu_1", ToolName: "clock"},
		{Role: schema.Tool, Content: "12:01", ToolCallID: "toolu_2", ToolName: "clock"},
	}
	if _, err := m.Generate(context.Background(), history); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	body := rec.body(t)
	if got := body["system"]; got != "be brief\n\nanswer in Chinese" {
		t.Errorf("system = %#v, want the two system messages joined with a blank line", got)
	}

	got := turns(t, body)
	if len(got) != 3 {
		t.Fatalf("turns = %d (%v), want 3: user, assistant, one merged tool-result turn", len(got), got)
	}
	for i, msg := range got {
		if msg["role"] == "system" {
			t.Errorf("turns[%d] is a system turn; the API rejects role system inside messages", i)
		}
		if i > 0 && msg["role"] == got[i-1]["role"] {
			t.Errorf("turns[%d] and turns[%d] are both %v; the API requires alternating roles", i-1, i, msg["role"])
		}
	}

	if got[0]["role"] != "user" {
		t.Errorf("turns[0].role = %v, want user", got[0]["role"])
	}
	assertBlock(t, blocks(t, got[0])[0], map[string]any{"type": "text", "text": "what time is it?"})

	if got[1]["role"] != "assistant" {
		t.Errorf("turns[1].role = %v, want assistant", got[1]["role"])
	}
	assistant := blocks(t, got[1])
	if len(assistant) != 3 {
		t.Fatalf("assistant blocks = %d (%v), want text + two tool_use", len(assistant), assistant)
	}
	assertBlock(t, assistant[0], map[string]any{"type": "text", "text": "let me check"})
	assertBlock(t, assistant[1], map[string]any{
		"type": "tool_use", "id": "toolu_1", "name": "clock",
		"input": map[string]any{"tz": "UTC"},
	})
	assertBlock(t, assistant[2], map[string]any{
		"type": "tool_use", "id": "toolu_2", "name": "clock",
		"input": map[string]any{},
	})

	if got[2]["role"] != "user" {
		t.Errorf("turns[2].role = %v, want user: tool results answer as a user turn", got[2]["role"])
	}
	results := blocks(t, got[2])
	if len(results) != 2 {
		t.Fatalf("tool-result blocks = %d (%v), want both results in ONE user turn", len(results), results)
	}
	assertBlock(t, results[0], map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "12:00"})
	assertBlock(t, results[1], map[string]any{"type": "tool_result", "tool_use_id": "toolu_2", "content": "12:01"})

	// Reasoning is dropped rather than replayed: the API accepts a thinking block
	// back only with the signature it issued, which the eino message does not
	// carry.
	if rec.hasRaw("the user wants the time") {
		t.Error("reasoning_content was sent back; Anthropic rejects a thinking block without its signature")
	}
	if rec.hasRaw(`"thinking"`) {
		t.Error("a thinking block was sent in the request")
	}
}

// TestAnthropic_ImageParts covers the two shapes an image block can take, and
// the parts that have no shape at all: a malformed content array fails the whole
// request, so an unusable part is dropped instead.
func TestAnthropic_ImageParts(t *testing.T) {
	srv, rec := anthropicServer(t, func(w http.ResponseWriter, _ int) {
		sendAnthropicJSON(w, anthropicOK)
	})
	m := anthropicModelFor(t, srv, nil)

	encoded := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	remote := "https://example.com/cat.png"
	if _, err := m.Generate(context.Background(), []*schema.Message{{
		Role:    schema.User,
		Content: "what is in this image?",
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "what is in this image?"},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{Base64Data: &encoded, MIMEType: "image/png"},
			}},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{URL: &remote},
			}},
			// Audio has no content block in the Messages API.
			{Type: schema.ChatMessagePartTypeAudioURL},
			// An image part with neither bytes nor a URL is nothing to send.
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{}},
		},
	}}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	got := blocks(t, turns(t, rec.body(t))[0])
	if len(got) != 3 {
		t.Fatalf("blocks = %d (%v), want text + two images", len(got), got)
	}
	assertBlock(t, got[0], map[string]any{"type": "text", "text": "what is in this image?"})
	assertBlock(t, got[1], map[string]any{"type": "image", "source": map[string]any{
		"type": "base64", "media_type": "image/png", "data": encoded,
	}})
	assertBlock(t, got[2], map[string]any{"type": "image", "source": map[string]any{
		"type": "url", "url": remote,
	}})

	// A turn whose parts are all unusable must stay a text turn instead of
	// becoming an empty content array.
	if _, err := m.Generate(context.Background(), []*schema.Message{{
		Role:                  schema.User,
		Content:               "text only",
		UserInputMultiContent: []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeAudioURL}},
	}}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	fallback := blocks(t, turns(t, rec.body(t))[0])
	if len(fallback) != 1 {
		t.Fatalf("blocks = %v, want the message left as one text block", fallback)
	}
	assertBlock(t, fallback[0], map[string]any{"type": "text", "text": "text only"})
}

// TestAnthropic_RequestDefaults covers the fields the API requires rather than
// accepts: max_tokens has no default upstream, so one has to be invented here,
// and a caller's own cap has to win over it.
func TestAnthropic_RequestDefaults(t *testing.T) {
	cases := []struct {
		name         string
		maxOutput    int
		opts         []model.Option
		wantMaxToken float64
		wantTemp     float64
		wantTempSet  bool
	}{
		{name: "default cap", wantMaxToken: DefaultAnthropicMaxTokens},
		{name: "provider cap", maxOutput: 321, wantMaxToken: 321},
		{name: "call cap wins over the provider cap", maxOutput: 321,
			opts: []model.Option{model.WithMaxTokens(64)}, wantMaxToken: 64},
		{name: "temperature is sent only when asked for", maxOutput: 100,
			opts: []model.Option{model.WithTemperature(0.3)}, wantMaxToken: 100, wantTemp: 0.3, wantTempSet: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := anthropicServer(t, func(w http.ResponseWriter, _ int) {
				sendAnthropicJSON(w, anthropicOK)
			})
			m := anthropicModelFor(t, srv, func(p *Provider) { p.MaxOutputTokens = tc.maxOutput })
			if _, err := m.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}}, tc.opts...); err != nil {
				t.Fatalf("Generate: %v", err)
			}
			body := rec.body(t)
			if got := body["model"]; got != "mock-model" {
				t.Errorf("model = %#v, want mock-model", got)
			}
			if got := body["max_tokens"]; got != tc.wantMaxToken {
				t.Errorf("max_tokens = %#v, want %v", got, tc.wantMaxToken)
			}
			if got := body["stream"]; got != false {
				t.Errorf("stream = %#v, want false on Generate", got)
			}
			if _, ok := body["system"]; ok {
				t.Error("system was sent although the history has no system message")
			}
			if _, ok := body["tools"]; ok {
				t.Error("tools was sent although none are bound")
			}
			gotTemp, ok := body["temperature"]
			if ok != tc.wantTempSet {
				t.Fatalf("temperature present = %v (%v), want %v", ok, gotTemp, tc.wantTempSet)
			}
			if tc.wantTempSet && gotTemp != tc.wantTemp {
				t.Errorf("temperature = %#v, want %v", gotTemp, tc.wantTemp)
			}
		})
	}
}

// TestAnthropic_ToolsOnTheWire covers the tool schema, and the copy-on-write
// binding the ReAct agent relies on: it calls WithTools per request, so a shared
// model that mutated here would leak one conversation's tools into another's.
func TestAnthropic_ToolsOnTheWire(t *testing.T) {
	srv, rec := anthropicServer(t, func(w http.ResponseWriter, _ int) {
		sendAnthropicJSON(w, anthropicOK)
	})
	base := anthropicModelFor(t, srv, nil)

	bound, err := base.(model.ToolCallingChatModel).WithTools([]*schema.ToolInfo{{
		Name: "clock", Desc: "tells the time",
		ParamsOneOf: mustParams(t, `{"type":"object","properties":{"tz":{"type":"string"}},"required":["tz"]}`),
	}, {
		// A tool with no parameters still needs a schema.
		Name: "now", Desc: "the current time",
	}})
	if err != nil {
		t.Fatalf("WithTools: %v", err)
	}
	if len(base.(*anthropicModel).tools) != 0 {
		t.Error("WithTools mutated the receiver; the agent reuses one base model per conversation")
	}
	if got := bound.(interface{ Name() string }).Name(); got != "mock-model" {
		t.Errorf("Name() after WithTools = %q, want mock-model", got)
	}
	if got := bound.(interface{ Provider() string }).Provider(); got != "mock" {
		t.Errorf("Provider() after WithTools = %q, want mock", got)
	}

	if _, err := bound.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	raw, ok := rec.body(t)["tools"].([]any)
	if !ok {
		t.Fatalf("tools is %T, want an array", rec.body(t)["tools"])
	}
	if len(raw) != 2 {
		t.Fatalf("tools = %d (%v), want 2", len(raw), raw)
	}
	first, _ := raw[0].(map[string]any)
	if first["name"] != "clock" || first["description"] != "tells the time" {
		t.Errorf("tools[0] = %v, want the name and description from ToolInfo", first)
	}
	schemaJSON, ok := first["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("input_schema is %T, want an object", first["input_schema"])
	}
	if schemaJSON["type"] != "object" {
		t.Errorf("input_schema.type = %v, want object", schemaJSON["type"])
	}
	props, _ := schemaJSON["properties"].(map[string]any)
	if _, ok := props["tz"]; !ok {
		t.Errorf("input_schema.properties = %v, want the tool's tz parameter", props)
	}
	second, _ := raw[1].(map[string]any)
	if empty, ok := second["input_schema"].(map[string]any); !ok || empty["type"] != "object" {
		t.Errorf("tools[1].input_schema = %v, want an empty object schema", second["input_schema"])
	}
}

// TestAnthropic_ResponseParsing covers what a non-streamed reply turns into.
// Every case matters: the argument string is what a tool is called with, the
// usage is what the turn costs, and "tool_use" as a stop reason is how the agent
// loop knows to run tools at all.
func TestAnthropic_ResponseParsing(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantContent   string
		wantReasoning string
		wantFinish    string
		wantToolCalls []schema.ToolCall
		wantUsage     *schema.TokenUsage
	}{
		{
			name: "text, thinking, a tool call and usage",
			body: `{"content":[{"type":"text","text":"Let me "},{"type":"text","text":"check."},` +
				`{"type":"thinking","thinking":"hmm"},` +
				`{"type":"tool_use","id":"toolu_1","name":"clock","input":{"tz":"UTC"}}],` +
				`"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":2}}`,
			wantContent:   "Let me check.",
			wantReasoning: "hmm",
			wantFinish:    "tool_use",
			wantToolCalls: []schema.ToolCall{{
				ID: "toolu_1", Type: "function",
				Function: schema.FunctionCall{Name: "clock", Arguments: `{"tz":"UTC"}`},
			}},
			wantUsage: &schema.TokenUsage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
		},
		{
			name: "a tool call with no arguments",
			body: `{"content":[{"type":"tool_use","id":"toolu_2","name":"now","input":{}}],` +
				`"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":1}}`,
			wantFinish: "tool_use",
			wantToolCalls: []schema.ToolCall{{
				ID: "toolu_2", Type: "function",
				Function: schema.FunctionCall{Name: "now", Arguments: "{}"},
			}},
			wantUsage: &schema.TokenUsage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
		},
		{
			name:        "a plain answer",
			body:        `{"content":[{"type":"text","text":"42"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":4}}`,
			wantContent: "42",
			wantFinish:  "end_turn",
			wantUsage:   &schema.TokenUsage{PromptTokens: 9, CompletionTokens: 4, TotalTokens: 13},
		},
		{
			// An empty answer is not a failure: the agent loop treats it as one.
			name:       "no content blocks",
			body:       `{"content":[],"stop_reason":"end_turn"}`,
			wantFinish: "end_turn",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := anthropicServer(t, func(w http.ResponseWriter, _ int) {
				sendAnthropicJSON(w, tc.body)
			})
			m := anthropicModelFor(t, srv, nil)
			got, err := m.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if got.Role != schema.Assistant {
				t.Errorf("role = %q, want assistant", got.Role)
			}
			if got.Content != tc.wantContent {
				t.Errorf("content = %q, want %q", got.Content, tc.wantContent)
			}
			if got.ReasoningContent != tc.wantReasoning {
				t.Errorf("reasoning = %q, want %q", got.ReasoningContent, tc.wantReasoning)
			}
			if !reflect.DeepEqual(got.ToolCalls, tc.wantToolCalls) {
				t.Errorf("tool calls = %+v, want %+v", got.ToolCalls, tc.wantToolCalls)
			}
			if got.ResponseMeta == nil {
				t.Fatal("missing response meta: usage and finish reason are how a turn is billed and how tools get run")
			}
			if got.ResponseMeta.FinishReason != tc.wantFinish {
				t.Errorf("finish reason = %q, want %q", got.ResponseMeta.FinishReason, tc.wantFinish)
			}
			if !reflect.DeepEqual(got.ResponseMeta.Usage, tc.wantUsage) {
				t.Errorf("usage = %+v, want %+v", got.ResponseMeta.Usage, tc.wantUsage)
			}
		})
	}
}

// TestAnthropic_HTTPError covers the failure path: the status has to survive into
// the error (the retry predicate reads it), and the endpoint's own explanation
// has to survive too, because it is the only thing that says what was wrong with
// the request.
func TestAnthropic_HTTPError(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantBody string
	}{
		{
			name:   "an anthropic error envelope",
			status: http.StatusBadRequest,
			body: `{"type":"error","error":{"type":"invalid_request_error",` +
				`"message":"max_tokens: must be greater than 0"}}`,
			wantBody: "max_tokens: must be greater than 0",
		},
		{
			name:     "a body that is not an envelope",
			status:   http.StatusInternalServerError,
			body:     "upstream exploded",
			wantBody: "upstream exploded",
		},
		{
			name:     "an envelope with no message",
			status:   http.StatusTooManyRequests,
			body:     `{"type":"error","error":{"type":"rate_limit_error"}}`,
			wantBody: `{"type":"error","error":{"type":"rate_limit_error"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := anthropicServer(t, func(w http.ResponseWriter, _ int) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			m := anthropicModelFor(t, srv, nil)
			_, err := m.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
			if err == nil {
				t.Fatal("expected an error for a non-2xx status")
			}
			var llmErr *LLMError
			if !errors.As(err, &llmErr) {
				t.Fatalf("error is %T, want *LLMError so retrying can read the status", err)
			}
			if llmErr.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", llmErr.StatusCode, tc.status)
			}
			if llmErr.Body != tc.wantBody {
				t.Errorf("body = %q, want %q", llmErr.Body, tc.wantBody)
			}
			if llmErr.Provider != "mock" || llmErr.Model != "mock-model" {
				t.Errorf("error does not name the provider and model: %+v", llmErr)
			}
			if !strings.Contains(err.Error(), "status="+itoa(tc.status)) {
				t.Errorf("error string %q does not carry the status", err.Error())
			}
		})
	}
}

// TestAnthropic_StreamFailsBeforeReturning covers the other half of the failure
// path: a stream that never opened must fail at Stream, not by handing back a
// reader that dies later — the retrying wrapper only re-attempts the first.
func TestAnthropic_StreamFailsBeforeReturning(t *testing.T) {
	srv, _ := anthropicServer(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	})
	m := anthropicModelFor(t, srv, nil)
	sr, err := m.Stream(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err == nil {
		sr.Close()
		t.Fatal("Stream returned a reader for a rejected request")
	}
	var llmErr *LLMError
	if !errors.As(err, &llmErr) || llmErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("error = %v, want an *LLMError carrying 401", err)
	}
}

// anthropicToolStream is a canned SSE body in the shape the Messages API sends
// for one tool-calling turn: a thinking block, a text block, and a tool_use block
// whose arguments arrive in fragments. Note there is no `data: [DONE]`: the
// stream ends at message_stop.
const anthropicToolStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"mock-model","usage":{"input_tokens":12,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me "}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"check."}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_9","name":"clock","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"tz\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"UTC\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

// TestAnthropic_StreamParsing drives the streaming path end to end: text deltas,
// a tool call whose arguments arrive in fragments, usage, and a clean end.
func TestAnthropic_StreamParsing(t *testing.T) {
	srv, rec := anthropicServer(t, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicToolStream))
	})
	m := anthropicModelFor(t, srv, nil)

	sr, err := m.Stream(context.Background(), []*schema.Message{
		{Role: schema.System, Content: "be brief"},
		{Role: schema.User, Content: "what time is it?"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drain(t, sr)

	body := rec.body(t)
	if got := body["stream"]; got != true {
		t.Errorf("stream = %#v, want true", got)
	}
	if got := rec.header("accept"); got != "text/event-stream" {
		t.Errorf("accept = %q, want text/event-stream", got)
	}
	if got := body["max_tokens"]; got != float64(DefaultAnthropicMaxTokens) {
		t.Errorf("max_tokens = %#v, want the default", got)
	}

	merged := mustConcat(t, chunks)
	if merged.Content != "Let me check." {
		t.Errorf("content = %q, want the text deltas joined", merged.Content)
	}
	if merged.ReasoningContent != "hmm" {
		t.Errorf("reasoning = %q, want the thinking delta", merged.ReasoningContent)
	}
	wantCall := schema.ToolCall{
		ID: "toolu_9", Type: "function",
		Function: schema.FunctionCall{Name: "clock", Arguments: `{"tz":"UTC"}`},
	}
	if !reflect.DeepEqual(merged.ToolCalls, []schema.ToolCall{wantCall}) {
		t.Errorf("tool calls = %+v, want exactly one %+v: fragments joined once, and not sent twice",
			merged.ToolCalls, wantCall)
	}
	// The call must arrive in exactly one chunk: eino's concat keeps calls with
	// no index as separate entries, so a call repeated in the closing chunk would
	// be executed twice.
	carriers := 0
	for _, chunk := range chunks {
		if len(chunk.ToolCalls) > 0 {
			carriers++
		}
	}
	if carriers != 1 {
		t.Errorf("%d chunks carried the tool call, want 1", carriers)
	}
	if merged.ResponseMeta == nil || merged.ResponseMeta.FinishReason != "tool_use" {
		t.Errorf("finish reason = %+v, want tool_use", merged.ResponseMeta)
	}
	want := &schema.TokenUsage{PromptTokens: 12, CompletionTokens: 7, TotalTokens: 19}
	if merged.ResponseMeta == nil || !reflect.DeepEqual(merged.ResponseMeta.Usage, want) {
		t.Errorf("usage = %+v, want %+v (input from message_start, output from message_delta)",
			merged.ResponseMeta, want)
	}
}

// TestAnthropic_StreamWithoutMessageStop covers a stream that just ends: the
// tokens already reported must still reach the caller, or the turn is billed as
// free.
func TestAnthropic_StreamWithoutMessageStop(t *testing.T) {
	srv, _ := anthropicServer(t, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"mock-model\",\"usage\":{\"input_tokens\":4,\"output_tokens\":0}}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":2}}\n\n"))
	})
	m := anthropicModelFor(t, srv, nil)
	sr, err := m.Stream(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	merged := mustConcat(t, drain(t, sr))
	if merged.Content != "partial" {
		t.Errorf("content = %q, want the delta that did arrive", merged.Content)
	}
	if merged.ResponseMeta == nil || merged.ResponseMeta.Usage == nil {
		t.Fatal("usage was dropped when the stream ended without message_stop")
	}
	if merged.ResponseMeta.Usage.TotalTokens != 6 {
		t.Errorf("total tokens = %d, want 6", merged.ResponseMeta.Usage.TotalTokens)
	}
	if merged.ResponseMeta.FinishReason != "max_tokens" {
		t.Errorf("finish reason = %q, want max_tokens", merged.ResponseMeta.FinishReason)
	}
}

// TestAnthropic_StreamErrorEvent covers the error event: the endpoint reports a
// failure mid-stream as an event, and the reader has to learn about it through
// Recv — that is the only channel a stream has.
func TestAnthropic_StreamErrorEvent(t *testing.T) {
	srv, _ := anthropicServer(t, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"mock-model\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))
	})
	m := anthropicModelFor(t, srv, nil)
	sr, err := m.Stream(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer sr.Close()

	var got error
	for {
		_, recvErr := sr.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			got = recvErr
			break
		}
	}
	if got == nil {
		t.Fatal("the error event was swallowed")
	}
	if !strings.Contains(got.Error(), "Overloaded") {
		t.Errorf("error = %v, want the endpoint's message", got)
	}
	// The model name came from message_start, which is how an operator can tell
	// which endpoint answered.
	if !strings.Contains(got.Error(), "mock-model") {
		t.Errorf("error = %v, want it to name the model that failed", got)
	}
}

// TestAnthropic_RetryWrapsTheAnthropicPath proves the retrying wrapper reaches
// this adapter too: a 429 is a transient failure, and without the wrapper every
// rate limit would end the turn.
func TestAnthropic_RetryWrapsTheAnthropicPath(t *testing.T) {
	srv, rec := anthropicServer(t, func(w http.ResponseWriter, call int) {
		if call == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		sendAnthropicJSON(w, `{"content":[{"type":"text","text":"second try"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	m, err := New(Provider{
		Name: "mock", BaseURL: srv.URL, APIKey: "k", Model: "mock-model",
		Kind: KindAnthropicMessages,
	}, WithRetry(retry.Policy{MaxAttempts: 2, BaseDelay: time.Millisecond}, zap.NewNop()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := m.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got.Content != "second try" {
		t.Errorf("content = %q, want the retried reply", got.Content)
	}
	if n := rec.count(); n != 2 {
		t.Errorf("requests = %d, want 2: the anthropic path must be wrapped in retrying", n)
	}
}

// countingTransport proves the adapter used the client it was given.
type countingTransport struct {
	inner http.RoundTripper
	calls *int
}

func (t countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*t.calls++
	return t.inner.RoundTrip(r)
}

// TestAnthropic_ClientIsInjectable covers the seam: a caller that needs a
// timeout replaces the client, and a test can point the adapter at a double
// without a global.
func TestAnthropic_ClientIsInjectable(t *testing.T) {
	srv, _ := anthropicServer(t, func(w http.ResponseWriter, _ int) {
		sendAnthropicJSON(w, anthropicOK)
	})

	var calls int
	client := srv.Client()
	client.Transport = countingTransport{inner: client.Transport, calls: &calls}
	m := newAnthropicModel(Provider{
		Name: "mock", BaseURL: srv.URL, APIKey: "k", Model: "mock-model",
	}, client)
	if _, err := m.Generate(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if calls != 1 {
		t.Errorf("the injected client saw %d requests, want 1", calls)
	}
	if def := newAnthropicModel(Provider{}, nil); def.httpClient != http.DefaultClient {
		t.Error("a nil client must default to http.DefaultClient")
	}
}

// TestNew_KindDispatch pins the rule that decides which protocol a provider
// speaks. Only an explicit anthropic-messages takes the new adapter: everything
// else — including a kind this build does not know — keeps the OpenAI path, so a
// stored row that predates this field cannot change behaviour.
func TestNew_KindDispatch(t *testing.T) {
	cases := []struct {
		name          string
		kind          string
		wantAnthropic bool
	}{
		{"no kind is the OpenAI protocol", "", false},
		{"explicit openai", KindOpenAI, false},
		{"a kind this build does not know", "bedrock", false},
		{"anthropic messages", KindAnthropicMessages, true},
		{"kind is trimmed and case-insensitive", "  Anthropic-Messages  ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := New(Provider{
				Name: "p", BaseURL: "https://example.invalid", APIKey: "k",
				Model: "m", Kind: tc.kind,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, isAnthropic := m.(*anthropicModel)
			if isAnthropic != tc.wantAnthropic {
				t.Errorf("kind %q produced the anthropic adapter = %v, want %v", tc.kind, isAnthropic, tc.wantAnthropic)
			}
			if !tc.wantAnthropic {
				if _, ok := m.(*openAIModel); !ok {
					t.Errorf("kind %q did not take the OpenAI path (%T)", tc.kind, m)
				}
			}
		})
	}
}

// TestNew_ValidatesTheAnthropicPath checks that the anthropic route is not a way
// around the validation every other provider goes through.
func TestNew_ValidatesTheAnthropicPath(t *testing.T) {
	cases := []struct {
		name string
		p    Provider
		want string
	}{
		{"empty name", Provider{Kind: KindAnthropicMessages, BaseURL: "x", Model: "y"}, "name is required"},
		{"empty base url", Provider{Kind: KindAnthropicMessages, Name: "x", Model: "y"}, "base_url is required"},
		{"empty model", Provider{Kind: KindAnthropicMessages, Name: "x", BaseURL: "y"}, "model is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.p)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestAnthropicMessagesURL covers where a call lands for the base URLs people
// actually paste: an Anthropic-shaped gateway, a plain host, and one that already
// carries the version segment.
func TestAnthropicMessagesURL(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{"https://api.deepseek.com/anthropic", "https://api.deepseek.com/anthropic/v1/messages"},
		{"https://api.deepseek.com/anthropic/", "https://api.deepseek.com/anthropic/v1/messages"},
		{"https://api.anthropic.com", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/v1", "https://api.anthropic.com/v1/messages"},
		{"http://localhost:8080/anthropic/v1", "http://localhost:8080/anthropic/v1/messages"},
	}
	for _, tc := range cases {
		t.Run(tc.base, func(t *testing.T) {
			if got := anthropicMessagesURL(tc.base); got != tc.want {
				t.Errorf("anthropicMessagesURL(%q) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}

// TestToolInput covers the argument shapes a model produces. Anything that is not
// a JSON object cannot be sent as one — the API rejects it, and marshalling a
// malformed fragment would fail the whole request — so it becomes empty
// arguments, which the tool layer reports as a call without arguments.
func TestToolInput(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"an object passes through", `{"tz":"UTC"}`, `{"tz":"UTC"}`},
		{"surrounding space is trimmed", "  {\"tz\":\"UTC\"}  ", `{"tz":"UTC"}`},
		{"empty means no arguments", "", `{}`},
		{"a truncated fragment is not an object", `{"tz":`, `{}`},
		{"an array is not an object", `["UTC"]`, `{}`},
		{"a bare literal is not an object", `null`, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolInput(tc.args)
			if string(got) != tc.want {
				t.Errorf("toolInput(%q) = %s, want %s", tc.args, got, tc.want)
			}
			if !json.Valid(got) {
				t.Errorf("toolInput(%q) produced invalid JSON: %s", tc.args, got)
			}
		})
	}
}

// TestSplitBase64DataURL covers the data URLs en route to an image block.
func TestSplitBase64DataURL(t *testing.T) {
	cases := []struct {
		name          string
		url           string
		wantMediaType string
		wantData      string
		wantOK        bool
	}{
		{"base64 data url", "data:image/png;base64,AAAA", "image/png", "AAAA", true},
		{"remote url", "https://example.com/cat.png", "", "", false},
		{"percent-encoded payload has no base64 marker", "data:image/png,AAAA", "", "", false},
		{"no comma", "data:image/png;base64", "", "", false},
		{"no media type", "data:;base64,AAAA", "", "", false},
		{"empty payload", "data:image/png;base64,", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mediaType, data, ok := splitBase64DataURL(tc.url)
			if ok != tc.wantOK || mediaType != tc.wantMediaType || data != tc.wantData {
				t.Errorf("splitBase64DataURL(%q) = %q, %q, %v; want %q, %q, %v",
					tc.url, mediaType, data, ok, tc.wantMediaType, tc.wantData, tc.wantOK)
			}
		})
	}
}

// itoa renders a status code for a substring check without pulling strconv into
// the import block of a file that needs it once.
func itoa(n int) string { return strconv.Itoa(n) }
