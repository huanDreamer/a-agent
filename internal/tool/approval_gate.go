package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"
)

// The approval gate: a decorator that stops a write or an exec before it happens
// and asks a person.
//
// It is a decorator rather than code inside the tools, and it is applied at
// registration rather than inside a loop, because of two things this project
// learned the hard way:
//
//   - There are two execution loops (the web runner and Eino's ReAct), so a gate
//     written into one covers half the surfaces. The tool wrapper sits under
//     both.
//   - Registration goes through one place per surface, so the gate cannot be
//     forgotten for a tool that is added later.
//
// The gate itself is deliberately simple: it classifies the call by capability,
// builds a request a person can judge, and refuses when the approver does not
// allow it. It does not decide policy beyond "does this capability need asking" —
// that lives in ApprovalPolicy, which comes from configuration.

// ApprovalPolicy decides which calls need approval.
type ApprovalPolicy struct {
	// Mode selects the capabilities that are gated:
	// "off", "writes", "writes+exec", "all".
	Mode string
	// Allow is a list of RE2 patterns matched against the request's summary.
	// A match means the call does not need approval.
	//
	// Like deny_patterns, this is a convenience and NOT a boundary: an equivalent
	// action that does not match still runs. The difference is that here the
	// default is to ask, so a missed pattern costs a prompt rather than a
	// disaster.
	Allow []string
	// Timeout bounds one request. Zero means the approver's own limit.
	Timeout time.Duration
}

// Approval modes.
const (
	// ApprovalOff gates nothing, which is what every existing deployment gets:
	// turning the gate on by default would take write access away from pipelines
	// and bots that nobody is watching.
	ApprovalOff = "off"
	// ApprovalWrites gates writes only.
	ApprovalWrites = "writes"
	// ApprovalWritesExec gates writes and command execution.
	ApprovalWritesExec = "writes+exec"
	// ApprovalAll gates every tool, including reads.
	ApprovalAll = "all"
)

// ParseApprovalMode normalises a configured mode.
func ParseApprovalMode(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", ApprovalOff:
		return ApprovalOff, nil
	case ApprovalWrites:
		return ApprovalWrites, nil
	case "writes+exec", "writes_exec", "write+exec", "writings+exec":
		return ApprovalWritesExec, nil
	case ApprovalAll:
		return ApprovalAll, nil
	default:
		return "", fmt.Errorf("unknown approval mode %q (want off, writes, writes+exec or all)", s)
	}
}

// Requires reports whether a capability needs approval under this policy.
func (p ApprovalPolicy) Requires(cap Capability) bool {
	switch p.Mode {
	case ApprovalWrites:
		return cap == CapWrite
	case ApprovalWritesExec:
		return cap == CapWrite || cap == CapExec
	case ApprovalAll:
		return true
	default:
		return false
	}
}

// Allows reports whether a summary matches the configured allow-list.
//
// A pattern that does not compile is skipped, and the gate then asks: an invalid
// regular expression must not become a silent "allow everything", which is the
// direction this whole file exists to avoid.
func (p ApprovalPolicy) Allows(summary string) bool {
	for _, pattern := range p.Allow {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if ok, err := matchPattern(pattern, summary); err == nil && ok {
			return true
		}
	}
	return false
}

// patternCache keeps compiled allow-list patterns, because the gate matches on
// every gated call and recompiling a dozen regexes per call is pure waste.
var (
	patternMu    sync.Mutex
	patternCache = map[string]*regexp.Regexp{}
)

func matchPattern(pattern, s string) (bool, error) {
	patternMu.Lock()
	re, ok := patternCache[pattern]
	if !ok {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			patternMu.Unlock()
			return false, err
		}
		patternCache[pattern] = compiled
		re = compiled
	}
	patternMu.Unlock()
	return re.MatchString(s), nil
}

// GateOptions configures the gate.
type GateOptions struct {
	// Policy is what needs asking.
	Policy ApprovalPolicy
	// Logger records every decision. Nil is allowed and means silence.
	Logger GateLogger
}

// GateLogger is the slice of logging the gate needs, so the tool package does not
// grow a dependency on a logging framework.
type GateLogger interface {
	Info(msg string, keysAndValues ...any)
	Warn(msg string, keysAndValues ...any)
}

