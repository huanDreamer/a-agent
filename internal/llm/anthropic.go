package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// This file implements the Anthropic Messages API: the protocol behind
// api.anthropic.com and behind the Anthropic-shaped gateways Claude Code is
// pointed at with ANTHROPIC_BASE_URL (for example
// https://api.deepseek.com/anthropic).
//
// It is a second adapter rather than another way of spelling an OpenAI request,
// because the two protocols disagree about things the OpenAI shape cannot
// express:
//
//   - the system prompt is a top-level field, not a message, so a system message
//     in the eino history has to be hoisted out (an Anthropic endpoint rejects
//     role "system" inside messages);
//   - a tool result is a content block inside a user turn, not a message with a
//     role of its own, and consecutive results have to be merged into one turn
//     because the API requires alternating roles and a tool_result immediately
//     after its tool_use;
//   - max_tokens is mandatory, so a provider that names no cap needs a default;
//   - a stream is a sequence of named events (message_start, content_block_delta,
//     message_stop, ...) rather than one repeated chunk shape, and it does not
//     end with `data: [DONE]`.

// anthropicModel is the eino adapter that speaks the Messages API.
type anthropicModel struct {
	provider Provider
	tools    []*schema.ToolInfo // bound tools (immutable; use WithTools to derive)
	// httpClient is a field rather than a package-level client so a test can
	// point the adapter at a server double, or at a client with a timeout, without
	// touching global state. It defaults to http.DefaultClient.
	httpClient *http.Client
}

// newAnthropicModel builds the adapter for one provider. The client argument is
// the seam a test replaces; nil means http.DefaultClient, which is what llm.New
// passes.
func newAnthropicModel(p Provider, client *http.Client) *anthropicModel {
	if client == nil {
		client = http.DefaultClient
	}
	return &anthropicModel{provider: p, httpClient: client}
}

// Name returns the provider's configured model name, for the same reason as
// openAIModel.Name: tracing and logging attribute a call to a model through it.
func (m *anthropicModel) Name() string { return m.provider.Model }

// Provider returns the provider name (deepseek, qwen, ...).
func (m *anthropicModel) Provider() string { return m.provider.Name }

// WithTools returns a copy of m with the given tools bound, leaving the receiver
// untouched for the same reason as openAIModel.WithTools: the ReAct agent calls
// it per request, and a shared model that mutated here would leak one
// conversation's tools into another's.
func (m *anthropicModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	cp := *m
	cp.tools = append([]*schema.ToolInfo(nil), tools...)
	return &cp, nil
}

// Generate implements model.BaseChatModel.
func (m *anthropicModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	body, err := m.post(ctx, m.buildRequest(msgs, false, opts), false)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, m.wrapErr(fmt.Errorf("anthropic: read response: %w", err), 0, "")
	}
	var resp anthropicResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, m.wrapErr(fmt.Errorf("anthropic: decode response: %w", err), 0, truncateBody(raw))
	}
	return resp.toMessage(), nil
}

// Stream implements model.BaseChatModel.
//
// The reader is returned before the response has been read at all, and the
// reading happens on a goroutine: the caller decides when to stop, and a stream
// nobody reads must not hold up the call. This is also why an HTTP failure
// before the first event is returned as an error from Stream itself while
// anything later travels through the reader.
func (m *anthropicModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	body, err := m.post(ctx, m.buildRequest(msgs, true, opts), true)
	if err != nil {
		return nil, err
	}

	sr, sw := newMessagePipe(64)
	go m.pump(sw, body)
	return sr, nil
}

