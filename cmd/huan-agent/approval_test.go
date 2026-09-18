package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/prompt"
	agenttool "github.com/huan/huan-agent/internal/tool"
)

// TestApprovalSurfaceCanAsk pins which surfaces have a way to ask. It is the
// difference between "the tool asks first" and "the tool is not on the menu", so
// getting it wrong in either direction is bad: a surface that cannot ask with
// gated tools registered refuses every write, and one that can ask but withholds
// has lost a capability it could have had.
func TestApprovalSurfaceCanAsk(t *testing.T) {
	cases := map[string]bool{
		prompt.SurfaceWeb:    true,  // the console renders the card
		prompt.SurfaceCLI:    true,  // the REPL reads y/t/n
		prompt.SurfaceFeishu: false, // no card yet
		prompt.SurfaceRun:    false, // nobody is watching by definition
		"something-new":      false, // the safe default for a surface nobody has wired
		"":                   false,
	}
	for surface, want := range cases {
		if got := approvalSurfaceCanAsk(surface); got != want {
			t.Errorf("approvalSurfaceCanAsk(%q) = %v, want %v", surface, got, want)
		}
	}
}

// TestApprovalPolicyFromConfig: an unparseable mode is a configuration error, not
// a silent fallback to off. An operator who asked for a gate and got none would
// believe writes were being reviewed.
func TestApprovalPolicyFromConfig(t *testing.T) {
	cfg := &config.Config{}
	policy, err := approvalPolicyFor(cfg)
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	if policy.Mode != agenttool.ApprovalOff {
		t.Errorf("default mode = %q, want off", policy.Mode)
	}

	cfg.Tools.Approval.Mode = "writes+exec"
	cfg.Tools.Approval.Allow = []string{`写入 docs/`}
	policy, err = approvalPolicyFor(cfg)
	if err != nil {
		t.Fatalf("writes+exec: %v", err)
	}
	if policy.Mode != agenttool.ApprovalWritesExec {
		t.Errorf("mode = %q", policy.Mode)
	}
	if policy.Timeout <= 0 {
		t.Errorf("a policy without a timeout would wait forever: %+v", policy)
	}
	if len(policy.Allow) != 1 {
		t.Errorf("allow list was dropped: %+v", policy)
	}

	cfg.Tools.Approval.Mode = "sometimes"
	if _, err := approvalPolicyFor(cfg); err == nil {
		t.Error("an unknown mode must be refused")
	}
}

// TestApprovalGateWithholdsOnSurfacesThatCannotAsk is the spec's rule in code:
// with the gate on, a surface that cannot ask loses the gated tools entirely
// rather than being offered calls that can only fail.
func TestApprovalGateWithholdsOnSurfacesThatCannotAsk(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Approval.Mode = "writes+exec"

	// A surface that cannot ask: write and exec go, reads stay.
	gate := approvalGate{policy: mustPolicy(t, cfg), canAsk: false, surface: prompt.SurfaceRun,
		logger: zap.NewNop()}
	tools := []agenttool.Tool{
		&capTool{name: "read_file", cap: agenttool.CapRead},
		&capTool{name: "write_file", cap: agenttool.CapWrite},
		&capTool{name: "bash", cap: agenttool.CapExec},
	}
	kept := gate.applyToTools(tools)
	if len(kept) != 1 {
		t.Fatalf("kept %d tools, want only the read one", len(kept))
	}
	if name := toolName(kept[0]); name != "read_file" {
		t.Errorf("kept %q, want read_file", name)
	}

	// A surface that can ask: everything stays, wrapped.
	gate.canAsk, gate.surface = true, prompt.SurfaceWeb
	kept = gate.applyToTools(tools)
	if len(kept) != 3 {
		t.Fatalf("kept %d tools, want all three", len(kept))
	}
	for _, tl := range kept {
		if agenttool.CapabilityOf(tl) == agenttool.CapRead {
			continue
		}
		if _, ok := tl.(*agenttool.ApprovalGate); !ok {
			t.Errorf("%s is %T, want it gated", toolName(tl), tl)
		}
	}
}

