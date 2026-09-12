package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/hertz/pkg/app"

	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/workspace"
)

// Attachment limits.
//
// The two caps answer different questions. MaxAttachmentBytes is what a user is
// allowed to send; MaxInlineImageBytes is what may be base64-encoded into a
// model request on every turn, which is a much smaller number because the
// payload is inflated by a third and paid for as prompt tokens.
const (
	// MaxAttachmentBytes caps one uploaded file. 25 MiB takes a phone photo or a
	// few minutes of speech and stays far below anything that would be streamed
	// into memory: the handler enforces it while reading, not afterwards.
	MaxAttachmentBytes int64 = 25 << 20
	// MaxInlineImageBytes caps an image that is inlined into a model request as
	// a data URL. A larger image is not sent inline; the model is told its path
	// instead and can look at it with the describe_image tool.
	MaxInlineImageBytes int64 = 8 << 20
	// uploadFieldName is the multipart field the file must arrive in.
	uploadFieldName = "file"
	// attachmentURLPrefix is where an uploaded asset is served from.
	attachmentURLPrefix = "/api/chat/attachments/"
	// attachmentDir is the workspace directory uploads are stored under.
	attachmentDir = "media"
	// attachmentTempPattern names the intermediate file used by the atomic
	// upload. It is hidden and distinctive so a directory listing never mistakes
	// it for user content, and it lives in the destination directory so the
	// rename stays on one filesystem (a cross-device rename is not atomic).
	attachmentTempPattern = ".huan-agent-upload-*"
	// sniffBytes is how much of an upload is inspected before it is stored. It
	// matches what http.DetectContentType looks at.
	sniffBytes = 512
	// maxAttachmentName bounds the client-supplied file name this endpoint
	// echoes back, so a pathological name cannot bloat a response or a header.
	maxAttachmentName = 120
	// attachmentCacheControl keeps an immutable attachment in the browser cache
	// for a year: the bytes behind an id never change, and re-fetching a picture
	// on every render of a reloaded conversation is pure waste.
	attachmentCacheControl = "private, max-age=31536000, immutable"
)

// attachmentKind is how a stored file may be presented to a model.
type attachmentKind string

const (
	kindImage attachmentKind = store.MediaKindImage
	kindAudio attachmentKind = store.MediaKindAudio
)

// attachmentType is the canonical form of one accepted upload type.
type attachmentType struct {
	kind attachmentKind
	// mime is the canonical content type, recorded on the asset row and served
	// back by the download endpoint. It can differ from what was sniffed: the
	// sniffer calls a WAV "audio/wave", an Ogg "application/ogg" and a WebM
	// "video/webm", and the file is stored under the name this endpoint means.
	mime string
	// ext is the stored file's extension, derived from the same entry as the
	// content type so the two can never disagree.
	ext string
}

// attachmentTypes maps a sniffed content type onto how it is stored.
//
// Only these types are accepted. Anything else is refused with the detected type
// named in the error, because "unsupported file" leaves the user guessing
// whether the server rejected a PDF or a truncated image.
var attachmentTypes = map[string]attachmentType{
	"image/png":  {kind: kindImage, mime: "image/png", ext: ".png"},
	"image/jpeg": {kind: kindImage, mime: "image/jpeg", ext: ".jpg"},
	"image/gif":  {kind: kindImage, mime: "image/gif", ext: ".gif"},
	"image/webp": {kind: kindImage, mime: "image/webp", ext: ".webp"},

	// Audio. The first spelling of each row is what this build means; the others
	// are the aliases the sniffer (or another mime table) may produce for it.
	"audio/mpeg":      {kind: kindAudio, mime: "audio/mpeg", ext: ".mp3"},
	"audio/mp3":       {kind: kindAudio, mime: "audio/mpeg", ext: ".mp3"},
	"audio/mp4":       {kind: kindAudio, mime: "audio/mp4", ext: ".m4a"},
	"audio/m4a":       {kind: kindAudio, mime: "audio/mp4", ext: ".m4a"},
	"audio/x-m4a":     {kind: kindAudio, mime: "audio/mp4", ext: ".m4a"},
	"audio/wav":       {kind: kindAudio, mime: "audio/wav", ext: ".wav"},
	"audio/wave":      {kind: kindAudio, mime: "audio/wav", ext: ".wav"},
	"audio/x-wav":     {kind: kindAudio, mime: "audio/wav", ext: ".wav"},
	"audio/webm":      {kind: kindAudio, mime: "audio/webm", ext: ".webm"},
	"video/webm":      {kind: kindAudio, mime: "audio/webm", ext: ".webm"},
	"audio/ogg":       {kind: kindAudio, mime: "audio/ogg", ext: ".ogg"},
	"application/ogg": {kind: kindAudio, mime: "audio/ogg", ext: ".ogg"},
	"audio/flac":      {kind: kindAudio, mime: "audio/flac", ext: ".flac"},
	"audio/x-flac":    {kind: kindAudio, mime: "audio/flac", ext: ".flac"},
}