// pump reads the SSE stream and forwards it as eino message chunks.
//
// Every path out of here closes the writer exactly once (the deferred Close), and
// an error leaves through sw.Send(nil, err) — the reader cannot be handed an
// error any other way — so a caller draining the stream always terminates
// instead of blocking on a writer that went away.
func (m *anthropicModel) pump(sw *schema.StreamWriter[*schema.Message], body io.ReadCloser) {
	defer sw.Close()
	defer func() { _ = body.Close() }()

	var (
		model        string
		inputTokens  int
		outputTokens int
		stopReason   string
		// open holds a tool_use block that is still being streamed, keyed by its
		// content-block index: the id and name arrive at content_block_start and
		// the arguments arrive afterwards as fragments.
		open = map[int]*anthropicPendingTool{}
		// done guards the closing chunk, which a provider that drops the
		// connection without message_stop would otherwise never get.
		done bool
	)

	send := func(msg *schema.Message) bool { return !sw.Send(msg, nil) }
	fail := func(err error) {
		_ = sw.Send(nil, err)
	}
	// finish emits the closing chunk: the stop reason and the token counts only
	// become known from the last events, so nothing earlier can carry them.
	finish := func() bool {
		if done {
			return true
		}
		done = true
		meta := &schema.ResponseMeta{FinishReason: stopReason, Usage: toAnthropicUsage(inputTokens, outputTokens)}
		if meta.FinishReason == "" && meta.Usage == nil {
			return true
		}
		return send(&schema.Message{Role: schema.Assistant, ResponseMeta: meta})
	}

	scanner := bufio.NewScanner(body)
	// A single SSE line can carry a whole tool input or a long text delta, which
	// overruns the scanner's 64 KiB default and would end the stream early.
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// The event name on an `event:` line is ignored: every payload repeats it
		// in a `type` field, and reading one source of truth cannot desynchronise
		// on a gateway that names the two differently.
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		// Anthropic ends a stream with message_stop and never sends the
		// OpenAI-style terminator; tolerating it costs one comparison and keeps a
		// gateway that adds it from failing the turn.
		if data == "[DONE]" {
			break
		}
		var ev anthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			fail(m.wrapErr(fmt.Errorf("anthropic: decode stream event%s: %w", atModel(model), err), 0, truncateBody([]byte(data))))
			return
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				model = ev.Message.Model
				if ev.Message.Usage != nil {
					inputTokens = ev.Message.Usage.InputTokens
				}
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == anthropicBlockToolUse {
				open[ev.Index] = &anthropicPendingTool{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text == "" {
					continue
				}
				if !send(&schema.Message{Role: schema.Assistant, Content: ev.Delta.Text}) {
					return
				}
			case "thinking_delta":
				if ev.Delta.Thinking == "" {
					continue
				}
				if !send(&schema.Message{Role: schema.Assistant, ReasoningContent: ev.Delta.Thinking}) {
					return
				}
			case "input_json_delta":
				// Fragments are not JSON on their own — `{"tz":` is a prefix —
				// so they are accumulated and only handed over once the block
				// closes, as one complete argument string.
				if p := open[ev.Index]; p != nil {
					p.arguments.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			p, ok := open[ev.Index]
			if !ok {
				continue
			}
			delete(open, ev.Index)
			// The completed call is emitted here, exactly once, rather than
			// repeated in the closing chunk: the ReAct loop concatenates every
			// chunk it reads, and eino's ConcatMessages keeps calls that carry no
			// index as separate entries, so the same call in two chunks would
			// arrive as two calls and be executed twice.
			if !send(&schema.Message{
				Role:      schema.Assistant,
				ToolCalls: []schema.ToolCall{p.toolCall()},
			}) {
				return
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
				outputTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			finish()
			return
		case "error":
			fail(m.wrapErr(fmt.Errorf("anthropic: stream error%s: %s", atModel(model), ev.errorMessage()), 0, ""))
			return
		}
	}
	if err := scanner.Err(); err != nil {
		fail(m.wrapErr(fmt.Errorf("anthropic: read stream%s: %w", atModel(model), err), 0, ""))
		return
	}
	// A stream that ends without message_stop still reports what it saw: token
	// usage that never reaches the reader bills the turn as free.
	finish()
}

// buildRequest assembles the Messages API body for one call.
func (m *anthropicModel) buildRequest(msgs []*schema.Message, stream bool, opts []model.Option) *anthropicRequest {
	system, messages := toAnthropicMessages(msgs)
	req := &anthropicRequest{
		Model:     m.provider.Model,
		MaxTokens: m.maxTokens(),
		System:    system,
		Messages:  messages,
		Stream:    stream,
	}
	if len(m.tools) > 0 {
		req.Tools = toAnthropicTools(m.tools)
	}
	o := model.GetCommonOptions(nil, opts...)
	if o.Temperature != nil {
		t := *o.Temperature
		req.Temperature = &t
	}
	// A caller that asks for a specific cap wins over the provider default, for
	// the same reason it does on the OpenAI path: the option describes this one
	// call, the provider describes the deployment.
	if o.MaxTokens != nil && *o.MaxTokens > 0 {
		req.MaxTokens = *o.MaxTokens
	}
	return req
}

// maxTokens is max_tokens for a request. The field is required by the API, so a
// provider that names no cap falls back to DefaultAnthropicMaxTokens rather than
// sending zero, which the API rejects outright.
func (m *anthropicModel) maxTokens() int {
	if m.provider.MaxOutputTokens > 0 {
		return m.provider.MaxOutputTokens
	}
	return DefaultAnthropicMaxTokens
}

// version is the anthropic-version header value. Pinning it is what keeps a
// gateway from answering in a shape this adapter does not parse.
func (m *anthropicModel) version() string {
	if v := strings.TrimSpace(m.provider.AnthropicVersion); v != "" {
		return v
	}
	return DefaultAnthropicVersion
}

// post sends one request and hands back the response body, which the caller
// closes.
func (m *anthropicModel) post(ctx context.Context, req *anthropicRequest, stream bool) (io.ReadCloser, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, m.wrapErr(fmt.Errorf("anthropic: encode request: %w", err), 0, "")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicMessagesURL(m.provider.BaseURL), bytes.NewReader(payload))
	if err != nil {
		return nil, m.wrapErr(fmt.Errorf("anthropic: build request: %w", err), 0, "")
	}
	httpReq.Header.Set("content-type", "application/json")
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	httpReq.Header.Set("accept", accept)
	httpReq.Header.Set("anthropic-version", m.version())
	// Which header carries the credential is a property of the endpoint, not of
	// the protocol: the public API takes x-api-key, while Claude Code pointed at
	// an Anthropic-shaped gateway sends the ANTHROPIC_AUTH_TOKEN as a bearer
	// token. Sending neither for a keyless endpoint (ollama and friends) keeps a
	// local server from rejecting an empty credential it never asked for.
	if key := m.provider.APIKey; key != "" {
		if m.provider.AuthStyle == AuthStyleBearer {
			httpReq.Header.Set("authorization", "Bearer "+key)
		} else {
			httpReq.Header.Set("x-api-key", key)
		}
	}

	resp, err := m.httpClient.Do(httpReq)
	if err != nil {
		return nil, m.wrapErr(err, 0, "")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		return nil, m.httpError(resp)
	}
	return resp.Body, nil
}

// anthropicErrorBodyLimit bounds how much of a failed response is kept. An error
// body is read to be shown to a human, so an endpoint that answers with
// something enormous must not turn into a memory problem or a log line nobody
// can read.
const anthropicErrorBodyLimit = 8 << 10

// httpError turns a failed response into an *LLMError, which is the same error
// type the OpenAI adapter returns: the retry predicate reads the status off it
// (see IsRetryable), and a second error type would make that a special case.
func (m *anthropicModel) httpError(resp *http.Response) error {
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, anthropicErrorBodyLimit))
	body := truncateBody(raw)
	var envelope anthropicErrorEnvelope
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error != nil && envelope.Error.Message != "" {
		// The envelope's message is the sentence worth showing; the JSON around
		// it is noise in a log line and in the UI.
		body = envelope.Error.Message
	}
	err := fmt.Errorf("anthropic: unexpected status %d", resp.StatusCode)
	if readErr != nil {
		err = fmt.Errorf("anthropic: status %d: read error body: %w", resp.StatusCode, readErr)
	}
	return m.wrapErr(err, resp.StatusCode, body)
}

