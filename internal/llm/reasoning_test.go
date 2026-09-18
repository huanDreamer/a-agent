package llm

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestAliasReasoningField(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "gateway spelling is renamed",
			in:   `data: {"choices":[{"delta":{"reasoning":"We"}}]}`,
			want: `data: {"choices":[{"delta":{"reasoning_content":"We"}}]}`,
		},
		{
			// The gateway sends this array next to the text; it must survive
			// intact, which is why the key is matched with its colon.
			name: "reasoning_details is left alone",
			in:   `data: {"delta":{"reasoning_details":[{"type":"reasoning.text","text":"We"}]}}`,
			want: `data: {"delta":{"reasoning_details":[{"type":"reasoning.text","text":"We"}]}}`,
		},
		{
			name: "deepseek spelling is untouched",
			in:   `data: {"delta":{"reasoning_content":"先分析"}}`,
			want: `data: {"delta":{"reasoning_content":"先分析"}}`,
		},
		{
			// A provider labelling one of them as the real field keeps its
			// choice instead of having the alias overwrite it.
			name: "both spellings keep the canonical one",
			in:   `data: {"delta":{"reasoning_content":"keep","reasoning":"drop"}}`,
			want: `data: {"delta":{"reasoning_content":"keep","reasoning":"drop"}}`,
		},
		{
			name: "whitespace before the colon is tolerated",
			in:   `data: {"delta":{"reasoning" : "We"}}`,
			want: `data: {"delta":{"reasoning_content": "We"}}`,
		},
		{
			name: "a line with no reasoning is untouched",
			in:   `data: {"delta":{"content":"答案"}}`,
			want: `data: {"delta":{"content":"答案"}}`,
		},
		{
			name: "the DONE sentinel is untouched",
			in:   `data: [DONE]`,
			want: `data: [DONE]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(aliasReasoningField([]byte(tc.in))); got != tc.want {
				t.Errorf("aliasReasoningField(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// oneByteReader hands out a single byte per Read, so every line is delivered
// split across many calls. A shim that rewrote per chunk instead of per line
// would corrupt the stream here.
type oneByteReader struct {
	src string
	pos int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.src) {
		return 0, io.EOF
	}
	p[0] = r.src[r.pos]
	r.pos++
	return 1, nil
}

func (r *oneByteReader) Close() error { return nil }

// TestReasoningAliasReaderLineBoundaries checks that a stream arriving one byte
// at a time is reassembled into whole lines and rewritten, and that a final line
// without a trailing newline is still served.
func TestReasoningAliasReaderLineBoundaries(t *testing.T) {
	in := `data: {"delta":{"reasoning":"We"}}` + "\n" +
		`data: {"delta":{"content":"hi"}}` + "\n" +
		`data: [DONE]` // no trailing newline

	src := &oneByteReader{src: in}
	r := &reasoningAliasReader{src: src, br: bufio.NewReader(src)}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	want := `data: {"delta":{"reasoning_content":"We"}}` + "\n" +
		`data: {"delta":{"content":"hi"}}` + "\n" +
		`data: [DONE]`
	if string(got) != want {
		t.Errorf("stream\n got %q\nwant %q", got, want)
	}
}

// TestStream_GatewayReasoningField is the end-to-end guard for the reported
// symptom: a gateway that streams `reasoning` must produce reasoning the chat
// page can show.
//
// The payload is the shape the commandcode endpoint actually sends — reasoning
// text under `reasoning`, mirrored in a `reasoning_details` array, then the
// answer under `content` — because that is what the SDK used to drop.
func TestStream_GatewayReasoningField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		write := func(line string) {
			_, _ = w.Write([]byte("data: " + line + "\n\n"))
			flusher.Flush()
		}
		write(`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"}}]}`)
		write(`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning":"We","reasoning_details":[{"type":"reasoning.text","text":"We","format":"unknown","index":0}]}}]}`)
		write(`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning":" need","reasoning_details":[{"type":"reasoning.text","text":" need","format":"unknown","index":0}]}}]}`)
		write(`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"答案是 42"}}]}`)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	m, err := New(Provider{Name: "commandcode", BaseURL: srv.URL, APIKey: "x", Model: "deepseek/deepseek-v4.1-flash"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := m.Stream(context.Background(), []*schema.Message{{Role: schema.User, Content: "q"}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var reasoning, content []string
	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
		if chunk == nil {
			continue
		}
		if chunk.ReasoningContent != "" {
			reasoning = append(reasoning, chunk.ReasoningContent)
		}
		if chunk.Content != "" {
			content = append(content, chunk.Content)
		}
	}

	if len(reasoning) == 0 {
		t.Fatal("the gateway's `reasoning` field was dropped; the chat page has no thinking process to show")
	}
	if got := strings.Join(reasoning, ""); got != "We need" {
		t.Errorf("reasoning = %q, want %q", got, "We need")
	}
	if got := strings.Join(content, ""); got != "答案是 42" {
		t.Errorf("content = %q, want %q", got, "答案是 42")
	}
}

// TestStream_BufferedResponseIsNotWrapped pins the scope of the shim: a buffered
// JSON response is handed through untouched, so nothing else about the client's
// HTTP behaviour changes.
func TestStream_BufferedResponseIsNotWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := (&reasoningAliasDoer{base: http.DefaultClient}).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if _, ok := resp.Body.(*reasoningAliasReader); ok {
		t.Error("a buffered JSON response was wrapped; only SSE bodies should be rewritten")
	}
}
