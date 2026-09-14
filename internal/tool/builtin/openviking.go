package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/documents"
)

// DocumentSaver stores a document. It is an interface so this package does not
// depend on the integration's wiring, and so a test can assert what the tool
// asked for without an OpenViking server.
type DocumentSaver interface {
	SaveDocument(ctx context.Context, doc documents.Document) (string, error)
}

// SaveDocumentInput is the parameter schema for the "save_document" tool.
type SaveDocumentInput struct {
	Title   string   `json:"title" jsonschema:"description=Short title of the document. It becomes part of the stored file name,required"`
	Content string   `json:"content" jsonschema:"description=Full document body in markdown. Write the finished document here, not a description of it.,required"`
	Tags    []string `json:"tags,omitempty" jsonschema:"description=Optional lowercase tags for later retrieval, for example deployment or design"`
	Source  string   `json:"source,omitempty" jsonschema:"description=Where the document came from, for example chat or a file path"`
}

// SaveDocumentOutput is what the tool returns.
type SaveDocumentOutput struct {
	URI     string `json:"uri"`
	Message string `json:"message"`
}

// NewSaveDocumentTool builds the tool that stores a document in OpenViking.
//
// It exists because "the report I just wrote should still be there tomorrow" is
// a promise the model cannot keep with only local files: a workspace file is
// invisible to the next conversation, while a document saved here is indexed
// and semantically searchable. The tool takes no URI — the destination is the
// operator's configuration, not the model's guess, which is what keeps every
// document in one retrievable subtree.
func NewSaveDocumentTool(saver DocumentSaver) (tool.InvokableTool, error) {
	if saver == nil {
		return nil, errors.New("builtin: a document saver is required")
	}
	return utils.InferTool("save_document",
		"Save a document (report, summary, note, decision record) to the long-term document store, where it stays searchable in later conversations. "+
			"Use this when the user asks you to save, file, archive or remember a piece of writing, or when a task produced a document worth keeping — "+
			"do NOT use it for a short answer or for code you are writing into the workspace. "+
			"Write the finished document in `content` (markdown); the stored file is named and placed by the server configuration, so no path or URI is needed. "+
			"Returns the stored `uri`, which can be quoted back to the user.",
		func(ctx context.Context, in SaveDocumentInput) (SaveDocumentOutput, error) {
			title := strings.TrimSpace(in.Title)
			if title == "" {
				return SaveDocumentOutput{}, errors.New("title is required")
			}
			if strings.TrimSpace(in.Content) == "" {
				return SaveDocumentOutput{}, errors.New("content is required")
			}
			source := strings.TrimSpace(in.Source)
			if source == "" {
				source = "chat"
			}
			uri, err := saver.SaveDocument(ctx, documents.Document{
				Title:   title,
				Content: in.Content,
				Tags:    cleanTags(in.Tags),
				Source:  source,
			})
			if err != nil {
				return SaveDocumentOutput{}, fmt.Errorf("save document: %w", err)
			}
			return SaveDocumentOutput{
				URI:     uri,
				Message: "saved and indexed; it can be found by search from now on",
			}, nil
		})
}

// cleanTags lowercases tags, drops blanks and caps the list, so a model that
// echoes a sentence into the tag field cannot fill the metadata with noise.
func cleanTags(tags []string) []string {
	const maxTags = 8
	out := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] || len(t) > 40 {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == maxTags {
			break
		}
	}
	return out
}
