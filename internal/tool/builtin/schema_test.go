package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspace"
)

// TestSchemaDescriptions_AreNotTruncated guards a silent failure mode.
//
// eino's JSON-schema generator splits the `jsonschema` struct tag on commas, so
// a comma inside a description silently discards everything after it. The model
// is then told half a sentence and no error is reported anywhere — the only
// symptom is a tool that is used badly.
//
// Reading the tags through reflection is exact, and it fails at build/test time
// rather than in production.
func TestSchemaDescriptions_AreNotTruncated(t *testing.T) {
	for name, sample := range map[string]any{
		"time":       TimeInput{},
		"calc":       CalcInput{},
		"echo":       EchoInput{},
		"read_file":  ReadFileInput{},
		"write_file": WriteFileInput{},
		"edit_file":  EditFileInput{},
		"list_dir":   ListDirInput{},
		"glob":       GlobInput{},
		"grep":       GrepInput{},
		"bash":       BashInput{},
	} {
		t.Run(name, func(t *testing.T) {
			rt := reflect.TypeOf(sample)
			for i := 0; i < rt.NumField(); i++ {
				f := rt.Field(i)
				tag := f.Tag.Get("jsonschema")
				if tag == "" {
					continue
				}
				parts := strings.Split(tag, ",")
				desc := parts[0]
				if !strings.HasPrefix(desc, "description=") {
					continue
				}
				text := strings.TrimPrefix(desc, "description=")

				// Anything after the first comma is parsed as an option. Only
				// "required" is legitimate (eino trims surrounding space, so
				// ", required" and ",required" are equivalent); anything else is
				// the tail of a description that a comma split off, which is
				// silently dropped and leaves the model with half a sentence.
				for _, opt := range parts[1:] {
					if strings.TrimSpace(opt) != "required" {
						t.Errorf("field %s: jsonschema tag %q has %q after the description, which is "+
							"not an option — a comma inside a description truncates it silently",
							f.Name, tag, opt)
					}
				}
				// The description must not be cut mid-sentence by the split.
				if strings.HasSuffix(strings.TrimSpace(text), "e.g") ||
					strings.HasSuffix(strings.TrimSpace(text), "such as") ||
					strings.HasSuffix(strings.TrimSpace(text), "for example") {
					t.Errorf("field %s: description ends mid-phrase (%q)", f.Name, text)
				}
				if strings.TrimSpace(text) == "" {
					t.Errorf("field %s: jsonschema description is empty", f.Name)
				}
				// A backslash is an invalid escape for StructTag.Get, so a tag
				// containing one may be dropped entirely.
				if strings.Contains(text, `\`) {
					t.Errorf("field %s: description contains a backslash, which is not a valid struct-tag escape: %q",
						f.Name, text)
				}
			}
		})
	}
}

// TestSchemaDescriptions_ReachTheModel verifies the description actually
// survives into the schema the provider receives, which is the property that
// matters rather than the tag text alone.
func TestSchemaDescriptions_ReachTheModel(t *testing.T) {
	ws, err := workspace.New(t.TempDir(), workspace.Options{})
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	read, err := NewReadFileTool(ws)
	if err != nil {
		t.Fatalf("read tool: %v", err)
	}
	bash, err := NewBashTool(ws, DefaultBashPolicy())
	if err != nil {
		t.Fatalf("bash tool: %v", err)
	}

	// A phrase that appears after where a comma would have been: it only
	// survives if the tag is comma-free.
	tests := []struct {
		name   string
		tool   tool.Tool
		field  string
		phrase string
	}{
		{"read_file.path", read, "path", "refused"},
		{"bash.command", bash, "command", "verbatim"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := tool.SpecOf(context.Background(), tc.tool)
			if err != nil {
				t.Fatalf("SpecOf: %v", err)
			}
			var s map[string]any
			if err := json.Unmarshal([]byte(spec.ParametersJSONSchema), &s); err != nil {
				t.Fatalf("schema is not valid JSON: %v", err)
			}
			props, _ := s["properties"].(map[string]any)
			prop, ok := props[tc.field].(map[string]any)
			if !ok {
				t.Fatalf("schema has no %q property", tc.field)
			}
			desc, _ := prop["description"].(string)
			if !strings.Contains(desc, tc.phrase) {
				t.Errorf("description %q lost the phrase %q", desc, tc.phrase)
			}
		})
	}
}

// TestToolDescriptionsAreSubstantive checks the tool-level description the model
// reads to decide whether to call the tool at all.
func TestToolDescriptionsAreSubstantive(t *testing.T) {
	ws, err := workspace.New(t.TempDir(), workspace.Options{})
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	makeTools := map[string]func() (tool.Tool, error){
		"time": func() (tool.Tool, error) { return NewTimeTool() },
		"read_file": func() (tool.Tool, error) {
			return NewReadFileTool(ws)
		},
		"grep": func() (tool.Tool, error) { return NewGrepTool(ws) },
		"bash": func() (tool.Tool, error) { return NewBashTool(ws, DefaultBashPolicy()) },
	}
	for name, mk := range makeTools {
		t.Run(name, func(t *testing.T) {
			tl, err := mk()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			info, err := tl.Info(context.Background())
			if err != nil {
				t.Fatalf("Info: %v", err)
			}
			if info.Name != name {
				t.Errorf("tool name = %q, want %q", info.Name, name)
			}
			// A useful description is a sentence or two, not a word.
			if len(strings.TrimSpace(info.Desc)) < 40 {
				t.Errorf("description is too short to guide the model: %q", info.Desc)
			}
			if !strings.HasSuffix(strings.TrimSpace(info.Desc), ".") {
				t.Errorf("description does not end in a sentence: %q", info.Desc)
			}
			// The raw schema must still be a valid object.
			spec, err := tool.SpecOf(context.Background(), tl)
			if err != nil {
				t.Fatalf("SpecOf: %v", err)
			}
			var s map[string]any
			if err := json.Unmarshal([]byte(spec.ParametersJSONSchema), &s); err != nil {
				t.Fatalf("schema invalid: %v", err)
			}
			if s["type"] != "object" {
				t.Errorf("schema type = %v, want object", s["type"])
			}
		})
	}
}

// unusedSchema keeps the schema import honest if the build tags change.
var _ = schema.ToolInfo{}
