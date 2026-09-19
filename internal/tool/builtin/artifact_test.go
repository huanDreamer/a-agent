package builtin

import (
	"context"
	"io"
	"strings"
	"testing"

	agenttool "github.com/huan/huan-agent/internal/tool"
)

// stubArtifactSaver records what the tool handed it, so the test can assert on the
// request rather than on a side effect.
type stubArtifactSaver struct {
	got  agenttool.ArtifactInput
	body string
	res  agenttool.ArtifactResult
	err  error
}

func (f *stubArtifactSaver) SaveArtifact(_ context.Context, in agenttool.ArtifactInput) (agenttool.ArtifactResult, error) {
	f.got = in
	if in.Content != nil {
		b, _ := io.ReadAll(in.Content)
		f.body = string(b)
	}
	return f.res, f.err
}

func TestSaveArtifact_PassesContentThrough(t *testing.T) {
	saver := &stubArtifactSaver{res: agenttool.ArtifactResult{
		URL:   "/api/artifacts/files/s1/x.html",
		Path:  "s1/x.html",
		Bytes: 12,
		MIME:  "text/html; charset=utf-8",
	}}
	ctx := agenttool.WithArtifacts(context.Background(), saver)

	out, err := saveArtifact(ctx, SaveArtifactInput{
		Title:   "Report",
		Content: "<h1>hello</h1>",
		Kind:    "html",
		Name:    "report.html",
	})
	if err != nil {
		t.Fatalf("saveArtifact: %v", err)
	}
	if saver.body != "<h1>hello</h1>" {
		t.Errorf("content = %q, want it forwarded unchanged", saver.body)
	}
	if saver.got.Title != "Report" || saver.got.Name != "report.html" || saver.got.Kind != "html" {
		t.Errorf("input = %+v", saver.got)
	}
	if saver.got.Source != SaveArtifactToolName {
		t.Errorf("Source = %q, want %q", saver.got.Source, SaveArtifactToolName)
	}
	if out.URL != "/api/artifacts/files/s1/x.html" {
		t.Errorf("URL = %q", out.URL)
	}
	if !strings.Contains(out.Message, "/api/artifacts/files/s1/x.html") {
		t.Errorf("Message = %q, want it to carry the URL", out.Message)
	}
}

// A deployment with no artifact store must say so in a way the model can act on,
// not fail with a nil-pointer-ish message it would retry against.
func TestSaveArtifact_NoSaver(t *testing.T) {
	_, err := saveArtifact(context.Background(), SaveArtifactInput{Title: "t", Content: "c"})
	if err == nil {
		t.Fatal("saveArtifact succeeded with no saver")
	}
	if !strings.Contains(err.Error(), "产物") {
		t.Errorf("error = %v, want an explanation naming the missing store", err)
	}
}

func TestSaveArtifact_RequiresTitleAndContent(t *testing.T) {
	saver := &stubArtifactSaver{}
	ctx := agenttool.WithArtifacts(context.Background(), saver)

	if _, err := saveArtifact(ctx, SaveArtifactInput{Content: "c"}); err == nil {
		t.Error("accepted an empty title")
	}
	if _, err := saveArtifact(ctx, SaveArtifactInput{Title: "t"}); err == nil {
		t.Error("accepted empty content")
	}
	if _, err := saveArtifact(ctx, SaveArtifactInput{Title: "  ", Content: "c"}); err == nil {
		t.Error("accepted a whitespace-only title")
	}
}

// No URL is a real case (a one-shot run with no console serving): the tool must
// say "saved, but there is nothing to open" rather than quote a path as a link.
func TestSaveArtifact_NoURLIsReportedHonestly(t *testing.T) {
	saver := &stubArtifactSaver{res: agenttool.ArtifactResult{Path: "s1/x.html", Bytes: 3}}
	ctx := agenttool.WithArtifacts(context.Background(), saver)

	out, err := saveArtifact(ctx, SaveArtifactInput{Title: "t", Content: "abc"})
	if err != nil {
		t.Fatalf("saveArtifact: %v", err)
	}
	if out.URL != "" {
		t.Errorf("URL = %q, want empty", out.URL)
	}
	if !strings.Contains(out.Message, "s1/x.html") || !strings.Contains(out.Message, "没有可用的访问 URL") {
		t.Errorf("Message = %q, want it to say the file exists but has no URL", out.Message)
	}
}

func TestNormalizeArtifactKind(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"HTML":       "html",
		" page ":     "html",
		"doc":        "document",
		"markdown":   "document",
		"img":        "image",
		"screenshot": "image",
		"other":      "other",
	}
	for in, want := range cases {
		got, err := normalizeArtifactKind(in)
		if err != nil {
			t.Errorf("normalizeArtifactKind(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeArtifactKind(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := normalizeArtifactKind("spreadsheet"); err == nil {
		t.Error("accepted an unknown kind")
	}
}

func TestSaveArtifactTool_RegisteredName(t *testing.T) {
	tl, err := NewSaveArtifactTool()
	if err != nil {
		t.Fatalf("NewSaveArtifactTool: %v", err)
	}
	info, err := tl.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != SaveArtifactToolName {
		t.Errorf("name = %q, want %q", info.Name, SaveArtifactToolName)
	}
	if !strings.Contains(info.Desc, "产物中心") {
		t.Errorf("description omits where the user finds it: %q", info.Desc)
	}
}
