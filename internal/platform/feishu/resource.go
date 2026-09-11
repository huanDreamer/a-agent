package feishu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// DefaultMaxResourceBytes caps a single downloaded attachment (32 MiB). Feishu
// itself caps uploads at 30 MiB for most message types, so this is a safety
// net rather than a normal limit.
const DefaultMaxResourceBytes int64 = 32 << 20

// ResourceDownloader downloads an attachment referenced by an inbound message
// and returns the local file path it was written to.
type ResourceDownloader interface {
	Download(ctx context.Context, messageID string, r Resource) (string, error)
}

// resourceGetter is the minimal lark API surface for downloading message
// resources. The SDK's *im.messageResource satisfies it.
type resourceGetter interface {
	Get(ctx context.Context, req *im.GetMessageResourceReq, options ...larkcore.RequestOptionFunc) (*im.GetMessageResourceResp, error)
}

// larkDownloader writes message resources to a local directory.
type larkDownloader struct {
	get      resourceGetter
	dir      string
	maxBytes int64
}

// RealDownloader builds a ResourceDownloader backed by a real lark client that
// stores attachments under dir. dir is created if missing.
func RealDownloader(appID, appSecret, domain, dir string) (ResourceDownloader, error) {
	if appID == "" {
		return nil, errors.New("feishu: app_id is required")
	}
	if dir == "" {
		return nil, errors.New("feishu: download dir is required")
	}
	if domain == "" {
		domain = lark.FeishuBaseUrl
	}
	opts := []lark.ClientOptionFunc{}
	if domain != lark.FeishuBaseUrl {
		opts = append(opts, lark.WithOpenBaseUrl(domain))
	}
	cli := lark.NewClient(appID, appSecret, opts...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("feishu: create download dir: %w", err)
	}
	return &larkDownloader{get: cli.Im.MessageResource, dir: dir, maxBytes: DefaultMaxResourceBytes}, nil
}

// NewResourceDownloader returns a downloader over the given lark API surface.
// It is exported for tests and for callers that already own a lark client.
func NewResourceDownloader(get resourceGetter, dir string, maxBytes int64) ResourceDownloader {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResourceBytes
	}
	return &larkDownloader{get: get, dir: dir, maxBytes: maxBytes}
}

// Download fetches the attachment and writes it into the download directory.
func (d *larkDownloader) Download(ctx context.Context, messageID string, r Resource) (string, error) {
	if messageID == "" {
		return "", errors.New("feishu: message_id is required to download a resource")
	}
	if r.Key == "" {
		return "", errors.New("feishu: resource key is required to download a resource")
	}
	if d.get == nil {
		return "", errors.New("feishu: no resource API configured")
	}

	req := im.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(r.Key).
		Type(resourceAPIType(r.Kind)).
		Build()
	resp, err := d.get.Get(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: download resource: %w", err)
	}
	if resp == nil {
		return "", errors.New("feishu: download resource: empty response")
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: download resource: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.File == nil {
		return "", errors.New("feishu: download resource: response has no body")
	}

	name := resourceFileName(r, resp.FileName)
	if err := os.MkdirAll(d.dir, 0o755); err != nil {
		return "", fmt.Errorf("feishu: create download dir: %w", err)
	}
	path := filepath.Join(d.dir, name)

	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("feishu: create resource file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Read one byte past the cap so an oversized body is detected rather than
	// silently truncated.
	written, err := io.Copy(f, io.LimitReader(resp.File, d.maxBytes+1))
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("feishu: write resource file: %w", err)
	}
	if written > d.maxBytes {
		_ = os.Remove(path)
		return "", fmt.Errorf("feishu: resource %q exceeds the %d byte limit", name, d.maxBytes)
	}
	return path, nil
}

// resourceAPIType maps a kind to the value the Feishu API expects.
func resourceAPIType(k ResourceKind) string {
	if k == ResourceImage {
		return "image"
	}
	return "file"
}

// resourceFileName builds a safe, collision-free local file name.
//
// The name is always prefixed with the sanitized resource key, which is unique
// per attachment, so two files a user sends in a row can never overwrite each
// other. The human-readable part (server-provided name, else the original file
// name) is appended when available, and an extension is inferred from that name
// (falling back to a per-kind default, since Feishu often omits it for images).
func resourceFileName(r Resource, serverName string) string {
	human := sanitizeFileName(serverName)
	if human == "" {
		human = sanitizeFileName(r.Name)
	}

	ext := filepath.Ext(human)
	if ext == "" {
		ext = defaultExtension(r.Kind)
	}
	stem := strings.TrimSuffix(human, filepath.Ext(human))
	if stem == "" {
		stem = "attachment"
	}

	key := sanitizeFileName(r.Key)
	if key == "" {
		key = "resource"
	}
	if len(key) > 64 {
		key = key[len(key)-64:]
	}
	return key + "_" + stem + ext
}

// defaultExtension returns a sensible extension per kind, used when Feishu does
// not provide a file name.
func defaultExtension(k ResourceKind) string {
	switch k {
	case ResourceImage, ResourceSticker:
		return ".jpg"
	case ResourceAudio:
		return ".opus"
	case ResourceMedia:
		return ".mp4"
	default:
		return ".bin"
	}
}

// sanitizeFileName strips directory separators and control characters so a
// remote name can never escape the download directory or create a hidden file.
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|':
			return '_'
		default:
			return r
		}
	}, name)
	name = strings.TrimLeft(name, ".")
	name = strings.TrimSpace(name)
	if len(name) > 180 {
		name = name[len(name)-180:]
	}
	return name
}
