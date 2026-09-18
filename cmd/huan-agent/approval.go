package main

// The approval gate at the command layer: which surfaces can ask, how the
// configured policy maps onto the tool set, and the CLI's own approver.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/prompt"
	agenttool "github.com/huan/huan-agent/internal/tool"
)

// approvalSurfaceCanAsk reports whether a surface has a way to put a request to a
// person and wait for the answer.
//
// This is the difference between "the tool asks before it acts" and "the tool is
// not on the menu". A surface that cannot ask and has gated tools registered
// would refuse every write — a menu entry that always fails, which this project
// treats as worse than an absent one.
func approvalSurfaceCanAsk(surface string) bool {
	switch surface {
	case prompt.SurfaceWeb:
		// The console renders the card and posts the decision back.
		return true
	case prompt.SurfaceCLI:
		// The REPL reads y / t / n from the terminal it is already reading.
		return true
	default:
		// Feishu has no card for this yet, and a one-shot run has nobody watching
		// by definition. Both withhold the gated tools instead.
		return false
	}
}

// approvalPolicyFor maps the configuration onto the tool package's policy.
func approvalPolicyFor(cfg *config.Config) (agenttool.ApprovalPolicy, error) {
	if cfg == nil {
		return agenttool.ApprovalPolicy{Mode: agenttool.ApprovalOff}, nil
	}
	mode, err := agenttool.ParseApprovalMode(cfg.Tools.Approval.ModeOr())
	if err != nil {
		return agenttool.ApprovalPolicy{}, err
	}
	return agenttool.ApprovalPolicy{
		Mode:    mode,
		Allow:   cfg.Tools.Approval.Allow,
		Timeout: cfg.Tools.Approval.Timeout(),
	}, nil
}

// approvalGate describes what to do with the gated tools of one surface.
type approvalGate struct {
	policy  agenttool.ApprovalPolicy
	canAsk  bool
	surface string
	logger  *zap.Logger
}

// active reports whether the gate is doing anything at all. When it is not, every
// caller skips it and the tool set is byte-for-byte what it was before this
// feature existed.
func (g approvalGate) active() bool {
	return g.policy.Mode != agenttool.ApprovalOff && g.policy.Mode != ""
}

// wrap gates one tool, or reports that it must be withheld.
//
// The withheld case is not a refusal: the tool is left out of the list entirely,
// so the model never sees it. "The call was refused" is a worse answer than a tool
// that was never offered, and a surface that cannot ask should be honest about
// what it can do rather than invite calls it will always turn down.
func (g approvalGate) wrap(t agenttool.Tool) (agenttool.Tool, bool) {
	if !g.active() || !g.policy.Requires(agenttool.CapabilityOf(t)) {
		return t, true
	}
	if !g.canAsk {
		return nil, false
	}
	return agenttool.Gate(t, agenttool.GateOptions{Policy: g.policy, Logger: gateLogger{g.logger}}), true
}

// applyToTools gates a slice, dropping what must be withheld.
func (g approvalGate) applyToTools(tools []agenttool.Tool) []agenttool.Tool {
	if !g.active() {
		return tools
	}
	out := make([]agenttool.Tool, 0, len(tools))
	var withheld []string
	for _, t := range tools {
		kept, ok := g.wrap(t)
		if !ok {
			if name := toolName(t); name != "" {
				withheld = append(withheld, name)
			}
			continue
		}
		out = append(out, kept)
	}
	g.reportWithheld(withheld)
	return out
}

// applyToRegistry gates what has already been registered into a base registry.
//
// A registry rather than a slice because the base is assembled in several places
// (the always-on tools, the media tools, the document tool, the skill tool), and a
// post-pass is the only way to cover all of them without every one of those places
// having to remember. Replacing goes through Unregister + Register because the
// registry has no "replace", which is deliberate: it is the same guarantee that
// stops two tools from silently sharing a name.
func (g approvalGate) applyToRegistry(ctx context.Context, reg *agenttool.Registry) error {
	if !g.active() || reg == nil {
		return nil
	}
	specs, err := reg.List(ctx)
	if err != nil {
		return fmt.Errorf("list tools for approval gate: %w", err)
	}
	var withheld []string
	for _, spec := range specs {
		t, ok := reg.Get(spec.Name)
		if !ok {
			continue
		}
		if !g.policy.Requires(agenttool.CapabilityOf(t)) {
			continue
		}
		if !g.canAsk {
			reg.Unregister(spec.Name)
			withheld = append(withheld, spec.Name)
			continue
		}
		gated := agenttool.Gate(t, agenttool.GateOptions{Policy: g.policy, Logger: gateLogger{g.logger}})
		reg.Unregister(spec.Name)
		if err := reg.Register(gated); err != nil {
			return fmt.Errorf("re-register %s behind the approval gate: %w", spec.Name, err)
		}
	}
	g.reportWithheld(withheld)
	return nil
}

// reportWithheld says once per surface which tools the gate took away, and why.
//
// Once, not per tool and not per turn: this is a startup fact about a deployment,
// and repeating it would bury the reason in the log it is meant to explain.
func (g approvalGate) reportWithheld(names []string) {
	if len(names) == 0 || g.logger == nil {
		return
	}
	g.logger.Info("approval gate withheld tools: this surface has no way to ask for approval",
		zap.String("surface", g.surface),
		zap.String("mode", g.policy.Mode),
		zap.Strings("tools", names),
		zap.String("hint", "set tools.approval.mode to off to give them back, or run on a surface that can ask"))
}