// wrapErr mirrors openAIModel.wrapErr so every failure this adapter returns
// carries the provider and model that produced it.
func (m *anthropicModel) wrapErr(err error, statusCode int, body string) error {
	return &LLMError{
		Provider:   m.provider.Name,
		Model:      m.provider.Model,
		StatusCode: statusCode,
		Body:       body,
		Err:        err,
	}
}

// anthropicMessagesURL is the endpoint for one call. The protocol path is fixed
// (/v1/messages), but a configured BaseURL may already carry the version
// segment — https://api.deepseek.com/anthropic does not, and pasting
// ".../anthropic/v1" into a config box is a natural thing to do — so the version
// is appended only when it is missing.
func anthropicMessagesURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/messages"
	}
	return base + "/v1/messages"
}

// truncateBody renders a bounded amount of a response body for an error report.
func truncateBody(raw []byte) string {
	if len(raw) > anthropicErrorBodyLimit {
		raw = raw[:anthropicErrorBodyLimit]
	}
	return strings.TrimSpace(string(raw))
}

// atModel renders " for model X" for an error message, or nothing when the
// stream never told us which model answered.
func atModel(model string) string {
	if model == "" {
		return ""
	}
	return " for model " + model
}

// The Anthropic wire shapes below cover both directions of the protocol. One
// type per block rather than two: a content block is the same JSON object coming
// and going, and the fields that do not apply to a direction are empty and
// omitted.

