package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	gctx "github.com/huan/huan-agent/internal/context"
	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/tool/builtin"
)

// The guard's own tests. They are unit tests against turnProgress rather than
// runs of the whole loop, because the interesting cases are sequences of steps
// that a scripted model would have to reproduce exactly — and because a guard
// that fires wrongly costs a working turn, which is worth pinning down case by
// case.

// guardRegistry is the registry the guard classifies against: a read tool, a
// write tool, and a bash tool that declares CapExec — which is the case worth
// testing, because a bash call has to be read back to what its command actually
// does rather than trusted by its declaration.
func guardRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	noop := func(context.Context, string) (string, error) { return "ok", nil }
	reg := tool.NewRegistry()
	entries := []struct {
		name string
		cap  tool.Capability
	}{
		{"read_file", tool.CapRead},
		{"write_file", tool.CapWrite},
		{"bash", tool.CapExec},
		// Declared read-only by the real registration, which is why the guard
		// names it explicitly.
		{"spawn_agent", tool.CapRead},
	}
	for _, e := range entries {
		ft := &fakeTool{name: e.name, desc: e.name, run: noop}
		if err := reg.Register(tool.WithCapability(ft, e.cap)); err != nil {
			t.Fatalf("register %s: %v", e.name, err)
		}
	}
	return reg
}

func TestCommandChangesNothing(t *testing.T) {
	cases := []struct {
		command string
		want    bool
		why     string
	}{
		{"ls -la", true, "listing"},
		{"cd /tmp && grep -n foo bar.go | head -20", true, "a pipeline of readers"},
		{"sed -n '1,120p' internal/server/chat.go", true, "a range read"},
		{"cat internal/store/media.go 2>/dev/null", true, "silencing a reader is still reading"},
		{"git status --short", true, "a read-only git subcommand"},
		{"git log --oneline | head -5", true, "read-only git in a pipeline"},
		{"git branch --list", true, "listing branches"},
		{"git config --get user.email", true, "reading a config value"},
		// ... and the same commands writing, which a plain whitelist got wrong:
		// these sat on the stop path, so a turn doing real work could be stopped
		// for "repeating a read".
		{"git branch -D feature", false, "deleting a branch"},
		{"git tag -d v1", false, "deleting a tag"},
		{"git config user.email a@b.c", false, "writing a config value"},
		{"git remote add origin git@x:y", false, "adding a remote"},
		{"git stash", false, "stashing"},
		{"git fetch --all", false, "fetching"},
		{"wc -l internal/chat/runner.go", true, "counting"},
		{"echo hello", true, "echo changes nothing"},
		// The other direction: anything that writes, runs a build, or cannot be
		// recognised counts as a change, because a false "read" can stop a turn
		// that is working while a false "change" only keeps the guard quiet.
		{"go build ./...", false, "a build is work"},
		{"go test ./internal/chat/", false, "a test run is work"},
		{"cat a.go > b.go", false, "a redirect writes"},
		{"grep foo bar.go > out.txt", false, "a redirect writes even next to a reader"},
		{"rm -rf build", false, "destructive"},
		{"git checkout -- .", false, "git can write"},
		{"find . -name '*.go' -delete", false, "-delete writes"},
		{"sed -i 's/a/b/' file.go", false, "in-place sed writes"},
		{"cat file.go | python3 -c 'import sys'", false, "an interpreter can do anything"},
		{"npm install", false, "installs"},
		{"", false, "an empty command is not a read"},
		{"mystery-tool --flag", false, "unrecognised verbs are treated as work"},
	}
	for _, tc := range cases {
		if got := commandChangesNothing(tc.command); got != tc.want {
			t.Errorf("commandChangesNothing(%q) = %v, want %v (%s)", tc.command, got, tc.want, tc.why)
		}
	}
}