// TestApprovalGateOffChangesNothing is the regression gate for every deployment
// that has not opted in.
func TestApprovalGateOffChangesNothing(t *testing.T) {
	gate := approvalGate{policy: agenttool.ApprovalPolicy{Mode: agenttool.ApprovalOff}, logger: zap.NewNop()}
	tools := []agenttool.Tool{
		&capTool{name: "read_file", cap: agenttool.CapRead},
		&capTool{name: "write_file", cap: agenttool.CapWrite},
		&capTool{name: "bash", cap: agenttool.CapExec},
	}
	kept := gate.applyToTools(tools)
	if len(kept) != 3 {
		t.Fatalf("kept %d tools, want all three", len(kept))
	}
	for i, tl := range kept {
		if tl != tools[i] {
			t.Errorf("tool %d was replaced with %T; an off gate must be a no-op", i, tl)
		}
	}
}

// TestApprovalGateRegistryPass covers the base registry, where the tools are
// assembled in four different places. A post-pass is what makes the coverage
// structural instead of something each of those places has to remember.
func TestApprovalGateRegistryPass(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Approval.Mode = "writes"

	reg := agenttool.NewRegistry()
	for _, tl := range []agenttool.Tool{
		agenttool.WithCapability(&capTool{name: "read_file", cap: agenttool.CapRead}, agenttool.CapRead),
		agenttool.WithCapability(&capTool{name: "write_file", cap: agenttool.CapWrite}, agenttool.CapWrite),
	} {
		if err := reg.Register(tl); err != nil {
			t.Fatal(err)
		}
	}

	gate := approvalGate{policy: mustPolicy(t, cfg), canAsk: true, surface: prompt.SurfaceWeb, logger: zap.NewNop()}
	if err := gate.applyToRegistry(t.Context(), reg); err != nil {
		t.Fatalf("applyToRegistry: %v", err)
	}
	if _, ok := reg.Get("read_file"); !ok {
		t.Error("a read tool must survive the gate")
	}
	got, ok := reg.Get("write_file")
	if !ok {
		t.Fatal("the write tool disappeared")
	}
	if _, gated := got.(*agenttool.ApprovalGate); !gated {
		t.Errorf("write_file is %T, want it gated", got)
	}

	// And with a surface that cannot ask, it is removed rather than left failing.
	gate.canAsk = false
	if err := gate.applyToRegistry(t.Context(), reg); err != nil {
		t.Fatalf("applyToRegistry (no asker): %v", err)
	}
	if _, ok := reg.Get("write_file"); ok {
		t.Error("a surface that cannot ask must not keep the gated tool")
	}
}

// TestRegistryPassCoversAllSurfaces is the structural claim: every surface's
// registry goes through registerBuiltinTools, so the gate cannot be forgotten for
// one of them.
func TestRegistryPassCoversAllSurfaces(t *testing.T) {
	for _, surface := range []string{prompt.SurfaceWeb, prompt.SurfaceCLI, prompt.SurfaceFeishu, prompt.SurfaceRun} {
		t.Run(surface, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Tools.Workspace = t.TempDir()
			cfg.Tools.EnableBash = true
			cfg.Tools.Approval.Mode = "writes+exec"

			reg := agenttool.NewRegistry()
			st := openTempStore(t)
			if err := registerBuiltinTools(reg, cfg, st, zaptest.NewLogger(t), nil,
				toolSetOptions{Surface: surface, AllowBackground: true}); err != nil {
				t.Fatalf("registerBuiltinTools: %v", err)
			}

			canAsk := approvalSurfaceCanAsk(surface)
			for _, name := range []string{"write_file", "edit_file", "bash"} {
				tl, ok := reg.Get(name)
				if !ok {
					if canAsk {
						t.Errorf("%s is missing on a surface that can ask", name)
					}
					continue
				}
				if !canAsk {
					t.Errorf("%s is present on a surface that cannot ask (it would refuse every call)", name)
					continue
				}
				if _, gated := tl.(*agenttool.ApprovalGate); !gated {
					t.Errorf("%s is %T, want it behind the gate", name, tl)
				}
			}
			// Reads are never withheld by the gate.
			if _, ok := reg.Get("read_file"); !ok {
				t.Error("read_file must survive on every surface")
			}
		})
	}
}