// anthropicRequest is the Messages API request body.
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream"`
	// Temperature is a pointer so an unset value is absent rather than 0, which
	// the API treats as a deliberate greedy setting.
	Temperature *float32 `json:"temperature,omitempty"`
}

// anthropicMessage is one turn. Content is always a block array: the API has no
// string shorthand that can carry a tool result or an image.
type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

// Content block types this adapter produces or reads.
const (
	anthropicBlockText       = "text"
	anthropicBlockImage      = "image"
	anthropicBlockToolUse    = "tool_use"
	anthropicBlockToolResult = "tool_result"
	anthropicBlockThinking   = "thinking"
)

// anthropicContentBlock is one content block: request text, an image, a tool the
// assistant wants called, the result of that call, or — coming back — the model's
// text, its thinking, or a tool call it decided on.
type anthropicContentBlock struct {
	Type string `json:"type"`
	// Text carries the text of a text block.
	Text string `json:"text,omitempty"`
	// Thinking carries the reasoning of a thinking block in a response. It is
	// never sent: see assistantBlocks.
	Thinking string `json:"thinking,omitempty"`
	// Source addresses an image.
	Source *anthropicImageSource `json:"source,omitempty"`
	// ID and Name identify the tool of a tool_use block; Input is its arguments
	// as a JSON object.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// ToolUseID and Content belong to a tool_result block: which call it answers
	// and what came back.
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

// anthropicImageSource is how an image block points at its bytes: inline base64
// with a media type, or a URL the endpoint fetches itself.
type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// anthropicTool is one tool definition. InputSchema is the JSON Schema of its
// arguments and is required, so a tool with no parameters still sends an empty
// object schema.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicUsage is the token accounting both a response and the stream carry.
type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// anthropicErrorEnvelope is the shape of an Anthropic error, on the HTTP status
// line and as a stream event.
type anthropicErrorEnvelope struct {
	Error *anthropicErrorDetail `json:"error"`
}

// anthropicErrorDetail is the error object inside the envelope.
type anthropicErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// anthropicResponse is a non-streamed reply.
type anthropicResponse struct {
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      *anthropicUsage         `json:"usage"`
}

// toMessage converts a reply into the eino shape the rest of the repository
// consumes.
func (r *anthropicResponse) toMessage() *schema.Message {
	out := &schema.Message{Role: schema.Assistant}
	var toolCalls []schema.ToolCall
	for _, block := range r.Content {
		switch block.Type {
		case anthropicBlockText:
			out.Content += block.Text
		case anthropicBlockThinking:
			// The model's thinking is carried for display, exactly as the OpenAI
			// path carries reasoning_content.
			out.ReasoningContent += block.Thinking
		case anthropicBlockToolUse:
			args := strings.TrimSpace(string(block.Input))
			if args == "" {
				// eino hands Arguments to the tool as a JSON document; an empty
				// string would make an argument-less tool look malformed.
				args = "{}"
			}
			toolCalls = append(toolCalls, schema.ToolCall{
				ID:       block.ID,
				Type:     "function",
				Function: schema.FunctionCall{Name: block.Name, Arguments: args},
			})
		}
	}
	if len(toolCalls) > 0 {
		out.ToolCalls = toolCalls
	}
	// stop_reason "tool_use" is an ordinary way for a turn to end — the model is
	// asking for a tool, not reporting a failure — so it travels as the finish
	// reason rather than becoming an error. The agent loop reads it to decide
	// whether to run tools.
	out.ResponseMeta = &schema.ResponseMeta{
		FinishReason: r.StopReason,
		Usage:        toAnthropicUsage(r.usageTokens()),
	}
	return out
}

// usageTokens splits the reply's usage into the pair toAnthropicUsage takes.
func (r *anthropicResponse) usageTokens() (int, int) {
	if r.Usage == nil {
		return 0, 0
	}
	return r.Usage.InputTokens, r.Usage.OutputTokens
}

// toAnthropicUsage maps the API's input/output pair onto eino's token usage. The
// API reports no total, so it is the sum — that is what the UI and the cost
// accounting read.
func toAnthropicUsage(input, output int) *schema.TokenUsage {
	if input == 0 && output == 0 {
		return nil
	}
	return &schema.TokenUsage{
		PromptTokens:     input,
		CompletionTokens: output,
		TotalTokens:      input + output,
	}
}

// toAnthropicMessages converts the eino history into the system prompt and the
// alternating turns of the Messages API.
//
// Two conversions are the point of it:
//
//   - system messages are hoisted into the top-level system field, joined with a
//     blank line, because the API rejects a message with role "system";
//   - tool results become content blocks of a user turn, and consecutive ones are
//     merged into a single turn, because the API requires alternating roles and
//     rejects a tool_result that does not immediately follow its tool_use.
//
// A message that converts to no blocks at all is dropped rather than sent as an
// empty content array, which the API rejects. (A history that begins with an
// assistant turn is left as it is: refusing it would mean inventing a user turn
// the model never saw.)
func toAnthropicMessages(msgs []*schema.Message) (string, []anthropicMessage) {
	var (
		system  []string
		out     []anthropicMessage
		openRes bool // the previous turn is a tool-result turn still accepting results
	)
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		switch msg.Role {
		case schema.System:
			if msg.Content != "" {
				system = append(system, msg.Content)
			}
		case schema.Tool:
			block := anthropicContentBlock{
				Type:      anthropicBlockToolResult,
				ToolUseID: msg.ToolCallID,
				Content:   msg.Content,
			}
			if openRes {
				last := &out[len(out)-1]
				last.Content = append(last.Content, block)
				continue
			}
			out = append(out, anthropicMessage{Role: "user", Content: []anthropicContentBlock{block}})
			openRes = true
		case schema.Assistant:
			blocks := assistantBlocks(msg)
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})
			openRes = false
		default:
			blocks := userBlocks(msg)
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropicMessage{Role: "user", Content: blocks})
			openRes = false
		}
	}
	return strings.Join(system, "\n\n"), out
}