// TestClassifyEffectTreatsUndeclaredToolsAsWork pins the rule that keeps the
// guard out of MCP's way: internal/mcp registers every tool without declaring a
// capability, and the default reads as "harmless". A turn whose work happens
// through MCP (create_pr, push, send_message) would otherwise be steered for
// idling and stopped for repeating itself.
func TestClassifyEffectTreatsUndeclaredToolsAsWork(t *testing.T) {
	reg := guardRegistry(t)
	undeclared := &fakeTool{name: "mcp__git__create_pr", desc: "opens a PR", run: func(context.Context, string) (string, error) {
		return "opened", nil
	}}
	if err := reg.Register(undeclared); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := classifyEffect(reg, ToolRun{Name: "mcp__git__create_pr", Args: "{}"}); got != effectChange {
		t.Errorf("an undeclared tool was classified as %v, want work", got)
	}
	// A tool nothing knows about is work too: guessing "read" would let a stream
	// of failing calls look idle.
	if got := classifyEffect(reg, ToolRun{Name: "no_such_tool", Args: "{}"}); got != effectChange {
		t.Errorf("an unknown tool was classified as %v, want work", got)
	}
}

// TestClassifyEffectResearchIsNotIdle: a turn that reads the web is a research
// turn, not a stuck one — the failure the idle rule exists for is a turn
// re-reading its own workspace.
func TestClassifyEffectResearchIsNotIdle(t *testing.T) {
	reg := guardRegistry(t)
	for _, name := range []string{"web_search", "web_fetch", "describe_image"} {
		if got := classifyEffect(reg, ToolRun{Name: name, Args: `{"url":"https://example.com"}`}); got != effectNeutral {
			t.Errorf("%s was classified as %v, want neutral (neither progress nor idling)", name, got)
		}
	}
}

func TestClassifyEffectUsesCapabilitiesAndReadsBashBack(t *testing.T) {
	reg := guardRegistry(t)
	cases := []struct {
		run  ToolRun
		want effect
	}{
		{ToolRun{Name: "read_file", Args: `{"path":"a.go"}`}, effectRead},
		{ToolRun{Name: "write_file", Args: `{"path":"a.go","content":"x"}`}, effectChange},
		{ToolRun{Name: "bash", Args: `{"command":"grep -rn foo ."}`}, effectRead},
		{ToolRun{Name: "bash", Args: `{"command":"go build ./..."}`}, effectChange},
		{ToolRun{Name: "plan_create", Args: `{}`}, effectChange},
		{ToolRun{Name: "plan_read", Args: `{}`}, effectRead},
		{ToolRun{Name: "ask_user", Args: `{}`}, effectNeutral},
		// spawn_agent declares CapRead (it changes nothing here), but a turn that
		// delegates is working, not idling.
		{ToolRun{Name: "spawn_agent", Args: `{"tasks":["read the store layer"]}`}, effectChange},
	}
	for _, tc := range cases {
		if got := classifyEffect(reg, tc.run); got != tc.want {
			t.Errorf("classifyEffect(%s %s) = %v, want %v", tc.run.Name, tc.run.Args, got, tc.want)
		}
	}
}

// TestGuardSteersBeforeItStops is the property that keeps the guard from being a
// step cap in disguise: the model is told what it is repeating, and only a
// repeat it keeps making ends the turn.
func TestGuardSteersBeforeItStops(t *testing.T) {
	reg := guardRegistry(t)
	p := newTurnProgress(GuardConfig{})
	read := []ToolRun{{Name: "bash", Args: `{"command":"grep -n TODO internal/chat/runner.go"}`}}

	var steers int
	for i := 1; i <= DefaultRepeatStop; i++ {
		s := p.observe(reg, read)
		switch {
		case i < DefaultRepeatNudge:
			if !s.empty() {
				t.Fatalf("step %d: the guard spoke before the nudge threshold: %+v", i, s)
			}
		case i < DefaultRepeatStop:
			// Told once, not once per step: the same complaint repeated on every
			// step is noise the model learns to skip, and it is re-sent on every
			// later step anyway because it stays in the window.
			if i == DefaultRepeatNudge {
				if s.Text == "" || s.StopReason != "" {
					t.Fatalf("step %d: want a steering message and no stop, got %+v", i, s)
				}
				if s.Kind != SteerRepeat {
					t.Errorf("step %d: Kind = %q, want %q", i, s.Kind, SteerRepeat)
				}
			} else if !s.empty() {
				t.Fatalf("step %d: the same complaint was sent twice: %+v", i, s)
			}
		default:
			if s.StopReason != StopLoop {
				t.Fatalf("step %d: StopReason = %q, want %q", i, s.StopReason, StopLoop)
			}
			if s.Detail == "" {
				t.Error("a loop stop must say what was repeated")
			}
		}
		if s.Kind == SteerRepeat && s.StopReason == "" {
			steers++
		}
	}
	if steers != 1 {
		t.Errorf("steered %d times, want exactly 1 before the stop", steers)
	}
}

