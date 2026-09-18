package tool

import (
	"context"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// TestConcurrencyDefaultsToSerial is the safety property: a tool nobody thought
// about runs alone.
func TestConcurrencyDefaultsToSerial(t *testing.T) {
	undeclared := &recordingTool{name: "new_tool", cap: CapRead}
	if got := ConcurrencyOf(undeclared); got != Unspecified {
		t.Errorf("an undeclared tool reports %q, want Unspecified so a test can see that nobody decided", got)
	}
	if got := EffectiveConcurrency(undeclared); got != Serial {
		t.Errorf("an undeclared tool behaves as %q, want serial: the safe direction is the default", got)
	}

	declared := WithConcurrency(undeclared, ParallelSafe)
	if got := ConcurrencyOf(declared); got != ParallelSafe {
		t.Errorf("declared mode = %q, want parallel", got)
	}
	if got := EffectiveConcurrency(declared); got != ParallelSafe {
		t.Errorf("effective mode = %q, want parallel", got)
	}
}

// TestConcurrencyIsSeparateFromCapability is the design claim, checked against the
// tools this repository actually has: three of them are read-only and must not
// overlap anything.
func TestConcurrencyIsSeparateFromCapability(t *testing.T) {
	type subject struct {
		name string
		cap  Capability
		mode Concurrency
	}
	subjects := []subject{
		// Reads that are genuinely pure.
		{"read_file", CapRead, ParallelSafe},
		{"grep", CapRead, ParallelSafe},
		// Read-only, and neither may overlap anything: one parks the turn waiting
		// for a person, the other is a read-modify-write on shared state, and the
		// third writes to a store.
		{"ask_user", CapRead, Serial},
		{"plan_update", CapRead, Serial},
		{"save_document", CapRead, Serial},
		// Writes and commands.
		{"write_file", CapWrite, Serial},
		{"bash", CapExec, Serial},
	}
	for _, s := range subjects {
		tool := WithConcurrency(WithCapability(&recordingTool{name: s.name, cap: s.cap}, s.cap), s.mode)
		if got := CapabilityOf(tool); got != s.cap {
			t.Errorf("%s: capability = %q, want %q", s.name, got, s.cap)
		}
		if got := EffectiveConcurrency(tool); got != s.mode {
			t.Errorf("%s: concurrency = %q, want %q", s.name, got, s.mode)
		}
	}

	// The point of the table: capability alone cannot tell these apart.
	sameCapability := map[Capability]map[Concurrency]bool{}
	for _, s := range subjects {
		if sameCapability[s.cap] == nil {
			sameCapability[s.cap] = map[Concurrency]bool{}
		}
		sameCapability[s.cap][s.mode] = true
	}
	if len(sameCapability[CapRead]) != 2 {
		t.Error("this test is only meaningful while CapRead covers both parallel-safe and serial tools")
	}
}

// TestConcurrencySurvivesDecoration is the trap that cost a real bug in the
// approval gate: embedding the Tool interface promotes only that interface's
// methods, so a decorator silently loses anything else it was asked to forward.
func TestConcurrencySurvivesDecoration(t *testing.T) {
	inner := WithConcurrency(&recordingTool{name: "grep", cap: CapRead}, ParallelSafe)

	// Every decorator this repository wraps tools in.
	decorated := []struct {
		name string
		tool Tool
	}{
		{"capability", WithCapability(inner, CapRead)},
		{"concurrency", WithConcurrency(inner, ParallelSafe)},
		{"gate", Gate(inner, GateOptions{Policy: ApprovalPolicy{Mode: ApprovalAll}})},
	}
	for _, d := range decorated {
		if got := EffectiveConcurrency(d.tool); got != ParallelSafe {
			t.Errorf("%s decorator lost the declaration: %q", d.name, got)
		}
		if got := CapabilityOf(d.tool); got != CapRead {
			t.Errorf("%s decorator lost the capability: %q", d.name, got)
		}
	}
}

// TestSegment is the schedule. It is the whole correctness story for parallel
// execution: reads may overlap, and everything else is a barrier.
func TestSegment(t *testing.T) {
	cases := []struct {
		name  string
		modes []Concurrency
		want  [][]int
	}{
		{"all reads", []Concurrency{ParallelSafe, ParallelSafe, ParallelSafe}, [][]int{{0, 1, 2}}},
		{"one call", []Concurrency{ParallelSafe}, [][]int{{0}}},
		{"no calls", nil, nil},

		// The canonical case: two reads overlap, the write runs alone, then the
		// reads after it overlap.
		{
			"read read write read read",
			[]Concurrency{ParallelSafe, ParallelSafe, Serial, ParallelSafe, ParallelSafe},
			[][]int{{0, 1}, {2}, {3, 4}},
		},
		// A write first: it is a barrier because everything after it waits, which
		// is what "write the config, then run the tests" means.
		{
			"write read",
			[]Concurrency{Serial, ParallelSafe},
			[][]int{{0}, {1}},
		},
		// Two writes never share a group, even though each is Serial on its own.
		{
			"write write",
			[]Concurrency{Serial, Serial},
			[][]int{{0}, {1}},
		},
		{
			"read write read",
			[]Concurrency{ParallelSafe, Serial, ParallelSafe},
			[][]int{{0}, {1}, {2}},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := Segment(tt.modes)
			if len(got) != len(tt.want) {
				t.Fatalf("groups = %v, want %v", got, tt.want)
			}
			for i := range got {
				if len(got[i]) != len(tt.want[i]) {
					t.Fatalf("group %d = %v, want %v", i, got[i], tt.want[i])
				}
				for j := range got[i] {
					if got[i][j] != tt.want[i][j] {
						t.Fatalf("group %d = %v, want %v", i, got[i], tt.want[i])
					}
				}
			}
		})
	}
}

