package claudehook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// defaultHandlerTimeout is §3's documented default for command/http/mcp_tool
// handlers. Per-event overrides (§3: UserPromptSubmit 30s, MessageDisplay 10s,
// SessionEnd's shared 1.5s budget) are applied on top of it by handlerTimeout;
// the events that share a budget only do so because Claude Code is tearing the
// session down, which is not a thing this runtime has to model.
const defaultHandlerTimeout = 600 * time.Second

// perEventDefaultTimeout are the events §3 gives a shorter default than 600s.
// Only UserPromptSubmit is dispatched by this build; the others are listed so a
// future event added to SupportedEvents inherits the documented value rather than
// the generic one.
var perEventDefaultTimeout = map[string]time.Duration{
	"UserPromptSubmit": 30 * time.Second,
	"PreModelSwitch":   30 * time.Second,
	"PostModelSwitch":  30 * time.Second,
	"MessageDisplay":   10 * time.Second,
}

// DefaultMaxTimeout and DefaultMaxRecords bound an engine that was not given
// options. MaxTimeout exists because this runtime runs inside a chat server: §5.7
// lets a config ask for a 600s hook, and a turn that waits 600s for a logger is
// indistinguishable from a hang. The clamp is therefore applied to every handler
// except async ones, and is deliberately lower than the protocol default.
const (
	DefaultMaxTimeout = 60 * time.Second
	DefaultMaxRecords = 200
)

// maxStreamBytes bounds the stdout/stderr this runtime will hold for one handler.
// A hook that prints in a loop would otherwise grow the process's heap until the
// server dies; §5.8's 10,000-character cap is about what reaches the model, not
// about what a broken script can produce, so the two bounds are separate.
const maxStreamBytes = 1 << 20 // 1 MiB

// asyncRecordExitCode marks a record for an async handler: §9 says the process is
// started and not awaited, so there is no exit code to report. -1 is used instead
// of 0 so the panel cannot show a fire-and-forget hook as a successful run.
const asyncRecordExitCode = -1

// Options wires an Engine.
type Options struct {
	Logger *zap.Logger
	// Dir is the working directory command hooks run in. Empty means the
	// process's own working directory.
	Dir string
	// Env is appended to the inherited environment, e.g. CLAUDE_PROJECT_DIR.
	Env []string
	// MaxTimeout caps a handler's timeout. 0 means DefaultMaxTimeout.
	MaxTimeout time.Duration
	// MCP is the caller mcp_tool handlers use. Nil records them as skipped.
	MCP MCPCaller
	// MaxRecords bounds the in-memory event log. 0 means DefaultMaxRecords.
	MaxRecords int
	// Now is injectable for tests. Nil means time.Now.
	Now func() time.Time
}

// Engine executes configured hooks.
//
// The configuration lives behind an atomic pointer rather than a mutex because
// Update is driven by a settings-file watcher while Dispatch runs on every turn:
// a reader that took a lock would serialise every turn behind a config reload it
// does not care about. The event log is the one thing that does need mutual
// exclusion, and it has its own mutex — the two are never held together.
type Engine struct {
	cfg atomic.Pointer[Config]

	logger     *zap.Logger
	dir        string
	env        []string
	maxTimeout time.Duration
	mcp        MCPCaller
	now        func() time.Time

	logMu   sync.Mutex
	records []Record
	maxRec  int
}

// NewEngine builds an engine. A zero Config yields an engine that runs nothing
// and reports Verdict{Ran:false}.
func NewEngine(cfg Config, opts Options) *Engine {
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	maxTimeout := opts.MaxTimeout
	if maxTimeout <= 0 {
		maxTimeout = DefaultMaxTimeout
	}
	maxRecords := opts.MaxRecords
	if maxRecords <= 0 {
		maxRecords = DefaultMaxRecords
	}

	e := &Engine{
		logger:     logger.Named("claudehook"),
		dir:        opts.Dir,
		env:        append([]string(nil), opts.Env...),
		maxTimeout: maxTimeout,
		mcp:        opts.MCP,
		now:        now,
		maxRec:     maxRecords,
	}
	e.Update(cfg)
	return e
}

// Update replaces the configuration (hot reload). Safe for concurrent use with
// Dispatch.
//
// A nil receiver is tolerated: the console builds its panel before the runtime is
// wired, and a nil engine silently accepting a config is far less disruptive than a
// panic in a settings watcher.
func (e *Engine) Update(cfg Config) {
	if e == nil {
		return
	}
	cfg = cloneConfig(cfg)
	if cfg.Groups == nil {
		cfg.Groups = map[string][]Group{}
	}
	if len(cfg.EventOrder) == 0 {
		// ParseConfig always fills EventOrder; a Config built by hand may not. A
		// missing order would make dispatch order depend on Go's map iteration,
		// so derive the sorted order the parser would have produced.
		order := make([]string, 0, len(cfg.Groups))
		for name := range cfg.Groups {
			order = append(order, name)
		}
		sort.Strings(order)
		cfg.EventOrder = order
	}
	e.cfg.Store(&cfg)
}

// Config returns the active configuration.
//
// The result is a deep copy. The stored config is read by every concurrent Dispatch,
// so handing out its maps and slices would let a caller (the /hooks panel, say)
// mutate live state from another goroutine — and would also make the panel's view
// change under it when a settings watcher reloads. Groups, handlers and every
// reference-typed field are copied; see cloneConfig.
func (e *Engine) Config() Config {
	if e == nil {
		return Config{Groups: map[string][]Group{}}
	}
	cfg := e.cfg.Load()
	if cfg == nil {
		return Config{Groups: map[string][]Group{}}
	}
	return cloneConfig(*cfg)
}

// cloneConfig deep-copies a Config so no two owners share mutable state.
//
// Every reference-typed field is rebuilt: the Groups map, each event's group slice,
// each group's handler slice, and inside a handler the Args slice, Headers and Input
// maps and AllowedEnvVars slice. A shallow copy would leave all of those aliased to
// the original, which is exactly the bug this exists to prevent.
func cloneConfig(cfg Config) Config {
	out := Config{
		DisableAll: cfg.DisableAll,
		Groups:     make(map[string][]Group, len(cfg.Groups)),
		EventOrder: append([]string(nil), cfg.EventOrder...),
	}
	for event, groups := range cfg.Groups {
		clonedGroups := make([]Group, len(groups))
		for i, group := range groups {
			clonedHandlers := make([]Handler, len(group.Handlers))
			for j, handler := range group.Handlers {
				clonedHandlers[j] = cloneHandler(handler)
			}
			clonedGroups[i] = Group{Matcher: group.Matcher, Handlers: clonedHandlers}
		}
		out.Groups[event] = clonedGroups
	}
	return out
}

