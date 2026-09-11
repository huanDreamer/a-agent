package feishu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	im "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// fakeResourceGetter is a stub lark resource API.
type fakeResourceGetter struct {
	resp  *im.GetMessageResourceResp
	err   error
	calls int
}

func (f *fakeResourceGetter) Get(_ context.Context, _ *im.GetMessageResourceReq, _ ...larkcore.RequestOptionFunc) (*im.GetMessageResourceResp, error) {
	f.calls++
	return f.resp, f.err
}

// okResp builds a successful resource response with the given body and name.
func okResp(body, name string) *im.GetMessageResourceResp {
	return &im.GetMessageResourceResp{
		CodeError: larkcore.CodeError{Code: 0, Msg: "ok"},
		File:      strings.NewReader(body),
		FileName:  name,
	}
}

// msgEvent builds a minimal P2 message event with the given type and content.
func msgEvent(msgType, content string) *im.P2MessageReceiveV1 {
	return &im.P2MessageReceiveV1{Event: &im.P2MessageReceiveV1Data{
		Message: &im.EventMessage{
			MessageId:   strp("om_1"),
			ChatId:      strp("oc_1"),
			ChatType:    strp("p2p"),
			MessageType: strp(msgType),
			Content:     strp(content),
		},
		Sender: &im.EventSender{SenderId: &im.UserId{OpenId: strp("ou_1")}},
	}}
}

func TestParseInbound_Resources(t *testing.T) {
	tests := []struct {
		name     string
		msgType  string
		content  string
		wantKind ResourceKind
		wantKey  string
		wantName string
		wantDur  int
	}{
		{"image", "image", `{"image_key":"img_v3_abc"}`, ResourceImage, "img_v3_abc", "", 0},
		{"file", "file", `{"file_key":"file_v3_x","file_name":"report.pdf"}`, ResourceFile, "file_v3_x", "report.pdf", 0},
		{"audio", "audio", `{"file_key":"file_v3_a","duration":3200}`, ResourceAudio, "file_v3_a", "", 3200},
		{"media", "media", `{"file_key":"file_v3_m","file_name":"clip.mp4","duration":8000}`, ResourceMedia, "file_v3_m", "clip.mp4", 8000},
		{"sticker", "sticker", `{"file_key":"file_v3_s"}`, ResourceSticker, "file_v3_s", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := parseInboundMessage(msgEvent(tc.msgType, tc.content))
			if !in.Handled() {
				t.Fatalf("%s message should be handled", tc.msgType)
			}
			if !in.HasResources() || len(in.Resources) != 1 {
				t.Fatalf("Resources = %+v, want exactly one", in.Resources)
			}
			r := in.Resources[0]
			if r.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", r.Kind, tc.wantKind)
			}
			if r.Key != tc.wantKey {
				t.Errorf("Key = %q, want %q", r.Key, tc.wantKey)
			}
			if r.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", r.Name, tc.wantName)
			}
			if r.Duration != tc.wantDur {
				t.Errorf("Duration = %d, want %d", r.Duration, tc.wantDur)
			}
		})
	}
}

func TestParseInbound_MalformedContent(t *testing.T) {
	for _, mt := range []string{"image", "file", "audio", "media", "sticker", "post", "text"} {
		t.Run(mt, func(t *testing.T) {
			in := parseInboundMessage(msgEvent(mt, "not json at all"))
			if in.Text != "" || in.HasResources() {
				t.Errorf("malformed %s content should yield empty body, got %+v", mt, in)
			}
		})
	}
	// file content missing the key is not a usable attachment
	in := parseInboundMessage(msgEvent("file", `{"file_name":"x.pdf"}`))
	if in.HasResources() {
		t.Errorf("file without file_key should not produce a resource, got %+v", in.Resources)
	}
}

func TestParseInbound_Post(t *testing.T) {
	content := `{
	  "title": "周报",
	  "content": [
	    [
	      {"tag":"text","text":"完成 "},
	      {"tag":"a","text":"文档","href":"https://example.com"},
	      {"tag":"at","user_name":"张三"}
	    ],
	    [{"tag":"img","image_key":"img_v3_p1"}],
	    [{"tag":"media","file_name":"demo.mp4"}]
	  ]
	}`
	in := parseInboundMessage(msgEvent("post", content))

	if !in.IsPost() {
		t.Fatal("IsPost should be true")
	}
	if in.Title != "周报" {
		t.Errorf("Title = %q, want 周报", in.Title)
	}
	for _, want := range []string{"完成", "[文档](https://example.com)", "@张三", "[图片]", "[视频](demo.mp4)"} {
		if !strings.Contains(in.Text, want) {
			t.Errorf("Text %q missing %q", in.Text, want)
		}
	}
	if len(in.Resources) != 1 || in.Resources[0].Kind != ResourceImage || in.Resources[0].Key != "img_v3_p1" {
		t.Errorf("post inline image not collected: %+v", in.Resources)
	}
}

