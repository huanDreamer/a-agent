package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/media"
	"github.com/huan/huan-agent/internal/workspace"
)

// The media tools give the agent eyes and ears, and the ability to draw. They
// differ from the file tools in one important way: their work happens on
// somebody else's machine, at a price, over a network the agent does not
// control. Three properties follow from that, and all three are enforced here
// rather than left to the caller:
//
//   - A tool is only ever built from a resolved media.Target, which the caller
//     obtains from media.Resolver. A capability with no usable model is never
//     registered, so the model cannot see — let alone call — a tool that cannot
//     work.
//   - Every outbound call is bounded: by a timeout, by a request-size cap, and
//     by a cap on the response the tool will read back. A provider that answers
//     with something enormous, or not at all, must not become the agent's
//     problem.
//   - The API key lives in a header and nowhere else. It is never logged, never
//     put in an error, and never quoted back in the excerpt of a provider's
//     error body that a failure reports (see mediaRedact).
//
// The tools speak the OpenAI-compatible HTTP protocol directly rather than
// through the go-openai SDK. The SDK supports the chat and transcription
// endpoints but not the request-size, response-size and error-body bounding
// described above, and having all three tools share one small transport keeps
// that behaviour in a single readable place.

// Limits and defaults for the media tools.
const (
	// DefaultMaxImageBytes caps the image handed to a vision model. Base64
	// inflates by a third and the whole payload is held in memory, so an
	// unbounded image is a real memory failure, not a theoretical one.
	DefaultMaxImageBytes int64 = 8 << 20
	// DefaultMaxAudioBytes caps the audio uploaded for transcription. 25 MiB is
	// the transcription API's own limit, so exceeding it is always a mistake
	// worth naming before the upload starts.
	DefaultMaxAudioBytes int64 = 25 << 20
	// maxProviderResponseBytes caps what a media call will read back from a
	// provider. A generated image arrives base64-inlined in JSON, so this needs
	// headroom; it exists to stop a runaway response from exhausting memory.
	maxProviderResponseBytes int64 = 32 << 20
	// maxErrorBodyBytes bounds how much of a failed response body is quoted back
	// to the model. The reason a call failed (model not found, key invalid) is
	// in there and is the most useful thing an operator can be told, but the
	// whole body of an HTML error page is not.
	maxErrorBodyBytes = 512
	// mediaSniffBytes is how much of a file is inspected to guess its type.
	mediaSniffBytes = 512
	// defaultDescribePrompt is the question asked when the model omits one.
	defaultDescribePrompt = "Describe this image in detail."
)

// MediaUsage is the token usage a provider reported for a media call. It is
// omitted entirely rather than zeroed when a provider reports none, because "no
// tokens" and "not reported" are different facts.
type MediaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// DescribeImageInput is the parameter schema for the "describe_image" tool.
type DescribeImageInput struct {
	Path   string `json:"path" jsonschema:"description=Image file to look at; a path inside the workspace either relative to its root or absolute. PNG/JPEG/GIF/WebP only, required"`
	Prompt string `json:"prompt,omitempty" jsonschema:"description=What to look for in the image. Omit to ask for a full description"`
}

// DescribeImageOutput is what the "describe_image" tool returns.
type DescribeImageOutput struct {
	// Path is the image's path relative to the workspace root, which is the
	// form every other tool accepts.
	Path string `json:"path"`
	// Text is the model's answer.
	Text string `json:"text"`
	// Model names the model that answered, so a user can tell which one it was.
	Model string      `json:"model"`
	Usage *MediaUsage `json:"usage,omitempty"`
}

// NewDescribeImageTool returns a Tool that shows an image in the workspace to a
// vision model and returns its answer.
//
// target must be the model bound to the vision capability; the caller resolves
// it and does not construct this tool when there is none. The image never
// enters the model's text context: only the answer does.
func NewDescribeImageTool(ws *workspace.Workspace, target media.Target) (tool.InvokableTool, error) {
	const op = "describe_image"
	client, err := newMediaClient(op, ws, target)
	if err != nil {
		return nil, err
	}
	return utils.InferTool(op,
		"Look at an image in the workspace with a vision model and answer a question about it. "+
			"Use it for screenshots, photos, diagrams and charts whose content matters but whose bytes are not text. "+
			"The call is refused when the file is not a PNG/JPEG/GIF/WebP image, when it is over the size limit, or when the path is outside the workspace. "+
			"The image itself is not returned, only the answer.",
		func(ctx context.Context, in DescribeImageInput) (DescribeImageOutput, error) {
			return describeImage(ctx, ws, client, in)
		})
}

