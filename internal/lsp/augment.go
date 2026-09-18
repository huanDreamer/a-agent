package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	einotool "github.com/cloudwego/eino/components/tool"

	"github.com/huan/huan-agent/internal/tool"
	"github.com/huan/huan-agent/internal/workspace"
)

// The edit-feedback decorator: after a write succeeds, the diagnostics for the
// file it just wrote are appended to the tool's result.
//
// This is the half of code intelligence that changes behaviour rather than adding
// a query. A `diagnostics` tool the model has to remember to call is not called at
// the moment it matters most — right after it decided the change was correct. So
// the feedback is not a tool, it is a change to what the editing tools return.
//
// It is a decorator rather than code inside write_file for three reasons: it
// keeps internal/tool/builtin free of any knowledge of language servers; it works
// for write_file, edit_file and the multi-file applier alike, so the behaviour
// cannot drift between them; and it can be applied at registration, which is
// where every surface already goes through one code path.

// Diagnosed says what a diagnostics answer means.
//
// It is an enum rather than a boolean because there are three outcomes and two of
// them used to be the same value: "no server covers this file" and "the server
// says the file is clean" are both an empty list, and conflating them made a
// fixed file look like a file nobody checked. The integration test that caught it
// was the only one that could: in-process fakes had been written to the same
// ambiguous shape.
type Diagnosed int

const (
	// NoServer means no language server handles this file, so nothing was asked.
	NoServer Diagnosed = iota
	// NotReady means a server was asked but has not caught up with the file's
	// current content, so the items may describe the previous version.
	NotReady
	// Ready means the answer is current. It may still be empty, and an empty
	// Ready is a real answer: the file is clean.
	Ready
)

// Diagnoser is what the decorator needs from the language-server layer.
//
// It is an interface so a test can drive the decorator exactly — the three
// outcomes above are all about what this returns, and none of them needs a real
// language server.
type Diagnoser interface {
	// FileDiagnostics returns the diagnostics for one file, and what the answer
	// means. items is non-nil exactly when a server reported something, including
	// an empty clean report.
	FileDiagnostics(ctx context.Context, path, ceiling string, wait time.Duration) (items []Diagnostic, state Diagnosed, err error)
}

// FeedbackOptions configures the decorator.
type FeedbackOptions struct {
	// Workspace resolves the path argument into an absolute one, so the same
	// sandbox rules apply as to the tool itself.
	Workspace *workspace.Workspace
	// Ceiling is the workspace root: the boundary a language server's project
	// search may not climb above.
	Ceiling string
	// PathField is the argument naming the file. write_file and edit_file both
	// use "path".
	PathField string
	// Max caps how many diagnostics are attached. Zero uses 20.
	Max int
	// Wait bounds how long the tool will wait for the server. Zero uses
	// DefaultWaitDiagnostics.
	Wait   time.Duration
	Logger Logger
}

// DefaultWaitDiagnostics bounds the wait for diagnostics after an edit.
//
// It is deliberately short. The feedback has to arrive fast enough to be worth
// having, and an edit must never be made slower by a server that is busy: two
// seconds is under the threshold where a tool call starts feeling stuck, and a
// slow server reports "not ready" instead of holding the turn.
const DefaultWaitDiagnostics = 2 * time.Second

// DefaultMaxDiagnostics caps the attached list; the count is always reported, so
// truncation is never silent.
const DefaultMaxDiagnostics = 20

// Feedback wraps a tool so its successful result carries diagnostics.
//
// The wrapped tool is embedded, so Info (the description and schema the model
// sees) is delegated unchanged: to the model this is the same tool, and the only
// difference is what comes back.
type Feedback struct {
	tool.Tool
	d      Diagnoser
	opts   FeedbackOptions
	invoke einotool.InvokableTool
}

// NewFeedback decorates a tool.
//
// A tool that is not invokable is returned unchanged rather than wrapped: there
// is nothing to attach feedback to, and a wrapper that silently does nothing is
// worse than not wrapping.
func NewFeedback(inner tool.Tool, d Diagnoser, opts FeedbackOptions) tool.Tool {
	if inner == nil || d == nil {
		return inner
	}
	inv, ok := inner.(einotool.InvokableTool)
	if !ok {
		return inner
	}
	if opts.Max <= 0 {
		opts.Max = DefaultMaxDiagnostics
	}
	if opts.Wait <= 0 {
		opts.Wait = DefaultWaitDiagnostics
	}
	if opts.PathField == "" {
		opts.PathField = "path"
	}
	return &Feedback{Tool: inner, d: d, opts: opts, invoke: inv}
}

