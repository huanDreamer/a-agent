package llm

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// Some OpenAI-compatible gateways — OpenRouter and the aggregators built on it,
// such as the commandcode endpoint — name the model's reasoning field
// `reasoning`, optionally alongside a `reasoning_details` array. The spelling
// the OpenAI SDK knows is `reasoning_content`, taken from DeepSeek's API.
//
// go-openai decodes a stream into a struct with only the latter field, so a
// gateway using the former has its reasoning dropped at the wire: no
// EventReasoningDelta is ever emitted, the stored step's reasoning stays empty,
// and the console has nothing to show for the model's thinking even though the
// model did produce it.
//
// The shim below sits on the HTTP client and renames the field inside a
// streaming response before the SDK decodes it. Renaming rather than
// reimplementing the stream is deliberate: the SDK keeps decoding tool-call
// deltas, usage, finish reasons and error frames, and this compatibility
// concern stays confined to one type.
//
// Only SSE responses are rewritten. The streaming chat endpoint is the one that
// carries reasoning deltas, and leaving buffered JSON responses untouched keeps
// this shim from buffering bodies it has no reason to touch.

// reasoningFieldPattern matches the `reasoning` key with the colon that makes it
// a key rather than a prefix. `reasoning_details` is therefore not matched, and
// whitespace before the colon is tolerated.
var reasoningFieldPattern = regexp.MustCompile(`"reasoning"\s*:`)

// reasoningContentKey is the SDK's own spelling, used to detect a response that
// already carries the field and must be left alone.
var reasoningContentKey = []byte(`"reasoning_content":`)

// aliasReasoningField renames a `reasoning` key to `reasoning_content` within
// one SSE data line.
//
// A line that already spells the field the SDK expects is returned untouched, so
// a provider sending both keeps the value it labelled as the real one instead of
// having the alias overwrite it.
func aliasReasoningField(line []byte) []byte {
	if !bytes.Contains(line, reasoningContentKey) && reasoningFieldPattern.Match(line) {
		return reasoningFieldPattern.ReplaceAll(line, reasoningContentKey)
	}
	return line
}

// reasoningAliasDoer wraps an HTTPDoer and aliases the reasoning field name of
// streaming responses on the way through.
type reasoningAliasDoer struct {
	base openai.HTTPDoer
}

// Do performs the request and, for a streaming response, hands back a body whose
// reasoning field carries the name the SDK decodes.
func (d reasoningAliasDoer) Do(req *http.Request) (*http.Response, error) {
	resp, err := d.base.Do(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return resp, nil
	}
	src := resp.Body
	resp.Body = &reasoningAliasReader{src: src, br: bufio.NewReader(src)}
	return resp, nil
}

// reasoningAliasReader rewrites an SSE body line by line as it is read.
//
// It holds at most one line, so a stream is never buffered in full: the SDK
// reads a chunk, gets the rewritten line, and the next line is fetched on
// demand. Line assembly is left to bufio, which is what makes this correct for a
// chunk boundary landing mid-line.
type reasoningAliasReader struct {
	src     io.ReadCloser
	br      *bufio.Reader
	pending []byte
	done    bool
}

// Read serves the rewritten stream. It returns whatever it has, refilling from
// the source only when the pending line is exhausted, so a caller's buffer size
// never has to line up with the stream's framing.
func (r *reasoningAliasReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.done {
			return 0, io.EOF
		}
		line, err := r.br.ReadBytes('\n')
		if len(line) > 0 {
			r.pending = aliasReasoningField(line)
		}
		if err != nil {
			r.done = true
			// A final line without its newline is still data: serve it before
			// reporting the error that ended the stream.
			if len(line) == 0 {
				if errors.Is(err, io.EOF) {
					return 0, io.EOF
				}
				return 0, err
			}
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// Close releases the underlying body.
func (r *reasoningAliasReader) Close() error { return r.src.Close() }
