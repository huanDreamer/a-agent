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

// TestRegistry_CloneIsIndependent: a clone is the seam a workspace-bound turn
// uses, so mutating it must not be visible to anyone else, and it must carry
// what was registered at runtime (an MCP server's tools) rather than a snapshot
// taken at start-up.
func TestRegistry_CloneIsIndependent(t *testing.T) {
	base := NewRegistry()
	if err := base.Register(newStub("read_file", "read", nil)); err != nil {
		t.Fatalf("register: %v", err)
	}

	clone := base.Clone()
	// A clone must contain whatever the registry holds at the moment it is
	// taken — an MCP server that registered after start-up included — which is
	// why a workspace-bound clone is built per turn rather than cached.
	if err := base.Register(newStub("mcp_find", "mcp", nil)); err != nil {
		t.Fatalf("register mcp: %v", err)
	}
	later := base.Clone()
	if _, ok := later.Get("mcp_find"); !ok {
		t.Error("a clone taken after a runtime registration is missing it")
	}
	if _, ok := clone.Get("mcp_find"); ok {
		t.Error("an earlier clone picked up a later registration")
	}
	if _, ok := clone.Get("read_file"); !ok {
		t.Error("clone is missing read_file")
	}

	// Replacing in the clone must leave the base alone.
	if err := clone.Replace(newStub("read_file", "replaced", nil)); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := clone.Register(newStub("only_here", "x", nil)); err != nil {
		t.Fatalf("register in clone: %v", err)
	}
	if got, _ := base.Get("read_file"); got == nil {
		t.Fatal("base lost read_file")
	} else if info, _ := got.Info(context.Background()); info.Desc != "read" {
		t.Errorf("base read_file was modified through the clone: %q", info.Desc)
	}
	if _, ok := base.Get("only_here"); ok {
		t.Error("a tool registered in the clone appeared in the base")
	}

	// Unregistering from the base must not affect an existing clone.
	base.Unregister("read_file")
	if _, ok := clone.Get("read_file"); !ok {
		t.Error("unregistering from the base removed the tool from a clone")
	}
}

// TestRegistry_CloneNil: a nil registry is a legitimate state (no tools), and
// cloning it must yield an empty registry rather than a panic.
func TestRegistry_CloneNil(t *testing.T) {
	var r *Registry
	c := r.Clone()
	if c == nil {
		t.Fatal("Clone of nil returned nil")
	}
	if len(c.Names()) != 0 {
		t.Errorf("clone of nil is not empty: %v", c.Names())
	}
}

// TestRegistry_ReplaceKeepsAllowList: replacing an implementation must never
// widen what the model may call. A name the allow-list excluded stays excluded.
func TestRegistry_ReplaceKeepsAllowList(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"a", "b"} {
		if err := r.Register(newStub(n, "x", nil)); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
	}
	if err := r.SetAllowList([]string{"a"}); err != nil {
		t.Fatalf("SetAllowList: %v", err)
	}

	if err := r.Replace(newStub("a", "replaced", nil)); err != nil {
		t.Fatalf("replace allowed: %v", err)
	}
	if err := r.Replace(newStub("b", "replaced", nil)); err != nil {
		t.Fatalf("replace disallowed: %v", err)
	}
	if !r.IsAllowed("a") {
		t.Error("replacing an allowed tool removed it from the allow-list")
	}
	if r.IsAllowed("b") {
		t.Error("replacing a disallowed tool added it to the allow-list")
	}

	// A name that never existed joins "allow all" but not an explicit list.
	fresh := NewRegistry()
	if err := fresh.Replace(newStub("new", "x", nil)); err != nil {
		t.Fatalf("replace into empty registry: %v", err)
	}
	if !fresh.IsAllowed("new") {
		t.Error("a tool replaced into an allow-all registry is not permitted")
	}
}

// TestRegistry_ReplaceValidates: the name comes from the tool itself, so a
// factory cannot register under a different name than the tool it replaced.
func TestRegistry_ReplaceValidates(t *testing.T) {
	r := NewRegistry()
	if err := r.Replace(nil); err == nil {
		t.Error("Replace(nil) should fail")
	}
	if err := r.Replace(newStub("", "no name", nil)); err == nil {
		t.Error("Replace of a nameless tool should fail")
	}
}