// Media tools the attachment note points the agent at. They are named here
// rather than discovered from the registry so the hint stays stable: the note is
// written into the transcript, and a tool that is missing is refused by the
// runner with a clearer message than the note could give.
const (
	describeImageTool  = "describe_image"
	transcribeToolName = "transcribe_audio"
)

// attachmentInfo is the upload response: everything a client needs to render the
// attachment and to reference it in the next message.
type attachmentInfo struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	MIME string `json:"mime"`
	// Bytes is the stored size, which is the size of what the client sent.
	Bytes int64 `json:"bytes"`
	// Name is the client's file name, sanitised and echoed back for display. It
	// is never part of the stored path.
	Name string `json:"name"`
	// Path is the file's path inside the workspace, which is what the media
	// tools take.
	Path string `json:"path"`
	// URL is where the bytes can be fetched back.
	URL string `json:"url"`
}

// statusError is an error that carries the HTTP status the handler should
// answer with. Everything the upload can refuse — no file, an unsupported type,
// an oversized body, a read-only workspace — is expressed as one of these, so
// none of them degrades into a generic 500.
type statusError struct {
	status int
	msg    string
}

// Error implements error.
func (e *statusError) Error() string { return e.msg }

// statusErr builds a statusError.
func statusErr(status int, format string, args ...any) error {
	return &statusError{status: status, msg: fmt.Sprintf(format, args...)}
}

// answerStatusError writes err as its own status when it carries one, and
// reports whether it did.
func (s *Server) answerStatusError(c *app.RequestContext, what string, err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		c.JSON(se.status, map[string]string{"error": se.msg})
		return true
	}
	s.fail(c, what, err)
	return false
}

// bufferedBodyLimit is the largest non-multipart request body the engine will
// hold in memory. It is the engine's own default, kept so that streaming request
// bodies does not widen what an ordinary endpoint will buffer.
const bufferedBodyLimit int64 = 4 << 20

// limitStreamedBody bounds a streamed request body for every request that is not
// an attachment upload.
//
// The engine streams request bodies, which is what lets a large attachment be
// read under the upload handler's own limit instead of being materialised whole
// first. Everything else reads its body through the binder, and the binder
// drains a streamed body completely: without this middleware an oversized POST —
// to an endpoint as open as /api/login — would be pulled into memory in full,
// where the engine's own cap would previously have refused it with a 413.
//
// The upload route is deliberately left alone: it is the one handler that reads
// the stream itself, under the attachment cap, which is what makes that cap apply
// while the bytes are read rather than afterwards.
func (s *Server) limitStreamedBody(ctx context.Context, c *app.RequestContext) {
	if !c.Request.IsBodyStream() || isAttachmentUpload(c) {
		c.Next(ctx)
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.BodyStream(), bufferedBodyLimit+1))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, map[string]string{"error": "reading the request body failed"})
		return
	}
	if int64(len(body)) > bufferedBodyLimit {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "request body is too large"})
		return
	}
	// Put the bytes back where every handler already reads them from, and let
	// the drained stream be closed by SetBody.
	c.Request.SetBody(body)
	c.Next(ctx)
}