// cloneHandler copies the reference-typed fields of one handler.
func cloneHandler(h Handler) Handler {
	out := h
	out.Args = append([]string(nil), h.Args...)
	out.AllowedEnvVars = append([]string(nil), h.AllowedEnvVars...)
	if h.Headers != nil {
		out.Headers = make(map[string]string, len(h.Headers))
		for k, v := range h.Headers {
			out.Headers[k] = v
		}
	}
	if h.Input != nil {
		out.Input = make(map[string]string, len(h.Input))
		for k, v := range h.Input {
			out.Input[k] = v
		}
	}
	return out
}

// Recent returns the most recent log records, newest first, bounded by limit. A
// limit of 0 or less returns everything the log holds.
func (e *Engine) Recent(limit int) []Record {
	if e == nil {
		return nil
	}
	e.logMu.Lock()
	defer e.logMu.Unlock()
	if limit <= 0 || limit > len(e.records) {
		limit = len(e.records)
	}
	out := make([]Record, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, e.records[len(e.records)-1-i])
	}
	return out
}

// Dispatch runs every handler whose event+matcher matches, in parallel, and
// aggregates their decisions per §5.5. It never returns an error: a failing hook
// is a recorded failure, not a failed turn.
func (e *Engine) Dispatch(ctx context.Context, in Input) Verdict {
	verdict := Verdict{Continue: true}
	if e == nil {
		return verdict
	}
	// A caller that passed a nil context gets Background rather than a panic:
	// there is no cancellation to honour, which is what a nil context means.
	if ctx == nil {
		ctx = context.Background()
	}

	cfg := e.Config()
	if cfg.DisableAll {
		// §1.3: disableAllHooks turns every hook off while keeping the config.
		// Recorded once rather than per handler: the operator needs to know the
		// configuration still exists and why nothing ran.
		verdict.Skipped = append(verdict.Skipped,
			"disableAllHooks 为 true，已跳过全部 hook（§1.3，配置保留）")
		return verdict
	}

	event := in.HookEventName
	if !IsSupported(event) {
		// All events are parsed and displayed, but only the five in
		// SupportedEvents are dispatched by this build. The configured groups are
		// named in the reason so the panel shows that the config was read.
		if groups := cfg.Groups[event]; len(groups) > 0 {
			verdict.Skipped = append(verdict.Skipped, fmt.Sprintf(
				"事件 %s 配置了 %d 个 matcher 组，但本版本不派发该事件（支持：%s）",
				event, len(groups), strings.Join(SupportedEvents, "、")))
		}
		return verdict
	}

	spec := SpecFor(event)
	value, hasMatcher := matchValue(spec, in)

	planned := e.plan(event, value, hasMatcher, in)
	if len(planned) == 0 {
		return verdict
	}

	payload, err := json.Marshal(in)
	if err != nil {
		// A payload that cannot be marshalled would make every handler run with
		// empty stdin, so no handler is run at all.
		verdict.Failures = append(verdict.Failures,
			fmt.Sprintf("序列化 hook 输入失败：%v", err))
		return verdict
	}

	// §1/§7-13: every matching handler runs in parallel, not serially. Each one
	// writes its own slot, so the aggregation below stays in configured order no
	// matter which handler finishes first.
	outcomes := make([]handlerOutcome, len(planned))
	var wg sync.WaitGroup
	for i := range planned {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			outcomes[slot] = e.runHandler(ctx, in, planned[slot], payload)
		}(i)
	}
	wg.Wait()

	return e.aggregate(verdict, outcomes)
}

// plan is one handler about to run, together with the facts the log record needs.
type plan struct {
	handler Handler
	// matcher and value are copied here so the record can report the exact test
	// that selected the handler, even if the matcher is changed by a hot reload
	// while the handler runs.
	matcher string
	value   string
	// skipReason is non-empty when configuration alone already decided that this
	// handler cannot run.
	skipReason string
}

// handlerOutcome is the result of one planned handler.
type handlerOutcome struct {
	plan    plan
	skipped bool
	// skipReason is the operator-facing line describing why nothing ran.
	skipReason string
	ran        bool
	output     parsedOutput
	// failures are non-blocking problems, phrased for the console panel.
	failures []string
	exitCode int
	// records are the event-log lines this handler produced, written by the
	// aggregate rather than by the handler itself. Collecting them here is what
	// makes the log deterministic: emitted at completion time, two handlers of
	// one dispatch would swap places run to run, and the Blocked column could not
	// be filled in before the decision was parsed.
	records []Record
}

// plan walks the event's groups in configured order and decides, per handler,
// whether it runs and if not, why. Skips decided here are pure configuration
// questions — no process has been started yet — while skips decided by runHandler
// come from the handler itself.
//
// Configured order is preserved because §7-13's parallelism applies to execution,
// not to reporting: the Verdict's skip and failure lines must not move around
// between two identical dispatches just because goroutines finished in a different
// order.
func (e *Engine) plan(event, value string, hasMatcher bool, in Input) []plan {
	cfg := e.Config()
	narrow := SpecFor(event).narrowCharset
	var plans []plan
	for _, group := range cfg.Groups[event] {
		// §2.1: an event without matcher support fires on every trigger, so the
		// configured matcher is ignored rather than used to filter it out.
		if hasMatcher && !matchesMatcher(group.Matcher, value, narrow) {
			// A matcher that did not select the handler is not a skip worth
			// reporting: it is the normal case for every group of the event that
			// was not aimed at this tool. Reporting it would bury the real skips.
			continue
		}
		for _, handler := range group.Handlers {
			plans = append(plans, plan{
				handler:    handler,
				matcher:    group.Matcher,
				value:      value,
				skipReason: e.preflightSkipReason(event, handler),
			})
		}
	}
	return plans
}