// TestSegmentIsAPartition: every call runs exactly once, in an order that keeps
// the barriers meaningful. A schedule that dropped or duplicated a call would be a
// silent wrong answer.
func TestSegmentIsAPartition(t *testing.T) {
	modes := []Concurrency{ParallelSafe, Serial, ParallelSafe, ParallelSafe, Serial, ParallelSafe}
	groups := Segment(modes)

	seen := map[int]int{}
	lastGroupOf := map[int]int{}
	for gi, g := range groups {
		for _, i := range g {
			seen[i]++
			lastGroupOf[i] = gi
		}
	}
	if len(seen) != len(modes) {
		t.Fatalf("the schedule covers %d of %d calls: %v", len(seen), len(modes), groups)
	}
	for i, n := range seen {
		if n != 1 {
			t.Errorf("call %d appears %d times", i, n)
		}
	}
	// Every group's indices are in ascending order, and the groups themselves are
	// ordered, so the program order is preserved by construction.
	prev := -1
	for _, g := range groups {
		for _, i := range g {
			if i <= prev {
				t.Errorf("the schedule reorders the program: %v", groups)
			}
			prev = i
		}
	}
}

// TestWithConcurrencyUnspecifiedIsANoop: declaring nothing must not add a
// decorator that then reports "declared".
func TestWithConcurrencyUnspecifiedIsANoop(t *testing.T) {
	inner := &recordingTool{name: "x", cap: CapRead}
	if got := WithConcurrency(inner, Unspecified); got != Tool(inner) {
		t.Error("WithConcurrency(Unspecified) should return the tool unchanged")
	}
	if got := WithConcurrency(nil, ParallelSafe); got != nil {
		t.Error("WithConcurrency(nil) should stay nil")
	}
}

// TestUndeclaredTools: the list a startup check and the test below rely on.
func TestUndeclaredTools(t *testing.T) {
	reg := NewRegistry()
	for _, tl := range []Tool{
		WithConcurrency(WithCapability(&recordingTool{name: "grep", cap: CapRead}, CapRead), ParallelSafe),
		WithCapability(&recordingTool{name: "forgotten", cap: CapWrite}, CapWrite),
	} {
		if err := reg.Register(tl); err != nil {
			t.Fatal(err)
		}
	}
	got := UndeclaredTools(context.Background(), reg)
	if len(got) != 1 || got[0] != "forgotten" {
		t.Errorf("undeclared = %v, want [forgotten]", got)
	}
	if err := ValidateConcurrency("forgotten", ConcurrencyOf(mustGet(t, reg, "forgotten"))); err == nil {
		t.Error("ValidateConcurrency should refuse a tool that declared nothing")
	}
	if err := ValidateConcurrency("grep", ConcurrencyOf(mustGet(t, reg, "grep"))); err != nil {
		t.Errorf("ValidateConcurrency(grep) = %v", err)
	}
	if err := ValidateConcurrency("weird", Concurrency("sometimes")); err == nil {
		t.Error("an unknown mode must be refused")
	}
}

// TestDescribeConcurrency reports the effective modes, which is what the console
// shows an operator.
func TestDescribeConcurrency(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(WithConcurrency(
		WithCapability(&recordingTool{name: "grep", cap: CapRead}, CapRead), ParallelSafe)); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(WithCapability(&recordingTool{name: "bash", cap: CapExec}, CapExec)); err != nil {
		t.Fatal(err)
	}
	got := DescribeConcurrency(context.Background(), reg)
	if got["grep"] != ParallelSafe {
		t.Errorf("grep = %q", got["grep"])
	}
	if got["bash"] != Serial {
		t.Errorf("bash = %q, want serial (its declared nothing, so effective is serial)", got["bash"])
	}
	if len(DescribeConcurrency(context.Background(), nil)) != 0 {
		t.Error("a nil registry should describe nothing rather than panic")
	}
}

func mustGet(t *testing.T, reg *Registry, name string) Tool {
	t.Helper()
	tl, ok := reg.Get(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	return tl
}

// keep the eino import used: recordingTool implements the invokable interface.
var _ einotool.InvokableTool = (*recordingTool)(nil)
var _ = schema.ToolInfo{}
