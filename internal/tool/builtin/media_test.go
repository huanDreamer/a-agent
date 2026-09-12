package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"

	"github.com/huan/huan-agent/internal/media"
	"github.com/huan/huan-agent/internal/workspace"
)

// These tests never touch the network: every provider is an httptest server.
// The point of them is the boundary between the agent and somebody else's
// machine, so what is asserted is what leaves the process (the exact request
// body, the auth header, the bounded error) and what arrives back (a written
// file, a transcript, a refusal).

// ---------------------------------------------------------------- fixtures --

// mediaPNG is a PNG header: enough for the content sniffer, which only reads
// the signature.
var mediaPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02")

var (
	mediaJPEG = []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
	mediaGIF  = []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00")
	mediaWEBP = append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), make([]byte, 8)...)
	mediaWAV  = append([]byte("RIFF\x24\x08\x00\x00WAVEfmt "), make([]byte, 8)...)
	mediaOGG  = append([]byte("OggS\x00\x02"), make([]byte, 24)...)
	mediaWEBM = []byte{0x1a, 0x45, 0xdf, 0xa3, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	mediaMP3  = append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), make([]byte, 8)...)
)

// mediaWorkspace returns a workspace rooted in a fresh temporary directory.
func mediaWorkspace(t *testing.T, opts workspace.Options) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.New(t.TempDir(), opts)
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	return ws
}

// mediaSeed writes a fixture file inside the workspace and returns its path
// relative to the root, which is the form a tool receives.
func mediaSeed(t *testing.T, ws *workspace.Workspace, name string, data []byte) string {
	t.Helper()
	abs := filepath.Join(ws.Root(), filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	return name
}

// mediaSparseFile writes a file of the given size without writing its bytes.
// The size limits are megabytes, and materialising them would only slow a test
// down.
func mediaSparseFile(t *testing.T, ws *workspace.Workspace, name string, size int64) string {
	t.Helper()
	abs := filepath.Join(ws.Root(), filepath.FromSlash(name))
	f, err := os.Create(abs)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatalf("truncate %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	return name
}

// mediaArgs renders arguments the way a model would send them.
func mediaArgs(t *testing.T, in any) string {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return string(b)
}

func mediaRun(t *testing.T, it tool.InvokableTool, args string) (string, error) {
	t.Helper()
	return it.InvokableRun(context.Background(), args)
}

func mediaDecode[T any](t *testing.T, out string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("decode tool output %q: %v", out, err)
	}
	return v
}

// mediaTargetFor points a Target at a test server, with the /v1 suffix a real
// provider's base URL carries.
func mediaTargetFor(srv *httptest.Server, model string) media.Target {
	return media.Target{
		ProviderID: "test-provider",
		ModelID:    model,
		BaseURL:    srv.URL + "/v1",
		APIKey:     "sk-test-key",
		Kind:       media.KindOpenAI,
		Timeout:    5 * time.Second,
	}
}

// ---------------------------------------------------------- request capture --

// recordedRequest is what a test server saw. Recording rather than asserting
// inside the handler keeps every failure on the test goroutine: a t.Errorf from
// a handler that outlives the test is a panic under -race.
type recordedRequest struct {
	Method        string
	Path          string
	ContentType   string
	Authorization string
	Body          []byte

	// Multipart requests are parsed here, because the tool's own body is a
	// stream nobody can inspect afterwards.
	Form         map[string][]string
	FileName     string
	FileData     []byte
	FilePartType string
	ParseError   string
}

type requestRecorder struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (r *requestRecorder) record(req recordedRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
}

func (r *requestRecorder) last(t *testing.T) recordedRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		t.Fatal("the tool made no request")
	}
	return r.reqs[len(r.reqs)-1]
}

func (r *requestRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// mediaServer records every request it receives and answers with status/body.
func mediaServer(t *testing.T, status int, body string) (*httptest.Server, *requestRecorder) {
	t.Helper()
	rec := &requestRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(readRecordedRequest(r))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// readRecordedRequest captures a request, parsing a multipart body when there
// is one.
func readRecordedRequest(r *http.Request) recordedRequest {
	out := recordedRequest{
		Method:        r.Method,
		Path:          r.URL.Path,
		ContentType:   r.Header.Get("Content-Type"),
		Authorization: r.Header.Get("Authorization"),
	}
	if !strings.HasPrefix(out.ContentType, "multipart/form-data") {
		data, _ := io.ReadAll(r.Body)
		out.Body = data
		return out
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		out.ParseError = err.Error()
		return out
	}
	out.Form = r.MultipartForm.Value
	file, header, err := r.FormFile("file")
	if err != nil {
		out.ParseError = err.Error()
		return out
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		out.ParseError = err.Error()
		return out
	}
	out.FileName = header.Filename
	out.FilePartType = header.Header.Get("Content-Type")
	out.FileData = data
	return out
}

// ---------------------------------------------------------- describe_image --

// visionBody is a canned chat completion response.
func visionBody(content, usage string) string {
	if usage == "" {
		return fmt.Sprintf(`{"choices":[{"message":{"content":%s}}]}`, content)
	}
	return fmt.Sprintf(`{"choices":[{"message":{"content":%s}}],"usage":%s}`, content, usage)
}

// visionRequestShape is the request the tool is expected to send, decoded.
type visionRequestShape struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
	} `json:"messages"`
}