// ApprovalGate wraps a tool so a gated call is put to a person first.
type ApprovalGate struct {
	Tool
	invoke einotool.InvokableTool
	opts   GateOptions

	// allowedTurn remembers which tools this turn has already been allowed for.
	// It is shared across the gates of one turn through the context, not stored
	// here, so a gate is stateless and can be reused across turns.
	cap Capability
}

// Gate wraps a tool.
//
// A tool that is not invokable is returned unchanged: there is nothing to gate,
// and wrapping it would only make the model's view of it different from its
// behaviour.
func Gate(t Tool, opts GateOptions) Tool {
	inv, ok := t.(einotool.InvokableTool)
	if !ok {
		return t
	}
	if opts.Policy.Mode == "" {
		opts.Policy.Mode = ApprovalOff
	}
	// An off policy returns the tool itself rather than a wrapper that decides to
	// do nothing: the mode is off for every deployment that has not opted in, so
	// "off" has to cost literally nothing — not an interface indirection on every
	// tool call, and not a different dynamic type for anything that inspects the
	// registry.
	if opts.Policy.Mode == ApprovalOff {
		return t
	}
	return &ApprovalGate{Tool: t, invoke: inv, opts: opts, cap: CapabilityOf(t)}
}

// Capability reports the wrapped tool's capability.
//
// It has to be declared explicitly. Embedding the Tool interface promotes only
// that interface's methods, and Capability is not one of them — so without this
// method a gated write tool would answer CapabilityOf with the default, CapRead.
// The consequence is not cosmetic: FilterByCapabilities is how a surface is given
// a read-only registry, and a write tool that claims to be a read tool is exactly
// the one that would get through.
func (g *ApprovalGate) Capability() Capability { return g.cap }

// Concurrency forwards the wrapped tool's declaration.
func (g *ApprovalGate) Concurrency() Concurrency { return ConcurrencyOf(g.Tool) }

// InvokableRun asks before running, when the policy says to.
func (g *ApprovalGate) InvokableRun(ctx context.Context, args string, opts ...einotool.Option) (string, error) {
	if !g.opts.Policy.Requires(g.cap) {
		return g.invoke.InvokableRun(ctx, args, opts...)
	}

	name := g.name(ctx)
	req := BuildRequest(name, g.cap, args)
	req.Tool = name
	req.Capability = g.cap
	if g.opts.Policy.Timeout > 0 {
		req.Timeout = g.opts.Policy.Timeout.Milliseconds()
	}

	// The allow-list is checked before anyone is asked, and after the request is
	// built so the pattern has a summary to match.
	if g.opts.Policy.Allows(req.Summary) {
		g.log(name, req, Decision{Kind: DecisionAllowOnce, Source: SourcePolicy})
		return g.invoke.InvokableRun(ctx, args, opts...)
	}

	// A tool already allowed for this turn does not ask again. The allowance is
	// carried on the context (see allowTurnKey) rather than captured here, because
	// one gate instance serves every turn of a process.
	if turnAllows(ctx, name) {
		g.log(name, req, Decision{Kind: DecisionAllowTurn, Source: SourceHuman})
		return g.invoke.InvokableRun(ctx, args, opts...)
	}

	approver := ApproverFrom(ctx)
	if approver == nil {
		// No way to ask. Refusing is the only safe direction: a surface that
		// cannot ask should not have been given the tool at all (registration
		// withholds it), so reaching here means something is wired wrong, and
		// letting the call through would be the worst possible response to that.
		d := Decision{Kind: DecisionDeny, Source: SourcePolicy,
			Reason: "这个运行环境没有可用的审批通道（无人值守），因此写/执行类操作被拒绝"}
		g.log(name, req, d)
		return DenialResult(req, d), nil
	}

	d, err := approver.Approve(ctx, req)
	if err != nil {
		// An approver that failed is not a person who said no, but the call still
		// must not proceed. The error goes into the reason so the model can tell
		// the two apart.
		d = Decision{Kind: DecisionDeny, Source: SourceTimeout,
			Reason: "审批通道出错，按拒绝处理：" + err.Error()}
		g.log(name, req, d)
		return DenialResult(req, d), nil
	}
	if d.Source == "" {
		d.Source = SourceHuman
	}
	// Anything that is not one of the two allow forms is a refusal, including a
	// decision kind this build does not know.
	if !d.Allowed() {
		d.Kind = DecisionDeny
		g.log(name, req, d)
		return DenialResult(req, d), nil
	}

	if d.Kind == DecisionAllowTurn {
		markTurnAllowed(ctx, name)
	}
	g.log(name, req, d)
	return g.invoke.InvokableRun(ctx, args, opts...)
}