// GenerateImageInput is the parameter schema for the "generate_image" tool.
type GenerateImageInput struct {
	Prompt string `json:"prompt" jsonschema:"description=What the image should show. Say what the subject is; add the style and composition you want, required"`
	Path   string `json:"path" jsonschema:"description=Where to save the image inside the workspace. Missing parent directories are created, required"`
	Size   string `json:"size,omitempty" jsonschema:"description=Image size such as 1024x1024. Omit to let the provider choose its default"`
	Model  string `json:"model,omitempty" jsonschema:"description=Image model to use instead of the one configured for image generation"`
}

// GenerateImageOutput is what the "generate_image" tool returns.
type GenerateImageOutput struct {
	// Path is where the image was written, relative to the workspace root.
	Path string `json:"path"`
	// Bytes is the size of the written file.
	Bytes int64 `json:"bytes"`
	// Model names the model that produced the image.
	Model string `json:"model"`
	// RevisedPrompt is the prompt the provider actually used, when it rewrites
	// the one it was given. It is worth showing: it explains the result.
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// NewGenerateImageTool returns a Tool that generates an image from a prompt and
// writes it into the workspace.
func NewGenerateImageTool(ws *workspace.Workspace, target media.Target) (tool.InvokableTool, error) {
	const op = "generate_image"
	client, err := newMediaClient(op, ws, target)
	if err != nil {
		return nil, err
	}
	return utils.InferTool(op,
		"Generate an image from a text prompt and save it into the workspace. "+
			"It returns the path to read the result from; the image itself is not returned as data. "+
			"The call is refused when the workspace is read-only, when the target path escapes the workspace, or when the image would exceed the workspace write limit.",
		func(ctx context.Context, in GenerateImageInput) (GenerateImageOutput, error) {
			return generateImage(ctx, ws, client, in)
		})
}

// TranscribeAudioInput is the parameter schema for the "transcribe_audio" tool.
type TranscribeAudioInput struct {
	Path     string `json:"path" jsonschema:"description=Audio file to transcribe; a path inside the workspace either relative to its root or absolute. MP3/M4A/WAV/WEBM/OGG/FLAC accepted, required"`
	Language string `json:"language,omitempty" jsonschema:"description=ISO-639-1 code of the spoken language such as en or zh. Omit to let the model detect it"`
	Prompt   string `json:"prompt,omitempty" jsonschema:"description=Context that helps the model; names and jargon it should expect to hear"`
}

// TranscribeAudioOutput is what the "transcribe_audio" tool returns.
type TranscribeAudioOutput struct {
	// Path is the audio file's path relative to the workspace root.
	Path string `json:"path"`
	// Text is the transcript.
	Text string `json:"text"`
	// Language is the language the provider reported, when it reports one.
	Language string `json:"language,omitempty"`
	// Model names the model that produced the transcript.
	Model string `json:"model"`
}

// NewTranscribeAudioTool returns a Tool that turns speech in an audio file into
// text, so the agent can act on a voice message or a recording.
func NewTranscribeAudioTool(ws *workspace.Workspace, target media.Target) (tool.InvokableTool, error) {
	const op = "transcribe_audio"
	client, err := newMediaClient(op, ws, target)
	if err != nil {
		return nil, err
	}
	return utils.InferTool(op,
		"Transcribe the speech in an audio file in the workspace into text. "+
			"Use it on a voice message or a recording before reasoning about what was said; the audio bytes are never returned as text. "+
			"The call is refused when the file is not a supported audio type, when it is over the size limit, or when it is outside the workspace.",
		func(ctx context.Context, in TranscribeAudioInput) (TranscribeAudioOutput, error) {
			return transcribeAudio(ctx, ws, client, in)
		})
}

// describeImage implements describe_image.
func describeImage(ctx context.Context, ws *workspace.Workspace, client *mediaClient, in DescribeImageInput) (DescribeImageOutput, error) {
	const op = "describe_image"

	abs, rel, err := mediaInputFile(ws, op, in.Path, DefaultMaxImageBytes,
		"for an image sent to a vision model")
	if err != nil {
		return DescribeImageOutput{}, err
	}
	// The file is read whole only after the checks above, and it is bounded by
	// DefaultMaxImageBytes: it goes into the request body, never into the
	// model's text context.
	data, err := os.ReadFile(abs)
	if err != nil {
		return DescribeImageOutput{}, fmt.Errorf("%s: read %s: %w", op, rel, err)
	}
	mimeType, err := imageMediaType(rel, data)
	if err != nil {
		return DescribeImageOutput{}, fmt.Errorf("%s: %w", op, err)
	}

	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		prompt = defaultDescribePrompt
	}

	// The OpenAI multimodal shape, spelled out: a text part with the question
	// and an image part holding the file inline as a data URL. Providers that
	// accept an image URL instead cannot be given one here — it would have to
	// be publicly reachable, and the whole point is that the image is a
	// private local file.
	body, err := json.Marshal(visionRequest{
		Model: client.target.ModelID,
		Messages: []visionMessage{{
			Role: "user",
			Content: []visionPart{
				{Type: "text", Text: prompt},
				{Type: "image_url", ImageURL: &visionImageURL{
					URL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data),
				}},
			},
		}},
	})
	if err != nil {
		return DescribeImageOutput{}, fmt.Errorf("%s: encode request: %w", op, err)
	}

	raw, err := client.postJSON(ctx, op, "/chat/completions", body)
	if err != nil {
		return DescribeImageOutput{}, err
	}

	var resp visionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return DescribeImageOutput{}, fmt.Errorf("%s: decode response: %w", op, err)
	}
	if len(resp.Choices) == 0 {
		return DescribeImageOutput{}, fmt.Errorf("%s: %s/%s returned no choices", op, client.target.ProviderID, client.target.ModelID)
	}
	text, err := visionText(resp.Choices[0].Message.Content)
	if err != nil {
		return DescribeImageOutput{}, fmt.Errorf("%s: %w", op, err)
	}
	if strings.TrimSpace(text) == "" {
		return DescribeImageOutput{}, fmt.Errorf("%s: %s answered with no text; the model may not support images", op, client.target.ModelID)
	}
	return DescribeImageOutput{
		Path:  rel,
		Text:  text,
		Model: client.target.ModelID,
		Usage: mediaUsage(resp.Usage),
	}, nil
}