func TestDescribeImageTool_SendsAMultimodalMessage(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK,
		visionBody(`"a red square"`, `{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}`))
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "shot.png", mediaPNG)

	it, err := NewDescribeImageTool(ws, mediaTargetFor(srv, "gpt-4o-mini"))
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	out, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path, Prompt: "what colour?"}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}

	got := rec.last(t)
	if got.Method != http.MethodPost || got.Path != "/v1/chat/completions" {
		t.Errorf("request = %s %s, want POST /v1/chat/completions", got.Method, got.Path)
	}
	if got.Authorization != "Bearer sk-test-key" {
		t.Errorf("authorization = %q", got.Authorization)
	}
	if got.ContentType != "application/json" {
		t.Errorf("content-type = %q", got.ContentType)
	}

	var req visionRequestShape
	if err := json.Unmarshal(got.Body, &req); err != nil {
		t.Fatalf("request is not JSON: %v (%s)", err, got.Body)
	}
	if req.Model != "gpt-4o-mini" {
		t.Errorf("model = %q", req.Model)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v, want one user message", req.Messages)
	}
	parts := req.Messages[0].Content
	if len(parts) != 2 {
		t.Fatalf("content parts = %d, want a text part and an image part", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "what colour?" {
		t.Errorf("text part = %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Fatalf("image part = %+v", parts[1])
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(parts[1].ImageURL.URL, prefix) {
		t.Fatalf("image url = %.40q, want a %s data url", parts[1].ImageURL.URL, prefix)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(parts[1].ImageURL.URL, prefix))
	if err != nil {
		t.Fatalf("image url is not valid base64: %v", err)
	}
	if string(decoded) != string(mediaPNG) {
		t.Errorf("image bytes did not survive the round trip")
	}

	var res DescribeImageOutput
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode output %q: %v", out, err)
	}
	if res.Path != "shot.png" || res.Text != "a red square" || res.Model != "gpt-4o-mini" {
		t.Errorf("output = %+v", res)
	}
	if res.Usage == nil || res.Usage.TotalTokens != 15 || res.Usage.PromptTokens != 11 || res.Usage.CompletionTokens != 4 {
		t.Errorf("usage = %+v, want the reported token counts", res.Usage)
	}
}

func TestDescribeImageTool_DefaultPrompt(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK, visionBody(`"a cat"`, ""))
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "cat.png", mediaPNG)

	it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "vision-model"))
	if _, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path})); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var req visionRequestShape
	if err := json.Unmarshal(rec.last(t).Body, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if got := req.Messages[0].Content[0].Text; got != defaultDescribePrompt {
		t.Errorf("prompt = %q, want the default %q", got, defaultDescribePrompt)
	}
}

func TestDescribeImageTool_UsageOmittedWhenUnreported(t *testing.T) {
	srv, _ := mediaServer(t, http.StatusOK, visionBody(`"a cat"`, ""))
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "cat.png", mediaPNG)

	it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "vision-model"))
	out, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if strings.Contains(out, "usage") {
		t.Errorf("output %s should omit usage entirely when the provider reports none", out)
	}
}

func TestDescribeImageTool_AcceptsTheSupportedImageTypes(t *testing.T) {
	cases := map[string][]byte{
		"png":  mediaPNG,
		"jpg":  mediaJPEG,
		"gif":  mediaGIF,
		"webp": mediaWEBP,
	}
	for ext, data := range cases {
		t.Run(ext, func(t *testing.T) {
			srv, rec := mediaServer(t, http.StatusOK, visionBody(`"ok"`, ""))
			ws := mediaWorkspace(t, workspace.Options{})
			path := mediaSeed(t, ws, "image."+ext, data)

			it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "vision-model"))
			if _, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path})); err != nil {
				t.Fatalf("invoke: %v", err)
			}
			if rec.count() != 1 {
				t.Errorf("sent %d requests, want 1", rec.count())
			}
		})
	}
}

func TestDescribeImageTool_RefusesNonImageByName(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		wantIn string
	}{
		{"notes.txt", []byte("just some text, no image here"), "text/plain"},
		{"report.pdf", []byte("%PDF-1.4\n%fake"), "application/pdf"},
		{"voice.wav", mediaWAV, "audio/wave"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := mediaServer(t, http.StatusOK, visionBody(`"ok"`, ""))
			ws := mediaWorkspace(t, workspace.Options{})
			path := mediaSeed(t, ws, tc.name, tc.data)

			it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "vision-model"))
			_, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path}))
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q should name what was found (%q)", err, tc.wantIn)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("error %q should name the file", err)
			}
			if rec.count() != 0 {
				t.Errorf("a refused image must not be uploaded")
			}
		})
	}
}

func TestDescribeImageTool_RefusesMissingFile(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK, visionBody(`"ok"`, ""))
	ws := mediaWorkspace(t, workspace.Options{})

	it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "vision-model"))
	_, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: "nope.png"}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error %q should say the file does not exist", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error %q should wrap fs.ErrNotExist", err)
	}
	if rec.count() != 0 {
		t.Error("a missing file must not be uploaded")
	}
}