// isAttachmentUpload reports whether a request targets an attachment upload.
//
// The route is recognised by its path rather than by its declared content type:
// a client can claim multipart/form-data on any request, and that claim must not
// become a way to hand an unbounded body to a handler that binds JSON. The match
// is on the path's shape so a deployment mounted under a URL prefix still works.
func isAttachmentUpload(c *app.RequestContext) bool {
	p := string(c.Request.URI().Path())
	return strings.Contains(p, "/chat/sessions/") && strings.HasSuffix(p, "/attachments")
}

// handleUploadAttachment stores one uploaded file for a session and records it
// as a media asset.
//
// The file is streamed: the bytes are sniffed from the head, written to a temp
// file in the destination directory while both the attachment cap and the
// workspace write limit are checked, and only then renamed into place. A refused
// upload therefore leaves no file behind, and a large one is never buffered
// whole in memory.
func (s *Server) handleUploadAttachment(ctx context.Context, c *app.RequestContext) {
	sessID := c.Param("id")
	sess, err := s.store.GetChatSession(ctx, sessID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The contract for this endpoint answers 400 rather than the 404 the
			// other session routes use.
			c.JSON(http.StatusBadRequest, map[string]string{"error": "session not found"})
			return
		}
		s.fail(c, "get chat session for upload", err)
		return
	}

	ws := s.chat.Workspace
	if ws == nil {
		// Refusing beats inventing a directory: an upload that landed outside
		// the workspace would be invisible to every tool the agent has.
		c.JSON(http.StatusForbidden, map[string]string{
			"error": "附件需要工作区：未配置 tools.workspace，无法保存上传的文件",
		})
		return
	}
	if ws.ReadOnly() {
		c.JSON(http.StatusForbidden, map[string]string{
			"error": "工作区为只读（tools.read_only），无法保存附件",
		})
		return
	}

	part, err := s.openUploadPart(c)
	if err != nil {
		s.answerStatusError(c, "store chat attachment", err)
		return
	}
	defer func() { _ = part.Close() }()

	asset, err := s.storeUpload(ctx, ws, sess.ID, part)
	if err != nil {
		s.answerStatusError(c, "store chat attachment", err)
		return
	}

	s.logger.Info("chat attachment stored",
		zapString("session", sess.ID),
		zapString("asset", asset.ID),
		zapString("kind", asset.Kind),
		zapString("mime", asset.MIME),
		zapString("path", asset.Path),
	)
	c.JSON(http.StatusOK, map[string]any{"attachment": attachmentInfo{
		ID:    asset.ID,
		Kind:  asset.Kind,
		MIME:  asset.MIME,
		Bytes: asset.Bytes,
		Name:  sanitiseAttachmentName(part.FileName()),
		Path:  asset.Path,
		URL:   attachmentURLPrefix + asset.ID,
	}})
}

// openUploadPart returns the multipart file part of an upload.
//
// It parses the body itself instead of using the engine's pre-parsed form so the
// limit is applied while the bytes are read: a 25 MiB upload must not become a
// 25 MiB allocation, and a part that never ends must not either. The reader is
// bounded by the attachment cap plus the multipart envelope, so an oversized
// body is cut off rather than read to the end.
func (s *Server) openUploadPart(c *app.RequestContext) (*multipart.Part, error) {
	boundary, err := multipartBoundary(c)
	if err != nil {
		return nil, err
	}
	body := c.Request.BodyStream()
	if body == nil {
		return nil, statusErr(http.StatusBadRequest, "请求体为空：需要 multipart/form-data 的 %q 字段", uploadFieldName)
	}
	// The envelope (boundaries, part headers) is small but real, so the outer
	// bound is deliberately a little above the file cap; the file cap itself is
	// enforced on the part, byte by byte.
	outer := s.maxAttachmentBytes() + multipartEnvelopeBytes
	limited := io.LimitReader(body, outer+1)
	mr := multipart.NewReader(limited, boundary)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, statusErr(http.StatusBadRequest, "请求里没有 %q 字段", uploadFieldName)
		}
		if err != nil {
			return nil, statusErr(http.StatusBadRequest, "解析 multipart 请求失败: %v", err)
		}
		if part.FormName() == uploadFieldName {
			return part, nil
		}
		// Skip anything else without materialising it.
		if _, err := io.Copy(io.Discard, io.LimitReader(part, outer)); err != nil {
			_ = part.Close()
			return nil, statusErr(http.StatusBadRequest, "读取 multipart 请求失败: %v", err)
		}
		_ = part.Close()
	}
}

