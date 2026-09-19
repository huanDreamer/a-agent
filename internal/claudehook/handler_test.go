package claudehook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// httpRecorder captures one POST an http handler made, so the tests can assert on
// the request as well as the response handling.
type httpRecorder struct {
	Method      string
	Path        string
	ContentType string
	Headers     http.Header
	Body        []byte
	Calls       int
}

// newHookServer starts an httptest server whose reply is produced by reply, which
// receives the recorder so a case can decide its response from the request.
func newHookServer(t *testing.T, reply func(w http.ResponseWriter, r *http.Request, rec *httpRecorder)) (*httptest.Server, *httpRecorder) {
	t.Helper()
	rec := &httpRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0, 1024)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		rec.Method = r.Method
		rec.Path = r.URL.Path
		rec.ContentType = r.Header.Get("Content-Type")
		rec.Headers = r.Header.Clone()
		rec.Body = body
		rec.Calls++
		reply(w, r, rec)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// httpHandler builds an http handler pointing at url, with the given extra fields.
func httpHandler(url string, timeout int) Handler {
	return Handler{Type: HandlerHTTP, URL: url, Timeout: timeout}
}

// TestHTTPPostsPayload covers §3/§5.6: the request body is the hook input JSON, the
// method is POST, and the content type is application/json.
func TestHTTPPostsPayload(t *testing.T) {
	srv, rec := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ *httpRecorder) {
		w.WriteHeader(http.StatusOK)
	})

	handler := httpHandler(srv.URL+"/hooks/pre-tool-use", 5)
	cfg := groupsFor("PreToolUse", "Bash", handler)
	verdict := dispatchInDir(t, cfg, Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second}, preToolUse("Bash"))

	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d（failures=%v）", verdict.Handlers, verdict.Failures)
	}
	if rec.Calls != 1 {
		t.Fatalf("服务端收到 %d 次请求", rec.Calls)
	}
	if rec.Method != http.MethodPost {
		t.Errorf("Method = %q，期望 POST", rec.Method)
	}
	if rec.Path != "/hooks/pre-tool-use" {
		t.Errorf("Path = %q", rec.Path)
	}
	if rec.ContentType != "application/json" {
		t.Errorf("Content-Type = %q（§3）", rec.ContentType)
	}
	// The body must be the same payload a command hook would get on stdin.
	var got map[string]any
	if err := json.Unmarshal(rec.Body, &got); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v（%s）", err, rec.Body)
	}
	if got["hook_event_name"] != "PreToolUse" || got["tool_name"] != "Bash" {
		t.Errorf("请求体不是 hook 输入：%v", got)
	}
	if _, ok := got["tool_input"]; !ok {
		t.Error("请求体缺 tool_input")
	}
}

// TestHTTPHeaderInterpolation covers §3's rule: header values interpolate $VAR and
// ${VAR} from allowedEnvVars ONLY, and an unlisted variable becomes the empty
// string. Both spellings of the variable reference are exercised.
func TestHTTPHeaderInterpolation(t *testing.T) {
	t.Setenv("HOOK_TOKEN", "s3cr3t")
	t.Setenv("HOOK_LEAK", "must-not-appear")

	srv, rec := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ *httpRecorder) {
		w.WriteHeader(http.StatusOK)
	})

	handler := httpHandler(srv.URL, 5)
	handler.Headers = map[string]string{
		"Authorization": "Bearer $HOOK_TOKEN",
		"X-Braced":      "token=${HOOK_TOKEN}",
		// HOOK_LEAK is deliberately absent from allowedEnvVars.
		"X-Unlisted":  "v=$HOOK_LEAK",
		"X-Unknown":   "v=$NOT_SET_ANYWHERE",
		"X-NoDollars": "plain",
	}
	handler.AllowedEnvVars = []string{"HOOK_TOKEN"}
	cfg := groupsFor("PreToolUse", "Bash", handler)

	verdict := dispatchInDir(t, cfg, Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second}, preToolUse("Bash"))
	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d（failures=%v）", verdict.Handlers, verdict.Failures)
	}

	if got := rec.Headers.Get("Authorization"); got != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q，期望 $HOOK_TOKEN 被解析", got)
	}
	if got := rec.Headers.Get("X-Braced"); got != "token=s3cr3t" {
		t.Errorf("X-Braced = %q，期望 ${HOOK_TOKEN} 被解析", got)
	}
	// §3: a variable that is not listed resolves to the empty string. This is the
	// security-relevant half of the rule.
	if got := rec.Headers.Get("X-Unlisted"); got != "v=" {
		t.Errorf("X-Unlisted = %q，未列入 allowedEnvVars 的变量应替换为空串（§3）", got)
	}
	if got := rec.Headers.Get("X-Unknown"); got != "v=" {
		t.Errorf("X-Unknown = %q，应为空串", got)
	}
	if got := rec.Headers.Get("X-NoDollars"); got != "plain" {
		t.Errorf("X-NoDollars = %q", got)
	}
	// The body of the request must not carry the secret anywhere.
	if strings.Contains(string(rec.Body), "must-not-appear") {
		t.Error("未授权变量泄漏进了请求体")
	}
}