func TestDescribeImageTool_RefusesPathOutsideTheWorkspace(t *testing.T) {
	srv, _ := mediaServer(t, http.StatusOK, visionBody(`"ok"`, ""))
	ws := mediaWorkspace(t, workspace.Options{})

	it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "vision-model"))
	for _, p := range []string{"../escape.png", "/etc/hosts"} {
		_, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: p}))
		if !errors.Is(err, workspace.ErrOutsideWorkspace) {
			t.Errorf("path %q: error = %v, want ErrOutsideWorkspace", p, err)
		}
	}
}

func TestDescribeImageTool_RefusesDirectory(t *testing.T) {
	srv, _ := mediaServer(t, http.StatusOK, visionBody(`"ok"`, ""))
	ws := mediaWorkspace(t, workspace.Options{})
	if err := os.Mkdir(filepath.Join(ws.Root(), "pics"), 0o755); err != nil {
		t.Fatal(err)
	}

	it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "vision-model"))
	_, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: "pics"}))
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("error = %v, want a directory refusal", err)
	}
}

func TestDescribeImageTool_RefusesOversizedImage(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK, visionBody(`"ok"`, ""))
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSparseFile(t, ws, "huge.png", DefaultMaxImageBytes+1)

	it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "vision-model"))
	_, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path}))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, fmt.Sprint(DefaultMaxImageBytes)) || !strings.Contains(msg, fmt.Sprint(DefaultMaxImageBytes+1)) {
		t.Errorf("error %q should name both the limit and the actual size", msg)
	}
	if rec.count() != 0 {
		t.Error("an oversized image must not be uploaded")
	}
}

func TestDescribeImageTool_SurfacesProviderError(t *testing.T) {
	srv, _ := mediaServer(t, http.StatusNotFound, `{"error":{"message":"model not found"}}`)
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "shot.png", mediaPNG)

	it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "missing-model"))
	_, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "model not found") {
		t.Errorf("error %q should carry the status and the provider's reason", err)
	}
}

func TestDescribeImageTool_BoundsAndRedactsTheErrorBody(t *testing.T) {
	// A provider that echoes the credential back, and answers with far more
	// text than an error needs.
	long := strings.Repeat("x", 4000)
	srv, _ := mediaServer(t, http.StatusUnauthorized,
		`{"error":{"message":"Incorrect API key provided: sk-test-key. `+long+`"}}`)
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "shot.png", mediaPNG)

	it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "vision-model"))
	_, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path}))
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "sk-test-key") {
		t.Fatalf("the API key leaked into the error: %q", msg)
	}
	if !strings.Contains(msg, "[redacted]") {
		t.Errorf("error %q should show that a secret was removed", msg)
	}
	if len(msg) > maxErrorBodyBytes+200 {
		t.Errorf("error is %d bytes, want the body bounded to about %d", len(msg), maxErrorBodyBytes)
	}
}

func TestDescribeImageTool_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, visionBody(`"late"`, ""))
	}))
	t.Cleanup(srv.Close)

	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "shot.png", mediaPNG)

	target := mediaTargetFor(srv, "vision-model")
	target.Timeout = 20 * time.Millisecond
	it, _ := NewDescribeImageTool(ws, target)

	started := time.Now()
	_, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path}))
	if err == nil {
		t.Fatal("expected the call to time out")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want a deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("call took %v; the timeout did not bound it", elapsed)
	}
}

func TestDescribeImageTool_AcceptsContentAsParts(t *testing.T) {
	// Some providers answer with an array of parts instead of a string.
	srv, _ := mediaServer(t, http.StatusOK,
		`{"choices":[{"message":{"content":[{"type":"text","text":"two "},{"type":"text","text":"parts"}]}}]}`)
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "shot.png", mediaPNG)

	it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "vision-model"))
	out, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if got := mediaDecode[DescribeImageOutput](t, out).Text; got != "two parts" {
		t.Errorf("text = %q, want the parts joined", got)
	}
}

