package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/huan/huan-agent/internal/documents"
)

// fakeSaver records what the tool asked to store.
type fakeSaver struct {
	docs []documents.Document
	uri  string
	err  error
}

func (f *fakeSaver) SaveDocument(_ context.Context, doc documents.Document) (string, error) {
	f.docs = append(f.docs, doc)
	if f.err != nil {
		return "", f.err
	}
	if f.uri == "" {
		return "viking://user/default/huan-agent/documents/x.md", nil
	}
	return f.uri, nil
}

func TestSaveDocumentToolStoresTheDocument(t *testing.T) {
	saver := &fakeSaver{uri: "viking://user/default/huan-agent/documents/2026/09/report-ab12cd34.md"}
	tool, err := NewSaveDocumentTool(saver)
	if err != nil {
		t.Fatalf("NewSaveDocumentTool: %v", err)
	}
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "save_document" {
		t.Errorf("name = %q, want save_document", info.Name)
	}

	out, err := tool.InvokableRun(context.Background(), `{"title":"Weekly report","content":"# Report\n\nAll good.","tags":["Weekly","  ","report"]}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	var got SaveDocumentOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode output %q: %v", out, err)
	}
	if got.URI != saver.uri {
		t.Errorf("uri = %q, want %q", got.URI, saver.uri)
	}
	if len(saver.docs) != 1 {
		t.Fatalf("saved = %d, want 1", len(saver.docs))
	}
	doc := saver.docs[0]
	if doc.Source != "chat" {
		t.Errorf("source = %q, want the chat default", doc.Source)
	}
	// Tags are normalised and deduplicated so a model that echoes a sentence
	// into the tag field cannot fill the metadata with noise.
	if len(doc.Tags) != 2 || doc.Tags[0] != "weekly" || doc.Tags[1] != "report" {
		t.Errorf("tags = %v, want [weekly report]", doc.Tags)
	}
}

func TestSaveDocumentToolRequiresTitleAndContent(t *testing.T) {
	tool, err := NewSaveDocumentTool(&fakeSaver{})
	if err != nil {
		t.Fatalf("NewSaveDocumentTool: %v", err)
	}
	for name, args := range map[string]string{
		"no title":   `{"title":"   ","content":"body"}`,
		"no content": `{"title":"t","content":"   "}`,
	} {
		if _, err := tool.InvokableRun(context.Background(), args); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestSaveDocumentToolSurfacesStoreFailure(t *testing.T) {
	tool, err := NewSaveDocumentTool(&fakeSaver{err: errors.New("openviking: down")})
	if err != nil {
		t.Fatalf("NewSaveDocumentTool: %v", err)
	}
	_, err = tool.InvokableRun(context.Background(), `{"title":"t","content":"c"}`)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "openviking: down") {
		t.Errorf("error = %q, want the store's message in it", err.Error())
	}
}

func TestNewSaveDocumentToolRejectsNilSaver(t *testing.T) {
	if _, err := NewSaveDocumentTool(nil); err == nil {
		t.Error("NewSaveDocumentTool(nil): want error, got nil")
	}
}

func TestCleanTags(t *testing.T) {
	got := cleanTags([]string{"A", "a", "", "  ", "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("cleanTags = %v, want [a b]", got)
	}
	long := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		long = append(long, string(rune('a'+i)))
	}
	if got := cleanTags(long); len(got) != 8 {
		t.Errorf("cleanTags(20 tags) = %d, want the 8-tag cap", len(got))
	}
}