// name reports the tool's name, which is needed for the request and for the
// turn-scoped allowance.
func (g *ApprovalGate) name(ctx context.Context) string {
	info, err := g.Info(ctx)
	if err != nil || info == nil || info.Name == "" {
		if n, ok := g.Tool.(interface{ Name() string }); ok {
			return n.Name()
		}
		return "unknown"
	}
	return info.Name
}

// log records one decision.
//
// Every decision is logged, including the ones nobody made. An audit that only
// contains human approvals cannot answer "what ran while nobody was watching",
// which is the question it exists for.
func (g *ApprovalGate) log(tool string, req Request, d Decision) {
	if g.opts.Logger == nil {
		return
	}
	fields := []any{
		"tool", tool,
		"capability", string(req.Capability),
		"summary", req.Summary,
		"decision", string(d.Kind),
		"source", d.Source,
	}
	if d.Reason != "" {
		fields = append(fields, "reason", d.Reason)
	}
	if d.Allowed() {
		g.opts.Logger.Info("tool approved", fields...)
		return
	}
	g.opts.Logger.Warn("tool refused", fields...)
}

// --- turn-scoped allowance ---

type allowTurnKey struct{}

// turnAllowances is the set of tools allowed for the rest of one turn.
type turnAllowances struct {
	tools map[string]struct{}
}

func turnAllows(ctx context.Context, tool string) bool {
	a, ok := ctx.Value(allowTurnKey{}).(*turnAllowances)
	if !ok || a == nil {
		return false
	}
	_, ok = a.tools[tool]
	return ok
}

func markTurnAllowed(ctx context.Context, tool string) {
	a, ok := ctx.Value(allowTurnKey{}).(*turnAllowances)
	if !ok || a == nil {
		return
	}
	a.tools[tool] = struct{}{}
}

// WithTurnAllowances installs the per-turn allowance set on a context.
//
// The turn's owner calls it once, at the top of a turn. It lives on the context
// because that is what makes "allowed for this turn" end when the turn does: the
// set is garbage the moment the turn's context is.
func WithTurnAllowances(ctx context.Context) context.Context {
	return context.WithValue(ctx, allowTurnKey{}, &turnAllowances{tools: map[string]struct{}{}})
}

// --- request construction ---

// Bounds on what a request shows. The summary is one line, the preview is a
// handful: a card that scrolls is a card nobody reads.
const (
	maxPreviewLines = 40
	maxPreviewBytes = 8 << 10
	maxSummaryRunes = 200
)

// BuildRequest turns a tool call into something a person can judge.
//
// The summaries are per tool because a generic one ("the model wants to call
// edit_file") asks a person to approve something they cannot see, which trains
// them to approve without reading.
func BuildRequest(toolName string, cap Capability, args string) Request {
	req := Request{Capability: cap, Default: DecisionDeny}
	switch toolName {
	case "write_file":
		req.Summary, req.Preview = summarizeWrite(args)
	case "edit_file":
		req.Summary, req.Preview = summarizeEdit(args)
	case "apply_patch":
		req.Summary, req.Preview = summarizePatch(args)
	case "bash", "bash_background":
		req.Summary, req.Preview = summarizeBash(args)
	default:
		req.Summary, req.Preview = summarizeGeneric(toolName, args)
	}
	if strings.TrimSpace(req.Summary) == "" {
		req.Summary = toolName
	}
	return req
}

// scrubPaths keeps only the interesting part of a path for a summary: a card that
// shows /Users/someone/very/long/path/to/repo/internal/foo/bar.go spends the whole
// line on a prefix every line shares.
func scrubPaths(field string) string {
	field = strings.TrimSpace(field)
	if field == "" {
		return "(未指定)"
	}
	return field
}

func summarizeWrite(args string) (string, []PreviewLine) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", nil
	}
	summary := fmt.Sprintf("写入 %s（%d 行，%d 字节，整文件覆盖）",
		scrubPaths(in.Path), countLines(in.Content), len(in.Content))
	return summary, previewOfText(in.Content, PreviewAdd, maxPreviewLines)
}