// gateLogger adapts *zap.Logger to the tool package's two-method interface.
type gateLogger struct{ l *zap.Logger }

func (g gateLogger) Info(msg string, kv ...any) {
	if g.l != nil {
		g.l.Sugar().Infow(msg, kv...)
	}
}

func (g gateLogger) Warn(msg string, kv ...any) {
	if g.l != nil {
		g.l.Sugar().Warnw(msg, kv...)
	}
}

// toolName reads a tool's name for a log line.
func toolName(t agenttool.Tool) string {
	if t == nil {
		return ""
	}
	info, err := t.Info(context.Background())
	if err != nil || info == nil {
		return ""
	}
	return info.Name
}

// --- the CLI approver ---

// cliApprover puts a request to the terminal the user is already typing into.
//
// It reads through the REPL's own scanner rather than opening os.Stdin, because
// two readers on one terminal fight: the REPL reads a line for the next prompt
// while the approver waits for one that was typed for it, and which one wins is
// whichever goroutine happens to reach the file descriptor first. Sharing the
// scanner makes the order the obvious one — the agent is running, so the next line
// typed answers the request.
type cliApprover struct {
	// readLine returns the next line typed, or false at end of input.
	readLine func() (string, bool)
	// out is where the request is described. It is stderr: the answer goes to
	// stdout, and mixing the two would corrupt a piped run.
	out io.Writer
	// interactive is false on a piped stdin, where the surface has no approver at
	// all and the gate withholds instead.
	interactive bool
	mu          sync.Mutex
}

// Approve shows the request and reads a decision.
//
// Every path that is not an explicit allow is a refusal, including end of input
// and a timeout: the gate holds when nobody is watching.
func (a *cliApprover) Approve(ctx context.Context, req agenttool.Request) (agenttool.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.interactive {
		return agenttool.Decision{Kind: agenttool.DecisionDeny, Source: agenttool.SourcePolicy,
			Reason: "stdin 不是终端，无法交互审批"}, nil
	}

	writeApprovalPrompt(a.out, req)

	line, ok := a.readLine()
	if !ok {
		fmt.Fprintln(a.out, "（输入结束，按拒绝处理）")
		return agenttool.Decision{Kind: agenttool.DecisionDeny, Source: agenttool.SourceHuman,
			Reason: "终端输入结束"}, nil
	}
	decision, reason := parseApprovalAnswer(line)
	switch decision {
	case agenttool.DecisionDeny:
		fmt.Fprintln(a.out, "（已拒绝）")
	case agenttool.DecisionAllowTurn:
		fmt.Fprintf(a.out, "（本轮内 %s 不再询问）\n", req.Tool)
	default:
		fmt.Fprintln(a.out, "（已允许一次）")
	}
	return agenttool.Decision{Kind: decision, Reason: reason, Source: agenttool.SourceHuman}, nil
}

// parseApprovalAnswer reads y / t / n [reason].
//
// Anything unrecognised is a refusal, and so is an empty line: a person who hits
// enter at a prompt asking "may I run this command" has not said yes, and
// treating silence as consent is the one behaviour this mechanism exists to
// prevent.
func parseApprovalAnswer(line string) (agenttool.DecisionKind, string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return agenttool.DecisionDeny, ""
	}
	head, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	switch strings.ToLower(strings.TrimSpace(head)) {
	case "y", "yes", "a", "allow":
		return agenttool.DecisionAllowOnce, ""
	case "t", "turn", "all":
		return agenttool.DecisionAllowTurn, ""
	case "n", "no", "d", "deny":
		return agenttool.DecisionDeny, rest
	default:
		// Free text is read as a reason, which is the most useful thing it can
		// be: "no, not that file" is a refusal with an explanation.
		return agenttool.DecisionDeny, line
	}
}

// writeApprovalPrompt describes a request on the terminal.
func writeApprovalPrompt(out io.Writer, req agenttool.Request) {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "需要确认：%s\n", req.Summary)
	for _, line := range req.Preview {
		text := strings.TrimRight(line.Text, "\n")
		switch line.Kind {
		case agenttool.PreviewAdd:
			fmt.Fprintf(out, "  + %s\n", text)
		case agenttool.PreviewDel:
			fmt.Fprintf(out, "  - %s\n", text)
		case agenttool.PreviewContext:
			fmt.Fprintf(out, "    %s\n", text)
		default:
			fmt.Fprintf(out, "  %s\n", text)
		}
	}
	fmt.Fprint(out, "允许这一次 / 本轮都允许 / 拒绝？[y/t/n，或直接写拒绝理由] ")
}

// newCLIApprover builds the approver for an interactive session, or a
// non-interactive one that refuses.
func newCLIApprover(readLine func() (string, bool), out io.Writer, interactive bool) *cliApprover {
	return &cliApprover{readLine: readLine, out: out, interactive: interactive}
}

// terminalInteractive reports whether stdin is a terminal, which is what decides
// whether the CLI can ask for approval at all.
//
// A character device is the cheap, dependency-free test: a pipe or a redirect is
// not one, and neither is /dev/null. It is the same distinction the bash tool
// makes about prompts, and for the same reason — nothing is there to type into.
func terminalInteractive() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