func TestDescribeImageTool_EmptyOrMalformedAnswers(t *testing.T) {
	cases := map[string]string{
		"no choices":          `{"choices":[]}`,
		"empty content":       `{"choices":[{"message":{"content":""}}]}`,
		"missing content":     `{"choices":[{"message":{}}]}`,
		"not json at all":     `this is not json`,
		"content is a number": `{"choices":[{"message":{"content":42}}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := mediaServer(t, http.StatusOK, body)
			ws := mediaWorkspace(t, workspace.Options{})
			path := mediaSeed(t, ws, "shot.png", mediaPNG)

			it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "vision-model"))
			if _, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path})); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestDescribeImageTool_EmptyAnswerNamesTheModel(t *testing.T) {
	srv, _ := mediaServer(t, http.StatusOK, `{"choices":[{"message":{"content":"   "}}]}`)
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "shot.png", mediaPNG)

	it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "text-only-model"))
	_, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "text-only-model") {
		t.Errorf("error %q should name the model, which is the likeliest cause", err)
	}
}

// ---------------------------------------------------------- generate_image --

func TestGenerateImageTool_WritesTheImageIntoTheWorkspace(t *testing.T) {
	image := append(append([]byte(nil), mediaPNG...), []byte("more pixels")...)
	resp := fmt.Sprintf(`{"data":[{"b64_json":%q,"revised_prompt":"a red square, studio light"}]}`,
		base64.StdEncoding.EncodeToString(image))
	srv, rec := mediaServer(t, http.StatusOK, resp)
	ws := mediaWorkspace(t, workspace.Options{})
	// An existing file at the destination proves the write replaces it.
	mediaSeed(t, ws, "out/pic.png", []byte("stale bytes"))

	it, err := NewGenerateImageTool(ws, mediaTargetFor(srv, "dall-e-3"))
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	out, err := mediaRun(t, it, mediaArgs(t, GenerateImageInput{
		Prompt: "a red square",
		Path:   "out/pic.png",
		Size:   "1024x1024",
	}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}

	got := rec.last(t)
	if got.Method != http.MethodPost || got.Path != "/v1/images/generations" {
		t.Errorf("request = %s %s", got.Method, got.Path)
	}
	if got.Authorization != "Bearer sk-test-key" {
		t.Errorf("authorization = %q", got.Authorization)
	}
	var req struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		Size           string `json:"size"`
		N              int    `json:"n"`
		ResponseFormat string `json:"response_format"`
	}
	if err := json.Unmarshal(got.Body, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if req.Model != "dall-e-3" || req.Prompt != "a red square" || req.Size != "1024x1024" || req.N != 1 || req.ResponseFormat != "b64_json" {
		t.Errorf("request body = %+v", req)
	}

	res := mediaDecode[GenerateImageOutput](t, out)
	if res.Path != "out/pic.png" || res.Bytes != int64(len(image)) || res.Model != "dall-e-3" {
		t.Errorf("output = %+v", res)
	}
	if res.RevisedPrompt != "a red square, studio light" {
		t.Errorf("revised prompt = %q", res.RevisedPrompt)
	}
	written, err := os.ReadFile(filepath.Join(ws.Root(), "out", "pic.png"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(written) != string(image) {
		t.Errorf("written file does not match the decoded image")
	}
}

func TestGenerateImageTool_CreatesParentDirectories(t *testing.T) {
	srv, _ := mediaServer(t, http.StatusOK,
		fmt.Sprintf(`{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(mediaPNG)))
	ws := mediaWorkspace(t, workspace.Options{})

	it, _ := NewGenerateImageTool(ws, mediaTargetFor(srv, "image-model"))
	if _, err := mediaRun(t, it, mediaArgs(t, GenerateImageInput{Prompt: "x", Path: "a/b/c/deep.png"})); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), "a", "b", "c", "deep.png")); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestGenerateImageTool_SizeOmittedWhenNotRequested(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK,
		fmt.Sprintf(`{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(mediaPNG)))
	ws := mediaWorkspace(t, workspace.Options{})

	it, _ := NewGenerateImageTool(ws, mediaTargetFor(srv, "image-model"))
	if _, err := mediaRun(t, it, mediaArgs(t, GenerateImageInput{Prompt: "x", Path: "pic.png"})); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if strings.Contains(string(rec.last(t).Body), `"size"`) {
		t.Errorf("request %s should omit size so the provider picks its default", rec.last(t).Body)
	}
}

func TestGenerateImageTool_ModelOverride(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK,
		fmt.Sprintf(`{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(mediaPNG)))
	ws := mediaWorkspace(t, workspace.Options{})

	it, _ := NewGenerateImageTool(ws, mediaTargetFor(srv, "configured-model"))
	out, err := mediaRun(t, it, mediaArgs(t, GenerateImageInput{Prompt: "x", Path: "pic.png", Model: "flux-pro"}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.last(t).Body, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if req.Model != "flux-pro" {
		t.Errorf("request model = %q, want the override", req.Model)
	}
	// The output names the model that actually produced the image.
	if got := mediaDecode[GenerateImageOutput](t, out).Model; got != "flux-pro" {
		t.Errorf("output model = %q", got)
	}
}

func TestGenerateImageTool_RefusesReadOnlyWorkspace(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK,
		fmt.Sprintf(`{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(mediaPNG)))
	ws := mediaWorkspace(t, workspace.Options{ReadOnly: true})

	it, _ := NewGenerateImageTool(ws, mediaTargetFor(srv, "image-model"))
	_, err := mediaRun(t, it, mediaArgs(t, GenerateImageInput{Prompt: "x", Path: "pic.png"}))
	if !errors.Is(err, workspace.ErrReadOnly) {
		t.Fatalf("error = %v, want ErrReadOnly", err)
	}
	if _, serr := os.Stat(filepath.Join(ws.Root(), "pic.png")); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("a refused write must not create the file (stat error = %v)", serr)
	}
	// The refusal must come before the provider call: generating an image that
	// cannot be stored is paying for nothing.
	if rec.count() != 0 {
		t.Errorf("the provider was called %d times for an image that cannot be saved", rec.count())
	}
}

func TestGenerateImageTool_RefusesAnImageOverTheWriteLimit(t *testing.T) {
	image := make([]byte, 64)
	srv, _ := mediaServer(t, http.StatusOK,
		fmt.Sprintf(`{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(image)))
	ws := mediaWorkspace(t, workspace.Options{Limits: workspace.Limits{MaxWriteBytes: 32}})

	it, _ := NewGenerateImageTool(ws, mediaTargetFor(srv, "image-model"))
	_, err := mediaRun(t, it, mediaArgs(t, GenerateImageInput{Prompt: "x", Path: "pic.png"}))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "64") || !strings.Contains(msg, "32") {
		t.Errorf("error %q should name the image size and the limit", msg)
	}
}

func TestGenerateImageTool_NeverWritesOutsideTheWorkspace(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK,
		fmt.Sprintf(`{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(mediaPNG)))
	ws := mediaWorkspace(t, workspace.Options{})

	it, _ := NewGenerateImageTool(ws, mediaTargetFor(srv, "image-model"))
	_, err := mediaRun(t, it, mediaArgs(t, GenerateImageInput{Prompt: "x", Path: "../escaped.png"}))
	if !errors.Is(err, workspace.ErrOutsideWorkspace) {
		t.Fatalf("error = %v, want ErrOutsideWorkspace", err)
	}
	outside := filepath.Join(filepath.Dir(ws.Root()), "escaped.png")
	if _, serr := os.Stat(outside); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("a file appeared outside the workspace at %s", outside)
	}
	if rec.count() != 0 {
		t.Error("a path that cannot be written must be refused before the provider is called")
	}
}

func TestGenerateImageTool_RequiresPromptAndPath(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK, `{"data":[]}`)
	ws := mediaWorkspace(t, workspace.Options{})
	it, _ := NewGenerateImageTool(ws, mediaTargetFor(srv, "image-model"))

	if _, err := mediaRun(t, it, mediaArgs(t, GenerateImageInput{Path: "pic.png"})); err == nil {
		t.Error("an empty prompt must be refused")
	}
	if _, err := mediaRun(t, it, mediaArgs(t, GenerateImageInput{Prompt: "x"})); err == nil {
		t.Error("an empty path must be refused")
	}
	if rec.count() != 0 {
		t.Error("a request must not be sent when the arguments are unusable")
	}
}

func TestGenerateImageTool_SurfacesProviderError(t *testing.T) {
	srv, _ := mediaServer(t, http.StatusBadRequest, `{"error":{"message":"prompt rejected by the safety system"}}`)
	ws := mediaWorkspace(t, workspace.Options{})

	it, _ := NewGenerateImageTool(ws, mediaTargetFor(srv, "image-model"))
	_, err := mediaRun(t, it, mediaArgs(t, GenerateImageInput{Prompt: "x", Path: "pic.png"}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "safety system") {
		t.Errorf("error = %v, want the status and the provider's reason", err)
	}
}

func TestGenerateImageTool_DegenerateResponses(t *testing.T) {
	cases := map[string]struct {
		body   string
		wantIn string
	}{
		"no data":            {`{"data":[]}`, "no image"},
		"empty base64":       {`{"data":[{"b64_json":""}]}`, "no image data"},
		"invalid base64":     {`{"data":[{"b64_json":"!!!not base64!!!"}]}`, "base64"},
		"url instead of b64": {`{"data":[{"url":"https://example.com/pic.png"}]}`, "returned a URL"},
		"not json":           {`<html>gateway error</html>`, "decode response"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := mediaServer(t, http.StatusOK, tc.body)
			ws := mediaWorkspace(t, workspace.Options{})
			it, _ := NewGenerateImageTool(ws, mediaTargetFor(srv, "image-model"))

			_, err := mediaRun(t, it, mediaArgs(t, GenerateImageInput{Prompt: "x", Path: "pic.png"}))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantIn)
			}
		})
	}
}

// -------------------------------------------------------- transcribe_audio --

func TestTranscribeAudioTool_SendsAMultipartForm(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK, `{"text":"hello world","language":"english"}`)
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "voice.wav", mediaWAV)

	it, err := NewTranscribeAudioTool(ws, mediaTargetFor(srv, "whisper-1"))
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	out, err := mediaRun(t, it, mediaArgs(t, TranscribeAudioInput{
		Path:     path,
		Language: "en",
		Prompt:   "huan-agent",
	}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}

	got := rec.last(t)
	if got.Method != http.MethodPost || got.Path != "/v1/audio/transcriptions" {
		t.Errorf("request = %s %s", got.Method, got.Path)
	}
	if !strings.HasPrefix(got.ContentType, "multipart/form-data; boundary=") {
		t.Errorf("content-type = %q, want multipart/form-data", got.ContentType)
	}
	if got.Authorization != "Bearer sk-test-key" {
		t.Errorf("authorization = %q", got.Authorization)
	}
	if got.ParseError != "" {
		t.Fatalf("server could not parse the multipart body: %s", got.ParseError)
	}
	for field, want := range map[string]string{
		"model":           "whisper-1",
		"response_format": "json",
		"language":        "en",
		"prompt":          "huan-agent",
	} {
		if v := got.Form[field]; len(v) != 1 || v[0] != want {
			t.Errorf("form field %s = %v, want %q", field, v, want)
		}
	}
	if got.FileName != "voice.wav" {
		t.Errorf("filename = %q", got.FileName)
	}
	if got.FilePartType != "audio/wav" {
		t.Errorf("file part content-type = %q, want audio/wav", got.FilePartType)
	}
	if string(got.FileData) != string(mediaWAV) {
		t.Errorf("the uploaded bytes do not match the file on disk")
	}

	res := mediaDecode[TranscribeAudioOutput](t, out)
	if res.Path != "voice.wav" || res.Text != "hello world" || res.Language != "english" || res.Model != "whisper-1" {
		t.Errorf("output = %+v", res)
	}
}

func TestTranscribeAudioTool_OmitsEmptyOptionalFields(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK, `{"text":"hi"}`)
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "voice.wav", mediaWAV)

	it, _ := NewTranscribeAudioTool(ws, mediaTargetFor(srv, "whisper-1"))
	out, err := mediaRun(t, it, mediaArgs(t, TranscribeAudioInput{Path: path}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	got := rec.last(t)
	for _, field := range []string{"language", "prompt"} {
		if _, present := got.Form[field]; present {
			t.Errorf("form field %s should be omitted when the caller gave none", field)
		}
	}
	if strings.Contains(out, "language") {
		t.Errorf("output %s should omit language when the provider reported none", out)
	}
}

func TestTranscribeAudioTool_AcceptsCommonAudioTypes(t *testing.T) {
	cases := map[string][]byte{
		"voice.wav":  mediaWAV,
		"voice.mp3":  mediaMP3, // ID3 header, sniffed
		"voice.ogg":  mediaOGG,
		"voice.webm": mediaWEBM,
		// The sniffer cannot tell these apart from any other binary, so the
		// extension is what accepts them.
		"voice.m4a":  append([]byte("\x00\x00\x00\x20ftypM4A "), make([]byte, 16)...),
		"voice.flac": append([]byte("fLaC\x00\x00\x00\x22"), make([]byte, 16)...),
		"raw.mp3":    {0xff, 0xfb, 0x90, 0x00, 0x00, 0x00, 0x00, 0x00},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			srv, rec := mediaServer(t, http.StatusOK, `{"text":"hi"}`)
			ws := mediaWorkspace(t, workspace.Options{})
			path := mediaSeed(t, ws, name, data)

			it, _ := NewTranscribeAudioTool(ws, mediaTargetFor(srv, "whisper-1"))
			if _, err := mediaRun(t, it, mediaArgs(t, TranscribeAudioInput{Path: path})); err != nil {
				t.Fatalf("invoke: %v", err)
			}
			if rec.count() != 1 {
				t.Errorf("sent %d requests, want 1", rec.count())
			}
		})
	}
}

func TestTranscribeAudioTool_RefusesOtherFileTypesByName(t *testing.T) {
	cases := []struct {
		name   string
		file   string
		wantIn string
	}{
		{"plain text", "notes.txt", "text/plain"},
		// A file renamed to an audio extension must not be uploaded: the
		// contents are what decide, and the refusal says so.
		{"text named mp3", "song.mp3", "text/plain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := mediaServer(t, http.StatusOK, `{"text":"hi"}`)
			ws := mediaWorkspace(t, workspace.Options{})
			path := mediaSeed(t, ws, tc.file, []byte("this is not audio at all"))

			it, _ := NewTranscribeAudioTool(ws, mediaTargetFor(srv, "whisper-1"))
			_, err := mediaRun(t, it, mediaArgs(t, TranscribeAudioInput{Path: path}))
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantIn)
			}
			if rec.count() != 0 {
				t.Error("a refused file must not be uploaded")
			}
		})
	}
}

func TestTranscribeAudioTool_EmptyFileIsRefused(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK, `{"text":"hi"}`)
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "empty.mp3", nil)

	it, _ := NewTranscribeAudioTool(ws, mediaTargetFor(srv, "whisper-1"))
	_, err := mediaRun(t, it, mediaArgs(t, TranscribeAudioInput{Path: path}))
	if err == nil || !strings.Contains(err.Error(), "no readable bytes") {
		t.Errorf("error = %v, want a refusal naming the empty file", err)
	}
	if rec.count() != 0 {
		t.Error("an empty file must not be uploaded")
	}
}

func TestTranscribeAudioTool_RefusesOversizedAudio(t *testing.T) {
	srv, rec := mediaServer(t, http.StatusOK, `{"text":"hi"}`)
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSparseFile(t, ws, "long.wav", DefaultMaxAudioBytes+1)

	it, _ := NewTranscribeAudioTool(ws, mediaTargetFor(srv, "whisper-1"))
	_, err := mediaRun(t, it, mediaArgs(t, TranscribeAudioInput{Path: path}))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(DefaultMaxAudioBytes)) {
		t.Errorf("error = %v, want it to name the limit", err)
	}
	if rec.count() != 0 {
		t.Error("an oversized recording must not be uploaded")
	}
}

func TestTranscribeAudioTool_RefusesMissingFileAndEscape(t *testing.T) {
	srv, _ := mediaServer(t, http.StatusOK, `{"text":"hi"}`)
	ws := mediaWorkspace(t, workspace.Options{})
	it, _ := NewTranscribeAudioTool(ws, mediaTargetFor(srv, "whisper-1"))

	if _, err := mediaRun(t, it, mediaArgs(t, TranscribeAudioInput{Path: "gone.wav"})); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error = %v, want fs.ErrNotExist", err)
	}
	if _, err := mediaRun(t, it, mediaArgs(t, TranscribeAudioInput{Path: "../secret.wav"})); !errors.Is(err, workspace.ErrOutsideWorkspace) {
		t.Errorf("error = %v, want ErrOutsideWorkspace", err)
	}
}

func TestTranscribeAudioTool_SurfacesProviderErrorAndEmptyTranscript(t *testing.T) {
	t.Run("provider error", func(t *testing.T) {
		srv, _ := mediaServer(t, http.StatusForbidden, `{"error":{"message":"no access to that model"}}`)
		ws := mediaWorkspace(t, workspace.Options{})
		path := mediaSeed(t, ws, "voice.wav", mediaWAV)

		it, _ := NewTranscribeAudioTool(ws, mediaTargetFor(srv, "whisper-1"))
		_, err := mediaRun(t, it, mediaArgs(t, TranscribeAudioInput{Path: path}))
		if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "no access") {
			t.Errorf("error = %v, want the status and the reason", err)
		}
	})
	t.Run("empty transcript", func(t *testing.T) {
		srv, _ := mediaServer(t, http.StatusOK, `{"text":""}`)
		ws := mediaWorkspace(t, workspace.Options{})
		path := mediaSeed(t, ws, "silence.wav", mediaWAV)

		it, _ := NewTranscribeAudioTool(ws, mediaTargetFor(srv, "whisper-1"))
		_, err := mediaRun(t, it, mediaArgs(t, TranscribeAudioInput{Path: path}))
		if err == nil || !strings.Contains(err.Error(), "empty transcript") {
			t.Errorf("error = %v, want an empty-transcript error", err)
		}
	})
}

func TestTranscribeAudioTool_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"text":"late"}`)
	}))
	t.Cleanup(srv.Close)

	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "voice.wav", mediaWAV)

	target := mediaTargetFor(srv, "whisper-1")
	target.Timeout = 20 * time.Millisecond
	it, _ := NewTranscribeAudioTool(ws, target)

	_, err := mediaRun(t, it, mediaArgs(t, TranscribeAudioInput{Path: path}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want a deadline exceeded", err)
	}
}