// preflightSkipReason reports why a handler cannot run at all, before any I/O.
// An empty result means the handler should be attempted.
func (e *Engine) preflightSkipReason(event string, h Handler) string {
	// §3: "if" is a permission-rule filter, evaluated only on the five tool
	// events. This build does not implement permission-rule matching, so a
	// handler carrying one is skipped with that reason instead of being run as if
	// the rule had matched. Running it would be the dangerous direction: the
	// operator wrote "Bash(git *)" expecting a narrow hook and would get every
	// call of every tool.
	if h.If != "" {
		return fmt.Sprintf("handler 带 if 过滤条件 %q：本版本未实现权限规则求值，无法判定是否命中，已跳过（§3）", h.If)
	}

	switch h.Type {
	case HandlerCommand:
		if strings.TrimSpace(h.Command) == "" {
			return "command 类型 handler 缺少 command 字段（§3 要求必填），已跳过"
		}
		// §3 allows shell "bash" or "powershell"; anything else is a config this
		// build cannot honour, and guessing bash for it would run the command in a
		// shell the author did not choose. Checked here so the skip is reported
		// before a process is started.
		switch h.Shell {
		case "", "bash", "sh":
		case "powershell":
			return "shell \"powershell\" 本版本不支持（§3 允许该值，但本运行时只有 sh/bash），已跳过"
		default:
			return fmt.Sprintf("shell %q 本版本不支持（§3 只定义 bash/powershell），已跳过", h.Shell)
		}
		return ""
	case HandlerHTTP:
		if strings.TrimSpace(h.URL) == "" {
			return "http 类型 handler 缺少 url 字段（§3 要求必填），已跳过"
		}
		if event == "SessionStart" {
			return "SessionStart 不支持 http 类型 handler（§3：该事件仅支持 command 与 mcp_tool），已跳过"
		}
		return ""
	case HandlerMCPTool:
		if strings.TrimSpace(h.Server) == "" || strings.TrimSpace(h.Tool) == "" {
			return "mcp_tool 类型 handler 缺少 server 或 tool 字段（§3 要求必填），已跳过"
		}
		// §3 warns that SessionStart fires before MCP servers are ready, so the
		// protocol itself skips these handlers.
		if event == "SessionStart" {
			return "SessionStart 上 MCP server 尚未就绪，mcp_tool hook 被跳过（§3）"
		}
		if e.mcp == nil {
			// The injected caller is missing: recorded as skipped, never a silent
			// no-op, so the console shows the handler exists and why it did not
			// run.
			return "本版本未接入 MCP caller，mcp_tool hook 已跳过"
		}
		return ""
	case HandlerPrompt:
		return "prompt 类型 handler 本版本未实现（不调用 LLM），已跳过（§9）"
	case HandlerAgent:
		return "agent 类型 handler 本版本未实现（不调用 LLM），已跳过（§9）"
	case "":
		return "handler 缺少 type 字段，已跳过（§3）"
	default:
		return fmt.Sprintf("handler 类型 %q 本版本不支持，已跳过（§3）", h.Type)
	}
}

// runHandler executes one planned handler and converts its result into an
// outcome. It never returns an error: everything that can go wrong is a recorded
// failure, because Dispatch's contract is that a broken hook does not fail a turn.
func (e *Engine) runHandler(ctx context.Context, in Input, p plan, payload []byte) handlerOutcome {
	outcome := handlerOutcome{plan: p}
	if p.skipReason != "" {
		outcome.skipped = true
		outcome.skipReason = p.skipReason
		return outcome
	}

	h := p.handler
	switch h.Type {
	case HandlerCommand:
		if h.Async || h.AsyncRewake {
			return e.runAsyncCommand(ctx, in, p, payload)
		}
		return e.runCommand(ctx, in, p, payload)
	case HandlerHTTP:
		return e.runHTTP(ctx, in, p, payload)
	case HandlerMCPTool:
		return e.runMCPTool(ctx, in, p)
	default:
		// Unreachable: preflightSkipReason refuses every other type. Kept so a
		// future type added to the switch above cannot fall through to "ran but
		// did nothing".
		outcome.skipped = true
		outcome.skipReason = fmt.Sprintf("handler 类型 %q 无执行实现，已跳过", h.Type)
		return outcome
	}
}

// runCommand runs a command handler in exec form or shell form (§3).
func (e *Engine) runCommand(ctx context.Context, in Input, p plan, payload []byte) handlerOutcome {
	outcome := handlerOutcome{plan: p}
	h := p.handler

	argv, err := e.commandArgv(h)
	if err != nil {
		outcome.failures = append(outcome.failures, err.Error())
		return outcome
	}

	runCtx, cancel := context.WithTimeout(ctx, e.handlerTimeout(in.HookEventName, h))
	defer cancel()

	stdout := &boundedBuffer{limit: maxStreamBytes}
	stderr := &boundedBuffer{limit: maxStreamBytes}
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	// nil Stdin would make the child inherit this process's stdin, which for a
	// server is the terminal or /dev/null depending on how it was started. An
	// explicit empty reader keeps the process table honest: the child sees EOF.
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Dir = e.workDir()
	cmd.Env = e.commandEnv()

	started := e.now()
	err = cmd.Run()
	outcome.ran = true
	outcome.exitCode = exitCodeOf(cmd, err)
	duration := e.now().Sub(started)

	// The argv is recorded rather than the configured command string, because
	// for an exec-form handler the two differ exactly where a debugging operator
	// needs to see the difference: after placeholder expansion.
	outcome.records = append(outcome.records, Record{
		At:         started,
		Event:      in.HookEventName,
		Matcher:    p.matcher,
		Value:      p.value,
		Handler:    h.Type,
		Command:    h.Command,
		SessionID:  in.SessionID,
		ToolName:   in.ToolName,
		DurationMs: duration.Milliseconds(),
		ExitCode:   outcome.exitCode,
		Reason:     strings.Join(argv, " "),
	})

	if runCtx.Err() != nil {
		// §5.7: a cancelled handler's output is discarded, so it contributes no
		// decision. For this build's five events that means "no decision", which
		// is exactly what falling through with an empty outcome produces.
		reason := "超时"
		if errors.Is(runCtx.Err(), context.Canceled) && ctx.Err() != nil {
			reason = "调用方取消了 hook"
		}
		outcome.failures = append(outcome.failures, fmt.Sprintf(
			"命中 %s 的 command hook %s %s（输出已丢弃，不影响决策，§5.7）",
			in.HookEventName, h.Command, reason))
		return outcome
	}

	if err != nil {
		// A process that could not be started at all (§5.2: a wrong path fails
		// silently in Claude Code too — this runtime at least reports it).
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			outcome.failures = append(outcome.failures,
				fmt.Sprintf("启动 hook 命令失败：%v", err))
			return outcome
		}
	}

	e.applyOutput(&outcome, in.HookEventName, stdout.Bytes(), stderr.String())
	return outcome
}

