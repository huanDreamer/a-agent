package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspace"
)

// mediaServerOpts configures the attachment test server.
type mediaServerOpts struct {
	// readOnly makes the workspace refuse writes, which must refuse uploads.
	readOnly bool
	// noWorkspace leaves ChatDeps.Workspace unset.
	noWorkspace bool
	// maxWrite caps one workspace write (0 = the workspace default).
	maxWrite int64
	// maxAttachment overrides the per-upload cap (0 = the server default).
	maxAttachment int64
	// maxInline overrides the inlined-image cap (0 = the server default).
	maxInline int64
	// builder replaces the per-session model builder.
	builder ModelBuilder
	// runnerModel is the model the default runner is built with. Tests that
	// assert on what reaches the model pass a recording model here.
	runnerModel model.BaseChatModel
	// turns feeds the scripted model used when runnerModel is nil.
	turns [][]*schema.Message
	// tools is the tool registry the runner may call.
	tools *tool.Registry
	// seed populates the store, e.g. with a model and its capabilities.
	seed func(store.Store)
}

// newMediaServer starts a chat server whose workspace is a temp directory, and
// returns the harness with that directory so a test can assert on what landed
// on disk.
func newMediaServer(t *testing.T, opts mediaServerOpts) (*harness, string) {
	t.Helper()

	root := t.TempDir()
	var ws *workspace.Workspace
	if !opts.noWorkspace {
		lim := workspace.Limits{}
		if opts.maxWrite > 0 {
			lim.MaxWriteBytes = opts.maxWrite
		}
		built, err := workspace.New(root, workspace.Options{ReadOnly: opts.readOnly, Limits: lim})
		if err != nil {
			t.Fatalf("workspace.New: %v", err)
		}
		ws = built
	}

	mdl := opts.runnerModel
	if mdl == nil {
		mdl = &scriptedModel{turns: opts.turns}
	}
	runner, err := chat.New(chat.Config{Model: mdl, Tools: opts.tools, MaxSteps: 4, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	srv, st := buildServerWith(t, buildOpts{seed: opts.seed, chat: ChatDeps{
		Runner:              runner,
		Builder:             opts.builder,
		Tools:               opts.tools,
		Workspace:           ws,
		MaxAttachmentBytes:  opts.maxAttachment,
		MaxInlineImageBytes: opts.maxInline,
	}})
	startHarness(t, srv)
	h := &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
	h.login(t)
	return h, root
}

// staticBuilder is a ModelBuilder that hands out one pre-built model, so a test
// can point a session at a mock provider and assert on what is sent to it.
type staticBuilder struct {
	provider string
	model    string
	cm       model.BaseChatModel
}

// Build implements ModelBuilder.
func (b *staticBuilder) Build(context.Context, string, string) (any, error) { return b.cm, nil }

// Catalog implements ModelBuilder.
func (b *staticBuilder) Catalog(context.Context) ModelCatalog {
	return ModelCatalog{
		Models: []ModelChoice{{
			Provider: b.provider, ProviderName: b.provider, Model: b.model, DisplayName: b.model,
			Capabilities: []string{string(store.CapChat)}, ChatCapable: true,
			Default: true, HasAPIKey: true,
		}},
		Providers: []ProviderChoice{{
			ID: b.provider, Name: b.provider, Enabled: true, HasAPIKey: true,
			ModelCount: 1, EnabledModelCount: 1, ChatModelCount: 1,
		}},
	}
}

// seedModel records a provider and one of its models, with the given
// capabilities. Capabilities live in the catalog, not in the config, so this is
// what decides whether an image is inlined for a session.
func seedModel(t *testing.T, st store.Store, provider, modelID string, caps ...store.Capability) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertProvider(ctx, store.Provider{
		ID: provider, Name: provider, Enabled: true, Source: store.SourceUser,
	}); err != nil {
		t.Fatalf("UpsertProvider: %v", err)
	}
	if err := st.UpsertModel(ctx, store.Model{
		ProviderID: provider, ModelID: modelID, Capabilities: caps, Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
}

// pngBytes builds a real PNG, so the upload path is exercised with bytes a
// browser would actually send rather than with a magic prefix.
func pngBytes(t *testing.T, side int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for x := 0; x < side; x++ {
		for y := 0; y < side; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 8), B: 0x40, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// wavBytes builds a minimal canonical WAV file: a RIFF/WAVE header and a few
// frames of silence.
func wavBytes(t *testing.T) []byte {
	t.Helper()
	const dataLen = 64
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // mono
	_ = binary.Write(&buf, binary.LittleEndian, uint32(8000))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16000))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	buf.Write(make([]byte, dataLen))
	return buf.Bytes()
}

// uploadFile posts one file to a session's attachment endpoint. The part is
// declared as application/octet-stream on purpose: the endpoint must sniff the
// bytes rather than believe the declared type.
func uploadFile(t *testing.T, h *harness, sessionID, name string, data []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(uploadFieldName, name)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost,
		h.base+"/api/chat/sessions/"+sessionID+"/attachments", &buf)
	if err != nil {
		t.Fatalf("new upload request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return resp
}

// uploadAttachment uploads a file and returns the decoded attachment.
func uploadAttachment(t *testing.T, h *harness, sessionID, name string, data []byte) attachmentInfo {
	t.Helper()
	resp := uploadFile(t, h, sessionID, name, data)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		t.Fatalf("upload status = %d, want 200 (body: %s)", resp.StatusCode, buf.String())
	}
	var out struct {
		Attachment attachmentInfo `json:"attachment"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	return out.Attachment
}

// errorMessage returns the "error" field of a JSON error response.
func errorMessage(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return body.Error
}

// filesUnder lists regular files below root, so a test can prove a refused
// upload wrote nothing.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func TestAttachment_UploadPNG(t *testing.T) {
	h, root := newMediaServer(t, mediaServerOpts{})
	id := createSession(t, h)

	want := pngBytes(t, 6)
	info := uploadAttachment(t, h, id, "我的 截图.png", want)

	if info.ID == "" {
		t.Fatal("upload returned no id")
	}
	if info.Kind != store.MediaKindImage {
		t.Errorf("kind = %q, want image", info.Kind)
	}
	if info.MIME != "image/png" {
		t.Errorf("mime = %q, want image/png", info.MIME)
	}
	if info.Bytes != int64(len(want)) {
		t.Errorf("bytes = %d, want %d", info.Bytes, len(want))
	}
	if info.URL != attachmentURLPrefix+info.ID {
		t.Errorf("url = %q, want %s%s", info.URL, attachmentURLPrefix, info.ID)
	}
	if info.Name != "我的 截图.png" {
		t.Errorf("name = %q, want the sanitised client name", info.Name)
	}

	// The path must be workspace-relative and point at a real file inside the
	// workspace: that is what the media tools are later told to read.
	if filepath.IsAbs(info.Path) {
		t.Errorf("path = %q, want a workspace-relative path", info.Path)
	}
	if !strings.HasPrefix(info.Path, attachmentDir+"/") {
		t.Errorf("path = %q, want it under %s/", info.Path, attachmentDir)
	}
	if !strings.HasSuffix(info.Path, ".png") {
		t.Errorf("path = %q, want the sniffed type's extension", info.Path)
	}
	abs := filepath.Join(root, filepath.FromSlash(info.Path))
	stored, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("the stored file is not inside the workspace: %v", err)
	}
	if !bytes.Equal(stored, want) {
		t.Error("the stored bytes differ from what was uploaded")
	}
}

func TestAttachment_PathIsNotBuiltFromTheClientName(t *testing.T) {
	h, root := newMediaServer(t, mediaServerOpts{})
	id := createSession(t, h)

	// A name that would escape the workspace if it were ever used as a path.
	info := uploadAttachment(t, h, id, "../../etc/passwd.png", pngBytes(t, 4))
	if strings.Contains(info.Path, "..") || strings.Contains(info.Path, "passwd") {
		t.Errorf("path = %q, want a path derived from the generated id only", info.Path)
	}
	if info.Name != "passwd.png" {
		t.Errorf("name = %q, want the directory components stripped", info.Name)
	}
	// Nothing may appear outside the media tree.
	for _, f := range filesUnder(t, root) {
		if !strings.HasPrefix(f, filepath.Join(root, "media")) {
			t.Errorf("a file was written outside the media tree: %s", f)
		}
	}
}

func TestAttachment_UploadAudio(t *testing.T) {
	h, _ := newMediaServer(t, mediaServerOpts{})
	id := createSession(t, h)

	want := wavBytes(t)
	info := uploadAttachment(t, h, id, "voice.wav", want)

	if info.Kind != store.MediaKindAudio {
		t.Errorf("kind = %q, want audio", info.Kind)
	}
	if info.MIME != "audio/wav" {
		t.Errorf("mime = %q, want audio/wav (the sniffer calls it audio/wave)", info.MIME)
	}
	if !strings.HasSuffix(info.Path, ".wav") {
		t.Errorf("path = %q, want a .wav extension", info.Path)
	}
}

func TestAttachment_SniffedTypes(t *testing.T) {
	// The accept table is the contract of this endpoint, and every row is
	// reached by sniffing bytes rather than by trusting a header or a name.
	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"png", []byte("\x89PNG\r\n\x1a\n............."), "image/png"},
		{"jpeg", []byte("\xff\xd8\xff\xe0....."), "image/jpeg"},
		{"gif", []byte("GIF89a....."), "image/gif"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"mp3 with id3", []byte("ID3\x04\x00\x00......"), "audio/mpeg"},
		{"mp3 frame sync", []byte("\xff\xfb\x90\x00......"), "audio/mpeg"},
		{"m4a", append([]byte("\x00\x00\x00\x20ftypM4A "), make([]byte, 16)...), "audio/mp4"},
		{"wav", wavBytes(t), "audio/wave"},
		{"webm", []byte("\x1a\x45\xdf\xa3........."), "video/webm"},
		{"ogg", []byte("OggS\x00....."), "application/ogg"},
		{"flac", []byte("fLaC\x00\x00\x00\x22....."), "audio/flac"},
		{"plain text", []byte("this is not media at all"), "text/plain; charset=utf-8"},
		{"pdf", []byte("%PDF-1.4....."), "application/pdf"},
		{"shell script", []byte("#!/bin/sh\necho hi\n"), "text/plain; charset=utf-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sniffAttachmentType(tc.head); got != tc.want {
				t.Errorf("sniffAttachmentType = %q, want %q", got, tc.want)
			}
		})
	}

	// Everything the endpoint accepts is stored under a canonical type and an
	// extension that matches it, so the served Content-Type can never disagree
	// with the file on disk.
	for sniffed, typ := range attachmentTypes {
		if typ.ext == "" || !strings.HasPrefix(typ.ext, ".") {
			t.Errorf("%s has no usable extension: %q", sniffed, typ.ext)
		}
		if typ.kind != kindImage && typ.kind != kindAudio {
			t.Errorf("%s has kind %q, want image or audio", sniffed, typ.kind)
		}
		if _, ok := attachmentTypes[typ.mime]; !ok {
			t.Errorf("%s normalises to %s, which is not itself an accepted type", sniffed, typ.mime)
		}
	}
}

func TestAttachment_UnsupportedTypeIsRefused(t *testing.T) {
	h, root := newMediaServer(t, mediaServerOpts{})
	id := createSession(t, h)

	// A text file named .png: believing either the name or the declared part
	// type would let a non-image through.
	resp := uploadFile(t, h, id, "not-really.png", []byte("this is plainly not an image, whatever it is called"))
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
	msg := errorMessage(t, resp)
	if !strings.Contains(msg, "text/plain") {
		t.Errorf("error = %q, want it to name the type that was detected", msg)
	}
	if files := filesUnder(t, root); len(files) != 0 {
		t.Errorf("a refused upload wrote %v", files)
	}
}

func TestAttachment_OverTheLimitIsRefused(t *testing.T) {
	// The cap is lowered so the test does not have to move 25 MiB to prove the
	// limit is enforced.
	h, root := newMediaServer(t, mediaServerOpts{maxAttachment: 4096})
	id := createSession(t, h)

	big := append(pngBytes(t, 4), make([]byte, 8192)...)
	resp := uploadFile(t, h, id, "big.png", big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if msg := errorMessage(t, resp); !strings.Contains(msg, "4096") {
		t.Errorf("error = %q, want it to name the limit", msg)
	}
	if files := filesUnder(t, root); len(files) != 0 {
		t.Errorf("a refused upload wrote %v", files)
	}
}

func TestAttachment_IsNotBoundedByTheToolWriteLimit(t *testing.T) {
	// The workspace's write limit bounds what the AGENT'S TOOLS may write, and
	// it is sized for source files (4 MiB by default). An upload is the
	// operator's own action, so applying that limit would refuse an ordinary
	// photo. The attachment cap is what bounds an upload instead.
	//
	// This deliberately reverses an earlier behaviour: with the shipped default
	// tools.max_write_mb of 4, a 25 MB photo was refused by a limit meant for
	// the agent's file writes.
	h, root := newMediaServer(t, mediaServerOpts{maxWrite: 1024, maxAttachment: 8 << 20})
	id := createSession(t, h)

	// Comfortably over the tool write limit, comfortably under the cap.
	resp := uploadFile(t, h, id, "shot.png", append(pngBytes(t, 4), make([]byte, 4096)...))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: an upload must not be bounded by the tool write limit (body: %s)",
			resp.StatusCode, errorMessage(t, resp))
	}
	if files := filesUnder(t, root); len(files) == 0 {
		t.Error("the accepted upload was not stored")
	}
}

func TestAttachment_IsStillBoundedByTheAttachmentCap(t *testing.T) {
	// Dropping the tool write limit must not drop the real one.
	h, root := newMediaServer(t, mediaServerOpts{maxWrite: 1024, maxAttachment: 2048})
	id := createSession(t, h)

	resp := uploadFile(t, h, id, "shot.png", append(pngBytes(t, 4), make([]byte, 8192)...))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if msg := errorMessage(t, resp); !strings.Contains(msg, "上限") {
		t.Errorf("error = %q, want it to name the limit", msg)
	}
	if files := filesUnder(t, root); len(files) != 0 {
		t.Errorf("a refused upload wrote %v", files)
	}
}

func TestAttachment_OverTheLimitIsRefusedWhileStreaming(t *testing.T) {
	// The engine's own request cap is 4 MiB, so a body above it reaches the
	// handler as a stream: the limit must still be applied to the bytes as they
	// are read, the answer must still be a JSON 413, and nothing may be left
	// behind — including when most of the upload was never read.
	h, root := newMediaServer(t, mediaServerOpts{maxAttachment: 64 << 10})
	id := createSession(t, h)

	huge := append(pngBytes(t, 4), make([]byte, 5<<20)...)
	resp := uploadFile(t, h, id, "huge.png", huge)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if msg := errorMessage(t, resp); !strings.Contains(msg, "上限") {
		t.Errorf("error = %q, want it to name the limit", msg)
	}
	if files := filesUnder(t, root); len(files) != 0 {
		t.Errorf("a refused upload wrote %v", files)
	}
}

func TestAttachment_LargeUploadIsAccepted(t *testing.T) {
	// The whole point of streaming the body is that a real photo is not cut off
	// by the engine's default 4 MiB cap on the way in.
	h, _ := newMediaServer(t, mediaServerOpts{maxAttachment: 8 << 20, maxWrite: 8 << 20})
	id := createSession(t, h)

	want := append(pngBytes(t, 4), make([]byte, 5<<20)...)
	info := uploadAttachment(t, h, id, "photo.png", want)
	if info.Bytes != int64(len(want)) {
		t.Errorf("bytes = %d, want %d", info.Bytes, len(want))
	}
	if info.Kind != store.MediaKindImage {
		t.Errorf("kind = %q, want image", info.Kind)
	}

	// And it can be read back byte for byte.
	resp := h.get(t, "/api/chat/attachments/"+info.ID)
	defer func() { _ = resp.Body.Close() }()
	requireStatusBody(t, resp, http.StatusOK, int64(len(want)))
}

// requireStatusBody asserts a status and that the body is the expected length.
func requireStatusBody(t *testing.T, resp *http.Response, want int, size int64) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d", resp.StatusCode, want)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if n != size {
		t.Errorf("body = %d bytes, want %d", n, size)
	}
}

func TestAttachment_ReadOnlyWorkspaceIsRefused(t *testing.T) {
	h, _ := newMediaServer(t, mediaServerOpts{readOnly: true})
	id := createSession(t, h)

	resp := uploadFile(t, h, id, "shot.png", pngBytes(t, 4))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if msg := errorMessage(t, resp); !strings.Contains(msg, "只读") {
		t.Errorf("error = %q, want a message that explains the read-only workspace", msg)
	}
}

func TestAttachment_NoWorkspaceIsRefused(t *testing.T) {
	// Without a workspace there is nowhere inside the sandbox to put the file,
	// and writing it anywhere else would hide it from every agent tool.
	h, _ := newMediaServer(t, mediaServerOpts{noWorkspace: true})
	id := createSession(t, h)

	resp := uploadFile(t, h, id, "shot.png", pngBytes(t, 4))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if msg := errorMessage(t, resp); !strings.Contains(msg, "工作区") {
		t.Errorf("error = %q, want a message that explains the missing workspace", msg)
	}
}

func TestStreamedBody_JSONHandlersStillSeeTheirWholeBody(t *testing.T) {
	// Request bodies are streamed so a large upload can be read under its own
	// limit. Every other endpoint reads its JSON through the binder, which drains
	// a streamed body: a body larger than the engine's stream threshold must
	// still arrive intact.
	h, _ := newMediaServer(t, mediaServerOpts{turns: [][]*schema.Message{{
		{Role: schema.Assistant, Content: "收到"},
	}}})
	id := createSession(t, h)

	content := strings.Repeat("很长的消息。", 4000) // well over the engine's 8 KiB stream threshold
	events := sendMessage(t, h, id, content, nil)
	if _, ok := findEvent(events, "done"); !ok {
		t.Fatalf("a large JSON body was not accepted: %v", eventTypes(events))
	}

	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &got)
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(got.Messages))
	}
	if got.Messages[0].Content != content {
		t.Errorf("stored content is %d bytes, want the %d that were sent", len(got.Messages[0].Content), len(content))
	}
}

func TestStreamedBody_OversizedNonUploadBodyIsRefused(t *testing.T) {
	// Streaming the body must not turn every endpoint into a way to allocate an
	// arbitrarily large buffer: a body over the buffer limit is refused before a
	// handler ever sees it.
	h, _ := newMediaServer(t, mediaServerOpts{})

	huge := `{"password":"` + strings.Repeat("x", int(bufferedBodyLimit)+(1<<20)) + `"}`
	resp, err := h.client.Post(h.base+"/api/login", "application/json", strings.NewReader(huge))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if msg := errorMessage(t, resp); !strings.Contains(msg, "too large") {
		t.Errorf("error = %q, want it to say the body was too large", msg)
	}

	// Declaring a multipart body must not be a way around the limit: only the
	// upload route is allowed to read its own body.
	req, err := http.NewRequest(http.MethodPost, h.base+"/api/login", strings.NewReader(huge))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	multipartResp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("post with a multipart content type: %v", err)
	}
	requireStatus(t, multipartResp, http.StatusRequestEntityTooLarge)
}

func TestAttachment_UploadValidation(t *testing.T) {
	h, _ := newMediaServer(t, mediaServerOpts{})
	id := createSession(t, h)

	t.Run("unknown session", func(t *testing.T) {
		resp := uploadFile(t, h, "nope", "shot.png", pngBytes(t, 4))
		requireStatus(t, resp, http.StatusBadRequest)
	})
	t.Run("no file field", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := mw.WriteField("note", "hi"); err != nil {
			t.Fatalf("write field: %v", err)
		}
		_ = mw.Close()
		req, err := http.NewRequest(http.MethodPost,
			h.base+"/api/chat/sessions/"+id+"/attachments", &buf)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
		requireStatus(t, resp, http.StatusBadRequest)
	})
	t.Run("not multipart", func(t *testing.T) {
		resp := h.postJSON(t, "/api/chat/sessions/"+id+"/attachments", map[string]string{"file": "x"})
		requireStatus(t, resp, http.StatusBadRequest)
	})
	t.Run("empty file", func(t *testing.T) {
		resp := uploadFile(t, h, id, "empty.png", nil)
		requireStatus(t, resp, http.StatusBadRequest)
	})
}

func TestAttachment_UploadIsIdempotentForIdenticalBytes(t *testing.T) {
	// The stored sha256 exists so the same file sent twice is recognisable
	// rather than duplicated in the workspace.
	h, root := newMediaServer(t, mediaServerOpts{})
	id := createSession(t, h)

	data := pngBytes(t, 5)
	first := uploadAttachment(t, h, id, "a.png", data)
	second := uploadAttachment(t, h, id, "again.png", data)
	if first.ID != second.ID {
		t.Errorf("identical re-upload got a new id (%s then %s)", first.ID, second.ID)
	}
	if files := filesUnder(t, root); len(files) != 1 {
		t.Errorf("workspace holds %d files, want 1: %v", len(files), files)
	}
}

func TestAttachment_Download(t *testing.T) {
	h, _ := newMediaServer(t, mediaServerOpts{})
	id := createSession(t, h)

	want := pngBytes(t, 5)
	info := uploadAttachment(t, h, id, "shot.png", want)

	resp := h.get(t, "/api/chat/attachments/"+info.ID)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	disposition := resp.Header.Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "inline;") || !strings.Contains(disposition, `filename="`) {
		t.Errorf("Content-Disposition = %q, want an inline disposition with a filename", disposition)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
		t.Errorf("Cache-Control = %q, want a long-lived cache directive", cc)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the served bytes differ from what was uploaded")
	}

	t.Run("unknown id", func(t *testing.T) {
		h.getJSON(t, "/api/chat/attachments/00000000000000000000000000000000", http.StatusNotFound, nil)
	})
	t.Run("stored file gone", func(t *testing.T) {
		// A row whose file was deleted must read as missing, not as a 500.
		other := uploadAttachment(t, h, id, "gone.png", pngBytes(t, 4))
		removed := filepath.Join(h.srv.chat.Workspace.Root(), filepath.FromSlash(other.Path))
		if err := os.Remove(removed); err != nil {
			t.Fatalf("remove stored file: %v", err)
		}
		h.getJSON(t, "/api/chat/attachments/"+other.ID, http.StatusNotFound, nil)
	})
}

func TestAttachment_DownloadRejectsAPathThatEscapesTheWorkspace(t *testing.T) {
	// A stored path is data. If a row ever held a traversal, serving it must be
	// refused by the workspace sandbox rather than joined into a real path.
	h, root := newMediaServer(t, mediaServerOpts{})
	id := createSession(t, h)

	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := h.store.CreateMediaAsset(context.Background(), store.MediaAsset{
		ID: "escape", SessionID: id, Kind: store.MediaKindImage,
		Path: "../outside.txt", MIME: "image/png", Bytes: 6,
	}); err != nil {
		t.Fatalf("CreateMediaAsset: %v", err)
	}

	resp := h.get(t, "/api/chat/attachments/escape")
	requireStatus(t, resp, http.StatusNotFound)
}

func TestChatSend_UnreadableAttachmentIsRefused(t *testing.T) {
	// A row whose file was deleted from the workspace must fail the send with a
	// message naming the path rather than quietly sending the text as if the
	// file were still there.
	h, root := newMediaServer(t, mediaServerOpts{turns: [][]*schema.Message{{
		{Role: schema.Assistant, Content: "ok"},
	}}})
	id := createSession(t, h)

	gone := uploadAttachment(t, h, id, "gone.png", pngBytes(t, 4))
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(gone.Path))); err != nil {
		t.Fatalf("remove stored file: %v", err)
	}
	resp := h.postJSON(t, "/api/chat/sessions/"+id+"/messages", map[string]any{
		"content": "看这张图", "attachments": []string{gone.ID},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if msg := errorMessage(t, resp); !strings.Contains(msg, "已不存在") {
		t.Errorf("error = %q, want it to say the file is gone", msg)
	}

	// A path that is a directory cannot be read as a file either.
	dir := uploadAttachment(t, h, id, "dir.png", pngBytes(t, 4))
	abs := filepath.Join(root, filepath.FromSlash(dir.Path))
	if err := os.Remove(abs); err != nil {
		t.Fatalf("remove stored file: %v", err)
	}
	if err := os.Mkdir(abs, 0o755); err != nil {
		t.Fatalf("mkdir at the stored path: %v", err)
	}
	resp = h.postJSON(t, "/api/chat/sessions/"+id+"/messages", map[string]any{
		"content": "看这个", "attachments": []string{dir.ID},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if msg := errorMessage(t, resp); !strings.Contains(msg, "目录") {
		t.Errorf("error = %q, want it to say the path is a directory", msg)
	}

	var got struct {
		Messages []any `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &got)
	if len(got.Messages) != 0 {
		t.Errorf("a refused turn persisted %d messages", len(got.Messages))
	}
}

func TestChatSend_AttachmentWithoutAWorkspaceIsRefused(t *testing.T) {
	// A deployment with no workspace can hold no readable file: the send must be
	// refused rather than read a path the sandbox does not cover.
	h, _ := newMediaServer(t, mediaServerOpts{noWorkspace: true})
	id := createSession(t, h)
	if err := h.store.CreateMediaAsset(context.Background(), store.MediaAsset{
		ID: "orphan", SessionID: id, Kind: store.MediaKindImage,
		Path: "media/2026/09/orphan.png", MIME: "image/png", Bytes: 10,
	}); err != nil {
		t.Fatalf("CreateMediaAsset: %v", err)
	}

	resp := h.postJSON(t, "/api/chat/sessions/"+id+"/messages", map[string]any{
		"content": "hi", "attachments": []string{"orphan"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if msg := errorMessage(t, resp); !strings.Contains(msg, "工作区") {
		t.Errorf("error = %q, want it to explain the missing workspace", msg)
	}
}

// sendMessage posts a turn with attachments and drains the SSE stream.
func sendMessage(t *testing.T, h *harness, sessionID, content string, attachments []string) []sseEvent {
	t.Helper()
	body := map[string]any{"content": content}
	if attachments != nil {
		body["attachments"] = attachments
	}
	return readSSE(t, h.client, h.base+"/api/chat/sessions/"+sessionID+"/messages", body)
}

func TestChatSend_AttachmentFromAnotherSessionIsRefused(t *testing.T) {
	h, _ := newMediaServer(t, mediaServerOpts{turns: [][]*schema.Message{{
		{Role: schema.Assistant, Content: "ok"},
	}}})
	owner := createSession(t, h)
	other := createSession(t, h)
	info := uploadAttachment(t, h, owner, "shot.png", pngBytes(t, 4))

	resp := h.postJSON(t, "/api/chat/sessions/"+other+"/messages", map[string]any{
		"content": "看这张图", "attachments": []string{info.ID},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if msg := errorMessage(t, resp); !strings.Contains(msg, "不属于当前会话") {
		t.Errorf("error = %q, want it to say the attachment belongs to another session", msg)
	}

	// A refused turn must leave no message behind.
	var got struct {
		Messages []any `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+other, http.StatusOK, &got)
	if len(got.Messages) != 0 {
		t.Errorf("the refused turn persisted %d messages", len(got.Messages))
	}
}

func TestChatSend_UnknownAttachmentIsRefused(t *testing.T) {
	h, _ := newMediaServer(t, mediaServerOpts{})
	id := createSession(t, h)

	resp := h.postJSON(t, "/api/chat/sessions/"+id+"/messages", map[string]any{
		"content": "hi", "attachments": []string{"deadbeef"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if msg := errorMessage(t, resp); !strings.Contains(msg, "附件不存在") {
		t.Errorf("error = %q, want it to say the attachment does not exist", msg)
	}
}

func TestChatSend_AttachmentOnlyTurn(t *testing.T) {
	// A picture with no words is a real message; it must not be refused for
	// having no text.
	h, _ := newMediaServer(t, mediaServerOpts{turns: [][]*schema.Message{{
		{Role: schema.Assistant, Content: "看到了"},
	}}})
	id := createSession(t, h)
	info := uploadAttachment(t, h, id, "shot.png", pngBytes(t, 4))

	events := sendMessage(t, h, id, "", []string{info.ID})
	if _, ok := findEvent(events, "done"); !ok {
		t.Fatalf("no done event: %v", eventTypes(events))
	}
	if _, ok := findEvent(events, "stream_end"); !ok {
		t.Error("no stream_end event")
	}

	var got struct {
		Messages []struct {
			Role        string `json:"role"`
			Content     string `json:"content"`
			Attachments string `json:"attachments"`
		} `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &got)
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(got.Messages))
	}
	if got.Messages[0].Content == "" {
		t.Error("an attachment-only turn must still persist some text")
	}
}

func TestChatSend_NonVisionModelIsToldThePath(t *testing.T) {
	// Without the vision capability the image cannot be inlined, so the model is
	// told where the file is and can reach it with describe_image.
	mdl := &recordingModel{}
	h, _ := newMediaServer(t, mediaServerOpts{runnerModel: mdl})
	id := createSession(t, h)
	info := uploadAttachment(t, h, id, "shot.png", pngBytes(t, 4))

	sendMessage(t, h, id, "这张图里是什么", []string{info.ID})

	mdl.mu.Lock()
	defer mdl.mu.Unlock()
	if len(mdl.seen) == 0 {
		t.Fatal("the model was never called")
	}
	last := mdl.seen[len(mdl.seen)-1]
	var user *schema.Message
	for i := len(last) - 1; i >= 0; i-- {
		if last[i].Role == schema.User {
			user = last[i]
			break
		}
	}
	if user == nil {
		t.Fatal("no user message reached the model")
	}
	if len(user.UserInputMultiContent) != 0 {
		t.Error("a text-only model was sent multimodal content; the request would be rejected")
	}
	if !strings.Contains(user.Content, info.Path) {
		t.Errorf("the message does not carry the attachment path %q: %q", info.Path, user.Content)
	}
	if !strings.Contains(user.Content, describeImageTool) {
		t.Errorf("the note does not point at %s: %q", describeImageTool, user.Content)
	}
	if !strings.Contains(user.Content, "这张图里是什么") {
		t.Errorf("the user's own text was lost: %q", user.Content)
	}
	if strings.Contains(user.Content, "base64") {
		t.Error("the note must not carry encoded bytes")
	}
}

func TestChatSend_VisionModelGetsAnImagePart(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()

		// A streamed answer, which is the shape the chat turn expects.
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"看到了"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	cm, err := llm.New(llm.Provider{
		Name: "local", BaseURL: upstream.URL, APIKey: "test", Model: "vision-model",
		DisableUsageRequest: true,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	h, _ := newMediaServer(t, mediaServerOpts{
		builder: &staticBuilder{provider: "local", model: "vision-model", cm: cm},
		seed: func(st store.Store) {
			seedModel(t, st, "local", "vision-model", store.CapChat, store.CapVision)
		},
	})

	resp := h.postJSON(t, "/api/chat/sessions", map[string]string{"provider": "local", "model": "vision-model"})
	var created struct {
		Session struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"session"`
	}
	decode(t, resp, &created)
	if created.Session.Model != "vision-model" {
		t.Fatalf("session model = %q, want vision-model", created.Session.Model)
	}

	info := uploadAttachment(t, h, created.Session.ID, "shot.png", pngBytes(t, 4))
	events := sendMessage(t, h, created.Session.ID, "这张图里是什么", []string{info.ID})
	if _, ok := findEvent(events, "done"); !ok {
		t.Fatalf("no done event: %v", eventTypes(events))
	}

	mu.Lock()
	count := len(bodies)
	first := ""
	if count > 0 {
		first = bodies[0]
	}
	mu.Unlock()
	if count == 0 {
		t.Fatal("the provider was never called")
	}
	if !strings.Contains(first, `"image_url"`) {
		t.Fatalf("the request carries no image_url: %s", first)
	}
	if !strings.Contains(first, `"data:image/png;base64,`) {
		t.Errorf("the image is not inlined as a data URL: %s", first)
	}
	if !strings.Contains(first, "这张图里是什么") {
		t.Errorf("the text part is missing: %s", first)
	}

	// Only the current turn's attachments are inlined: replaying them would
	// re-send the picture as prompt tokens on every later turn. The upstream
	// handler records each request under the same lock, so it must not be held
	// while a turn runs.
	sendMessage(t, h, created.Session.ID, "还有呢", nil)
	mu.Lock()
	count = len(bodies)
	last := bodies[len(bodies)-1]
	mu.Unlock()
	if count < 2 {
		t.Fatalf("the second turn never reached the provider: %d requests", count)
	}
	if strings.Contains(last, "image_url") {
		t.Errorf("a later turn replayed the attachment: %s", last)
	}
}

func TestChatSend_OversizedImageFallsBackToAPathNote(t *testing.T) {
	// Base64 inflates the payload, so an image over the inline cap is sent as a
	// note instead of being inlined.
	var (
		mu     sync.Mutex
		bodies []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	cm, err := llm.New(llm.Provider{
		Name: "local", BaseURL: upstream.URL, APIKey: "test", Model: "vision-model",
		DisableUsageRequest: true,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	h, _ := newMediaServer(t, mediaServerOpts{
		builder:   &staticBuilder{provider: "local", model: "vision-model", cm: cm},
		maxInline: 32, // far below any real PNG
		seed: func(st store.Store) {
			seedModel(t, st, "local", "vision-model", store.CapChat, store.CapVision)
		},
	})
	resp := h.postJSON(t, "/api/chat/sessions", map[string]string{"provider": "local", "model": "vision-model"})
	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decode(t, resp, &created)

	info := uploadAttachment(t, h, created.Session.ID, "big.png", pngBytes(t, 16))
	sendMessage(t, h, created.Session.ID, "看这张图", []string{info.ID})

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("the provider was never called")
	}
	body := bodies[0]
	if strings.Contains(body, "image_url") {
		t.Errorf("an over-cap image was inlined anyway: %s", body)
	}
	if !strings.Contains(body, info.Path) {
		t.Errorf("the request does not carry the attachment path %q: %s", info.Path, body)
	}
	if !strings.Contains(body, "内联上限") {
		t.Errorf("the note does not explain why the image was not inlined: %s", body)
	}
}

func TestChatSend_AudioIsNeverInlined(t *testing.T) {
	// A chat model cannot take audio as multimodal content, so an audio
	// attachment is always a path note, whatever the session's capabilities are.
	var (
		mu     sync.Mutex
		bodies []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	cm, err := llm.New(llm.Provider{
		Name: "local", BaseURL: upstream.URL, APIKey: "test", Model: "vision-model",
		DisableUsageRequest: true,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	h, _ := newMediaServer(t, mediaServerOpts{
		builder: &staticBuilder{provider: "local", model: "vision-model", cm: cm},
		seed: func(st store.Store) {
			seedModel(t, st, "local", "vision-model", store.CapChat, store.CapVision, store.CapAudioTranscribe)
		},
	})
	resp := h.postJSON(t, "/api/chat/sessions", map[string]string{"provider": "local", "model": "vision-model"})
	var created struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	decode(t, resp, &created)

	info := uploadAttachment(t, h, created.Session.ID, "voice.wav", wavBytes(t))
	sendMessage(t, h, created.Session.ID, "听听这个", []string{info.ID})

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("the provider was never called")
	}
	body := bodies[0]
	if strings.Contains(body, "audio_url") || strings.Contains(body, "input_audio") {
		t.Errorf("audio was inlined into the request: %s", body)
	}
	if !strings.Contains(body, info.Path) || !strings.Contains(body, transcribeToolName) {
		t.Errorf("the note does not point at %q for transcription: %s", info.Path, body)
	}
}

func TestChatSend_PersistedMessageCarriesTheAttachmentIds(t *testing.T) {
	h, _ := newMediaServer(t, mediaServerOpts{turns: [][]*schema.Message{{
		{Role: schema.Assistant, Content: "ok"},
	}}})
	id := createSession(t, h)
	first := uploadAttachment(t, h, id, "a.png", pngBytes(t, 4))
	second := uploadAttachment(t, h, id, "voice.wav", wavBytes(t))

	sendMessage(t, h, id, "两个附件", []string{first.ID, second.ID})

	var got struct {
		Messages []struct {
			Role        string `json:"role"`
			Content     string `json:"content"`
			Attachments string `json:"attachments"`
		} `json:"messages"`
	}
	h.getJSON(t, "/api/chat/sessions/"+id, http.StatusOK, &got)
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(got.Messages))
	}
	stored := got.Messages[0]
	if stored.Content != "两个附件" {
		t.Errorf("stored content = %q, want the user's own text", stored.Content)
	}
	if stored.Attachments == "" {
		t.Fatal("the persisted message carries no attachments, so a reload cannot render them")
	}
	// The convention matches tool_calls and usage: a JSON array inside a JSON
	// string, which the UI parses defensively.
	var ids []string
	if err := json.Unmarshal([]byte(stored.Attachments), &ids); err != nil {
		t.Fatalf("attachments = %q, want a JSON array: %v", stored.Attachments, err)
	}
	if len(ids) != 2 || ids[0] != first.ID || ids[1] != second.ID {
		t.Errorf("attachments = %v, want [%s %s]", ids, first.ID, second.ID)
	}
}

func TestChatStream_ToolCallInATurnWithAnAttachment(t *testing.T) {
	// A turn that carries an attachment must still be able to call a tool: the
	// message is now multimodal, and nothing in the streaming loop may depend on
	// it being plain text.
	var ran atomic.Bool
	tl := &stubTool{name: "clock", desc: "tells the time", run: func(context.Context, string) (string, error) {
		ran.Store(true)
		return "12:00", nil
	}}
	reg := tool.NewRegistry()
	if err := reg.Register(tl); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	h, _ := newMediaServer(t, mediaServerOpts{
		tools: reg,
		turns: [][]*schema.Message{
			{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				ID: "c1", Type: "function",
				Function: schema.FunctionCall{Name: "clock", Arguments: "{}"},
			}}}},
			{{Role: schema.Assistant, Content: "现在是 12:00"}},
		},
	})
	id := createSession(t, h)
	info := uploadAttachment(t, h, id, "shot.png", pngBytes(t, 4))

	events := sendMessage(t, h, id, "几点了，顺便看看这张图", []string{info.ID})

	if !ran.Load() {
		t.Error("the tool was never executed in a turn that had an attachment")
	}
	typeList := eventTypes(events)
	if _, ok := findEvent(events, "tool_call"); !ok {
		t.Errorf("no tool_call event: %v", typeList)
	}
	if _, ok := findEvent(events, "tool_result"); !ok {
		t.Errorf("no tool_result event: %v", typeList)
	}
	done, ok := findEvent(events, "done")
	if !ok {
		t.Fatalf("no done event: %v", typeList)
	}
	if done["text"] != "现在是 12:00" {
		t.Errorf("done text = %v", done["text"])
	}
	if typeList[len(typeList)-1] != "stream_end" {
		t.Errorf("last event = %q, want stream_end", typeList[len(typeList)-1])
	}
}

func TestAttachment_HumanBytes(t *testing.T) {
	// The note tells the user why a file was not inlined, and the size is part
	// of that explanation.
	cases := map[int64]string{
		0:          "0 字节",
		999:        "999 字节",
		2048:       "2 KiB",
		(3 << 20):  "3 MiB",
		(25 << 20): "25 MiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusError_CarriesItsMessage(t *testing.T) {
	err := statusErr(http.StatusUnsupportedMediaType, "不支持的附件类型 %q", "text/plain")
	if err.Error() != `不支持的附件类型 "text/plain"` {
		t.Errorf("Error() = %q", err.Error())
	}
	var se *statusError
	if !errors.As(err, &se) || se.status != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", se.status)
	}
}

func TestAttachment_SanitiseName(t *testing.T) {
	cases := map[string]string{
		"shot.png":               "shot.png",
		"/etc/passwd":            "passwd",
		`C:\Users\me\shot.png`:   "shot.png",
		"../shot.png":            "shot.png",
		"a\"b.png":               "a_b.png",
		"line\nbreak.png":        "linebreak.png",
		"":                       "attachment",
		"   ":                    "attachment",
		"../../":                 "attachment",
		strings.Repeat("x", 200): strings.Repeat("x", maxAttachmentName),
	}
	for in, want := range cases {
		if got := sanitiseAttachmentName(in); got != want {
			t.Errorf("sanitiseAttachmentName(%q) = %q, want %q", in, got, want)
		}
	}
}