// TestGuardIgnoresARepeatedCommandAfterAnEdit: running the same build or test
// again after changing a file is how work is done, not a loop.
func TestGuardIgnoresARepeatedCommandAfterAnEdit(t *testing.T) {
	reg := guardRegistry(t)
	p := newTurnProgress(GuardConfig{})
	build := []ToolRun{{Name: "bash", Args: `{"command":"go test ./..."}`}}

	for i := 0; i < 6; i++ {
		if s := p.observe(reg, build); s.StopReason != "" {
			t.Fatalf("iteration %d: the guard stopped a test run that follows an edit: %+v", i, s)
		}
		if s := p.observe(reg, []ToolRun{{Name: "write_file", Args: `{"path":"a.go","content":"x"}`}}); !s.empty() {
			t.Fatalf("iteration %d: editing a file must not be an event: %+v", i, s)
		}
	}
}

// TestGuardSteersOnIdleButNeverStopsUnwarned: a long read-only stretch is steered
// first, and the stop only ever follows a steering message.
func TestGuardSteersOnIdleButNeverStopsUnwarned(t *testing.T) {
	reg := guardRegistry(t)
	rules := guardRules{
		repeatNudge: 3, repeatStop: 5,
		idleNudge: 4, idleStop: 8,
		maxSteers: 6, rereadNudge: 100, // rereads off: this test is about idling
	}
	p := newTurnProgress(GuardConfig{IdleNudgeSteps: 4, IdleStopSteps: 8, RereadNudge: 100})

	// Each step reads a different file, so only the idle streak can fire.
	var steers, stops int
	for i := 0; i < 12; i++ {
		run := ToolRun{Name: "read_file", Args: `{"path":"file` + string(rune('a'+i)) + `.go"}`}
		s := p.observe(reg, []ToolRun{run})
		if s.Text != "" {
			steers++
			if s.Kind != SteerIdle {
				t.Errorf("step %d: Kind = %q, want %q", i+1, s.Kind, SteerIdle)
			}
		}
		if s.StopReason != "" {
			stops++
			if s.StopReason != StopIdle {
				t.Errorf("step %d: StopReason = %q, want %q", i+1, s.StopReason, StopIdle)
			}
			if steers == 0 {
				t.Fatal("the turn was stopped without ever being steered")
			}
			break
		}
	}
	if steers == 0 {
		t.Error("the guard never steered an idle stretch")
	}
	if stops == 0 {
		t.Errorf("the guard never stopped after %d idle steps (rules: nudge %d, stop %d)",
			12, rules.idleNudge, rules.idleStop)
	}
}

// TestGuardResetsIdleOnAChange: any step that changes something starts the idle
// count over, which is what keeps a productive turn out of the guard's way.
func TestGuardResetsIdleOnAChange(t *testing.T) {
	reg := guardRegistry(t)
	p := newTurnProgress(GuardConfig{IdleNudgeSteps: 3, IdleStopSteps: 9, RereadNudge: 100})
	read := ToolRun{Name: "read_file", Args: `{"path":"a.go"}`}
	write := ToolRun{Name: "write_file", Args: `{"path":"a.go","content":"x"}`}

	for i := 0; i < 5; i++ {
		if s := p.observe(reg, []ToolRun{read, read}); s.Text != "" {
			t.Fatalf("iteration %d: steered during a working turn: %+v", i, s)
		}
		p.observe(reg, []ToolRun{write})
	}
	if p.idle != 0 {
		t.Errorf("idle = %d after 5 iterations ending in a change, want 0", p.idle)
	}
}

// TestGuardSpeaksAtMostMaxSteers: every steering message is replayed on every
// later step, so a model that ignores them must not accumulate a paragraph of
// them.
func TestGuardSpeaksAtMostMaxSteers(t *testing.T) {
	reg := guardRegistry(t)
	// Twenty different files, each read twice, with a low reread threshold: the
	// guard has something to say about every one of them, and the budget is what
	// stops it saying it.
	p := newTurnProgress(GuardConfig{MaxSteers: 2, RereadNudge: 2, IdleNudgeSteps: 100, IdleStopSteps: 100})
	steers := 0
	for round := 0; round < 2; round++ {
		for i := 0; i < 20; i++ {
			s := p.observe(reg, []ToolRun{{Name: "read_file", Args: `{"path":"f` + string(rune('a'+i)) + `.go"}`}})
			if s.Text != "" && s.StopReason == "" {
				steers++
			}
		}
	}
	if steers > 2 {
		t.Errorf("steered %d times, want at most 2", steers)
	}
	if steers == 0 {
		t.Error("the guard never steered at all, so the budget was not what limited it")
	}
}