// runAsyncCommand starts a command handler without waiting for it (§9).
//
// The record is written immediately with a synthetic exit code, because there is
// no result yet: §9 says an async hook cannot block or control Claude, so its
// decision fields are meaningless and are never read. The process is deliberately
// detached from ctx — tying it to the turn's context would kill exactly the long
// jobs async exists for — but the context's values are kept for logging.
//
// asyncRewake (§3) additionally wakes Claude when the process exits 2. That needs
// a session to wake, which this runtime does not have, so it is treated as plain
// async and the record says so.
func (e *Engine) runAsyncCommand(ctx context.Context, in Input, p plan, payload []byte) handlerOutcome {
	outcome := handlerOutcome{plan: p}
	h := p.handler

	argv, err := e.commandArgv(h)
	if err != nil {
		outcome.failures = append(outcome.failures, err.Error())
		return outcome
	}

	reason := "async: 已后台启动，不参与决策"
	if h.AsyncRewake {
		reason = "async: 已后台启动，不参与决策（asyncRewake 本版本不支持，按 async 处理）"
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.Dir = e.workDir()
	cmd.Env = e.commandEnv()
	if err := cmd.Start(); err != nil {
		outcome.ran = true
		outcome.exitCode = asyncRecordExitCode
		outcome.failures = append(outcome.failures,
			fmt.Sprintf("启动 async hook 命令失败：%v", err))
		outcome.records = append(outcome.records, Record{
			At:        e.now(),
			Event:     in.HookEventName,
			Matcher:   p.matcher,
			Value:     p.value,
			Handler:   h.Type,
			Command:   h.Command,
			SessionID: in.SessionID,
			ToolName:  in.ToolName,
			ExitCode:  asyncRecordExitCode,
			Reason:    reason,
			Error:     err.Error(),
		})
		return outcome
	}

	// Reaping is the goroutine's only job; the caller never sees the result.
	go func() {
		if waitErr := cmd.Wait(); waitErr != nil {
			e.logger.Debug("async hook 进程结束",
				zap.String("event", in.HookEventName),
				zap.String("command", h.Command),
				zap.Error(waitErr))
		}
	}()
	_ = ctx

	outcome.ran = true
	outcome.exitCode = asyncRecordExitCode
	outcome.records = append(outcome.records, Record{
		At:        e.now(),
		Event:     in.HookEventName,
		Matcher:   p.matcher,
		Value:     p.value,
		Handler:   h.Type,
		Command:   h.Command,
		SessionID: in.SessionID,
		ToolName:  in.ToolName,
		ExitCode:  asyncRecordExitCode,
		Reason:    reason,
	})
	return outcome
}

// runHTTP posts the payload to an http handler (§3, §5.6).
func (e *Engine) runHTTP(ctx context.Context, in Input, p plan, payload []byte) handlerOutcome {
	outcome := handlerOutcome{plan: p}
	h := p.handler

	url := e.expandString(h.URL)
	runCtx, cancel := context.WithTimeout(ctx, e.handlerTimeout(in.HookEventName, h))
	defer cancel()

	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		outcome.failures = append(outcome.failures,
			fmt.Sprintf("构造 hook HTTP 请求失败：%v", err))
		return outcome
	}
	// §3: the request body is the hook input JSON. Setting the header explicitly
	// (rather than letting Go sniff) also keeps a caller-supplied header from
	// silently changing the encoding.
	req.Header.Set("Content-Type", "application/json")
	for name, value := range h.Headers {
		// §3: header values interpolate $VAR/${VAR} from allowedEnvVars only;
		// a variable that is not listed becomes the empty string.
		req.Header.Set(name, interpolateAllowedEnv(value, h.AllowedEnvVars))
	}

	started := e.now()
	resp, err := e.httpClient().Do(req)
	outcome.ran = true
	duration := e.now().Sub(started)

	record := Record{
		At:         started,
		Event:      in.HookEventName,
		Matcher:    p.matcher,
		Value:      p.value,
		Handler:    h.Type,
		Command:    url,
		SessionID:  in.SessionID,
		ToolName:   in.ToolName,
		DurationMs: duration.Milliseconds(),
		ExitCode:   -1,
	}

	if err != nil {
		if runCtx.Err() != nil {
			record.Reason = "超时（§5.7：hook 被取消）"
			outcome.failures = append(outcome.failures, fmt.Sprintf(
				"命中 %s 的 http hook %s 超时，已取消（§5.7）", in.HookEventName, url))
		} else {
			// §5.6: a connection failure is a non-blocking error.
			record.Error = err.Error()
			outcome.failures = append(outcome.failures,
				fmt.Sprintf("http hook %s 请求失败（非阻塞错误，§5.6）：%v", url, err))
		}
		outcome.records = append(outcome.records, record)
		return outcome
	}
	defer func() { _ = resp.Body.Close() }()
	record.ExitCode = resp.StatusCode

	// The body is read through the same bound as a command hook's stdout, so a
	// response that never ends cannot grow the process.
	body := &boundedBuffer{limit: maxStreamBytes}
	if _, err := io.Copy(body, resp.Body); err != nil {
		record.Error = err.Error()
		outcome.failures = append(outcome.failures,
			fmt.Sprintf("读取 http hook %s 响应失败：%v", url, err))
		outcome.records = append(outcome.records, record)
		return outcome
	}
	raw := body.Bytes()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// §5.6: non-2xx is a non-blocking error and execution continues. HTTP can
		// never block on a status code alone — a block needs a decision field in a
		// 2xx body.
		record.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		outcome.failures = append(outcome.failures, fmt.Sprintf(
			"http hook %s 返回 HTTP %d（非阻塞错误，§5.6：HTTP 无法只靠状态码阻塞）：%s",
			url, resp.StatusCode, truncateForLog(strings.TrimSpace(string(raw)))))
		outcome.records = append(outcome.records, record)
		return outcome
	}

	trimmed := bytes.TrimSpace(raw)
	switch {
	case len(trimmed) == 0:
		// §5.6: 2xx + empty body is success, equal to exit 0 with no output.
		record.Reason = "2xx 空响应体：成功，无决策（§5.6）"
		outcome.records = append(outcome.records, record)
		return outcome
	case trimmed[0] == '{':
		out, parseErr := parseOutput(in.HookEventName, trimmed)
		if parseErr != nil {
			record.Error = parseErr.Error()
			outcome.failures = append(outcome.failures,
				fmt.Sprintf("http hook %s 响应体校验失败（非阻塞错误，§5.6）：%v", url, parseErr))
			outcome.records = append(outcome.records, record)
			return outcome
		}
		record.Reason = "2xx JSON 决策"
		outcome.records = append(outcome.records, record)
		e.applyParsed(&outcome, out)
		return outcome
	default:
		// §5.6: 2xx + non-JSON body is a non-blocking error, and the text is NOT
		// added to Claude's context — unlike a command hook's plain stdout on
		// SessionStart/UserPromptSubmit.
		record.Error = "响应体不是 JSON 对象"
		outcome.failures = append(outcome.failures, fmt.Sprintf(
			"http hook %s 返回非 JSON 响应体（非阻塞错误，文本不会进入上下文，§5.6）：%s",
			url, truncateForLog(string(trimmed))))
		outcome.records = append(outcome.records, record)
		return outcome
	}
}