// assistantBlocks converts one assistant turn.
//
// ReasoningContent is deliberately dropped. Anthropic takes a thinking block
// back only together with the signature it issued alongside it, and the eino
// message carries the text alone, so replaying it would be rejected — dropping
// it is the correct, safe behaviour. The stored reasoning is for display.
func assistantBlocks(msg *schema.Message) []anthropicContentBlock {
	var out []anthropicContentBlock
	if msg.Content != "" {
		out = append(out, anthropicContentBlock{Type: anthropicBlockText, Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		out = append(out, anthropicContentBlock{
			Type:  anthropicBlockToolUse,
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: toolInput(tc.Function.Arguments),
		})
	}
	return out
}

// userBlocks converts one user turn. An attached image travels as content
// blocks — the API has no separate multimodal field — and the block array
// replaces the text exactly as it does on the OpenAI path, where the caller is
// expected to put the text in the first part.
func userBlocks(msg *schema.Message) []anthropicContentBlock {
	if blocks := toAnthropicParts(msg.UserInputMultiContent); len(blocks) > 0 {
		return blocks
	}
	if msg.Content == "" {
		return nil
	}
	return []anthropicContentBlock{{Type: anthropicBlockText, Text: msg.Content}}
}

// toAnthropicParts converts eino's user input parts into content blocks. It
// mirrors toOpenAIMultiContent: a part that cannot be rendered in a shape the API
// accepts (audio, an image with neither bytes nor a URL, inline bytes without a
// media type) is dropped rather than sent, because one malformed block fails the
// whole request — dropping the part is the lesser failure.
func toAnthropicParts(parts []schema.MessageInputPart) []anthropicContentBlock {
	var out []anthropicContentBlock
	for _, p := range parts {
		switch p.Type {
		case schema.ChatMessagePartTypeText:
			if p.Text == "" {
				continue
			}
			out = append(out, anthropicContentBlock{Type: anthropicBlockText, Text: p.Text})
		case schema.ChatMessagePartTypeImageURL:
			block, ok := toAnthropicImage(p.Image)
			if !ok {
				continue
			}
			out = append(out, block)
		}
	}
	return out
}

// toAnthropicImage renders one image part as an image block. imagePartURL does
// the addressing (inline bytes become an RFC-2397 data URL, a remote image stays
// a URL); the work here is telling the two cases apart, because the API wants
// base64 and media type as separate fields and rejects a data URL handed to it as
// a URL.
func toAnthropicImage(img *schema.MessageInputImage) (anthropicContentBlock, bool) {
	url := imagePartURL(img)
	if url == "" {
		return anthropicContentBlock{}, false
	}
	mediaType, data, ok := splitBase64DataURL(url)
	if ok {
		return anthropicContentBlock{
			Type:   anthropicBlockImage,
			Source: &anthropicImageSource{Type: "base64", MediaType: mediaType, Data: data},
		}, true
	}
	if strings.HasPrefix(url, "data:") {
		// A data URL that is not base64-encoded has no image block the API
		// accepts, and sending it as a URL source would be rejected.
		return anthropicContentBlock{}, false
	}
	return anthropicContentBlock{
		Type:   anthropicBlockImage,
		Source: &anthropicImageSource{Type: "url", URL: url},
	}, true
}

// splitBase64DataURL splits "data:<media type>;base64,<data>" into its two parts.
func splitBase64DataURL(url string) (mediaType, data string, ok bool) {
	rest, found := strings.CutPrefix(url, "data:")
	if !found {
		return "", "", false
	}
	meta, payload, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	meta, found = strings.CutSuffix(meta, ";base64")
	if !found || meta == "" || payload == "" {
		return "", "", false
	}
	return meta, payload, true
}

// toolInput renders a tool call's arguments as the JSON object the API requires.
//
// eino stores them as the JSON string the model produced. A value that is not an
// object — an empty string, a truncated fragment, an array — cannot be sent as
// it stands: the API rejects it, and json.Marshal of a malformed RawMessage would
// fail the whole request. Such a call travels with empty arguments instead, which
// the tool layer reports as a tool that was called without arguments.
func toolInput(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return json.RawMessage("{}")
	}
	raw := json.RawMessage(trimmed)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return json.RawMessage("{}")
	}
	return raw
}