// multipartBoundary extracts the multipart boundary from the request.
func multipartBoundary(c *app.RequestContext) (string, error) {
	contentType := string(c.GetHeader("Content-Type"))
	if contentType == "" {
		return "", statusErr(http.StatusBadRequest, "需要 multipart/form-data 请求（缺少 Content-Type）")
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", statusErr(http.StatusBadRequest, "无法解析 Content-Type %q: %v", contentType, err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return "", statusErr(http.StatusBadRequest, "需要 multipart/form-data 请求，收到 %q", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return "", statusErr(http.StatusBadRequest, "multipart 请求缺少 boundary")
	}
	return boundary, nil
}

// multipartEnvelopeBytes is the headroom the whole-body bound allows for the
// multipart envelope (boundaries and part headers) on top of the file cap.
const multipartEnvelopeBytes int64 = 1 << 20

// storeUpload streams part into the workspace and records the asset.
func (s *Server) storeUpload(ctx context.Context, ws *workspace.Workspace, sessionID string, part *multipart.Part) (store.MediaAsset, error) {
	head := make([]byte, sniffBytes)
	headLen, err := io.ReadFull(part, head)
	switch {
	case errors.Is(err, io.EOF):
		return store.MediaAsset{}, statusErr(http.StatusBadRequest, "上传的文件是空的")
	case errors.Is(err, io.ErrUnexpectedEOF), err == nil:
		// A file shorter than the sniff window is normal; headLen says how much
		// there is.
	default:
		return store.MediaAsset{}, statusErr(http.StatusBadRequest, "读取上传的文件失败: %v", err)
	}
	head = head[:headLen]

	// The type comes from the bytes, never from the client's Content-Type or the
	// file name: both are attacker-controlled and a wrong one decides whether the
	// file is inlined into a model request or served back as an image.
	detected := sniffAttachmentType(head)
	typ, ok := attachmentTypes[detected]
	if !ok {
		return store.MediaAsset{}, statusErr(http.StatusUnsupportedMediaType,
			"不支持的附件类型 %q：仅支持 PNG/JPEG/GIF/WebP 图片与 MP3/M4A/WAV/WebM/OGG/FLAC 音频", detected)
	}

	id, err := newAttachmentID()
	if err != nil {
		return store.MediaAsset{}, fmt.Errorf("generate attachment id: %w", err)
	}
	now := time.Now().UTC()
	// The path is built from the generated id and the sniffed type only. The
	// client's file name is never a component of it, so nothing a client sends
	// can influence where the file lands.
	rel := path.Join(attachmentDir, now.Format("2006"), now.Format("01"), id+typ.ext)

	// ResolveForWrite applies the workspace's read-only mode and confinement
	// before any syscall, so a refused upload creates neither the file nor its
	// parent directories.
	abs, err := ws.ResolveForWrite(rel, 0)
	if err != nil {
		if errors.Is(err, workspace.ErrReadOnly) {
			return store.MediaAsset{}, statusErr(http.StatusForbidden, "工作区为只读，无法保存附件: %v", err)
		}
		return store.MediaAsset{}, fmt.Errorf("resolve attachment path %s: %w", rel, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return store.MediaAsset{}, fmt.Errorf("create attachment directory for %s: %w", rel, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(abs), attachmentTempPattern)
	if err != nil {
		return store.MediaAsset{}, fmt.Errorf("create temp file for %s: %w", rel, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: after a successful rename there is nothing left at
	// tmpName, so this only removes a file from a refused upload.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	// The copy is what enforces the caps, and it reads one buffer at a time.
	cap := s.maxAttachmentBytes()
	digest := sha256.New()
	reader := io.LimitReader(io.MultiReader(bytes.NewReader(head), part), cap+1)
	size, err := io.Copy(io.MultiWriter(tmp, digest), reader)
	if err != nil {
		return store.MediaAsset{}, statusErr(http.StatusBadRequest, "写入附件失败: %v", err)
	}
	if size > cap {
		return store.MediaAsset{}, statusErr(http.StatusRequestEntityTooLarge,
			"附件超过 %d 字节的上限（当前 %d 字节）", cap, size)
	}
	// The workspace's write limit deliberately does NOT apply here: it bounds
	// what the agent's tools may write, and it is sized for source files (4 MiB
	// by default), which would refuse an ordinary photo. An upload is the
	// operator's own action and is bounded by the attachment cap above.
	// Read-only mode and confinement still apply.
	if _, err := ws.ResolveForUpload(rel); err != nil {
		return store.MediaAsset{}, statusErr(http.StatusRequestEntityTooLarge, "附件无法写入工作区: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		return store.MediaAsset{}, fmt.Errorf("sync attachment %s: %w", rel, err)
	}
	if err := tmp.Close(); err != nil {
		return store.MediaAsset{}, fmt.Errorf("close attachment %s: %w", rel, err)
	}

	sum := hex.EncodeToString(digest.Sum(nil))
	// An identical file re-uploaded into the same conversation is the same
	// attachment: reusing the row keeps the ids a client already holds valid and
	// avoids a second copy of the bytes.
	if existing, ok := s.reuseIdenticalAsset(ctx, ws, sessionID, sum); ok {
		return existing, nil
	}

	if err := os.Rename(tmpName, abs); err != nil {
		return store.MediaAsset{}, fmt.Errorf("rename attachment into place %s: %w", rel, err)
	}
	asset := store.MediaAsset{
		ID:        id,
		SessionID: sessionID,
		Kind:      string(typ.kind),
		Path:      rel,
		MIME:      typ.mime,
		Bytes:     size,
		SHA256:    sum,
	}
	if err := s.store.CreateMediaAsset(ctx, asset); err != nil {
		// The row is what makes the file reachable, so a file without one is
		// garbage; remove it rather than leave it to be found by a listing.
		_ = os.Remove(abs)
		return store.MediaAsset{}, fmt.Errorf("record media asset: %w", err)
	}
	return asset, nil
}

// reuseIdenticalAsset returns an already-stored asset with the same bytes, when
// its file is still there. A missing file means the previous row is stale, and a
// new file is written instead.
func (s *Server) reuseIdenticalAsset(ctx context.Context, ws *workspace.Workspace, sessionID, digest string) (store.MediaAsset, bool) {
	existing, err := s.store.FindMediaAssetBySHA256(ctx, sessionID, digest)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.logger.Warn("look up identical attachment failed", zapError(err))
		}
		return store.MediaAsset{}, false
	}
	abs, err := ws.Resolve(existing.Path)
	if err != nil {
		return store.MediaAsset{}, false
	}
	if info, err := os.Stat(abs); err != nil || info.IsDir() {
		return store.MediaAsset{}, false
	}
	return existing, true
}

// newAttachmentID returns a random hex id for an asset. It is the only thing the
// stored path is built from, so it must not be guessable or collide.
func newAttachmentID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// handleGetAttachment serves a stored attachment's bytes.
//
// The stored path is data, not a path: it is put through the workspace sandbox
// before it is opened, so a row that somehow holds "../../etc/passwd" cannot
// read outside the workspace, and a file that has since been deleted is reported
// as missing rather than as a server error.
func (s *Server) handleGetAttachment(ctx context.Context, c *app.RequestContext) {
	asset, err := s.store.GetMediaAsset(ctx, c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "attachment not found"})
			return
		}
		s.fail(c, "get media asset", err)
		return
	}
	if s.chat.Workspace == nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": "attachment file is not available"})
		return
	}
	abs, err := s.chat.Workspace.Resolve(asset.Path)
	if err != nil {
		// A path that escapes the workspace is a corrupted row, not a client
		// error; log it so it is visible, and answer 404 so nothing is leaked.
		s.logger.Warn("attachment path rejected",
			zapString("asset", asset.ID), zapString("path", asset.Path), zapError(err))
		c.JSON(http.StatusNotFound, map[string]string{"error": "attachment file is not available"})
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, map[string]string{"error": "attachment file is not available"})
			return
		}
		s.fail(c, "open media asset", err)
		return
	}
	// The size comes from the open handle rather than from a separate stat, so
	// the Content-Length always describes the bytes that are about to be sent.
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		_ = f.Close()
		c.JSON(http.StatusNotFound, map[string]string{"error": "attachment file is not available"})
		return
	}

	// Own the content type rather than letting it be derived from the stored
	// extension, and refuse to echo anything the accept table does not know.
	contentType := asset.MIME
	if _, ok := attachmentTypes[contentType]; !ok {
		contentType = "application/octet-stream"
	}
	c.Response.Header.Set("Content-Type", contentType)
	c.Response.Header.Set("Content-Disposition", contentDisposition(path.Base(asset.Path)))
	c.Response.Header.Set("Cache-Control", attachmentCacheControl)
	// Uploaded bytes are attacker-chosen, so stop a browser from re-sniffing
	// them into something else.
	c.Response.Header.Set("X-Content-Type-Options", "nosniff")
	// The body is streamed straight from the file: a 25 MiB attachment should
	// not be read into memory to be served, and the engine closes the stream
	// once the body is written (which happens after this handler returns, so the
	// file must not be closed here).
	c.SetBodyStream(f, int(info.Size()))
}