// TestGuardDisabledIsSilent: the escape hatch has to be complete, or a
// deployment that turned the guard off still gets its messages in the window.
func TestGuardDisabledIsSilent(t *testing.T) {
	reg := guardRegistry(t)
	p := newTurnProgress(GuardConfig{Disable: true})
	if p != nil {
		t.Fatal("a disabled guard must be nil, so nothing has to check a flag per step")
	}
	for i := 0; i < 100; i++ {
		if s := p.observe(reg, []ToolRun{{Name: "bash", Args: `{"command":"ls"}`}}); !s.empty() {
			t.Fatalf("step %d: %+v", i, s)
		}
	}
}

func TestCapToolResultKeepsBothEnds(t *testing.T) {
	body := strings.Repeat("x", 500) + "THE END"
	got := capToolResult(body, 100)
	if len(got) >= len(body) {
		t.Fatalf("result was not bounded: %d bytes", len(got))
	}
	if !strings.Contains(got, "[结果过长]") {
		t.Errorf("the cut is not announced: %q", got)
	}
	if !strings.HasSuffix(got, "THE END") {
		t.Errorf("the tail was dropped, and the tail is where the error usually is: %q", got)
	}
	if !strings.HasPrefix(got, "xxxx") {
		t.Errorf("the head was dropped, and the head says what ran: %q", got)
	}

	// A result that fits is untouched, and "no bound" means no bound.
	if small := capToolResult("done", 100); small != "done" {
		t.Errorf("capToolResult(small) = %q", small)
	}
	if big := capToolResult(body, 0); big != body {
		t.Error("a zero cap must mean unbounded, not empty")
	}
	if big := capToolResult(body, -1); big != body {
		t.Error("a negative cap must mean unbounded")
	}
}

func TestResolvedToolResultCap(t *testing.T) {
	if got := resolvedToolResultCap(0); got != DefaultToolResultMaxChars {
		t.Errorf("resolvedToolResultCap(0) = %d, want the default %d", got, DefaultToolResultMaxChars)
	}
	if got := resolvedToolResultCap(-5); got != 0 {
		t.Errorf("resolvedToolResultCap(-5) = %d, want 0 (unbounded)", got)
	}
	if got := resolvedToolResultCap(1234); got != 1234 {
		t.Errorf("resolvedToolResultCap(1234) = %d, want 1234", got)
	}
}