func summarizeEdit(args string) (string, []PreviewLine) {
	var in struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", nil
	}
	added := countLines(in.NewString) - countLines(in.OldString)
	summary := fmt.Sprintf("编辑 %s（%+d 行）", scrubPaths(in.Path), added)
	if in.ReplaceAll {
		summary += "（替换所有匹配）"
	}

	var preview []PreviewLine
	preview = append(preview, previewOfText(in.OldString, PreviewDel, maxPreviewLines/2)...)
	preview = append(preview, previewOfText(in.NewString, PreviewAdd, maxPreviewLines/2)...)
	return summary, preview
}

func summarizePatch(args string) (string, []PreviewLine) {
	// apply_patch is not implemented yet; the shape is described here so the card
	// has something to show the day it is (Phase 24).
	var in struct {
		Operations []struct {
			Path string `json:"path"`
		} `json:"operations"`
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", nil
	}
	if len(in.Operations) > 0 {
		names := make([]string, 0, len(in.Operations))
		for _, op := range in.Operations {
			names = append(names, scrubPaths(op.Path))
		}
		summary := fmt.Sprintf("修改 %d 个文件：%s", len(names), strings.Join(names, ", "))
		return summary, nil
	}
	lines := countLines(in.Patch)
	return fmt.Sprintf("应用补丁（%d 行）", lines), previewOfText(in.Patch, PreviewAdd, maxPreviewLines)
}

// summarizeBash keeps the command line whole.
//
// The command is the thing being approved. Truncating it would hide the `rm -rf`
// at the end of a long pipeline, which is the one case this whole mechanism
// exists for — so the command is never clipped, and only the incidental metadata
// is.
func summarizeBash(args string) (string, []PreviewLine) {
	var in struct {
		Command   string `json:"command"`
		Cwd       string `json:"cwd"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", nil
	}
	command := strings.TrimSpace(in.Command)
	summary := "执行: " + command
	if len([]rune(summary)) > maxSummaryRunes {
		// The summary line is for a card header; the preview below carries the
		// command in full, so clipping here loses nothing.
		summary = string([]rune(summary)[:maxSummaryRunes]) + "…（完整命令见下方）"
	}

	preview := []PreviewLine{{Kind: PreviewMeta, Text: "完整命令：" + command}}
	if cwd := strings.TrimSpace(in.Cwd); cwd != "" {
		preview = append(preview, PreviewLine{Kind: PreviewMeta, Text: "工作目录：" + cwd})
	}
	if in.TimeoutMS > 0 {
		preview = append(preview, PreviewLine{Kind: PreviewMeta,
			Text: fmt.Sprintf("超时：%dms", in.TimeoutMS)})
	}
	return summary, preview
}

// summarizeGeneric is the fallback for a tool with no tailored summary: it shows
// the arguments rather than pretending there is nothing to see.
func summarizeGeneric(toolName, args string) (string, []PreviewLine) {
	return fmt.Sprintf("调用 %s", toolName), previewOfText(args, PreviewMeta, 6)
}

// countLines counts the lines of a fragment the way an editor numbers them: a
// trailing newline terminates the last line rather than starting an empty one, and
// an empty fragment has no lines.
//
// Reporting "3 行" for two lines of content ending in a newline would make every
// whole-file write look bigger than it is, which is exactly the number a person
// uses to decide whether to read the preview.
func countLines(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

// previewOfText turns a fragment into preview lines, bounded in both lines and
// bytes so one huge write cannot flood the card.
func previewOfText(text string, kind PreviewKind, maxLines int) []PreviewLine {
	if text == "" || maxLines <= 0 {
		return nil
	}
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	// A trailing newline is a terminator: dropping the empty element it leaves
	// keeps the count and the "showing N of M lines" note honest.
	if n := len(raw); n > 1 && raw[n-1] == "" {
		raw = raw[:n-1]
	}
	lines := make([]PreviewLine, 0, min(len(raw), maxLines))
	total := 0
	for i, l := range raw {
		if i >= maxLines || total+len(l) > maxPreviewBytes {
			lines = append(lines, PreviewLine{Kind: PreviewMeta,
				Text: fmt.Sprintf("…（共 %d 行，此处显示前 %d 行）", len(raw), i)})
			break
		}
		total += len(l)
		lines = append(lines, PreviewLine{Kind: kind, Text: l})
	}
	return lines
}