// Capability reports the wrapped tool's capability.
//
// Embedding the Tool interface does not promote Capability, so without this the
// decorator would answer CapabilityOf with the default (CapRead) — and a write
// tool that claims to be a read tool survives a read-only filter.
func (f *Feedback) Capability() tool.Capability { return tool.CapabilityOf(f.Tool) }

// Concurrency forwards the wrapped tool's declaration.
func (f *Feedback) Concurrency() tool.Concurrency { return tool.ConcurrencyOf(f.Tool) }

// InvokableRun runs the tool and appends diagnostics when it succeeded.
//
// The order matters and is the point: the write happens first, the diagnostics
// are collected after, and a failure is returned untouched. An edit is never
// made to depend on a language server.
func (f *Feedback) InvokableRun(ctx context.Context, args string, opts ...einotool.Option) (string, error) {
	result, err := f.invoke.InvokableRun(ctx, args, opts...)
	if err != nil {
		// A failed edit keeps its failure. Mixing "why it failed" with "and here
		// is what the file looked like" dilutes both.
		return result, err
	}

	path, ok := f.pathFrom(args)
	if !ok {
		return result, nil
	}

	items, state, derr := f.d.FileDiagnostics(ctx, path, f.opts.Ceiling, f.opts.Wait)
	if derr != nil {
		// A language server that is unavailable or wedged must not turn a
		// successful write into a failed one, and it is not a covered file with no
		// problems: the result is left alone and the reason is logged.
		if f.opts.Logger != nil {
			f.opts.Logger.Warnf("lsp: diagnostics for %s unavailable: %v", path, derr)
		}
		return result, nil
	}

	switch state {
	case NoServer:
		// No server covers this file. The result is returned byte for byte as it
		// was: with no language server, this decorator does not exist.
		return result, nil
	case NotReady:
		return result + renderNotReady(f.opts.Wait), nil
	default:
		return result + f.render(path, items), nil
	}
}

// pathFrom pulls the file path out of the tool's arguments.
func (f *Feedback) pathFrom(args string) (string, bool) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", false
	}
	raw, ok := in[f.opts.PathField]
	if !ok {
		return "", false
	}
	var path string
	if err := json.Unmarshal(raw, &path); err != nil || strings.TrimSpace(path) == "" {
		return "", false
	}
	if f.opts.Workspace != nil {
		abs, err := f.opts.Workspace.Resolve(path)
		if err != nil {
			return "", false
		}
		return abs, true
	}
	return path, true
}

// render builds the appended section.
//
// Go builds it from a struct rather than assembling a string per field, so the
// alignment stays stable and the output is testable. Only errors and warnings are
// shown by default: a medium Go file has hundreds of hints, and a wall of them
// would bury the one line that matters.
func (f *Feedback) render(path string, items []Diagnostic) string {
	shown := make([]Diagnostic, 0, len(items))
	total := 0
	for _, d := range items {
		if d.Severity != SeverityError && d.Severity != SeverityWarning {
			continue
		}
		total++
		if len(shown) < f.opts.Max {
			shown = append(shown, d)
		}
	}
	if total == 0 {
		// Saying so is not noise: an absent section reads as "the check did not
		// run", and the model would then either ignore it or re-run a check.
		return "\n\n诊断（该文件）：无"
	}
	SortDiagnostics(shown)

	var b strings.Builder
	fmt.Fprintf(&b, "\n\n诊断（该文件，%d 条 error/warning%s）：", total, truncatedSuffix(total, len(shown)))
	for _, d := range shown {
		at := d.Range.Start
		fmt.Fprintf(&b, "\n  %s:%d:%d  %s  %s", displayPath(path, f.opts.Workspace),
			at.Line+1, at.Character+1, SeverityName(d.Severity), oneLineMessage(d.Message))
		if d.Source != "" {
			fmt.Fprintf(&b, "  (%s)", d.Source)
		}
	}
	return b.String()
}

// renderNotReady explains a server that did not answer in time.
func renderNotReady(wait time.Duration) string {
	return fmt.Sprintf("\n\n诊断（该文件）：尚未就绪（语言服务器未在 %s 内返回）", wait.Round(time.Millisecond))
}

func truncatedSuffix(total, shown int) string {
	if total <= shown {
		return ""
	}
	return fmt.Sprintf("，共 %d，显示前 %d", total, shown)
}

// displayPath shows a workspace-relative path when there is a workspace, because
// that is the spelling every other tool result uses.
func displayPath(abs string, ws *workspace.Workspace) string {
	if ws != nil {
		if rel := ws.Rel(abs); rel != "" {
			return rel
		}
	}
	return abs
}

// oneLineMessage keeps a diagnostic on one line: multi-line messages are common
// (a compiler prints suggestions inline) and they destroy the alignment that
// makes a list of diagnostics scannable.
func oneLineMessage(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > 300 {
		return string([]rune(s)[:300]) + "…"
	}
	return s
}
