package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// A tool whose invocation is recorded, so a test can assert the one thing that
// matters about a gate: whether the action happened.
type recordingTool struct {
	name   string
	cap    Capability
	result string
	ran    int
	err    error
}

func (r *recordingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: r.name, Desc: "recording"}, nil
}

func (r *recordingTool) Capability() Capability { return r.cap }

func (r *recordingTool) InvokableRun(context.Context, string, ...einotool.Option) (string, error) {
	r.ran++
	return r.result, r.err
}

// scriptedApprover answers every request with a fixed decision.
type scriptedApprover struct {
	decision Decision
	err      error
	seen     []Request
}

func (a *scriptedApprover) Approve(_ context.Context, req Request) (Decision, error) {
	a.seen = append(a.seen, req)
	if a.err != nil {
		return Decision{}, a.err
	}
	return a.decision, nil
}

func TestParseApprovalMode(t *testing.T) {
	cases := map[string]string{
		"":            ApprovalOff,
		"off":         ApprovalOff,
		"OFF":         ApprovalOff,
		"writes":      ApprovalWrites,
		"writes+exec": ApprovalWritesExec,
		"writes_exec": ApprovalWritesExec,
		"all":         ApprovalAll,
	}
	for in, want := range cases {
		got, err := ParseApprovalMode(in)
		if err != nil {
			t.Errorf("ParseApprovalMode(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseApprovalMode(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := ParseApprovalMode("sometimes"); err == nil {
		t.Error("an unknown mode must be refused, not treated as off: the operator asked for a gate and would silently not get one")
	}
}

// TestPolicyRequires is the matrix the gate is built on: what needs asking is
// decided by capability, so a new tool needs no new policy.
func TestPolicyRequires(t *testing.T) {
	cases := []struct {
		mode string
		cap  Capability
		want bool
	}{
		{ApprovalOff, CapRead, false},
		{ApprovalOff, CapWrite, false},
		{ApprovalOff, CapExec, false},
		{ApprovalWrites, CapRead, false},
		{ApprovalWrites, CapWrite, true},
		{ApprovalWrites, CapExec, false},
		{ApprovalWritesExec, CapRead, false},
		{ApprovalWritesExec, CapWrite, true},
		{ApprovalWritesExec, CapExec, true},
		{ApprovalAll, CapRead, true},
		{ApprovalAll, CapWrite, true},
		{ApprovalAll, CapExec, true},
	}
	for _, tt := range cases {
		p := ApprovalPolicy{Mode: tt.mode}
		if got := p.Requires(tt.cap); got != tt.want {
			t.Errorf("mode %q, capability %q: Requires = %v, want %v", tt.mode, tt.cap, got, tt.want)
		}
	}
}

// TestGateOffRunsEverythingUnchanged is the regression gate for every existing
// deployment: with the mode off, the tool is called exactly as before.
func TestGateOffRunsEverythingUnchanged(t *testing.T) {
	inner := &recordingTool{name: "bash", cap: CapExec, result: "ok"}
	gated := Gate(inner, GateOptions{Policy: ApprovalPolicy{Mode: ApprovalOff}})

	out, err := gated.(einotool.InvokableTool).InvokableRun(context.Background(), `{"command":"ls"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if out != "ok" || inner.ran != 1 {
		t.Errorf("the tool did not run normally: out=%q ran=%d", out, inner.ran)
	}
	// And it is not even wrapped: an off gate must cost nothing.
	if _, wrapped := gated.(*ApprovalGate); wrapped {
		t.Error("with the mode off the tool should not be wrapped at all")
	}
}

// TestGateAllowsAndRuns: an approval lets the action happen.
func TestGateAllowsAndRuns(t *testing.T) {
	inner := &recordingTool{name: "write_file", cap: CapWrite, result: `{"bytes":3}`}
	approver := &scriptedApprover{decision: Decision{Kind: DecisionAllowOnce}}
	gated := Gate(inner, GateOptions{Policy: ApprovalPolicy{Mode: ApprovalWrites}})

	ctx := WithApprover(context.Background(), approver)
	out, err := gated.(einotool.InvokableTool).InvokableRun(ctx, `{"path":"a.go","content":"hi"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if inner.ran != 1 {
		t.Errorf("an approved call must run: ran=%d", inner.ran)
	}
	if out != `{"bytes":3}` {
		t.Errorf("result = %q, want the tool's own result", out)
	}
	if len(approver.seen) != 1 {
		t.Fatalf("approver saw %d requests, want 1", len(approver.seen))
	}
	req := approver.seen[0]
	if req.Tool != "write_file" || req.Capability != CapWrite {
		t.Errorf("request = %+v", req)
	}
	if !strings.Contains(req.Summary, "a.go") {
		t.Errorf("the summary must name the file, got %q", req.Summary)
	}
}

// TestGateDenyDoesNotRun is the property the whole feature exists for.
func TestGateDenyDoesNotRun(t *testing.T) {
	inner := &recordingTool{name: "bash", cap: CapExec, result: "should not happen"}
	approver := &scriptedApprover{decision: Decision{Kind: DecisionDeny, Reason: "不要动主分支"}}
	gated := Gate(inner, GateOptions{Policy: ApprovalPolicy{Mode: ApprovalWritesExec}})

	ctx := WithApprover(context.Background(), approver)
	out, err := gated.(einotool.InvokableTool).InvokableRun(ctx, `{"command":"git push origin main"}`)
	if err != nil {
		t.Fatalf("a refusal is not an error: %v", err)
	}
	if inner.ran != 0 {
		t.Fatal("the tool ran despite a refusal")
	}
	// The refusal comes back as the tool's observation, with the reason, because
	// the model has to pick a different approach rather than repeat itself.
	if !strings.Contains(out, "已被拒绝") {
		t.Errorf("the model must be told it was refused, got: %q", out)
	}
	if !strings.Contains(out, "不要动主分支") {
		t.Errorf("the person's reason must reach the model, got: %q", out)
	}
	if !strings.Contains(out, "不要重复同一个请求") {
		t.Errorf("a refusal should say what to do instead, got: %q", out)
	}
}

// TestGateTimeoutIsARefusal is the difference from ask_user, and it is the one
// behaviour that must not be got wrong: a timeout means nobody was watching, and
// nobody watching is exactly when the gate has to hold.
func TestGateTimeoutIsARefusal(t *testing.T) {
	inner := &recordingTool{name: "write_file", cap: CapWrite}
	approver := &scriptedApprover{decision: Decision{Kind: DecisionDeny, Source: SourceTimeout}}
	gated := Gate(inner, GateOptions{Policy: ApprovalPolicy{Mode: ApprovalWrites}})

	ctx := WithApprover(context.Background(), approver)
	out, err := gated.(einotool.InvokableTool).InvokableRun(ctx, `{"path":"a.go","content":"x"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if inner.ran != 0 {
		t.Fatal("a timed-out approval must not let the action through")
	}
	if !strings.Contains(out, "超时") {
		t.Errorf("the model should be told nobody answered, got: %q", out)
	}
}

// TestGateWithoutAnApproverRefuses: reaching the gate with no approver means the
// surface was wired wrong (registration withholds the tools), and letting the call
// through would be the worst possible response to that.
func TestGateWithoutAnApproverRefuses(t *testing.T) {
	inner := &recordingTool{name: "write_file", cap: CapWrite}
	gated := Gate(inner, GateOptions{Policy: ApprovalPolicy{Mode: ApprovalWrites}})

	out, err := gated.(einotool.InvokableTool).InvokableRun(context.Background(), `{"path":"a.go","content":"x"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if inner.ran != 0 {
		t.Fatal("with no way to ask, the action must not happen")
	}
	if !strings.Contains(out, "没有可用的审批通道") {
		t.Errorf("the model should be told why, got: %q", out)
	}
}

// TestGateApproverFailureRefuses: a broken approver is not a person saying yes.
func TestGateApproverFailureRefuses(t *testing.T) {
	inner := &recordingTool{name: "bash", cap: CapExec}
	approver := &scriptedApprover{err: errors.New("hub exploded")}
	gated := Gate(inner, GateOptions{Policy: ApprovalPolicy{Mode: ApprovalWritesExec}})

	out, err := gated.(einotool.InvokableTool).InvokableRun(
		WithApprover(context.Background(), approver), `{"command":"rm -rf /tmp/x"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if inner.ran != 0 {
		t.Fatal("a failed approval channel must not let the action through")
	}
	if !strings.Contains(out, "hub exploded") {
		t.Errorf("the failure should be visible to the model, got: %q", out)
	}
}

// TestGateAllowTurnIsScopedToOneTurn: "remember this forever" turns one click into
// a rule nobody reviews again, so the allowance dies with the turn's context.
func TestGateAllowTurnIsScopedToOneTurn(t *testing.T) {
	inner := &recordingTool{name: "write_file", cap: CapWrite}
	approver := &scriptedApprover{decision: Decision{Kind: DecisionAllowTurn}}
	gated := Gate(inner, GateOptions{Policy: ApprovalPolicy{Mode: ApprovalWrites}})

	first := WithApprover(WithTurnAllowances(context.Background()), approver)
	if _, err := gated.(einotool.InvokableTool).InvokableRun(first, `{"path":"a.go","content":"1"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := gated.(einotool.InvokableTool).InvokableRun(first, `{"path":"b.go","content":"2"}`); err != nil {
		t.Fatal(err)
	}
	if len(approver.seen) != 1 {
		t.Errorf("asked %d times within one allowed turn, want 1", len(approver.seen))
	}
	if inner.ran != 2 {
		t.Errorf("both writes should have run, ran=%d", inner.ran)
	}

	// A new turn asks again.
	second := WithApprover(WithTurnAllowances(context.Background()), approver)
	if _, err := gated.(einotool.InvokableTool).InvokableRun(second, `{"path":"c.go","content":"3"}`); err != nil {
		t.Fatal(err)
	}
	if len(approver.seen) != 2 {
		t.Errorf("a new turn must ask again: asked %d times total, want 2", len(approver.seen))
	}
}

// TestGateAllowTurnIsPerTool: allowing edits must not silently allow commands.
func TestGateAllowTurnIsPerTool(t *testing.T) {
	write := &recordingTool{name: "write_file", cap: CapWrite}
	bash := &recordingTool{name: "bash", cap: CapExec}
	policy := ApprovalPolicy{Mode: ApprovalWritesExec}
	approver := &scriptedApprover{decision: Decision{Kind: DecisionAllowTurn}}

	ctx := WithApprover(WithTurnAllowances(context.Background()), approver)
	gateW := Gate(write, GateOptions{Policy: policy})
	gateB := Gate(bash, GateOptions{Policy: policy})

	if _, err := gateW.(einotool.InvokableTool).InvokableRun(ctx, `{"path":"a.go","content":"1"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := gateB.(einotool.InvokableTool).InvokableRun(ctx, `{"command":"ls"}`); err != nil {
		t.Fatal(err)
	}
	if len(approver.seen) != 2 {
		t.Errorf("each tool must be asked about separately: asked %d times, want 2", len(approver.seen))
	}
}

// TestGateAllowListSkipsTheQuestion: a configured allow pattern means the call
// runs without asking, and the audit says it was the policy rather than a person.
func TestGateAllowListSkipsTheQuestion(t *testing.T) {
	inner := &recordingTool{name: "write_file", cap: CapWrite}
	approver := &scriptedApprover{decision: Decision{Kind: DecisionDeny}}
	policy := ApprovalPolicy{Mode: ApprovalWrites, Allow: []string{`写入 docs/`}}
	gated := Gate(inner, GateOptions{Policy: policy})

	ctx := WithApprover(context.Background(), approver)
	if _, err := gated.(einotool.InvokableTool).InvokableRun(ctx, `{"path":"docs/a.md","content":"x"}`); err != nil {
		t.Fatal(err)
	}
	if inner.ran != 1 {
		t.Error("an allowed summary must run without asking")
	}
	if len(approver.seen) != 0 {
		t.Error("an allowed summary must not reach the approver")
	}

	// A pattern that does not match still asks.
	if _, err := gated.(einotool.InvokableTool).InvokableRun(ctx, `{"path":"internal/a.go","content":"y"}`); err != nil {
		t.Fatal(err)
	}
	if len(approver.seen) != 1 {
		t.Errorf("a non-matching write must be asked about: asked %d times", len(approver.seen))
	}
}

// TestGateInvalidAllowPatternStillAsks: a broken regex must not become "allow
// everything", which is the direction this whole file exists to avoid.
func TestGateInvalidAllowPatternStillAsks(t *testing.T) {
	inner := &recordingTool{name: "write_file", cap: CapWrite}
	approver := &scriptedApprover{decision: Decision{Kind: DecisionDeny}}
	policy := ApprovalPolicy{Mode: ApprovalWrites, Allow: []string{`([unclosed`}}
	gated := Gate(inner, GateOptions{Policy: policy})

	if _, err := gated.(einotool.InvokableTool).InvokableRun(
		WithApprover(context.Background(), approver), `{"path":"a.go","content":"x"}`); err != nil {
		t.Fatal(err)
	}
	if len(approver.seen) != 1 {
		t.Error("an uncompilable pattern must leave the gate asking")
	}
	if inner.ran != 0 {
		t.Error("the tool ran despite the refusal")
	}
}

// TestGateReadToolsAreUntouched: a read is not what the gate is for, and gating
// it in the writes mode would make the mode useless.
func TestGateReadToolsAreUntouched(t *testing.T) {
	inner := &recordingTool{name: "read_file", cap: CapRead}
	approver := &scriptedApprover{decision: Decision{Kind: DecisionDeny}}
	gated := Gate(inner, GateOptions{Policy: ApprovalPolicy{Mode: ApprovalWrites}})

	if _, err := gated.(einotool.InvokableTool).InvokableRun(
		WithApprover(context.Background(), approver), `{"path":"a.go"}`); err != nil {
		t.Fatal(err)
	}
	if inner.ran != 1 {
		t.Error("reads must not be gated in the writes mode")
	}
	if len(approver.seen) != 0 {
		t.Error("the approver must not be consulted for a read")
	}
}

// --- request construction ---

// TestBuildRequestBashKeepsTheCommandWhole: the command is the thing being
// approved. Clipping it would hide the `rm -rf` at the end of a pipeline, which is
// the one case this mechanism exists for.
func TestBuildRequestBashKeepsTheCommandWhole(t *testing.T) {
	long := "echo " + strings.Repeat("x", 400) + " && rm -rf /tmp/important"
	req := BuildRequest("bash", CapExec, `{"command":`+jsonString(long)+`,"cwd":"/srv/app"}`)

	if !strings.Contains(req.Summary, "执行:") {
		t.Errorf("summary = %q", req.Summary)
	}
	var full string
	for _, l := range req.Preview {
		if strings.HasPrefix(l.Text, "完整命令：") {
			full = strings.TrimPrefix(l.Text, "完整命令：")
		}
	}
	if full != long {
		t.Errorf("the preview must carry the command in full:\ngot:  %q\nwant: %q", full, long)
	}
	if !strings.Contains(full, "rm -rf /tmp/important") {
		t.Error("the dangerous tail was lost")
	}
	// The metadata the person needs to judge the command is present too.
	joined := ""
	for _, l := range req.Preview {
		joined += l.Text + "\n"
	}
	if !strings.Contains(joined, "/srv/app") {
		t.Errorf("the working directory should be shown: %v", req.Preview)
	}
}

// TestBuildRequestEditShowsBothSides: an edit card that shows only the new text
// asks a person to approve a change without seeing what it replaces.
func TestBuildRequestEditShowsBothSides(t *testing.T) {
	req := BuildRequest("edit_file", CapWrite,
		`{"path":"internal/a.go","old_string":"func Foo() {","new_string":"func Bar() {"}`)

	if !strings.Contains(req.Summary, "internal/a.go") {
		t.Errorf("summary = %q", req.Summary)
	}
	var hasDel, hasAdd bool
	for _, l := range req.Preview {
		if l.Kind == PreviewDel && strings.Contains(l.Text, "Foo") {
			hasDel = true
		}
		if l.Kind == PreviewAdd && strings.Contains(l.Text, "Bar") {
			hasAdd = true
		}
	}
	if !hasDel || !hasAdd {
		t.Errorf("the preview must show what is removed and what replaces it: %v", req.Preview)
	}
}

// TestBuildRequestWriteReportsSizeAndReplacement: "整文件覆盖" is the fact that
// makes a whole-file write worth a second look.
func TestBuildRequestWriteReportsSizeAndReplacement(t *testing.T) {
	req := BuildRequest("write_file", CapWrite, `{"path":"a.go","content":"line1\nline2\n"}`)
	if !strings.Contains(req.Summary, "a.go") || !strings.Contains(req.Summary, "整文件覆盖") {
		t.Errorf("summary = %q", req.Summary)
	}
	if !strings.Contains(req.Summary, "2 行") {
		t.Errorf("summary should count the lines: %q", req.Summary)
	}
}

// TestBuildRequestGenericShowsSomething: a tool with no tailored summary must
// still show what it was asked to do, or the card asks for a blind approval.
func TestBuildRequestGenericShowsSomething(t *testing.T) {
	req := BuildRequest("some_new_tool", CapWrite, `{"thing":"value"}`)
	if !strings.Contains(req.Summary, "some_new_tool") {
		t.Errorf("summary = %q", req.Summary)
	}
	if len(req.Preview) == 0 {
		t.Error("a generic request should show its arguments rather than nothing")
	}
}

// TestBuildRequestBoundsThePreview: a card that scrolls is a card nobody reads.
func TestBuildRequestBoundsThePreview(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	req := BuildRequest("write_file", CapWrite, `{"path":"a.go","content":`+jsonString(b.String())+`}`)

	if len(req.Preview) > maxPreviewLines+1 {
		t.Errorf("preview has %d lines, want at most %d plus a truncation note", len(req.Preview), maxPreviewLines)
	}
	last := req.Preview[len(req.Preview)-1]
	if last.Kind != PreviewMeta || !strings.Contains(last.Text, "共 500 行") {
		t.Errorf("truncation must be stated, got %+v", last)
	}
}

// TestBuildRequestMalformedArgsFallsBack: a gate must not refuse the whole action
// because a summary could not be built. It shows the tool name instead.
func TestBuildRequestMalformedArgsFallsBack(t *testing.T) {
	req := BuildRequest("bash", CapExec, `{not json`)
	if req.Summary != "bash" {
		t.Errorf("summary = %q, want the tool name as a fallback", req.Summary)
	}
}

// TestWithApproverNilIsANoop: a surface with no approver must not end up with a
// non-nil interface holding a nil pointer, which is how "no approver" turns into a
// panic inside a tool call.
func TestWithApproverNilIsANoop(t *testing.T) {
	ctx := WithApprover(context.Background(), nil)
	if ApproverFrom(ctx) != nil {
		t.Error("a nil approver must leave the context without one")
	}
	var typed *scriptedApprover
	ctx = WithApprover(context.Background(), typed)
	if ApproverFrom(ctx) != nil {
		t.Error("a typed nil approver must not be installed: it would not compare equal to nil later")
	}
}

func TestDecisionAllowed(t *testing.T) {
	if !(Decision{Kind: DecisionAllowOnce}).Allowed() {
		t.Error("allow_once must allow")
	}
	if !(Decision{Kind: DecisionAllowTurn}).Allowed() {
		t.Error("allow_turn must allow")
	}
	if (Decision{Kind: DecisionDeny}).Allowed() {
		t.Error("deny must not allow")
	}
	if (Decision{}).Allowed() {
		t.Error("an empty decision must not allow: the zero value has to be the safe one")
	}
	if (Decision{Kind: "whatever"}).Allowed() {
		t.Error("an unknown decision kind must not allow")
	}
}

func TestDecisionKindValid(t *testing.T) {
	for _, k := range []DecisionKind{DecisionAllowOnce, DecisionAllowTurn, DecisionDeny} {
		if !k.Valid() {
			t.Errorf("%q should be valid", k)
		}
	}
	for _, k := range []DecisionKind{"", "yes", "ALLOW_ONCE "} {
		if k.Valid() {
			t.Errorf("%q should not be valid", k)
		}
	}
}

// TestGateUsesTheConfiguredTimeout: the request carries the wait so the surface can
// show a countdown, and so a surface with its own limit can be overridden.
func TestGateUsesTheConfiguredTimeout(t *testing.T) {
	inner := &recordingTool{name: "bash", cap: CapExec}
	approver := &scriptedApprover{decision: Decision{Kind: DecisionAllowOnce}}
	gated := Gate(inner, GateOptions{Policy: ApprovalPolicy{
		Mode: ApprovalWritesExec, Timeout: 90 * time.Second,
	}})

	if _, err := gated.(einotool.InvokableTool).InvokableRun(
		WithApprover(context.Background(), approver), `{"command":"ls"}`); err != nil {
		t.Fatal(err)
	}
	if got := approver.seen[0].Timeout; got != 90_000 {
		t.Errorf("timeout = %dms, want 90000", got)
	}
}

// jsonString renders a Go string as a JSON string literal for a test argument.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestGateKeepsTheCapabilityVisible is a regression guard for a trap that cost a
// real bug: embedding the Tool interface promotes only that interface's methods,
// so a gate that does not declare Capability reports CapRead for everything.
//
// FilterByCapabilities is how a surface is given a read-only registry. A gated
// write tool claiming to be a read tool is precisely the tool that would survive
// that filter.
func TestGateKeepsTheCapabilityVisible(t *testing.T) {
	for _, cap := range []Capability{CapRead, CapWrite, CapExec} {
		inner := WithCapability(&recordingTool{name: "t", cap: cap}, cap)
		gated := Gate(inner, GateOptions{Policy: ApprovalPolicy{Mode: ApprovalAll}})
		if got := CapabilityOf(gated); got != cap {
			t.Errorf("a gated %s tool reports %s", cap, got)
		}
	}

	// And through the filter that depends on it.
	reg := NewRegistry()
	if err := reg.Register(Gate(WithCapability(&recordingTool{name: "write_file", cap: CapWrite}, CapWrite),
		GateOptions{Policy: ApprovalPolicy{Mode: ApprovalWrites}})); err != nil {
		t.Fatal(err)
	}
	readOnly, err := FilterByCapabilities(reg, CapRead)
	if err != nil {
		t.Fatalf("FilterByCapabilities: %v", err)
	}
	if _, ok := readOnly.Get("write_file"); ok {
		t.Error("a gated write tool survived a read-only filter: the capability was lost by the wrapper")
	}
}
