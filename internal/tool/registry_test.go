package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

type stubTool struct {
	name, desc string
	fn         func(string) (string, error)
}

func (s *stubTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: s.name, Desc: s.desc}, nil
}

func (s *stubTool) InvokableRun(_ context.Context, args string, _ ...einotool.Option) (string, error) {
	if s.fn == nil {
		return args, nil
	}
	return s.fn(args)
}

func newStub(name, desc string, fn func(string) (string, error)) *stubTool {
	return &stubTool{name: name, desc: desc, fn: fn}
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newStub("a", "first", nil)); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := r.Register(newStub("b", "second", nil)); err != nil {
		t.Fatalf("register b: %v", err)
	}

	got, ok := r.Get("a")
	if !ok || got == nil {
		t.Fatal("Get(a) failed")
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("Get(missing) should fail")
	}

	names := r.Names()
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("Names = %v, want [a b]", names)
	}
}

func TestRegistry_DuplicateRegister(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newStub("dup", "x", nil)); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := r.Register(newStub("dup", "y", nil))
	if err == nil {
		t.Fatal("expected error on duplicate register")
	}
}

func TestRegistry_NilRegister(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Error("Register(nil) should fail")
	}
}

func TestRegistry_DefaultAllowAll(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newStub("a", "", nil)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(newStub("b", "", nil)); err != nil {
		t.Fatal(err)
	}
	if !r.IsAllowed("a") || !r.IsAllowed("b") {
		t.Error("default state should allow all")
	}
	if got := r.AllowList(); len(got) != 2 {
		t.Errorf("AllowList = %v, want 2 entries", got)
	}

	specs, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(specs) != 2 {
		t.Errorf("len(specs) = %d, want 2", len(specs))
	}
}

func TestRegistry_ExplicitAllowList(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"a", "b", "c"} {
		if err := r.Register(newStub(n, "", nil)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SetAllowList([]string{"a", "c"}); err != nil {
		t.Fatal(err)
	}
	if r.IsAllowed("a") != true {
		t.Error("a should be allowed")
	}
	if r.IsAllowed("b") {
		t.Error("b should not be allowed")
	}
	if r.IsAllowed("c") != true {
		t.Error("c should be allowed")
	}

	specs, _ := r.List(context.Background())
	if len(specs) != 2 || specs[0].Name != "a" || specs[1].Name != "c" {
		t.Errorf("specs = %+v", specs)
	}
}

func TestRegistry_AllowListUnknownTool(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newStub("a", "", nil)); err != nil {
		t.Fatal(err)
	}
	err := r.SetAllowList([]string{"a", "nope"})
	if err == nil {
		t.Fatal("expected error for unknown tool in allow-list")
	}
	// Allow-list should not have been partially applied.
	if r.allowListSet {
		t.Error("allow-list should not be marked as set after a failed call")
	}
}

func TestRegistry_AllowListResetToAll(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(newStub("a", "", nil)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetAllowList([]string{"a"}); err != nil {
		t.Fatal(err)
	}
	// Setting an empty list should restore "allow all registered".
	if err := r.SetAllowList(nil); err != nil {
		t.Fatal(err)
	}
	if !r.IsAllowed("a") {
		t.Error("a should be allowed after reset to nil")
	}
}

func TestRegistry_ToolReturnsError(t *testing.T) {
	r := NewRegistry()
	want := errors.New("boom")
	if err := r.Register(newStub("bad", "x", func(string) (string, error) { return "", want })); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get("bad")
	if !ok {
		t.Fatal("missing tool")
	}
	out, err := got.InvokableRun(context.Background(), `{}`)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if out != "" {
		t.Errorf("out = %q, want empty", out)
	}
}

func TestSpecOf(t *testing.T) {
	s := newStub("foo", "does foo things", nil)
	spec, err := SpecOf(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "foo" || spec.Description != "does foo things" {
		t.Errorf("spec = %+v", spec)
	}
}

func TestAsEinoTool_Passthrough(t *testing.T) {
	et := newStub("a", "", nil)
	got := AsEinoTool(et)
	if got != einotool.InvokableTool(et) {
		t.Error("AsEinoTool should pass through an eino tool")
	}
}

// TestSpecOf_ParameterSchemaIsValid guards the regression that made every tool
// call fail against real providers: ParamsOneOf keeps its schema in unexported
// fields, so marshalling it produced "{}" and the model received a schema with
// type null ("Invalid schema for function ... got 'type: null'").
func TestSpecOf_ParameterSchemaIsValid(t *testing.T) {
	tests := []struct {
		name string
		tool Tool
	}{
		{"declared params", &schemaTool{name: "with", params: `{"type":"object","properties":{"a":{"type":"string"}}}`}},
		{"no params", &schemaTool{name: "without"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := SpecOf(context.Background(), tc.tool)
			if err != nil {
				t.Fatalf("SpecOf: %v", err)
			}
			var parsed map[string]any
			if err := json.Unmarshal([]byte(spec.ParametersJSONSchema), &parsed); err != nil {
				t.Fatalf("schema is not valid JSON (%q): %v", spec.ParametersJSONSchema, err)
			}
			if parsed["type"] != "object" {
				t.Errorf("schema type = %v, want object; got %s",
					parsed["type"], spec.ParametersJSONSchema)
			}
			if _, ok := parsed["properties"]; !ok {
				t.Errorf("schema has no properties: %s", spec.ParametersJSONSchema)
			}
		})
	}

	// A tool that declares properties keeps them.
	spec, err := SpecOf(context.Background(), &schemaTool{
		name:   "keep",
		params: `{"type":"object","properties":{"expression":{"type":"string"}},"required":["expression"]}`,
	})
	if err != nil {
		t.Fatalf("SpecOf: %v", err)
	}
	if !strings.Contains(spec.ParametersJSONSchema, "expression") {
		t.Errorf("declared property lost: %s", spec.ParametersJSONSchema)
	}
}

// schemaTool is a Tool that declares an explicit parameter schema.
type schemaTool struct {
	name   string
	params string
}

func (s *schemaTool) Info(context.Context) (*schema.ToolInfo, error) {
	info := &schema.ToolInfo{Name: s.name, Desc: "test"}
	if s.params == "" {
		return info, nil
	}
	var js jsonschema.Schema
	if err := json.Unmarshal([]byte(s.params), &js); err != nil {
		return nil, err
	}
	info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(&js)
	return info, nil
}

func (s *schemaTool) InvokableRun(context.Context, string, ...einotool.Option) (string, error) {
	return "", nil
}