// generateImage implements generate_image.
func generateImage(ctx context.Context, ws *workspace.Workspace, client *mediaClient, in GenerateImageInput) (GenerateImageOutput, error) {
	const op = "generate_image"

	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return GenerateImageOutput{}, fmt.Errorf("%s: prompt is required", op)
	}
	if strings.TrimSpace(in.Path) == "" {
		return GenerateImageOutput{}, fmt.Errorf("%s: path is required", op)
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = client.target.ModelID
	}

	// Everything that can be decided locally is decided before the request.
	// Generating an image costs money and takes seconds; discovering afterwards
	// that it could not be saved would be paying for nothing, so the read-only
	// check and the confinement check both happen first.
	if ws.ReadOnly() {
		return GenerateImageOutput{}, fmt.Errorf("%s: %w: cannot save %s",
			op, workspace.ErrReadOnly, strings.TrimSpace(in.Path))
	}
	dest, err := ws.Resolve(in.Path)
	if err != nil {
		return GenerateImageOutput{}, fmt.Errorf("%s: %w", op, err)
	}

	body, err := json.Marshal(imageGenerationRequest{
		Model:          model,
		Prompt:         prompt,
		Size:           strings.TrimSpace(in.Size),
		N:              1,
		ResponseFormat: "b64_json",
	})
	if err != nil {
		return GenerateImageOutput{}, fmt.Errorf("%s: encode request: %w", op, err)
	}

	raw, err := client.postJSON(ctx, op, "/images/generations", body)
	if err != nil {
		return GenerateImageOutput{}, err
	}

	var resp imageGenerationResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return GenerateImageOutput{}, fmt.Errorf("%s: decode response: %w", op, err)
	}
	if len(resp.Data) == 0 {
		return GenerateImageOutput{}, fmt.Errorf("%s: %s/%s returned no image", op, client.target.ProviderID, client.target.ModelID)
	}
	first := resp.Data[0]
	if strings.TrimSpace(first.B64JSON) == "" {
		// Some providers ignore response_format and answer with a URL. Saying so
		// is far more useful than "empty image data": it names the cause.
		if strings.TrimSpace(first.URL) != "" {
			return GenerateImageOutput{}, fmt.Errorf(
				"%s: %s ignored response_format=b64_json and returned a URL instead; this tool only accepts inline image data",
				op, client.target.ModelID)
		}
		return GenerateImageOutput{}, fmt.Errorf("%s: %s returned no image data", op, client.target.ModelID)
	}

	decoded, err := base64.StdEncoding.DecodeString(first.B64JSON)
	if err != nil {
		return GenerateImageOutput{}, fmt.Errorf("%s: decode base64 image: %w", op, err)
	}

	// Check the workspace's own write limit before ResolveForWrite does, so the
	// message names both numbers instead of surfacing a bare limit error from
	// deep inside the write path.
	limits := ws.Limits()
	if int64(len(decoded)) > limits.MaxWriteBytes {
		return GenerateImageOutput{}, fmt.Errorf(
			"%s: the generated image is %d bytes, over the workspace write limit of %d bytes; raise tools.max_write_mb or ask for a smaller size",
			op, len(decoded), limits.MaxWriteBytes)
	}
	// ResolveForWrite re-checks confinement and the read-only state, and applies
	// the write limit to the bytes that are actually about to be written, so
	// nothing is created when the write must not happen.
	if _, err := ws.ResolveForWrite(in.Path, int64(len(decoded))); err != nil {
		return GenerateImageOutput{}, fmt.Errorf("%s: %w", op, err)
	}
	abs := fileRealPath(dest)
	rel := ws.Rel(abs)

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return GenerateImageOutput{}, fmt.Errorf("%s: create parent directories for %s: %w", op, rel, err)
	}
	// The same atomic write the file tools use: an interrupted call must not
	// leave a half-written image where a previous one used to be.
	n, err := fileWriteAtomic(abs, decoded, fileModeNew)
	if err != nil {
		return GenerateImageOutput{}, fmt.Errorf("%s: %s: %w", op, rel, err)
	}
	return GenerateImageOutput{
		Path:          rel,
		Bytes:         n,
		Model:         model,
		RevisedPrompt: strings.TrimSpace(first.RevisedPrompt),
	}, nil
}