// --- the CLI approver ---

func TestParseApprovalAnswer(t *testing.T) {
	cases := []struct {
		line   string
		want   agenttool.DecisionKind
		reason string
	}{
		{"y", agenttool.DecisionAllowOnce, ""},
		{"Y", agenttool.DecisionAllowOnce, ""},
		{"yes", agenttool.DecisionAllowOnce, ""},
		{"allow", agenttool.DecisionAllowOnce, ""},
		{"t", agenttool.DecisionAllowTurn, ""},
		{"turn", agenttool.DecisionAllowTurn, ""},
		{"n", agenttool.DecisionDeny, ""},
		{"no", agenttool.DecisionDeny, ""},
		{"n 不要动主分支", agenttool.DecisionDeny, "不要动主分支"},
		// An empty line is not consent. This is the one behaviour the whole
		// mechanism exists to prevent getting wrong.
		{"", agenttool.DecisionDeny, ""},
		{"   ", agenttool.DecisionDeny, ""},
		// Free text is read as a reason, which is the most useful thing it can be.
		{"这个文件不要改", agenttool.DecisionDeny, "这个文件不要改"},
	}
	for _, tt := range cases {
		got, reason := parseApprovalAnswer(tt.line)
		if got != tt.want {
			t.Errorf("parseApprovalAnswer(%q) = %q, want %q", tt.line, got, tt.want)
		}
		if reason != tt.reason {
			t.Errorf("parseApprovalAnswer(%q) reason = %q, want %q", tt.line, reason, tt.reason)
		}
	}
}

// TestCLIApproverNonInteractiveRefuses: a piped session has nobody to ask, and the
// safe direction is refusal rather than a write nobody approved.
func TestCLIApproverNonInteractiveRefuses(t *testing.T) {
	var out bytes.Buffer
	a := newCLIApprover(func() (string, bool) { return "y", true }, &out, false)

	d, err := a.Approve(t.Context(), agenttool.Request{Tool: "bash", Capability: agenttool.CapExec,
		Summary: "执行: rm -rf /"})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if d.Allowed() {
		t.Error("a non-interactive session must not approve anything")
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be printed when there is nobody to read it: %q", out.String())
	}
}

// TestCLIApproverShowsTheRequestAndReadsTheAnswer: the terminal half of the gate.
func TestCLIApproverShowsTheRequestAndReadsTheAnswer(t *testing.T) {
	var out bytes.Buffer
	lines := []string{"y"}
	var i int
	a := newCLIApprover(func() (string, bool) {
		if i >= len(lines) {
			return "", false
		}
		l := lines[i]
		i++
		return l, true
	}, &out, true)

	req := agenttool.Request{
		Tool: "bash", Capability: agenttool.CapExec,
		Summary: "执行: git push origin main",
		Preview: []agenttool.PreviewLine{
			{Kind: agenttool.PreviewMeta, Text: "完整命令：git push origin main"},
			{Kind: agenttool.PreviewAdd, Text: "new line"},
			{Kind: agenttool.PreviewDel, Text: "old line"},
		},
	}
	d, err := a.Approve(t.Context(), req)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if !d.Allowed() || d.Kind != agenttool.DecisionAllowOnce {
		t.Errorf("decision = %+v", d)
	}
	shown := out.String()
	for _, want := range []string{"需要确认", "git push origin main", "完整命令", "+ new line", "- old line", "[y/t/n"} {
		if !strings.Contains(shown, want) {
			t.Errorf("the prompt is missing %q:\n%s", want, shown)
		}
	}
}

// TestCLIApproverEndOfInputRefuses: Ctrl-D at a confirmation prompt is not a yes.
func TestCLIApproverEndOfInputRefuses(t *testing.T) {
	var out bytes.Buffer
	a := newCLIApprover(func() (string, bool) { return "", false }, &out, true)

	d, err := a.Approve(t.Context(), agenttool.Request{Tool: "write_file", Capability: agenttool.CapWrite,
		Summary: "写入 a.go"})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if d.Allowed() {
		t.Error("end of input must be a refusal")
	}
	if !strings.Contains(out.String(), "输入结束") {
		t.Errorf("the session should say why: %q", out.String())
	}
}