// --------------------------------------------------- constructors and seams --

func TestMediaToolConstructors_ValidateTheirTarget(t *testing.T) {
	ws := mediaWorkspace(t, workspace.Options{})
	good := media.Target{ProviderID: "p", ModelID: "m", BaseURL: "https://example.com/v1", Kind: media.KindOpenAI}

	ctors := map[string]func(*workspace.Workspace, media.Target) (tool.InvokableTool, error){
		"describe_image":   NewDescribeImageTool,
		"generate_image":   NewGenerateImageTool,
		"transcribe_audio": NewTranscribeAudioTool,
	}
	bad := map[string]media.Target{
		"no model id":      {ProviderID: "p", BaseURL: good.BaseURL, Kind: media.KindOpenAI},
		"no base url":      {ProviderID: "p", ModelID: "m", Kind: media.KindOpenAI},
		"relative base":    {ProviderID: "p", ModelID: "m", BaseURL: "example.com/v1"},
		"ftp base":         {ProviderID: "p", ModelID: "m", BaseURL: "ftp://example.com"},
		"unsupported kind": {ProviderID: "p", ModelID: "m", BaseURL: good.BaseURL, Kind: "anthropic"},
	}
	for name, ctor := range ctors {
		for why, target := range bad {
			if _, err := ctor(ws, target); err == nil {
				t.Errorf("%s with %s should fail at construction", name, why)
			}
		}
		// A blank kind means the OpenAI protocol, which is what every provider
		// in the catalog speaks by default.
		blank := good
		blank.Kind = ""
		if _, err := ctor(ws, blank); err != nil {
			t.Errorf("%s with a blank kind: %v", name, err)
		}
		if _, err := ctor(nil, good); err == nil {
			t.Errorf("%s with no workspace should fail at construction", name)
		}
	}
}