// transcribeAudio implements transcribe_audio.
func transcribeAudio(ctx context.Context, ws *workspace.Workspace, client *mediaClient, in TranscribeAudioInput) (TranscribeAudioOutput, error) {
	const op = "transcribe_audio"

	abs, rel, err := mediaInputFile(ws, op, in.Path, DefaultMaxAudioBytes,
		"the transcription API accepts")
	if err != nil {
		return TranscribeAudioOutput{}, err
	}

	f, err := os.Open(abs)
	if err != nil {
		return TranscribeAudioOutput{}, fmt.Errorf("%s: open %s: %w", op, rel, err)
	}
	defer func() { _ = f.Close() }()

	// The type is read from the file's own bytes, so a file whose extension lies
	// is caught here rather than by a provider error after the upload.
	head := make([]byte, mediaSniffBytes)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return TranscribeAudioOutput{}, fmt.Errorf("%s: read %s: %w", op, rel, err)
	}
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return TranscribeAudioOutput{}, fmt.Errorf("%s: rewind %s: %w", op, rel, err)
	}
	mimeType, err := audioMediaType(rel, head)
	if err != nil {
		return TranscribeAudioOutput{}, fmt.Errorf("%s: %w", op, err)
	}

	// The upload is buffered, which is acceptable precisely because the size cap
	// above bounds it at DefaultMaxAudioBytes.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, field := range []struct{ name, value string }{
		{"model", client.target.ModelID},
		{"response_format", "json"},
		{"language", strings.TrimSpace(in.Language)},
		{"prompt", strings.TrimSpace(in.Prompt)},
	} {
		// An omitted field is not the same as an empty one: sending
		// language="" asks the provider to transcribe into no language.
		if field.value == "" {
			continue
		}
		if err := mw.WriteField(field.name, field.value); err != nil {
			return TranscribeAudioOutput{}, fmt.Errorf("%s: encode request field %s: %w", op, field.name, err)
		}
	}
	if err := mediaWriteFilePart(mw, rel, mimeType, f); err != nil {
		return TranscribeAudioOutput{}, fmt.Errorf("%s: encode request file: %w", op, err)
	}
	if err := mw.Close(); err != nil {
		return TranscribeAudioOutput{}, fmt.Errorf("%s: encode request: %w", op, err)
	}

	raw, err := client.post(ctx, op, "/audio/transcriptions", mw.FormDataContentType(), &buf)
	if err != nil {
		return TranscribeAudioOutput{}, err
	}

	var resp audioTranscriptionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return TranscribeAudioOutput{}, fmt.Errorf("%s: decode response: %w", op, err)
	}
	if strings.TrimSpace(resp.Text) == "" {
		return TranscribeAudioOutput{}, fmt.Errorf("%s: %s returned an empty transcript", op, client.target.ModelID)
	}
	return TranscribeAudioOutput{
		Path:     rel,
		Text:     resp.Text,
		Language: strings.TrimSpace(resp.Language),
		Model:    client.target.ModelID,
	}, nil
}