// runMCPTool calls a connected MCP server's tool (§3: the tool's text output is
// parsed with the same rules as a command hook's stdout).
func (e *Engine) runMCPTool(ctx context.Context, in Input, p plan) handlerOutcome {
	outcome := handlerOutcome{plan: p}
	h := p.handler

	input := flattenInputVars(h.Input, in, e.pathVars())

	started := e.now()
	text, err := e.mcp.Call(ctx, h.Server, h.Tool, input)
	duration := e.now().Sub(started)
	outcome.ran = true

	record := Record{
		At:         started,
		Event:      in.HookEventName,
		Matcher:    p.matcher,
		Value:      p.value,
		Handler:    h.Type,
		Command:    h.Server + "." + h.Tool,
		SessionID:  in.SessionID,
		ToolName:   in.ToolName,
		DurationMs: duration.Milliseconds(),
		ExitCode:   0,
	}
	if err != nil {
		record.ExitCode = -1
		record.Error = err.Error()
		outcome.failures = append(outcome.failures, fmt.Sprintf(
			"mcp_tool hook %s.%s 调用失败（非阻塞错误）：%v", h.Server, h.Tool, err))
		outcome.records = append(outcome.records, record)
		return outcome
	}

	record.Reason = truncateForLog(text)
	outcome.records = append(outcome.records, record)
	e.applyOutput(&outcome, in.HookEventName, []byte(text), "")
	return outcome
}

// applyOutput turns a handler's stdout-equivalent and stderr into the outcome,
// implementing §5.1 (parsing), §5.2/§5.3 (exit codes) and §5.5 (decisions).
//
// The stderr text is kept even when stdout parsed cleanly, because §5.2 makes
// stderr the fallback block reason for exit 2. For exit 0 stderr only ever
// reaches the debug log — §5.2 is explicit that Claude never sees it — so it is
// not turned into a message of any kind.
func (e *Engine) applyOutput(outcome *handlerOutcome, event string, stdout []byte, stderr string) {
	exitCode := outcome.exitCode
	spec := SpecFor(event)
	stderrTrimmed := strings.TrimSpace(stderr)

	trimmed := bytes.TrimSpace(stdout)
	parsed, parseErr := parseOutput(event, trimmed)
	validJSON := parseErr == nil
	empty := len(trimmed) == 0

	switch exitCode {
	case 0:
		if validJSON || !empty {
			// §5.1: output that is not a JSON object is plain text, and
			// parseOutput has already routed it per event.
			e.applyParsed(outcome, parsed)
		}
		if stderrTrimmed != "" {
			e.logger.Debug("hook stderr（exit 0，仅进调试日志，§5.2）",
				zap.String("event", event), zap.String("stderr", truncateForLog(stderrTrimmed)))
		}
		return
	case 2:
		// §5.2/§5.3: exit 2 blocks on the events that can block, and Claude Code
		// still reads valid JSON from stdout. On the events that cannot block,
		// exit 2 only surfaces the stderr text.
		if !spec.Blockable {
			// §5.3: PostToolUse's exit 2 shows stderr to Claude (the tool has
			// already run); SessionStart's shows it to the user and Claude never
			// sees it. Neither blocks.
			outcome.failures = append(outcome.failures, fmt.Sprintf(
				"hook 以 exit 2 结束，但事件 %s 不能阻塞（§5.3），已按非阻塞处理%s",
				event, stderrSuffix(stderrTrimmed)))
		} else {
			// §5.2: the block reason comes from the JSON decision when there is
			// one, otherwise from stderr. The order matters: overwriting a JSON
			// reason with stderr would report the wrong reason for the block.
			if parsed.BlockReason == "" {
				parsed.BlockReason = stderrTrimmed
			}
			parsed.Block = true
			// §5.2/§8: exit 2 routes like a deny and cannot be overridden by
			// permissionDecision "allow" on stdout. "ask"/"defer" are overwritten
			// too: the block has already decided the outcome, and reporting "ask"
			// would describe a prompt that never appears.
			switch parsed.PermissionDecision {
			case "deny":
			default:
				parsed.PermissionDecision = "deny"
			}
			if parsed.BlockReason == "" {
				outcome.failures = append(outcome.failures, fmt.Sprintf(
					"hook 以 exit 2 阻塞了 %s，但既无 JSON 决策也无 stderr，没有可用的阻塞原因（§5.2）", event))
			}
		}
		if !validJSON && !empty {
			// §5.2: exit 2 with unparseable stdout still blocks; the failed
			// validation is only recorded (§5.2, v2.1.214+ behaviour). An empty
			// stdout is not a validation failure, so it is not reported as one.
			outcome.failures = append(outcome.failures,
				"exit 2 时 stdout 不是合法 JSON 对象，已忽略其内容（§5.2）")
		}
		e.applyParsed(outcome, parsed)
		return
	default:
		// §5.2: any other exit code is a non-blocking failure, INCLUDING 1 — a
		// policy hook that exits 1 fails open, which is why the reference tells
		// implementers to block with exit 2. Valid JSON on stdout is ignored: the
		// exit code is not 0 or 2, so there is no decision to act on.
		//
		// §5.2 is explicit that an empty stdout with a non-0/non-2 exit code is
		// itself the non-blocking error, so the two shapes are reported with the
		// text that describes them rather than as one generic message.
		switch {
		case validJSON:
			outcome.failures = append(outcome.failures, fmt.Sprintf(
				"hook 以 exit %d 结束：非阻塞错误，stdout 上的 JSON 决策被忽略（§5.2）", exitCode))
		case empty:
			outcome.failures = append(outcome.failures, fmt.Sprintf(
				"hook 以 exit %d 结束且无输出：非阻塞错误，动作继续（§5.2）%s",
				exitCode, stderrSuffix(stderrTrimmed)))
		default:
			outcome.failures = append(outcome.failures, fmt.Sprintf(
				"hook 以 exit %d 结束且 stdout 不是合法 JSON：非阻塞错误，动作继续（§5.2）%s",
				exitCode, stderrSuffix(stderrTrimmed)))
		}
		return
	}
}