func TestMediaTools_AreRegisteredUnderTheirNames(t *testing.T) {
	ws := mediaWorkspace(t, workspace.Options{})
	target := media.Target{ProviderID: "p", ModelID: "m", BaseURL: "https://example.com/v1", Kind: media.KindOpenAI}
	cases := map[string]func(*workspace.Workspace, media.Target) (tool.InvokableTool, error){
		"describe_image":   NewDescribeImageTool,
		"generate_image":   NewGenerateImageTool,
		"transcribe_audio": NewTranscribeAudioTool,
	}
	for want, ctor := range cases {
		it, err := ctor(ws, target)
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		info, err := it.Info(context.Background())
		if err != nil {
			t.Fatalf("%s: Info: %v", want, err)
		}
		if info.Name != want {
			t.Errorf("tool name = %q, want %q", info.Name, want)
		}
	}
}

func TestMediaReadBody_BoundsTheResponse(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(strings.Repeat("a", 32))),
	}
	if _, err := mediaReadBody(resp, 8); err == nil {
		t.Error("expected an over-limit error")
	} else if !strings.Contains(err.Error(), "8 byte limit") {
		t.Errorf("error = %v, want it to name the limit", err)
	}

	resp = &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("12345678")),
	}
	data, err := mediaReadBody(resp, 8)
	if err != nil {
		t.Fatalf("a body exactly at the limit should be accepted: %v", err)
	}
	if string(data) != "12345678" {
		t.Errorf("body = %q", data)
	}
}