func TestParseInbound_PostEmptyParagraphs(t *testing.T) {
	in := parseInboundMessage(msgEvent("post", `{"content":[[{"tag":"text","text":"  "}],[{"tag":"text","text":"real"}]]}`))
	if in.Text != "real" {
		t.Errorf("Text = %q, want %q (blank paragraphs dropped)", in.Text, "real")
	}
}

func TestInbound_Handled(t *testing.T) {
	tests := []struct {
		name string
		in   Inbound
		want bool
	}{
		{"text", Inbound{MsgType: "text"}, true},
		{"post", Inbound{MsgType: "post"}, true},
		{"image", Inbound{MsgType: "image", Resources: []Resource{{Kind: ResourceImage}}}, true},
		{"unknown type without resources", Inbound{MsgType: "recall"}, false},
		{"unknown type with resources", Inbound{MsgType: "weird", Resources: []Resource{{Kind: ResourceFile}}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Handled(); got != tc.want {
				t.Errorf("Handled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInbound_PromptText(t *testing.T) {
	tests := []struct {
		name string
		in   Inbound
		want string
	}{
		{"text trimmed", Inbound{MsgType: "text", Text: "  hi  "}, "hi"},
		{"post with title", Inbound{MsgType: "post", Title: "T", Text: "body"}, "T\nbody"},
		{"post without title", Inbound{MsgType: "post", Text: "body"}, "body"},
		{"image", Inbound{MsgType: "image", Resources: []Resource{{Kind: ResourceImage}}}, "[图片]"},
		{"file named", Inbound{MsgType: "file", Resources: []Resource{{Kind: ResourceFile, Name: "a.pdf"}}}, "[文件: a.pdf]"},
		{"file unnamed", Inbound{MsgType: "file", Resources: []Resource{{Kind: ResourceFile}}}, "[文件]"},
		{"audio", Inbound{MsgType: "audio", Resources: []Resource{{Kind: ResourceAudio, Duration: 3200}}}, "[语音 3s]"},
		{"media named", Inbound{MsgType: "media", Resources: []Resource{{Kind: ResourceMedia, Name: "v.mp4"}}}, "[视频: v.mp4]"},
		{"sticker", Inbound{MsgType: "sticker", Resources: []Resource{{Kind: ResourceSticker}}}, "[表情]"},
		{"unknown kind", Inbound{MsgType: "x", Resources: []Resource{{Kind: "other"}}}, "[附件]"},
		{"mixed", Inbound{MsgType: "x", Resources: []Resource{{Kind: ResourceImage}, {Kind: ResourceFile, Name: "a.txt"}}}, "[图片] [文件: a.txt]"},
		{"nothing", Inbound{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.PromptText(); got != tc.want {
				t.Errorf("PromptText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeFileName(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "report.pdf", "report.pdf"},
		{"path traversal", "../../etc/passwd", "_.._etc_passwd"},
		{"absolute", "/etc/passwd", "_etc_passwd"},
		{"windows sep", `a\b.txt`, "a_b.txt"},
		{"colon and star", "a:b*c?.txt", "a_b_c_.txt"},
		{"leading dot", "...hidden", "hidden"},
		{"control chars", "a\x00b\x1fc", "abc"},
		{"trims space", "  x.txt  ", "x.txt"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeFileName(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeFileName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Security property: the result can never point outside its
			// directory or hide itself.
			if strings.ContainsAny(got, `/\`) {
				t.Errorf("sanitizeFileName(%q) = %q still contains a path separator", tc.in, got)
			}
			if strings.HasPrefix(got, ".") && got != "" {
				t.Errorf("sanitizeFileName(%q) = %q still starts with a dot", tc.in, got)
			}
		})
	}
}

func TestResourceFileName(t *testing.T) {
	tests := []struct {
		name       string
		r          Resource
		serverName string
		want       string
	}{
		{"server name wins", Resource{Kind: ResourceFile, Key: "k1", Name: "orig.txt"}, "server.pdf", "k1_server.pdf"},
		{"falls back to original name", Resource{Kind: ResourceFile, Key: "k2", Name: "orig.txt"}, "", "k2_orig.txt"},
		{"image gets default ext", Resource{Kind: ResourceImage, Key: "img1"}, "", "img1_attachment.jpg"},
		{"audio gets opus", Resource{Kind: ResourceAudio, Key: "a1"}, "", "a1_attachment.opus"},
		{"media gets mp4", Resource{Kind: ResourceMedia, Key: "m1"}, "", "m1_attachment.mp4"},
		{"sticker gets jpg", Resource{Kind: ResourceSticker, Key: "s1"}, "", "s1_attachment.jpg"},
		{"unknown kind gets bin", Resource{Kind: "zzz", Key: "u1"}, "", "u1_attachment.bin"},
		{"name without ext keeps human part", Resource{Kind: ResourceFile, Key: "k3", Name: "noext"}, "", "k3_noext.bin"},
		{"hostile names are neutralised", Resource{Kind: ResourceFile, Key: "k4", Name: "../../evil.sh"}, "", "k4__.._evil.sh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resourceFileName(tc.r, tc.serverName)
			if got != tc.want {
				t.Errorf("resourceFileName() = %q, want %q", got, tc.want)
			}
			// The produced name must stay a single path element so joining it
			// onto the download dir can never escape that dir.
			if filepath.Base(got) != got {
				t.Errorf("resourceFileName() = %q is not a bare file name", got)
			}
		})
	}
}

func TestResourceAPIType(t *testing.T) {
	if got := resourceAPIType(ResourceImage); got != "image" {
		t.Errorf("image → %q, want image", got)
	}
	for _, k := range []ResourceKind{ResourceFile, ResourceAudio, ResourceMedia, ResourceSticker, "other"} {
		if got := resourceAPIType(k); got != "file" {
			t.Errorf("%s → %q, want file", k, got)
		}
	}
}

func TestDownloader_Success(t *testing.T) {
	dir := t.TempDir()
	get := &fakeResourceGetter{resp: okResp("file-bytes", "report.pdf")}
	d := NewResourceDownloader(get, dir, 0)

	path, err := d.Download(context.Background(), "om_1", Resource{Kind: ResourceFile, Key: "k1"})
	if err != nil {
		t.Fatalf("Download error = %v", err)
	}
	if get.calls != 1 {
		t.Errorf("get called %d times, want 1", get.calls)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("path %q not inside %q", path, dir)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "file-bytes" {
		t.Errorf("content = %q, want file-bytes", got)
	}
}

func TestDownloader_CreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "media")
	d := NewResourceDownloader(&fakeResourceGetter{resp: okResp("x", "a.png")}, dir, 0)

	path, err := d.Download(context.Background(), "om_1", Resource{Kind: ResourceImage, Key: "k"})
	if err != nil {
		t.Fatalf("Download error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestDownloader_Errors(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing message id", func(t *testing.T) {
		d := NewResourceDownloader(&fakeResourceGetter{resp: okResp("x", "a")}, dir, 0)
		if _, err := d.Download(context.Background(), "", Resource{Key: "k"}); err == nil {
			t.Error("want error for empty message id")
		}
	})
	t.Run("missing key", func(t *testing.T) {
		d := NewResourceDownloader(&fakeResourceGetter{resp: okResp("x", "a")}, dir, 0)
		if _, err := d.Download(context.Background(), "om_1", Resource{}); err == nil {
			t.Error("want error for empty resource key")
		}
	})
	t.Run("nil getter", func(t *testing.T) {
		d := &larkDownloader{dir: dir}
		if _, err := d.Download(context.Background(), "om_1", Resource{Key: "k"}); err == nil {
			t.Error("want error when no resource API configured")
		}
	})
	t.Run("transport error", func(t *testing.T) {
		d := NewResourceDownloader(&fakeResourceGetter{err: errors.New("boom")}, dir, 0)
		if _, err := d.Download(context.Background(), "om_1", Resource{Key: "k"}); err == nil {
			t.Error("want wrapped transport error")
		}
	})
	t.Run("nil response", func(t *testing.T) {
		d := NewResourceDownloader(&fakeResourceGetter{}, dir, 0)
		if _, err := d.Download(context.Background(), "om_1", Resource{Key: "k"}); err == nil {
			t.Error("want error for nil response")
		}
	})
	t.Run("api error code", func(t *testing.T) {
		resp := okResp("x", "a")
		resp.CodeError = larkcore.CodeError{Code: 234001, Msg: "bad"}
		d := NewResourceDownloader(&fakeResourceGetter{resp: resp}, dir, 0)
		_, err := d.Download(context.Background(), "om_1", Resource{Key: "k"})
		if err == nil || !strings.Contains(err.Error(), "234001") {
			t.Errorf("err = %v, want it to mention code 234001", err)
		}
	})
	t.Run("nil body", func(t *testing.T) {
		resp := okResp("x", "a")
		resp.File = nil
		d := NewResourceDownloader(&fakeResourceGetter{resp: resp}, dir, 0)
		if _, err := d.Download(context.Background(), "om_1", Resource{Key: "k"}); err == nil {
			t.Error("want error for missing body")
		}
	})
}

func TestDownloader_RejectsOversizedBody(t *testing.T) {
	dir := t.TempDir()
	d := NewResourceDownloader(&fakeResourceGetter{resp: okResp(strings.Repeat("A", 100), "big.bin")}, dir, 10)

	_, err := d.Download(context.Background(), "om_1", Resource{Key: "k"})
	if err == nil {
		t.Fatal("want error for oversized resource")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want it to mention the size limit", err)
	}
	// the partial file must not be left behind
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("oversized download left %d files behind", len(entries))
	}
}

func TestNewResourceDownloader_DefaultsMaxBytes(t *testing.T) {
	d := NewResourceDownloader(&fakeResourceGetter{}, t.TempDir(), 0).(*larkDownloader)
	if d.maxBytes != DefaultMaxResourceBytes {
		t.Errorf("maxBytes = %d, want default %d", d.maxBytes, DefaultMaxResourceBytes)
	}
}