// TestHTTPNoAllowedEnvVarsBlanksInterpolation covers the other half of §3's rule: a
// header template with no allowedEnvVars at all resolves every variable to empty,
// rather than sending the literal "$TOKEN" and confusing the server.
func TestHTTPNoAllowedEnvVarsBlanksInterpolation(t *testing.T) {
	t.Setenv("HOOK_TOKEN", "s3cr3t")

	srv, rec := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ *httpRecorder) {
		w.WriteHeader(http.StatusOK)
	})
	handler := httpHandler(srv.URL, 5)
	handler.Headers = map[string]string{"Authorization": "Bearer $HOOK_TOKEN"}
	cfg := groupsFor("PreToolUse", "Bash", handler)

	dispatchInDir(t, cfg, Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second}, preToolUse("Bash"))
	// The header arrives as "Bearer" rather than "Bearer ": an empty substitution
	// leaves a trailing space, and net/http trims surrounding whitespace on the wire.
	// What matters is that the reference is gone and the secret with it.
	if got := rec.Headers.Get("Authorization"); got != "Bearer" {
		t.Errorf("Authorization = %q，未给 allowedEnvVars 时应替换为空串（§3）", got)
	}
	if strings.Contains(strings.Join(rec.Headers.Values("Authorization"), ","), "s3cr3t") {
		t.Error("未列入 allowedEnvVars 的变量泄漏进了请求头")
	}
}

// TestHTTPResponse2xxEmpty covers §5.6's first row: 2xx with an empty body is
// success, equal to exit 0 with no output.
func TestHTTPResponse2xxEmpty(t *testing.T) {
	srv, _ := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ *httpRecorder) {
		w.WriteHeader(http.StatusNoContent)
	})
	cfg := groupsFor("PreToolUse", "Bash", httpHandler(srv.URL, 5))

	verdict := dispatchInDir(t, cfg, Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second}, preToolUse("Bash"))
	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d", verdict.Handlers)
	}
	if len(verdict.Failures) != 0 {
		t.Errorf("2xx 空响应体是成功，不应有失败：%v", verdict.Failures)
	}
	if verdict.Block || len(verdict.SystemMessages) != 0 {
		t.Errorf("空响应体不应产生决策：%+v", verdict)
	}
}

// TestHTTPResponse2xxJSON covers §5.6's second row: a 2xx JSON object body is
// parsed with the same schema as command stdout.
func TestHTTPResponse2xxJSON(t *testing.T) {
	srv, _ := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ *httpRecorder) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"服务端拒绝"}}`))
	})
	cfg := groupsFor("PreToolUse", "Bash", httpHandler(srv.URL, 5))

	verdict := dispatchInDir(t, cfg, Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second}, preToolUse("Bash"))
	if !verdict.Block {
		t.Fatalf("Block = false（failures=%v）", verdict.Failures)
	}
	if verdict.BlockReason != "服务端拒绝" {
		t.Errorf("BlockReason = %q", verdict.BlockReason)
	}
	if verdict.PermissionDecision != "deny" {
		t.Errorf("PermissionDecision = %q", verdict.PermissionDecision)
	}
}

// TestHTTPResponse2xxJSONRequiresHookEventName covers §5.4's mandatory
// hookEventName for HTTP too: a mismatched name invalidates the object.
func TestHTTPResponse2xxJSONRequiresHookEventName(t *testing.T) {
	srv, _ := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ *httpRecorder) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"hookSpecificOutput":{"hookEventName":"Stop","permissionDecision":"deny"}}`))
	})
	cfg := groupsFor("PreToolUse", "Bash", httpHandler(srv.URL, 5))

	verdict := dispatchInDir(t, cfg, Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second}, preToolUse("Bash"))
	if verdict.Block {
		t.Error("hookEventName 不匹配时不应阻塞")
	}
	if !contains(verdict.Failures, "hookEventName") {
		t.Errorf("拒绝应被记录：%v", verdict.Failures)
	}
}