func TestTruncateText_KeepsValidUTF8(t *testing.T) {
	// "é" is two bytes, so a cut at 3 would split it without the rune check.
	text := "aébcdef"
	got := truncateText(text, 3)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated text = %q, want an ellipsis", got)
	}
	if strings.ContainsRune(got, '\ufffd') {
		t.Errorf("truncated text = %q, want no broken rune", got)
	}
	if truncateText("short", 100) != "short" {
		t.Error("text under the limit should be unchanged")
	}
}

func TestMediaRedact(t *testing.T) {
	if got := mediaRedact("key sk-abc is wrong", "sk-abc"); strings.Contains(got, "sk-abc") {
		t.Errorf("redact left the secret in %q", got)
	}
	if got := mediaRedact("nothing secret here", ""); got != "nothing secret here" {
		t.Errorf("an empty secret should change nothing, got %q", got)
	}
}

func TestSniffMediaType_DropsTheCharset(t *testing.T) {
	if got := sniffMediaType([]byte("hello, this is text")); got != "text/plain" {
		t.Errorf("sniffed %q, want text/plain", got)
	}
	if got := sniffMediaType(nil); got != "" {
		t.Errorf("empty data sniffed as %q, want empty", got)
	}
	if got := sniffMediaType(mediaPNG); got != "image/png" {
		t.Errorf("sniffed %q, want image/png", got)
	}
}