// TestLedgerRecordsWhatTheTurnMustNotForget is the fix for the failure this file
// exists for: a compressed window keeps what happened and loses what was
// decided, so the runner keeps its own record of the goal, the plan and the
// files already written.
func TestLedgerRecordsWhatTheTurnMustNotForget(t *testing.T) {
	l := newTurnLedger("加一个「产物」的功能，可以保存 agent 执行过程中的各种资源")

	plan, err := json.Marshal(builtin.PlanOutput{
		Status:    "updated",
		Summary:   "已完成 1/3 · 进行中：落地 store 层",
		Checklist: "[x] t1 读齐既有实现\n[~] t2 落地 store 层\n[ ] t3 前端抽屉",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	l.observe([]ToolRun{
		{Name: "plan_create", Result: string(plan)},
		{Name: "write_file", Result: `{"bytes":12}`, Args: `{"path":"internal/store/artifacts.go","content":"x"}`},
		{Name: "bash", Result: "ok", Args: `{"command":"go build ./..."}`},
		{Name: "bash", Result: "ok", Args: `{"command":"grep -rn artifact internal/"}`},
		{Name: "read_file", Result: "…", Args: `{"path":"internal/store/media.go"}`},
	})

	got := l.render()
	for _, want := range []string{
		"用户要求", "产物",
		"计划：已完成 1/3", "落地 store 层",
		"internal/store/artifacts.go",
		"go build ./...",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the ledger does not mention %q:\n%s", want, got)
		}
	}
	// A read-only command is not work: recording it would fill the ledger with
	// the exploration it exists to make unnecessary.
	if strings.Contains(got, "grep -rn artifact") {
		t.Errorf("the ledger recorded a read-only command:\n%s", got)
	}
}

func TestWithLedgerGoesRightAfterThePinnedHead(t *testing.T) {
	window := []*schema.Message{
		{Role: schema.System, Content: "SYSTEM"},
		{Role: schema.System, Content: "SURFACE"},
		{Role: schema.System, Content: "summary"},
		{Role: schema.User, Content: "the goal"},
		{Role: schema.Assistant, Content: "…"},
	}
	out := withLedger(window, 2, "【本轮台账】x")
	if len(out) != len(window)+1 {
		t.Fatalf("window grew by %d, want 1", len(out)-len(window))
	}
	if out[2].Content != "【本轮台账】x" {
		t.Errorf("the ledger landed at %d: %v", 2, contentsOf(out))
	}
	if out[0].Content != "SYSTEM" || out[len(out)-1].Content != "…" {
		t.Errorf("the window was reordered: %v", contentsOf(out))
	}
	// Nothing to say means nothing added.
	if same := withLedger(window, 2, "   "); len(same) != len(window) {
		t.Errorf("an empty ledger was inserted anyway: %v", contentsOf(same))
	}
}

func TestLedgerRenderIsBounded(t *testing.T) {
	l := newTurnLedger(strings.Repeat("目", 2000))
	for i := 0; i < 50; i++ {
		l.observe([]ToolRun{
			{Name: "write_file", Result: "{}", Args: `{"path":"file` + string(rune('a'+i%26)) + `.go"}`},
			{Name: "bash", Result: "ok", Args: `{"command":"go build ./cmd/` + string(rune('a'+i%26)) + `"}`},
		})
	}
	got := l.render()
	if len(got) > 4000 {
		t.Errorf("the ledger is %d bytes: it is re-sent every step and must stay small", len(got))
	}
	if len(l.writes) > 20 || len(l.commands) > 6 {
		t.Errorf("ledger lists grew unbounded: %d writes, %d commands", len(l.writes), len(l.commands))
	}
}

func TestNewTurnProgressNilWhenDisabled(t *testing.T) {
	if p := newTurnProgress(GuardConfig{}); p == nil {
		t.Fatal("the guard is on by default")
	}
	if p := newTurnProgress(GuardConfig{Disable: true}); p != nil {
		t.Fatal("Disable must produce a nil guard")
	}
}

// TestGuardRulesKeepStopAboveNudge pins the ordering the guard's messaging
// depends on, for every way a deployment can set the two thresholds wrong.
func TestGuardRulesKeepStopAboveNudge(t *testing.T) {
	cases := []GuardConfig{
		{RepeatNudge: 5, RepeatStop: 2},
		{IdleNudgeSteps: 30, IdleStopSteps: 10},
		{RepeatStop: 1},
		{IdleNudgeSteps: -3},
	}
	for _, cfg := range cases {
		r, on := cfg.rules()
		if !on {
			t.Fatal("the guard is off")
		}
		if r.repeatStop <= r.repeatNudge {
			t.Errorf("cfg %+v: repeatStop %d <= repeatNudge %d", cfg, r.repeatStop, r.repeatNudge)
		}
		if r.idleStop <= r.idleNudge {
			t.Errorf("cfg %+v: idleStop %d <= idleNudge %d", cfg, r.idleStop, r.idleNudge)
		}
		if r.maxSteers <= 0 || r.rereadNudge <= 0 {
			t.Errorf("cfg %+v: thresholds must be positive: %+v", cfg, r)
		}
	}
}

// ---------------------------------------------------------------- end to end --

// scriptedCalls builds a model that makes one tool call per step and then
// answers, which is what a turn that is never stopped would do.
func scriptedCalls(n int, name, args string) *fakeModel {
	turns := make([]*schema.Message, 0, n+1)
	for i := 0; i < n; i++ {
		turns = append(turns, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID:   "c",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      name,
				Arguments: args,
			},
		}}})
	}
	turns = append(turns, &schema.Message{Role: schema.Assistant, Content: "done"})
	return &fakeModel{turns: turns}
}