// sniffAttachmentType reports the content type of the uploaded bytes.
//
// http.DetectContentType is the first answer — it implements the WHATWG sniffing
// algorithm and, unlike the request headers and the file name, cannot be forged
// by the client. It does not cover every container this endpoint accepts, so the
// gaps are closed by explicit signature checks: FLAC, an M4A whose sniffed name
// would be video/mp4, and an MP3 without an ID3 tag all come back from the
// sniffer as application/octet-stream or worse.
func sniffAttachmentType(head []byte) string {
	if sig := sniffExtraSignature(head); sig != "" {
		return sig
	}
	detected := http.DetectContentType(head)
	// The sniffer appends a charset to text types; the accept table keys on the
	// base type.
	if i := strings.IndexByte(detected, ';'); i >= 0 {
		detected = strings.TrimSpace(detected[:i])
	}
	if _, ok := attachmentTypes[detected]; ok {
		return detected
	}
	// Report the full detected value, parameters included: it is what tells the
	// user which of their files was refused.
	return http.DetectContentType(head)
}

// sniffExtraSignature recognises the containers the WHATWG sniffer names
// differently or not at all, from their own magic bytes.
func sniffExtraSignature(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("fLaC")):
		return "audio/flac"
	case isMpegFrameSync(head):
		return "audio/mpeg"
	case mp4AudioBrand(head):
		return "audio/mp4"
	}
	return ""
}