// toAnthropicTools converts eino tool definitions into the Messages API shape.
// It mirrors toOpenAITools, including its silence about a missing schema: a tool
// with no parameters still needs input_schema, so it gets an object schema with
// no properties.
func toAnthropicTools(specs []*schema.ToolInfo) []anthropicTool {
	out := make([]anthropicTool, 0, len(specs))
	for _, s := range specs {
		schemaJSON := json.RawMessage(`{"type":"object","properties":{}}`)
		if s.ParamsOneOf != nil {
			if js, err := s.ParamsOneOf.ToJSONSchema(); err == nil && js != nil {
				if b, mErr := json.Marshal(js); mErr == nil {
					schemaJSON = b
				}
			}
		}
		out = append(out, anthropicTool{
			Name:        s.Name,
			Description: s.Desc,
			InputSchema: schemaJSON,
		})
	}
	return out
}

// anthropicStreamEvent is one decoded SSE payload. Anthropic repeats the event
// name in the payload's type field, and only that is read here.
type anthropicStreamEvent struct {
	Type string `json:"type"`
	// Index identifies the content block a delta belongs to; blocks are numbered
	// from zero, so the zero value is a valid index rather than "absent".
	Index int `json:"index"`
	// Message is set on message_start and carries the model and the input token
	// count of the whole call.
	Message *struct {
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`
	// ContentBlock is set on content_block_start, where a tool_use block
	// announces its id and name.
	ContentBlock *anthropicContentBlock `json:"content_block"`
	Delta        *anthropicStreamDelta  `json:"delta"`
	// Usage is set on message_delta with the output token count. The API reports
	// input and output tokens in two different events, so the two are
	// accumulated separately.
	Usage *anthropicUsage       `json:"usage"`
	Error *anthropicErrorDetail `json:"error"`
}

// errorMessage is the sentence to report for an error event.
func (e *anthropicStreamEvent) errorMessage() string {
	if e.Error != nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return "the endpoint reported an error without a message"
}

// anthropicStreamDelta is the delta of one streamed event.
type anthropicStreamDelta struct {
	Type string `json:"type"`
	// Text belongs to text_delta, Thinking to thinking_delta, and PartialJSON to
	// input_json_delta — a fragment of the tool arguments that is only valid JSON
	// once concatenated with its neighbours.
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// anthropicPendingTool is a tool_use block being streamed: the id and name come
// from content_block_start, the arguments from the input_json_delta fragments in
// between.
type anthropicPendingTool struct {
	id        string
	name      string
	arguments strings.Builder
}

// toolCall is the finished call, ready to hand to the ReAct loop.
func (p *anthropicPendingTool) toolCall() schema.ToolCall {
	args := strings.TrimSpace(p.arguments.String())
	if args == "" {
		args = "{}"
	}
	return schema.ToolCall{
		ID:       p.id,
		Type:     "function",
		Function: schema.FunctionCall{Name: p.name, Arguments: args},
	}
}