// TestRun_StopsALoopThatIgnoresTheSteer is the whole point of the guard, run
// through the real loop: a model that repeats one call forever stops after the
// stop threshold with a reason the caller can report, instead of burning the
// step budget it was given.
func TestRun_StopsALoopThatIgnoresTheSteer(t *testing.T) {
	m := scriptedCalls(50, "read_file", `{"path":"internal/chat/runner.go"}`)
	tl := &fakeTool{name: "read_file", desc: "r", run: func(context.Context, string) (string, error) {
		return "the whole file, yet again", nil
	}}
	// Declared CapRead, as the real registration declares it: an undeclared tool
	// is treated as work (see the MCP case in classifyEffect), so a bare fake
	// would not exercise the repeat rule at all.
	reg := tool.NewRegistry()
	if err := reg.Register(tool.WithCapability(tl, tool.CapRead)); err != nil {
		t.Fatalf("register: %v", err)
	}
	r, err := New(Config{
		Model:    m,
		Tools:    reg,
		MaxSteps: 50,
		Guard:    GuardConfig{RepeatNudge: 3, RepeatStop: 5},
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	events, emit := collect()
	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "read it until you get it"}},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopLoop {
		t.Fatalf("StopReason = %q (text %q), want %q", res.StopReason, res.Text, StopLoop)
	}
	if res.Steps != 5 {
		t.Errorf("Steps = %d, want 5: the guard stops at the repeat threshold, not at the step cap", res.Steps)
	}
	if !strings.Contains(res.Text, "重复") {
		t.Errorf("the answer does not explain the stop: %q", res.Text)
	}
	if !res.BudgetExhausted() {
		t.Error("a guard stop must count as a turn that did not answer")
	}

	var steered, stopped bool
	for _, e := range *events {
		switch e.Type {
		case EventSteer:
			steered = true
			if e.SteerKind != SteerRepeat || e.Text == "" {
				t.Errorf("steer event = %+v", e)
			}
		case EventBudgetStop:
			stopped = true
			if e.Reason != StopLoop {
				t.Errorf("budget_stop reason = %q", e.Reason)
			}
		}
	}
	if !steered {
		t.Error("no steer event: the console cannot show that the model was warned")
	}
	if !stopped {
		t.Error("no budget_stop event")
	}

	// The steering message has to reach the model, not just the console: it is
	// the model's only chance to change course before the stop. It travels as a
	// system message so it can never be mistaken for the person speaking, and so
	// it does not take the pinned "most recent user message" slot away from the
	// request the turn is actually answering.
	warned := false
	for _, window := range m.gotMessages {
		for _, msg := range window {
			if strings.Contains(msg.Content, "系统提醒") {
				warned = true
				if msg.Role != schema.System {
					t.Errorf("the steering message arrived as role %q, want %q", msg.Role, schema.System)
				}
			}
		}
	}
	if !warned {
		t.Error("the steering message never reached the model")
	}
}

// TestRun_StopsATurnThatOnlyLooks is the other half: nothing is repeated, and
// nothing is done either. This is the shape the incident had — hundreds of
// steps of grep and sed — and the guard ends it instead of paying for the whole
// step budget.
func TestRun_StopsATurnThatOnlyLooks(t *testing.T) {
	// A distinct file each step, so only the idle streak can fire.
	turns := make([]*schema.Message, 0, 30)
	for i := 0; i < 30; i++ {
		turns = append(turns, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID:   "c",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "bash",
				Arguments: `{"command":"sed -n '1,40p' internal/store/file` + string(rune('a'+i)) + `.go"}`,
			},
		}}})
	}
	m := &fakeModel{turns: turns}
	// bash declares CapExec, so this is exactly the case where the capability
	// alone would say "work is happening".
	tl := &fakeTool{name: "bash", desc: "sh", run: func(context.Context, string) (string, error) {
		return "package store", nil
	}}
	reg := tool.NewRegistry()
	if err := reg.Register(tool.WithCapability(tl, tool.CapExec)); err != nil {
		t.Fatalf("register: %v", err)
	}
	r, err := New(Config{
		Model:    m,
		Tools:    reg,
		MaxSteps: 30,
		Guard:    GuardConfig{IdleNudgeSteps: 4, IdleStopSteps: 8, RereadNudge: 100},
		Logger:   zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{{Role: schema.User, Content: "have a look around"}},
	}, func(Event) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopIdle {
		t.Fatalf("StopReason = %q (text %q), want %q", res.StopReason, res.Text, StopIdle)
	}
	if res.Steps != 8 {
		t.Errorf("Steps = %d, want 8 (the idle stop rule)", res.Steps)
	}
	if !strings.Contains(res.Text, "空转") {
		t.Errorf("the answer does not explain the stop: %q", res.Text)
	}
}