// ---------------------------------------------------------------- transport --

// mediaClient is the signing, transport and bounding half of the media tools.
// Every request goes through it, so the authorization header, the timeout and
// the response cap are applied in exactly one place — and so the key exists in
// one place only: read from the target, written to a header, never logged and
// never quoted in an error.
type mediaClient struct {
	target  media.Target
	baseURL string
}

// newMediaClient validates what a media tool cannot work without, at
// construction time rather than on the first call: a tool that cannot sign a
// request must not be registered at all.
func newMediaClient(op string, ws *workspace.Workspace, target media.Target) (*mediaClient, error) {
	if ws == nil {
		return nil, fmt.Errorf("%s: workspace is required", op)
	}
	if strings.TrimSpace(target.ModelID) == "" {
		return nil, fmt.Errorf("%s: target has no model id", op)
	}
	if target.Kind != "" && target.Kind != media.KindOpenAI {
		return nil, fmt.Errorf("%s: provider %q speaks %q, which this build cannot call", op, target.ProviderID, target.Kind)
	}
	base := strings.TrimRight(strings.TrimSpace(target.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("%s: target has no base URL", op)
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("%s: base URL %q is not an http(s) URL", op, base)
	}
	return &mediaClient{target: target, baseURL: base}, nil
}

// postJSON sends a JSON body to an endpoint under the provider's base URL.
func (c *mediaClient) postJSON(ctx context.Context, op, endpoint string, body []byte) ([]byte, error) {
	return c.post(ctx, op, endpoint, "application/json", bytes.NewReader(body))
}

// post sends one request and returns the response body, or an error naming the
// status and a bounded slice of the body.
func (c *mediaClient) post(ctx context.Context, op, endpoint, contentType string, body io.Reader) ([]byte, error) {
	// The timeout is applied per call rather than on a shared client, so a
	// caller can tune one tool without changing another.
	ctx, cancel := context.WithTimeout(ctx, c.target.CallTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", op, err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	// A provider without a key (a local one) gets no header at all rather than
	// an empty Bearer, which some gateways reject.
	if key := strings.TrimSpace(c.target.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// The transport error carries the URL, never the headers, so the key
		// cannot appear here.
		return nil, fmt.Errorf("%s: call %s/%s: %w", op, c.target.ProviderID, c.target.ModelID, err)
	}
	data, err := mediaReadBody(resp, maxProviderResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %s/%s: %w", op, c.target.ProviderID, c.target.ModelID, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, mediaStatusError(op, c.target, resp.StatusCode, data)
	}
	return data, nil
}

// mediaReadBody reads a bounded response body and closes it. The cap is what
// stops a provider — or something answering in its place — from deciding how
// much memory this process uses. It takes the limit as an argument so the
// bounding itself is testable without moving 32 MiB through a test server.
func mediaReadBody(resp *http.Response, limit int64) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response is over the %d byte limit", limit)
	}
	return data, nil
}

// mediaStatusError renders a failed call. The status and the provider's own
// words are the two things that make a media failure actionable (model not
// found, key invalid, quota exhausted), so both are included — but the body is
// bounded, and the key is removed from it before it is shown.
func mediaStatusError(op string, target media.Target, status int, body []byte) error {
	detail := mediaRedact(strings.TrimSpace(string(body)), target.APIKey)
	if detail == "" {
		return fmt.Errorf("%s: %s/%s returned status %d with an empty body", op, target.ProviderID, target.ModelID, status)
	}
	return fmt.Errorf("%s: %s/%s returned status %d: %s",
		op, target.ProviderID, target.ModelID, status,
		truncateText(detail, maxErrorBodyBytes))
}