// isMpegFrameSync reports whether the bytes start with an MPEG audio frame
// header. An MP3 without an ID3 tag is common and is not a format the WHATWG
// table knows, so it would otherwise be refused as a binary blob. A JPEG starts
// with FF D8 (which is not a frame sync), so the two cannot be confused.
func isMpegFrameSync(head []byte) bool {
	if len(head) < 2 {
		return false
	}
	// Eleven set bits: a sync word, then the MPEG version and layer bits must
	// not be the reserved "01" combination.
	if head[0] != 0xFF || head[1]&0xE0 != 0xE0 {
		return false
	}
	return head[1]&0x18 != 0x08
}

// mp4AudioBrand reports whether the file is an ISO base media file whose major
// brand says it is audio (an M4A voice memo, for instance).
//
// The brand is read rather than pattern-matched across the whole compatible
// brand list, because a video MP4 lists "mp4" there too and would otherwise be
// accepted as audio.
func mp4AudioBrand(head []byte) bool {
	if len(head) < 12 || !bytes.Equal(head[4:8], []byte("ftyp")) {
		return false
	}
	switch string(head[8:12]) {
	case "M4A ", "M4B ", "M4P ":
		return true
	default:
		return false
	}
}

// sanitiseAttachmentName cleans a client-supplied file name for display.
//
// The name is never used to build the stored path — that is derived from the
// generated id and the sniffed type — but it is echoed in a JSON response and in
// a Content-Disposition header, so directory components and control characters
// are stripped rather than trusted.
func sanitiseAttachmentName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r == utf8.RuneError:
			return -1
		case r < 0x20 || r == 0x7f:
			return -1
		case r == '"' || r == '\\':
			// Quoting hazards in the Content-Disposition header.
			return '_'
		default:
			return r
		}
	}, name)
	name = strings.TrimSpace(name)
	if name == "" {
		return "attachment"
	}
	if len(name) > maxAttachmentName {
		cut := maxAttachmentName
		for cut > 0 && !utf8Start(name[cut]) {
			cut--
		}
		name = name[:cut]
	}
	return name
}