// TestRun_CompressedWindowCarriesTheLedger: after a compression the model must
// still be able to see what the turn has established. Without this it re-derives
// the same decisions from the repository, over and over, which is the failure
// this whole change is about.
func TestRun_CompressedWindowCarriesTheLedger(t *testing.T) {
	m := scriptedCalls(20, "bash", `{"command":"cat internal/store/media.go"}`)
	tl := &fakeTool{name: "bash", desc: "sh", run: func(context.Context, string) (string, error) {
		return strings.Repeat("a long file listing that fills the window. ", 40), nil
	}}
	sum := &countingSummarizer{}
	mgr, err := gctx.NewManager(gctx.Budget{MaxTokens: 2000, KeepRecent: 3, Summarizer: sum}, nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	r, err := New(Config{
		Model:    m,
		Tools:    newRegistry(t, tl),
		MaxSteps: 25,
		// The guard is off here: the point is what the window carries after a
		// compression, and this script repeats one read on purpose.
		Guard:     GuardConfig{Disable: true},
		Condenser: mgr,
		Logger:    zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := r.Run(context.Background(), Request{
		Messages: []*schema.Message{
			{Role: schema.System, Content: "SYSTEM"},
			{Role: schema.User, Content: "把产物功能加上"},
		},
	}, func(Event) {}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.calls == 0 {
		t.Fatal("the window was never compressed, so this test proves nothing")
	}

	last := m.gotMessages[len(m.gotMessages)-1]
	found := false
	for _, msg := range last {
		if strings.Contains(msg.Content, "本轮台账") {
			found = true
			if !strings.Contains(msg.Content, "把产物功能加上") {
				t.Errorf("the ledger lost the goal: %q", msg.Content)
			}
		}
	}
	if !found {
		t.Errorf("the compressed window carries no ledger: %v", contentsOf(last))
	}
}

// TestRereadCountsPartsNotCalls: reading one large file in six slices is how a
// large file is read at all, and the first version of this rule told the model
// to stop doing it.
func TestRereadCountsPartsNotCalls(t *testing.T) {
	reg := guardRegistry(t)
	p := newTurnProgress(GuardConfig{RereadNudge: 3, IdleNudgeSteps: 100, IdleStopSteps: 200})
	// Six consecutive windows of one file: no steering message, because each read
	// is a different part.
	for i := 0; i < 6; i++ {
		args := fmt.Sprintf(`{"path":"big.go","offset":%d,"limit":120}`, i*120)
		if s := p.observe(reg, []ToolRun{{Name: "read_file", Args: args}}); s.Text != "" {
			t.Fatalf("chunk %d was treated as a reread: %s", i, s.Text)
		}
	}
	// The same window three times is a reread, and it is named as such.
	var said string
	for i := 0; i < 4; i++ {
		if s := p.observe(reg, []ToolRun{{Name: "read_file", Args: `{"path":"big.go","offset":0,"limit":120}`}}); s.Text != "" {
			said = s.Text
			break
		}
	}
	if said == "" {
		t.Fatal("reading the same window four times was not noticed")
	}
	if !strings.Contains(said, "big.go") {
		t.Errorf("the message does not name the file: %q", said)
	}

	// The same for bash: a chunked sed read is not a reread, a repeated one is.
	q := newTurnProgress(GuardConfig{RereadNudge: 3, IdleNudgeSteps: 100, IdleStopSteps: 200})
	for i := 0; i < 6; i++ {
		cmd := fmt.Sprintf(`{"command":"sed -n '%d,%dp' big.go"}`, i*100+1, (i+1)*100)
		if s := q.observe(reg, []ToolRun{{Name: "bash", Args: cmd}}); s.Text != "" {
			t.Fatalf("sed chunk %d was treated as a reread: %s", i, s.Text)
		}
	}
}