func TestMediaInputFile_TooLargeHintNamesTheLimit(t *testing.T) {
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSparseFile(t, ws, "big.bin", 4096)
	_, _, err := mediaInputFile(ws, "op", path, 1024, "for a test")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "4096") || !strings.Contains(err.Error(), "1024") || !strings.Contains(err.Error(), "for a test") {
		t.Errorf("error = %v, want both numbers and the hint", err)
	}
}

func TestDescribeImageTool_EmptyProviderErrorBody(t *testing.T) {
	// A gateway that fails without saying anything: the message still has to
	// name the status, so the operator knows what happened.
	srv, _ := mediaServer(t, http.StatusInternalServerError, "")
	ws := mediaWorkspace(t, workspace.Options{})
	path := mediaSeed(t, ws, "shot.png", mediaPNG)

	it, _ := NewDescribeImageTool(ws, mediaTargetFor(srv, "vision-model"))
	_, err := mediaRun(t, it, mediaArgs(t, DescribeImageInput{Path: path}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "empty body") {
		t.Errorf("error = %v, want the status and a note about the empty body", err)
	}
}

func TestMediaTypeRefusals_NameWhatWasFound(t *testing.T) {
	t.Run("empty file", func(t *testing.T) {
		if _, err := imageMediaType("empty.png", nil); err == nil || !strings.Contains(err.Error(), "no readable bytes") {
			t.Errorf("image: err = %v", err)
		}
		if _, err := audioMediaType("empty.mp3", nil); err == nil || !strings.Contains(err.Error(), "no readable bytes") {
			t.Errorf("audio: err = %v", err)
		}
	})
	t.Run("extensionless binary", func(t *testing.T) {
		blob := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}
		_, err := imageMediaType("blob", blob)
		if err == nil || !strings.Contains(err.Error(), "unknown binary content") {
			t.Errorf("err = %v, want the type described for a human", err)
		}
	})
	t.Run("audio with a misleading extension", func(t *testing.T) {
		// A .wav that is really text is caught by the sniffer only if the
		// sniffer recognises the real content; text is not audio, and the
		// extension must not be enough to send it.
		_, err := audioMediaType("recording.wav", []byte("this is plain text"))
		if err == nil {
			t.Fatal("text named .wav must not be sent as audio")
		}
		if !strings.Contains(err.Error(), "text/plain") || !strings.Contains(err.Error(), ".wav") {
			t.Errorf("err = %v, want both the detected type and the extension", err)
		}
	})
}

func TestVisionText_SkipsNonTextParts(t *testing.T) {
	// A provider that echoes the image part back in the answer: only the text
	// parts are the answer.
	got, err := visionText(json.RawMessage(
		`[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"text","text":"looks fine"}]`))
	if err != nil {
		t.Fatalf("visionText: %v", err)
	}
	if got != "looks fine" {
		t.Errorf("text = %q, want only the text parts", got)
	}
}