// applyParsed folds one handler's parsed decision into its outcome.
func (e *Engine) applyParsed(outcome *handlerOutcome, parsed parsedOutput) {
	outcome.output = parsed
	outcome.failures = append(outcome.failures, parsed.Failures...)
}

// aggregate combines every handler's outcome into the single Verdict the caller
// sees, applying §5.5's precedence rules:
//
//   - Block wins if any handler blocks, and the first blocking reason is kept.
//     The others become failures so they are still visible.
//   - continue:false beats everything (§5.4: "优先级高于事件自身的决策字段").
//   - permissionDecision follows deny > defer > ask > allow; regardless of
//     arrival order, a later deny overrides an earlier allow, and a later allow
//     must not resurrect an input a deny already rejected.
//
// Handlers are walked in configured order rather than completion order so the
// aggregate does not depend on scheduling — the parallel run is an execution
// detail, not part of the verdict. The event log is written from here for the same
// reason: records land in configured order, which is what makes two identical
// dispatches produce identical logs, and the Blocked column can be filled in from
// the parsed decision.
func (e *Engine) aggregate(verdict Verdict, outcomes []handlerOutcome) Verdict {
	denied := false
	for i := range outcomes {
		o := &outcomes[i]
		if o.skipped {
			verdict.Skipped = append(verdict.Skipped, o.skipReason)
			continue
		}
		if !o.ran {
			continue
		}
		verdict.Ran = true
		verdict.Handlers++
		verdict.Failures = append(verdict.Failures, o.failures...)

		p := o.output
		e.writeRecords(o.records, p.Block)
		verdict.SystemMessages = append(verdict.SystemMessages, p.SystemMessages...)
		verdict.AdditionalContext = append(verdict.AdditionalContext, p.AdditionalContext...)
		if p.InitialUserMessage != "" {
			verdict.InitialUserMessage = p.InitialUserMessage
		}
		if p.SessionTitle != "" {
			verdict.SessionTitle = p.SessionTitle
		}
		if len(p.WatchPaths) > 0 {
			verdict.WatchPaths = append(verdict.WatchPaths, p.WatchPaths...)
		}
		if p.ReloadSkills {
			verdict.ReloadSkills = true
		}
		if p.Retry {
			verdict.Retry = true
		}
		if p.Continue != nil && !*p.Continue {
			verdict.Continue = false
			if verdict.StopReason == "" {
				verdict.StopReason = p.StopReason
			}
		}

		if p.Block {
			if !verdict.Block {
				verdict.Block = true
				verdict.BlockReason = p.BlockReason
			} else if p.BlockReason != "" && p.BlockReason != verdict.BlockReason {
				// §5.5 keeps one verdict, but a second blocking reason is still
				// information the operator needs; it must not be dropped just
				// because another hook blocked first.
				verdict.Failures = append(verdict.Failures,
					fmt.Sprintf("另一个 hook 也阻止了本次调用：%s", p.BlockReason))
			}
		}

		switch p.PermissionDecision {
		case "deny":
			denied = true
			verdict.PermissionDecision = "deny"
			// §5.5/§8: updatedInput is ignored for deny, and the point of a deny
			// winning is that the tool must not run — so any input an earlier allow
			// contributed is dropped here rather than left for a caller that reads
			// UpdatedInput without also checking Block.
			verdict.UpdatedInput = nil
			if !verdict.Block {
				// §8: deny blocks the tool call.
				verdict.Block = true
			}
			if verdict.BlockReason == "" {
				verdict.BlockReason = p.BlockReason
			}
		case "defer", "ask", "allow":
			if denied {
				// §5.5: a deny that already won is final. A later allow must not
				// change the outcome, and it must not leave behind the input an
				// earlier allow contributed — the caller would then apply an input
				// belonging to a decision that did not win.
				verdict.UpdatedInput = nil
				continue
			}
			if decisionRank(p.PermissionDecision) > decisionRank(verdict.PermissionDecision) {
				verdict.PermissionDecision = p.PermissionDecision
				// §5.5/§8: updatedInput is kept for allow and ask; with defer it is
				// documented as ignored, so a defer must CLEAR any input an earlier
				// allow contributed — leaving it behind would hand the caller an
				// input belonging to a decision that no longer wins.
				switch p.PermissionDecision {
				case "allow", "ask":
					if len(bytes.TrimSpace(p.UpdatedInput)) > 0 {
						verdict.UpdatedInput = p.UpdatedInput
					}
				default:
					verdict.UpdatedInput = nil
				}
			}
		}

		if p.HasUpdatedOutput {
			verdict.UpdatedToolOutput = p.UpdatedToolOutput
			verdict.HasUpdatedOutput = true
		}
	}

	if verdict.Block && verdict.BlockReason == "" {
		verdict.BlockReason = "hook 阻止了本次调用，但未给出原因"
	}
	return verdict
}