// mediaRedact removes a secret from text that is about to be shown. Providers
// do echo the credential back on an auth failure ("Incorrect API key provided:
// sk-..."), and the one place a key must never surface is an error message that
// ends up in a log, a chat transcript or an audit row.
func mediaRedact(text, secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" || !strings.Contains(text, secret) {
		return text
	}
	return strings.ReplaceAll(text, secret, "[redacted]")
}

// truncateText bounds text to max bytes on a rune boundary, so a bounded error
// message is still valid UTF-8.
func truncateText(text string, max int) string {
	if len(text) <= max {
		return text
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

// mediaInputFile resolves a workspace path, refuses anything that is not a
// regular file within maxBytes, and returns the absolute and workspace-relative
// paths. Both reading media tools start here, so a refused input reads the same
// way whichever of them was called; limitHint finishes the sentence that names
// the limit, because the sensible limit differs per tool.
func mediaInputFile(ws *workspace.Workspace, op, path string, maxBytes int64, limitHint string) (abs, rel string, err error) {
	abs, err = ws.Resolve(path)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", op, err)
	}
	rel = ws.Rel(abs)

	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", fmt.Errorf("%s: %s does not exist in the workspace (%w)", op, rel, err)
		}
		return "", "", fmt.Errorf("%s: stat %s: %w", op, rel, err)
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("%s: %s is a directory; pass a file", op, rel)
	}
	if info.Size() > maxBytes {
		// Both numbers are named: "file too large" leaves the operator with no
		// idea whether they are just over the limit or far past it.
		return "", "", fmt.Errorf("%s: %s is %d bytes, over the %d byte limit %s",
			op, rel, info.Size(), maxBytes, limitHint)
	}
	return abs, rel, nil
}

// mediaWriteFilePart adds the audio file to a multipart form with its real
// content type. CreateFormFile would send application/octet-stream, which
// leaves the provider to guess from the filename alone.
func mediaWriteFilePart(mw *multipart.Writer, name, contentType string, r io.Reader) error {
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(name)))
	header.Set("Content-Type", contentType)
	part, err := mw.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(part, r)
	return err
}

// --------------------------------------------------------------- media types --

// imageMediaTypes are the image formats a vision request may carry. The list is
// deliberately shorter than what a browser would display: these are the ones
// the vision API itself documents.
var imageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// audioMediaTypes maps both sniffed content types and file extensions to the
// content type to send. One table, because the keys cannot be confused: an
// extension always starts with a dot.
//
// Extensions are here because Go's content sniffer is not an audio decoder. A
// WAV, an Ogg and a WebM are recognised, but M4A, FLAC and an MP3 without an
// ID3 tag all come back as application/octet-stream, and refusing them would
// reject most real recordings.
var audioMediaTypes = map[string]string{
	"audio/mpeg":      "audio/mpeg",
	"audio/wave":      "audio/wav",
	"audio/wav":       "audio/wav",
	"audio/x-wav":     "audio/wav",
	"video/webm":      "audio/webm",
	"audio/webm":      "audio/webm",
	"application/ogg": "audio/ogg",
	"audio/ogg":       "audio/ogg",
	"audio/flac":      "audio/flac",
	"audio/x-flac":    "audio/flac",
	"audio/aac":       "audio/aac",
	"audio/mp4":       "audio/mp4",
	"audio/x-m4a":     "audio/mp4",
	".mp3":            "audio/mpeg",
	".mpga":           "audio/mpeg",
	".m4a":            "audio/mp4",
	".mp4":            "audio/mp4",
	".wav":            "audio/wav",
	".webm":           "audio/webm",
	".ogg":            "audio/ogg",
	".oga":            "audio/ogg",
	".opus":           "audio/ogg",
	".flac":           "audio/flac",
	".aac":            "audio/aac",
	".mpeg":           "audio/mpeg",
}

// imageMediaType decides an image's content type from its bytes, and refuses
// anything that is not an image this build will send. The message names what
// was actually found, because "unsupported file" leaves the model guessing
// whether it passed a PDF, a directory or an audio file.
func imageMediaType(name string, data []byte) (string, error) {
	detected := sniffMediaType(data)
	if imageMediaTypes[detected] {
		return detected, nil
	}
	if detected == "" {
		return "", fmt.Errorf("%s holds no readable bytes; it is not a PNG/JPEG/GIF/WebP image", name)
	}
	return "", fmt.Errorf("%s is %s (%s), which is not a supported image type; PNG, JPEG, GIF and WebP are accepted",
		name, detected, describeMediaType(detected, name))
}