// contentDisposition builds an inline disposition for a stored file name.
func contentDisposition(name string) string {
	return `inline; filename="` + sanitiseAttachmentName(name) + `"`
}

// ------------------------------------------------------- sending with files --

// attachmentNoteEntry is one file the model is told about by path.
type attachmentNoteEntry struct {
	asset store.MediaAsset
	// reason says, in the user's language, why this file is not inlined.
	reason string
}

// attachmentNote renders the machine-readable note appended to a message whose
// files are not inlined.
//
// It is written in Simplified Chinese to match the rest of the user-facing text
// this project renders, and it is bracketed as a system note so it is not
// mistaken for something the user typed: the note ends up in the transcript the
// model sees, and a model that believes the user wrote "the file is at media/…"
// may answer as if the user were making a claim about a path instead of being
// told where a file is. Each path is on its own line, bare, so it can be lifted
// straight into a tool call.
func attachmentNote(entries []attachmentNoteEntry) string {
	var b strings.Builder
	b.WriteString("[系统提示：本条消息由用户附带了文件，文件已保存在工作区（workspace）以下路径，")
	fmt.Fprintf(&b, "可用 %s 查看图片、%s 转写音频，路径可原样作为工具参数：]", describeImageTool, transcribeToolName)
	for _, e := range entries {
		fmt.Fprintf(&b, "\n- %s（%s，%s，%d 字节）", e.asset.Path, e.asset.MIME, e.reason, e.asset.Bytes)
	}
	return b.String()
}

// defaultAttachmentText stands in for a message that is only an attachment.
//
// A message with no text at all cannot be sent: the user message is the turn's
// text, and both the model and the transcript need something to show. The
// placeholder is what gets persisted, so a reloaded conversation reads sensibly.
const defaultAttachmentText = "（附件）"

// hasAnyID reports whether an attachment list holds at least one usable id.
func hasAnyID(ids []string) bool {
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			return true
		}
	}
	return false
}

// loadTurnAttachments validates the referenced ids and decides how each file is
// presented to the model for this turn.
//
// An id from another session is refused rather than ignored: accepting it would
// let one conversation pull in a file another one owns, and the upload endpoint
// already recorded which session each asset belongs to.
func (s *Server) loadTurnAttachments(ctx context.Context, sess store.ChatSession, ids []string) (inline []schema.MessageInputPart, notes []attachmentNoteEntry, all []string, err error) {
	if len(ids) == 0 {
		return nil, nil, nil, nil
	}
	ws := s.chat.Workspace
	vision := s.sessionSupportsVision(ctx, sess)
	inlineCap := s.maxInlineImageBytes()

	seen := make(map[string]bool, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		asset, err := s.store.GetMediaAsset(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil, nil, statusErr(http.StatusBadRequest, "附件不存在: %s", id)
			}
			return nil, nil, nil, fmt.Errorf("load media asset %s: %w", id, err)
		}
		if asset.SessionID != sess.ID {
			return nil, nil, nil, statusErr(http.StatusBadRequest,
				"附件 %s 不属于当前会话，无法发送", id)
		}
		all = append(all, asset.ID)

		abs, size, err := s.attachmentFile(ws, asset)
		if err != nil {
			return nil, nil, nil, err
		}
		if vision && asset.Kind == store.MediaKindImage {
			if size > inlineCap {
				notes = append(notes, attachmentNoteEntry{asset: asset,
					reason: fmt.Sprintf("图片超过 %s 的内联上限，未直接发送给模型", humanBytes(inlineCap))})
				continue
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("read attachment %s: %w", asset.Path, err)
			}
			inline = append(inline, imageInputPart(asset.MIME, data))
			continue
		}
		notes = append(notes, attachmentNoteEntry{asset: asset, reason: notInlinedReason(asset, vision)})
	}
	return inline, notes, all, nil
}