// decisionRank orders permissionDecision values by §8's precedence
// deny > defer > ask > allow. An empty decision ranks lowest so any real decision
// replaces it.
func decisionRank(decision string) int {
	switch decision {
	case "deny":
		return 3
	case "defer":
		return 2
	case "ask":
		return 1
	case "allow":
		return 0
	default:
		return -1
	}
}

// handlerTimeout resolves a handler's effective timeout and clamps it to the
// engine's MaxTimeout.
//
// The clamp is this build's own rule, not the protocol's: §3/§5.7 allow a config
// to ask for up to 600s, but this runtime serves interactive turns, and a turn
// that waits ten minutes for a logging hook is indistinguishable from a hang.
// Documented here so the difference from Claude Code is not a surprise: with
// MaxTimeout at its default the effective ceiling is 60s, and a config asking for
// more is clamped rather than rejected. An async handler is exempt (§9: Claude
// Code also stops enforcing timeout on it), because the caller does not wait for
// it at all.
func (e *Engine) handlerTimeout(event string, h Handler) time.Duration {
	timeout := defaultHandlerTimeout
	if d, ok := perEventDefaultTimeout[event]; ok {
		timeout = d
	}
	if h.Timeout > 0 {
		timeout = time.Duration(h.Timeout) * time.Second
	}
	if timeout > e.maxTimeout {
		return e.maxTimeout
	}
	return timeout
}

// commandArgv builds the argv for a command handler (§3).
//
// exec form (args present): command is resolved via PATH and spawned directly,
// one element per array entry, with ${...} placeholders expanded by plain string
// substitution. shell form (no args): the whole string goes to sh -c.
//
// The Args slice being non-nil, rather than non-empty, is what selects exec form:
// §9's async example writes "args": [] precisely to get exec form with no
// arguments, so treating an empty array as shell form would silently hand a
// script path to sh.
func (e *Engine) commandArgv(h Handler) ([]string, error) {
	if h.Args == nil {
		shell, err := shellArgv(h.Shell)
		if err != nil {
			return nil, err
		}
		// §3: shell form hands the entire string to the shell, so pipes, &&,
		// redirection and variable expansion all stay available.
		return append(shell, e.expandString(h.Command)), nil
	}
	argv := make([]string, 0, len(h.Args)+1)
	argv = append(argv, e.expandString(h.Command))
	for _, arg := range h.Args {
		// §3: every element is expanded as a plain string substitution. No
		// quoting or word splitting happens, which is exactly the difference from
		// shell form.
		argv = append(argv, e.expandString(arg))
	}
	return argv, nil
}

// shellArgv returns the shell invocation prefix for a handler's shell field.
func shellArgv(shell string) ([]string, error) {
	switch shell {
	case "", "bash", "sh":
		return []string{"sh", "-c"}, nil
	case "powershell":
		// Documented as an option (§3), but PowerShell's flag for "run this
		// string" is not this platform's concern and guessing at it would be worse
		// than refusing. A config that asks for it gets a clear failure.
		return nil, errors.New("claudehook: shell \"powershell\" 本版本不支持")
	default:
		return nil, fmt.Errorf("claudehook: 不支持的 shell %q", shell)
	}
}

// workDir resolves the directory command hooks run in (§1.2: the current working
// directory).
//
// §1.2 documents a fallback chain for a working directory that has been deleted
// out from under the session (session start dir → project root → home → temp). It
// is implemented here in its cheap form: the configured directory when it exists,
// otherwise the process's own working directory. The full chain is not modelled
// because this runtime always passes a directory it created or was given, so the
// deleted-directory case can only arise from an operator editing config
// underneath a running server.
func (e *Engine) workDir() string {
	if e.dir == "" {
		return ""
	}
	if info, err := os.Stat(e.dir); err == nil && info.IsDir() {
		return e.dir
	}
	e.logger.Warn("配置的 hook 工作目录不存在，回退到进程工作目录", zap.String("dir", e.dir))
	return ""
}

// commandEnv builds the child environment: the inherited environment plus
// Options.Env, with a placeholder-free view of the path variables.
func (e *Engine) commandEnv() []string {
	overrides := make(map[string]string, len(e.env))
	order := make([]string, 0, len(e.env))
	for _, kv := range e.env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		if _, seen := overrides[name]; !seen {
			order = append(order, name)
		}
		overrides[name] = value
	}
	if len(overrides) == 0 {
		return os.Environ()
	}

	merged := os.Environ()
	index := make(map[string]int, len(merged))
	for i, kv := range merged {
		if name, _, ok := strings.Cut(kv, "="); ok {
			index[name] = i
		}
	}
	for _, name := range order {
		kv := name + "=" + overrides[name]
		if i, ok := index[name]; ok {
			merged[i] = kv
			continue
		}
		merged = append(merged, kv)
	}
	return merged
}

// httpClient is the client http handlers use. The per-handler timeout lives on the
// request context, so the client itself carries none — a shared client with a
// fixed timeout would silently cap every hook at the same value.
func (e *Engine) httpClient() *http.Client { return &http.Client{} }

// envValue looks a variable up in Options.Env first and the process environment
// second. Options.Env wins because it is the caller's explicit statement about
// this runtime's session; the process environment is what a hook would inherit
// anyway.
func (e *Engine) envValue(name string) string {
	if e.env != nil {
		prefix := name + "="
		for _, kv := range e.env {
			if strings.HasPrefix(kv, prefix) {
				return strings.TrimPrefix(kv, prefix)
			}
		}
	}
	return os.Getenv(name)
}