// audioMediaType decides an audio file's content type from its bytes, falling
// back to the extension when the sniffer cannot tell (see audioMediaTypes).
func audioMediaType(name string, head []byte) (string, error) {
	detected := sniffMediaType(head)
	if mimeType, ok := audioMediaTypes[detected]; ok {
		return mimeType, nil
	}
	ext := strings.ToLower(filepath.Ext(name))

	// The contents decide. The extension is only a fallback for the one answer
	// the sniffer cannot make — a generic binary — because M4A, FLAC, AAC and an
	// MP3 without an ID3 tag all land there. It must never override a sniffer
	// that recognised something else: "text/plain" or "image/png" is positive
	// evidence that the file is not audio, and a file renamed to .mp3 is not a
	// recording. An empty file is refused outright for the same reason an empty
	// answer is: there is nothing in it to transcribe.
	if detected == "application/octet-stream" {
		if mimeType, ok := audioMediaTypes[ext]; ok {
			return mimeType, nil
		}
	}
	switch detected {
	case "":
		return "", fmt.Errorf("%s holds no readable bytes; its name %q is not a recognised audio extension either", name, ext)
	case "application/octet-stream":
		return "", fmt.Errorf("%s is unknown binary data and its name %q is not a recognised audio extension; MP3, M4A, WAV, WEBM, OGG and FLAC are accepted",
			name, ext)
	default:
		return "", fmt.Errorf("%s is %s (%s) and is not audio; its name %q does not change that. MP3, M4A, WAV, WEBM, OGG and FLAC are accepted",
			name, describeMediaType(detected, name), detected, ext)
	}
}

// sniffMediaType reports the content type Go's sniffer infers, without the
// charset parameter it appends to text types.
func sniffMediaType(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	detected := http.DetectContentType(data)
	if i := strings.IndexByte(detected, ';'); i >= 0 {
		detected = detected[:i]
	}
	return strings.TrimSpace(detected)
}

// describeMediaType says what a sniffed type means for a human reading the
// refusal: "application/octet-stream" on its own tells an operator nothing
// about which of their files was refused.
func describeMediaType(detected, name string) string {
	switch detected {
	case "application/octet-stream":
		if ext := filepath.Ext(name); ext != "" {
			return "unknown binary content, named " + ext
		}
		return "unknown binary content"
	case "text/plain":
		return "plain text"
	case "application/pdf":
		return "a PDF document"
	default:
		return "detected from its contents"
	}
}

// visionRequest is the OpenAI multimodal chat request, narrowed to what a
// single-image question needs.
type visionRequest struct {
	Model    string          `json:"model"`
	Messages []visionMessage `json:"messages"`
}

type visionMessage struct {
	Role    string       `json:"role"`
	Content []visionPart `json:"content"`
}

type visionPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *visionImageURL `json:"image_url,omitempty"`
}

type visionImageURL struct {
	URL string `json:"url"`
}

// visionResponse is the part of a chat completion the tool reads.
type visionResponse struct {
	Choices []struct {
		Message struct {
			// Content is left raw because a provider may send either a string
			// or an array of parts, and both are seen in the wild.
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *tokenUsage `json:"usage"`
}

// tokenUsage is the usage block a provider may report. A nil pointer and an
// all-zero block both mean "not reported".
type tokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// visionText extracts the answer from a message content field that may be a
// plain string or an array of typed parts.
func visionText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("the response carried no message content")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("the response message content is neither text nor a list of parts: %w", err)
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "" && p.Type != "text" {
			continue
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// mediaUsage converts reported usage, treating an all-zero report as no report.
func mediaUsage(u *tokenUsage) *MediaUsage {
	if u == nil || (u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0) {
		return nil
	}
	return &MediaUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}

// imageGenerationRequest is the request body for POST /images/generations.
type imageGenerationRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Size           string `json:"size,omitempty"`
	N              int    `json:"n"`
	ResponseFormat string `json:"response_format"`
}

// imageGenerationResponse is the part of the image response the tool reads.
type imageGenerationResponse struct {
	Data []struct {
		B64JSON       string `json:"b64_json"`
		URL           string `json:"url"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
}

// audioTranscriptionResponse is the JSON transcript shape (response_format=json).
type audioTranscriptionResponse struct {
	Text     string `json:"text"`
	Language string `json:"language"`
}