// TestHTTPResponse2xxNonJSON covers §5.6's third row: 2xx with a non-JSON body is a
// non-blocking error AND the text is not added to the context — the documented
// difference from a command hook's plain stdout on UserPromptSubmit/SessionStart.
func TestHTTPResponse2xxNonJSON(t *testing.T) {
	srv, _ := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ *httpRecorder) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK, looked at it"))
	})
	cfg := groupsFor("PreToolUse", "Bash", httpHandler(srv.URL, 5))

	verdict := dispatchInDir(t, cfg, Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second}, preToolUse("Bash"))

	if len(verdict.AdditionalContext) != 0 {
		t.Errorf("HTTP 的非 JSON 文本不应进入上下文（§5.6）：%v", verdict.AdditionalContext)
	}
	if !contains(verdict.Failures, "非 JSON") {
		t.Errorf("应以非阻塞错误记录：%v", verdict.Failures)
	}
	if verdict.Block {
		t.Error("2xx 非 JSON 不应阻塞（§5.6）")
	}
	if verdict.Handlers != 1 {
		t.Errorf("Handlers = %d", verdict.Handlers)
	}
}

// TestHTTPResponseNon2xx covers §5.6's fourth row: a non-2xx status is a
// non-blocking error, and HTTP can never block on a status code alone.
func TestHTTPResponseNon2xx(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			srv, _ := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ *httpRecorder) {
				w.WriteHeader(status)
				// A deny decision in the body must NOT be honoured: only a 2xx
				// body carries a decision (§5.6).
				_, _ = w.Write([]byte(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"不该生效"}}`))
			})
			cfg := groupsFor("PreToolUse", "Bash", httpHandler(srv.URL, 5))

			verdict := dispatchInDir(t, cfg, Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second}, preToolUse("Bash"))
			if verdict.Block {
				t.Errorf("HTTP %d 不应阻塞（§5.6：HTTP 无法只靠状态码阻塞）", status)
			}
			if !contains(verdict.Failures, fmt.Sprintf("HTTP %d", status)) {
				t.Errorf("失败记录应带上状态码：%v", verdict.Failures)
			}
			if verdict.Continue != true {
				t.Error("Continue 应为 true：非 2xx 是非阻塞错误，执行继续")
			}
		})
	}
}

// TestHTTPConnectionFailure covers §5.6's fifth row: a connection failure is a
// non-blocking error. The port is closed by closing the server first, so no network
// access is needed.
func TestHTTPConnectionFailure(t *testing.T) {
	srv, _ := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ *httpRecorder) {
		w.WriteHeader(http.StatusOK)
	})
	url := srv.URL
	srv.Close() // the port is now refused

	cfg := groupsFor("PreToolUse", "Bash", httpHandler(url, 5))
	verdict := dispatchInDir(t, cfg, Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second}, preToolUse("Bash"))

	if verdict.Block {
		t.Error("连接失败不应阻塞（§5.6）")
	}
	if len(verdict.Failures) == 0 {
		t.Error("连接失败应被记录")
	}
	if verdict.Handlers != 1 {
		t.Errorf("连接失败的 handler 仍然算运行过（有记录可查）：Handlers = %d", verdict.Handlers)
	}
}

// TestHTTPTimeout covers §5.7: a slow http handler is cancelled. The server sleeps
// longer than the handler's timeout, which is set well below MaxTimeout so the
// cancellation is the handler's own.
func TestHTTPTimeout(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	srv, _ := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ *httpRecorder) {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	})
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	handler := httpHandler(srv.URL, 1) // 1 second, below MaxTimeout below
	cfg := groupsFor("PreToolUse", "Bash", handler)

	opts := Options{Dir: t.TempDir(), MaxTimeout: 5 * time.Second}
	start := time.Now()
	verdict := dispatchInDir(t, cfg, opts, preToolUse("Bash"))
	elapsed := time.Since(start)

	if elapsed > 4*time.Second {
		t.Errorf("超时未生效，耗时 %v", elapsed)
	}
	if verdict.Block {
		t.Error("PreToolUse 的 http hook 超时不阻塞（§5.7）")
	}
	if !contains(verdict.Failures, "超时") {
		t.Errorf("超时应被记录：%v", verdict.Failures)
	}
}

// TestHTTPMissingURLCoveredBySkip covers the PreToolUse preflight for an http
// handler with no url: it is skipped, with the reason naming the §3 requirement.
func TestHTTPMissingURLIsSkipped(t *testing.T) {
	cfg := groupsFor("PreToolUse", "Bash", Handler{Type: HandlerHTTP})
	verdict := dispatchInDir(t, cfg, Options{Dir: t.TempDir()}, preToolUse("Bash"))

	if verdict.Handlers != 0 {
		t.Errorf("Handlers = %d", verdict.Handlers)
	}
	if !contains(verdict.Skipped, "缺少 url") {
		t.Errorf("Skipped = %v", verdict.Skipped)
	}
}

// TestMCPToolHandler covers §3's mcp_tool transport end to end: the input map is
// substituted from the payload, the configured server and tool are used, and the
// returned text is parsed with the same rules as a command hook's stdout.
func TestMCPToolHandler(t *testing.T) {
	caller := &fakeMCP{response: `{"hookSpecificOutput":{"hookEventName":"PostToolUse","updatedToolOutput":"扫描结果"}}`}
	handler := Handler{
		Type:   HandlerMCPTool,
		Server: "my_server",
		Tool:   "security_scan",
		Input:  map[string]string{"file_path": "${tool_input.file_path}", "mode": "ro"},
	}
	cfg := groupsFor("PostToolUse", "Write|Edit", handler)

	in := Input{
		SessionID:     "s",
		HookEventName: "PostToolUse",
		ToolName:      "Write",
		ToolInput:     json.RawMessage(`{"file_path":"/tmp/a.txt","content":"x"}`),
		ToolUseID:     "call_1",
		ToolResponse:  json.RawMessage(`{"type":"text"}`),
	}
	opts := Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second, MCP: caller}
	verdict := dispatchInDir(t, cfg, opts, in)

	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d（failures=%v）", verdict.Handlers, verdict.Failures)
	}
	if len(caller.calls) != 1 {
		t.Fatalf("MCP 被调用 %d 次", len(caller.calls))
	}
	call := caller.calls[0]
	if call.server != "my_server" || call.tool != "security_scan" {
		t.Errorf("server/tool = %q/%q", call.server, call.tool)
	}
	// §3: string values support ${path} substitution from the hook input, so the
	// tool receives the resolved path rather than the placeholder.
	if call.input["file_path"] != "/tmp/a.txt" {
		t.Errorf("input.file_path = %v，期望 /tmp/a.txt（§3 替换）", call.input["file_path"])
	}
	if call.input["mode"] != "ro" {
		t.Errorf("input.mode = %v，字面量应原样传递", call.input["mode"])
	}
	// The tool's text output is parsed with the command-hook stdout rules.
	if !verdict.HasUpdatedOutput || verdict.UpdatedToolOutput != "扫描结果" {
		t.Errorf("updatedToolOutput 未生效：%+v", verdict)
	}
}

// TestMCPToolSubstitutionMissingField covers the substitution rule for a path the
// payload does not carry: it becomes the empty string rather than a literal
// placeholder, which is what stops a tool being handed "${tool_input.file_path}".
func TestMCPToolSubstitutionMissingField(t *testing.T) {
	caller := &fakeMCP{response: `{"systemMessage":"已扫描"}`}
	handler := Handler{
		Type:   HandlerMCPTool,
		Server: "my_server",
		Tool:   "scan",
		Input:  map[string]string{"file_path": "${tool_input.file_path}", "other": "${nope}"},
	}
	cfg := groupsFor("PostToolUse", "Bash", handler)

	// A Bash call has no file_path, which is exactly the case.
	opts := Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second, MCP: caller}
	dispatchInDir(t, cfg, opts, Input{
		SessionID:     "s",
		HookEventName: "PostToolUse",
		ToolName:      "Bash",
		ToolInput:     json.RawMessage(`{"command":"ls"}`),
	})

	if len(caller.calls) != 1 {
		t.Fatalf("MCP 被调用 %d 次", len(caller.calls))
	}
	if got := caller.calls[0].input["file_path"]; got != "" {
		t.Errorf("file_path = %v，缺失字段应替换为空串", got)
	}
	if got := caller.calls[0].input["other"]; got != "" {
		t.Errorf("other = %v", got)
	}
}

// TestMCPToolPlainTextOutput covers the documented rule that a tool's text output
// is read the same way as command-hook stdout: on SessionStart that means plain
// text reaches the model, but SessionStart skips mcp_tool hooks, so the case uses
// UserPromptSubmit.
func TestMCPToolPlainTextOutput(t *testing.T) {
	caller := &fakeMCP{response: "当前部署目标是生产环境"}
	handler := Handler{Type: HandlerMCPTool, Server: "s", Tool: "t"}
	cfg := groupsFor("UserPromptSubmit", "*", handler)

	opts := Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second, MCP: caller}
	verdict := dispatchInDir(t, cfg, opts, Input{
		SessionID:     "s",
		HookEventName: "UserPromptSubmit",
		Prompt:        "部署吧",
	})

	if len(verdict.AdditionalContext) != 1 || verdict.AdditionalContext[0] != "当前部署目标是生产环境" {
		t.Errorf("AdditionalContext = %v（failures=%v）", verdict.AdditionalContext, verdict.Failures)
	}
}

// TestMCPToolError covers a failing tool call: a non-blocking error, per §3's "if
// the named server is not connected, or the tool returns isError: true, the hook
// produces a non-blocking error and execution continues".
func TestMCPToolError(t *testing.T) {
	caller := &fakeMCP{err: errors.New("server not connected")}
	handler := Handler{Type: HandlerMCPTool, Server: "my_server", Tool: "scan"}
	cfg := groupsFor("PostToolUse", "Bash", handler)

	opts := Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second, MCP: caller}
	verdict := dispatchInDir(t, cfg, opts, Input{
		SessionID:     "s",
		HookEventName: "PostToolUse",
		ToolName:      "Bash",
	})

	if verdict.Block {
		t.Error("MCP 调用失败不应阻塞")
	}
	if !contains(verdict.Failures, "server not connected") {
		t.Errorf("错误应被记录：%v", verdict.Failures)
	}
	if verdict.Handlers != 1 {
		t.Errorf("失败的 handler 仍然算运行过：Handlers = %d", verdict.Handlers)
	}
}

// TestMCPToolReceivedContext covers the caller being handed the dispatch context, so
// a cancelled turn cancels the outbound call too.
func TestMCPToolReceivedContext(t *testing.T) {
	caller := &ctxMCP{}
	handler := Handler{Type: HandlerMCPTool, Server: "s", Tool: "t"}
	cfg := groupsFor("PostToolUse", "Bash", handler)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts := Options{Dir: t.TempDir(), MaxTimeout: 10 * time.Second, MCP: caller}
	verdict := NewEngine(cfg, opts).Dispatch(ctx, Input{
		SessionID:     "s",
		HookEventName: "PostToolUse",
		ToolName:      "Bash",
	})

	if !verdict.Ran {
		t.Error("handler 应运行（失败也是运行）")
	}
	if caller.err == nil {
		t.Error("已取消的 context 应传给 MCP caller")
	}
}

// ctxMCP reports whether the context it was given was already cancelled.
type ctxMCP struct {
	err error
}

func (c *ctxMCP) Call(ctx context.Context, _, _ string, _ map[string]any) (string, error) {
	if err := ctx.Err(); err != nil {
		c.err = err
		return "", err
	}
	return "", nil
}

// TestHTTPAndCommandInParallel covers §1's parallelism across handler types. The
// HTTP reply is held until the command handler has finished, so the two can only
// both complete if they overlapped: a serial implementation would time out waiting
// for the reply before ever starting the command.
func TestHTTPAndCommandInParallel(t *testing.T) {
	helper := newHelper(t, "sleep", "CLAUDEHOOK_HELPER_SLEEP=1")
	commandDone := make(chan struct{})

	srv, _ := newHookServer(t, func(w http.ResponseWriter, _ *http.Request, _ *httpRecorder) {
		// Wait for the command handler's own side effect, with a bound so a serial
		// implementation fails visibly instead of hanging the suite.
		select {
		case <-commandDone:
		case <-time.After(3 * time.Second):
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// A watcher releases the HTTP response as soon as the command handler's record
	// file appears.
	go func() {
		for {
			if helper.read() != "" {
				close(commandDone)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	cfg := groupsFor("PreToolUse", "Bash",
		httpHandler(srv.URL, 8),
		handlerWithExitTimeout(helper, 8),
	)

	start := time.Now()
	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	elapsed := time.Since(start)

	if verdict.Handlers != 2 {
		t.Fatalf("Handlers = %d（skipped=%v failures=%v）", verdict.Handlers, verdict.Skipped, verdict.Failures)
	}
	var httpStatus int
	for _, rec := range NewEngine(Config{}, Options{}).Recent(1) {
		_ = rec
	}
	_ = httpStatus
	// The HTTP handler's request must have completed successfully, which required
	// the command handler to have run while it was outstanding.
	if contains(verdict.Failures, "HTTP 5") {
		t.Errorf("HTTP handler 未在 command handler 运行时收到响应：%v", verdict.Failures)
	}
	if elapsed > 4*time.Second {
		t.Errorf("两个 handler 耗时 %v，看起来是串行执行", elapsed)
	}
}