// pathVars is the substitution table for ${...} placeholders.
//
// CLAUDE_PROJECT_DIR, CLAUDE_PLUGIN_ROOT and CLAUDE_PLUGIN_DATA are the three
// documented placeholders (§3). CLAUDE_PLUGIN_* are resolved from the environment
// only: this build has no plugin layer, and inventing a value would point a hook
// at a directory that does not exist. CLAUDE_PROJECT_DIR falls back to the
// working directory, which is this runtime's best available reading of "the
// project root" — the same fallback §1.2 uses when the session directory is gone.
func (e *Engine) pathVars() map[string]string {
	// Only names that actually resolve are put in the table, so an unset variable
	// leaves its placeholder in the string instead of blanking it (the rule
	// substituteKnownVars implements).
	vars := map[string]string{}
	for _, name := range []string{"CLAUDE_PROJECT_DIR", "CLAUDE_PLUGIN_ROOT", "CLAUDE_PLUGIN_DATA"} {
		if value := e.envValue(name); value != "" {
			vars[name] = value
		}
	}
	if _, ok := vars["CLAUDE_PROJECT_DIR"]; !ok {
		// The project directory is the one placeholder a caller can expect to be
		// filled in without configuring anything, so it falls back to the working
		// directory — the same fallback §1.2 uses when the session directory is
		// gone. CLAUDE_PLUGIN_* deliberately have no fallback: this build has no
		// plugin layer, and inventing a value would point a hook at a directory
		// that does not exist.
		if wd := e.workDir(); wd != "" {
			vars["CLAUDE_PROJECT_DIR"] = wd
		} else if cwd, err := os.Getwd(); err == nil {
			vars["CLAUDE_PROJECT_DIR"] = cwd
		}
	}
	return vars
}

// expandString applies §3's placeholder rules to one string: the path
// placeholders, then "~" at the start of the value as the user's home directory.
//
// Both are plain string substitutions — no shell is involved — which is what §3
// says the exec form does. A placeholder this build does not know stays as
// written rather than being blanked: dropping it would turn a path into a
// different path, and there is no documented rule for unknown placeholders
// (文档未说明), so leaving the literal text makes the failure visible in the
// handler's own error message.
func (e *Engine) expandString(value string) string {
	if value == "" {
		return value
	}
	// Only the known path variables are substituted; an unset one stays as written
	return expandTilde(substituteKnownVars(value, e.pathVars()))
}

// expandTilde replaces a leading "~" with the user's home directory. Only the
// leading segment is touched: a "~" anywhere else is a literal character, which is
// what every shell does and therefore what a hook author expects.
func expandTilde(value string) string {
	if value != "~" && !strings.HasPrefix(value, "~/") {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return value
	}
	return home + strings.TrimPrefix(value, "~")
}

// interpolateAllowedEnv resolves $VAR and ${VAR} in a header value using ONLY the
// variables named in allowedEnvVars (§3). An unlisted variable becomes the empty
// string, and a value with no allowedEnvVars at all resolves every variable to
// empty — which is the documented behaviour, and the safe direction: a header
// cannot leak a secret the config did not explicitly list.
func interpolateAllowedEnv(value string, allowed []string) string {
	if value == "" {
		return value
	}
	// Only the allowed names are in the table, so a reference to anything else is a
	// lookup miss and resolves to the empty string — exactly §3's rule, and the safe
	// direction: a header cannot leak a secret the config did not list. An empty
	// allowedEnvVars therefore blanks every reference, which is also documented
	// ("required for any env var interpolation to work").
	//
	// The payload is deliberately NOT a resolution source here, unlike in command
	// argument expansion: §3 makes header interpolation read environment variables
	// only, and a header value that quietly read the payload would be a channel the
	// config never opened.
	vars := make(map[string]string, len(allowed))
	for _, name := range allowed {
		vars[name] = os.Getenv(name)
	}
	return substituteAnyVar(value, vars)
}

// truncateForLog bounds a string that is going into a log record or a failure
// line. Records are rendered in a console panel, and an unbounded stderr dump
// would push every other record off the screen.
func truncateForLog(s string) string {
	const limit = 2000
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + fmt.Sprintf("…（已截断 %d 字符）", len(runes)-limit)
}

// stderrSuffix renders an optional stderr tail for a failure line.
func stderrSuffix(stderr string) string {
	if stderr == "" {
		return ""
	}
	return "；stderr：" + truncateForLog(stderr)
}

// exitCodeOf extracts a process's exit code. A signalled process (the usual shape
// of a timeout kill) has no exit code of its own, and -1 is used so the panel
// cannot mistake a killed hook for a successful one.
func exitCodeOf(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	if err == nil {
		return 0
	}
	return -1
}

// writeRecords appends a handler's log records under one lock acquisition.
//
// The whole batch is written together so a handler that produced several records
// (an async start plus a delivery note, say) cannot be interleaved with another
// handler's records. `blocked` comes from the handler's parsed decision, which is
// the only thing that can answer the panel's question — "which hook blocked this
// call" — rather than "did anything block during this dispatch".
func (e *Engine) writeRecords(records []Record, blocked bool) {
	if len(records) == 0 {
		return
	}
	e.logMu.Lock()
	defer e.logMu.Unlock()
	for _, rec := range records {
		if rec.At.IsZero() {
			rec.At = e.now()
		}
		rec.Blocked = blocked
		e.records = append(e.records, rec)
		if over := len(e.records) - e.maxRec; over > 0 {
			e.records = append(e.records[:0], e.records[over:]...)
		}
	}
}

// boundedBuffer collects at most limit bytes and then discards the rest, counting
// what it dropped so a truncation is visible rather than looking like a complete
// output.
type boundedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	room := b.limit - b.buf.Len()
	if room <= 0 {
		b.truncated += n
		return n, nil
	}
	if n > room {
		b.truncated += n - room
		p = p[:room]
	}
	b.buf.Write(p)
	return n, nil
}

// Bytes returns what was collected, with a note appended when the bound was hit.
func (b *boundedBuffer) Bytes() []byte {
	out := b.buf.Bytes()
	if b.truncated == 0 {
		return out
	}
	note := fmt.Sprintf("\n…（输出超过 %d 字节上限，已丢弃 %d 字节）", b.limit, b.truncated)
	return append(append([]byte(nil), out...), note...)
}

// String is Bytes as a string, for the paths that want to log or match on it.
func (b *boundedBuffer) String() string { return string(b.Bytes()) }