// notInlinedReason explains why a file is only referenced by path.
func notInlinedReason(asset store.MediaAsset, vision bool) string {
	switch {
	case asset.Kind == store.MediaKindAudio:
		return "音频不能内联到对话请求，需要工具转写"
	case !vision:
		return "当前会话的模型未声明 vision 能力，图片未直接发送"
	default:
		return "未内联"
	}
}

// attachmentFile resolves a stored asset to a readable path, verifying that it
// is still inside the workspace.
func (s *Server) attachmentFile(ws *workspace.Workspace, asset store.MediaAsset) (string, int64, error) {
	if ws == nil {
		return "", 0, statusErr(http.StatusBadRequest, "附件 %s 无法读取：未配置工作区", asset.ID)
	}
	abs, err := ws.Resolve(asset.Path)
	if err != nil {
		s.logger.Warn("attachment path rejected",
			zapString("asset", asset.ID), zapString("path", asset.Path), zapError(err))
		return "", 0, statusErr(http.StatusBadRequest, "附件 %s 的文件不在工作区内，无法读取", asset.Path)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, statusErr(http.StatusBadRequest, "附件文件已不存在: %s", asset.Path)
		}
		return "", 0, fmt.Errorf("stat attachment %s: %w", asset.Path, err)
	}
	if info.IsDir() {
		return "", 0, statusErr(http.StatusBadRequest, "附件路径 %s 是目录，不是文件", asset.Path)
	}
	return abs, info.Size(), nil
}

// imageInputPart builds the model-facing image part for an attachment.
//
// The bytes travel as base64 with their MIME type rather than as a URL: the file
// lives in the workspace and has no address a provider could fetch.
func imageInputPart(mimeType string, data []byte) schema.MessageInputPart {
	encoded := base64.StdEncoding.EncodeToString(data)
	return schema.MessageInputPart{
		Type: schema.ChatMessagePartTypeImageURL,
		Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{
				Base64Data: &encoded,
				MIMEType:   mimeType,
			},
			Detail: schema.ImageURLDetailAuto,
		},
	}
}

// buildUserMessage turns the turn's text and its inlined parts into the message
// sent to the model.
func buildUserMessage(text string, inline []schema.MessageInputPart) *schema.Message {
	msg := &schema.Message{Role: schema.User, Content: text}
	if len(inline) == 0 {
		return msg
	}
	// The text is part of the multimodal content: a message that carried both
	// Content and parts is rejected by the OpenAI-compatible APIs, and the text
	// has to stay in order with the images it introduces. Content is still set,
	// because it is what tracing and the transcript read.
	parts := make([]schema.MessageInputPart, 0, len(inline)+1)
	parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: text})
	parts = append(parts, inline...)
	msg.UserInputMultiContent = parts
	return msg
}

// attachmentsJSON encodes the asset ids recorded on a message.
//
// The wire shape mirrors ToolCalls and UsageJSON: a JSON array carried in a text
// column and therefore serialised as a JSON *string*, which the UI already
// parses defensively for its siblings.
func attachmentsJSON(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	b, err := json.Marshal(ids)
	if err != nil {
		// []string cannot fail to marshal; keep the column empty rather than
		// writing a half-formed value.
		return ""
	}
	return string(b)
}

// humanBytes renders a byte count for a message a user reads.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	default:
		return fmt.Sprintf("%d 字节", n)
	}
}