// TestCLIApproverIsSerialised: two tools asking at once must not interleave their
// prompts, which would make the answer read against the wrong request.
func TestCLIApproverIsSerialised(t *testing.T) {
	var out bytes.Buffer
	var mu sync.Mutex
	order := []string{}
	answers := []string{"y", "n"}
	var i int
	a := newCLIApprover(func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(answers) {
			return "", false
		}
		l := answers[i]
		i++
		return l, true
	}, &out, true)

	var wg sync.WaitGroup
	for _, cmd := range []string{"first", "second"} {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			d, _ := a.Approve(t.Context(), agenttool.Request{Tool: "bash",
				Capability: agenttool.CapExec, Summary: "执行: " + c})
			mu.Lock()
			order = append(order, string(d.Kind))
			mu.Unlock()
		}(cmd)
	}
	wg.Wait()

	if len(order) != 2 {
		t.Fatalf("decisions = %v", order)
	}
	// One prompt's answer cannot be consumed by the other: without the lock a
	// goroutine could read a line typed for the prompt it never showed.
	shown := out.String()
	if strings.Count(shown, "需要确认") != 2 {
		t.Errorf("expected two complete prompts, got:\n%s", shown)
	}
}

// TestWorkspaceToolSetResolvesItsOwnGate: the console builds its per-turn bindings
// from a different place than the CLI does, so a gate that only covered the
// startup registry would leave every per-turn write ungated while looking
// configured.
func TestWorkspaceToolSetResolvesItsOwnGate(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Workspace = t.TempDir()
	cfg.Tools.EnableBash = true
	cfg.Tools.Approval.Mode = "writes"

	st := openTempStore(t)
	set := newWorkspaceToolSet(cfg, st, zap.NewNop(), toolSetOptions{Surface: prompt.SurfaceWeb})
	if !set.gate.active() {
		t.Fatal("the tool set did not pick up the configured gate")
	}
	if set.gate.policy.Mode != agenttool.ApprovalWrites {
		t.Errorf("gate mode = %q, want writes", set.gate.policy.Mode)
	}
	if !set.gate.canAsk {
		t.Error("the web surface can ask, so the gate should know it")
	}
}

// capTool is a tool with a declared capability and nothing else.
type capTool struct {
	name string
	cap  agenttool.Capability
}

func (c *capTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: c.name, Desc: "cap"}, nil
}

func (c *capTool) Capability() agenttool.Capability { return c.cap }

func (c *capTool) InvokableRun(context.Context, string, ...einotool.Option) (string, error) {
	return "ok", nil
}

func mustPolicy(t *testing.T, cfg *config.Config) agenttool.ApprovalPolicy {
	t.Helper()
	p, err := approvalPolicyFor(cfg)
	if err != nil {
		t.Fatalf("approvalPolicyFor: %v", err)
	}
	return p
}

// TestEveryRegisteredToolDeclaresConcurrency is the guard the design needs: the
// default (Serial) is the safe behaviour, so a tool that nobody thought about
// works — and silently. This test makes "nobody thought about it" a failure, which
// is the only way the declaration table stays true as tools are added.
//
// It runs over every surface, because the surfaces register different sets: a tool
// that only exists on Feishu is still a tool that runs.
func TestEveryRegisteredToolDeclaresConcurrency(t *testing.T) {
	for _, surface := range []string{prompt.SurfaceWeb, prompt.SurfaceCLI, prompt.SurfaceFeishu, prompt.SurfaceRun} {
		t.Run(surface, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Tools.Workspace = t.TempDir()
			cfg.Tools.EnableBash = true
			cfg.Tools.EnableBackground = true

			reg := agenttool.NewRegistry()
			st := openTempStore(t)
			if err := registerBuiltinTools(reg, cfg, st, zaptest.NewLogger(t), nil,
				toolSetOptions{Surface: surface, AllowBackground: true}); err != nil {
				t.Fatalf("registerBuiltinTools: %v", err)
			}

			undeclared := agenttool.UndeclaredTools(t.Context(), reg)
			if len(undeclared) > 0 {
				t.Errorf("these tools never declared whether they may run in parallel: %v\n"+
					"Add agenttool.WithConcurrency(...) where they are registered: ParallelSafe for a "+
					"pure read, Serial for anything that writes, blocks, or touches shared state.",
					undeclared)
			}

			// And the declarations are usable, not just present.
			modes := agenttool.DescribeConcurrency(t.Context(), reg)
			for name, mode := range modes {
				if err := agenttool.ValidateConcurrency(name, agenttool.Concurrency(mode)); err != nil {
					t.Errorf("%v", err)
				}
			}
		})
	}
}

// TestConcurrencyExpectations pins the modes that matter, so a later edit cannot
// quietly make a write parallel-safe or a read a barrier.
func TestConcurrencyExpectations(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Workspace = t.TempDir()
	cfg.Tools.EnableBash = true
	cfg.Tools.EnableBackground = true
	cfg.Chat.Plan.Enable = true

	jobMgr, err := newJobManager(cfg, zap.NewNop())
	if err != nil {
		t.Fatalf("job manager: %v", err)
	}
	defer jobMgr.Close()

	reg := agenttool.NewRegistry()
	st := openTempStore(t)
	if err := registerBuiltinTools(reg, cfg, st, zaptest.NewLogger(t), nil,
		toolSetOptions{Surface: prompt.SurfaceWeb, Jobs: jobMgr}); err != nil {
		t.Fatalf("registerBuiltinTools: %v", err)
	}
	modes := agenttool.DescribeConcurrency(t.Context(), reg)

	// Tools this surface must always have. Without this list the table below could
	// pass by checking nothing at all.
	for _, name := range []string{
		"read_file", "grep", "write_file", "edit_file", "bash",
		"ask_user", "plan_update", "bash_background", "bash_output",
	} {
		if _, ok := modes[name]; !ok {
			t.Errorf("%s is not registered on the web surface", name)
		}
	}

	want := map[string]agenttool.Concurrency{
		// Reads that observe and touch nothing.
		"read_file": agenttool.ParallelSafe,
		"list_dir":  agenttool.ParallelSafe,
		"glob":      agenttool.ParallelSafe,
		"grep":      agenttool.ParallelSafe,
		"time":      agenttool.ParallelSafe,
		"calc":      agenttool.ParallelSafe,
		"echo":      agenttool.ParallelSafe,
		// Read-only in capability, and none of them may overlap: ask_user parks
		// the turn, the plan tools are a read-modify-write on one shared plan, and
		// save_document writes to a store.
		"ask_user":    agenttool.Serial,
		"plan_create": agenttool.Serial,
		"plan_add":    agenttool.Serial,
		"plan_update": agenttool.Serial,
		"plan_read":   agenttool.Serial,
		// Writes and commands.
		"write_file":      agenttool.Serial,
		"edit_file":       agenttool.Serial,
		"bash":            agenttool.Serial,
		"bash_background": agenttool.Serial,
		"bash_stop":       agenttool.Serial,
		// Reading a job's log is the one background tool a model calls several
		// times in one breath, while watching a build.
		"bash_output": agenttool.ParallelSafe,
	}
	for name, mode := range want {
		got, ok := modes[name]
		if !ok {
			// Registered only on some setups (bash_stop needs a job manager's
			// fuller surface). The presence list above is what fails when a tool
			// disappears; this table is about the modes of the ones that exist.
			continue
		}
		if got != mode {
			t.Errorf("%s declares %q, want %q", name, got, mode)
		}
	}
	// save_document needs a document console, which this harness does not build.
	// Its declaration is in the same registration function as the others and is
	// covered by the "every tool declares" test whenever a console is wired.
	if _, ok := modes["save_document"]; ok {
		if modes["save_document"] != agenttool.Serial {
			t.Errorf("save_document declares %q, want serial", modes["save_document"])
		}
	}
}
