package claudehook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file holds the subprocess scaffolding the tests share. Real hook handlers
// are separate processes by definition, so the tests drive a thin re-exec of the
// test binary itself: TestMain sees the helper markers and acts as the handler
// instead of running the test suite.
//
// Re-execing os.Args[0] (rather than writing one shell script per case) is what
// lets a test cover both command forms honestly: exec form needs a real
// executable resolved via PATH, and the test binary is one. The shell-form cases
// still go through /bin/sh, because that is the form under test.

// Helper-mode environment markers.
const (
	// envHelperMode turns the test binary into a hook handler.
	envHelperMode = "CLAUDEHOOK_HELPER_MODE"
	// envHelperFile is where helper mode writes its record of what it saw.
	envHelperFile = "CLAUDEHOOK_HELPER_FILE"
	// envHelperDelay makes helper mode sleep before answering.
	envHelperDelay = "CLAUDEHOOK_HELPER_DELAY"
	// envHelperStdout is the text the "echo_stdout" mode prints verbatim.
	envHelperStdout = "CLAUDEHOOK_HELPER_STDOUT"
	// envHelperSleep is how many whole seconds the "sleep" mode sleeps for.
	envHelperSleep = "CLAUDEHOOK_HELPER_SLEEP"
)

func TestMain(m *testing.M) {
	if err := helperMain(); err != nil {
		fmt.Fprintf(os.Stderr, "claudehook test helper: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// helperMain implements the handler modes.
//
// The mode switch is the FIRST thing it does, before any output, for a reason that
// cost a debugging session: the child is the Go test binary, and `go test` prints
// its own "PASS" and "ok <pkg>" lines to stdout. A mode that printed its JSON and
// then returned to m.Run() would hand the engine a stdout of
// `{"..."}PASS` followed by `ok <pkg>`, which §5.1 reads as plain text (it starts
// with "{" but does not end with "}") and reports as a hook with no decision.
// Exiting inside the mode means the helper owns its stdout exactly.
func helperMain() error {
	mode := os.Getenv(envHelperMode)
	if mode == "" {
		return nil
	}

	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("读取 stdin 失败：%w", err)
	}
	// Every mode records what it received, which is how the stdin-payload and
	// argv tests assert on what the engine actually handed the handler.
	if file := os.Getenv(envHelperFile); file != "" {
		// One %q-quoted argv element per line: quoting makes an argument that
		// contains spaces or shell metacharacters unambiguous to read back, which
		// is precisely the property the exec-form tests assert on.
		var buf strings.Builder
		for i, arg := range os.Args[1:] {
			fmt.Fprintf(&buf, "argv%d=%q\n", i, arg)
		}
		buf.WriteString("stdin=" + string(payload) + "\n")
		if err := os.WriteFile(file, []byte(buf.String()), 0o600); err != nil {
			return fmt.Errorf("写入记录文件失败：%w", err)
		}
	}

	if delay := os.Getenv(envHelperDelay); delay != "" {
		if d, err := time.ParseDuration(delay); err == nil && d > 0 {
			time.Sleep(d)
		}
	}

	exitHookModes(mode)
	return fmt.Errorf("未知的 helper 模式 %q", mode)
}

// exitHookModes runs one handler mode. It never returns: each branch ends in
// os.Exit so nothing else can write to the child's stdout.
func exitHookModes(mode string) {
	switch mode {
	case "env":
		// Reports two environment variables the tests control: one from the
		// process (PATH) and one from Options.Env. The pair is what proves the
		// child got both the inherited environment and the caller's additions.
		fmt.Printf(`{"systemMessage":"MARKER=%s PATH_SET=%v"}`,
			os.Getenv("MARKER"), os.Getenv("PATH") != "")
	case "echo_system_message":
		fmt.Print(`{"systemMessage":"来自 helper 的提示"}`)
	case "echo_stdout":
		// Prints the exact bytes the case asked for, with no newline appended: the
		// §5.1 parsing rule turns on the first and last byte of stdout, so adding
		// one would change what is being tested.
		fmt.Print(os.Getenv(envHelperStdout))
	case "echo_stderr_only":
		fmt.Fprint(os.Stderr, "写入 stderr 的诊断信息")
	case "echo_both":
		fmt.Print(os.Getenv(envHelperStdout))
		fmt.Fprint(os.Stderr, "stderr 内容")
	case "exit":
		if msg := os.Getenv("CLAUDEHOOK_HELPER_STDERR"); msg != "" {
			fmt.Fprint(os.Stderr, msg)
		}
		code, _ := strconv.Atoi(os.Getenv("CLAUDEHOOK_HELPER_EXIT"))
		os.Exit(code)
	case "reason":
		// Emits a PreToolUse deny whose reason is the first real argument. The
		// reason travels through argv rather than the environment because an
		// Engine has one Env but many handlers, and a case that needs two
		// different blocking reasons in one dispatch can only vary them per
		// handler.
		fmt.Printf(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":%s}}`,
			quoteJSON(reasonArg()))
	case "sleep":
		// Sleeps for the number of seconds given as the first real argument, falling
		// back to envHelperSleep. The duration comes through argv because the
		// parallelism test needs two handlers in ONE dispatch to sleep different
		// amounts, and an Engine has a single Env — argv is also what a real
		// exec-form config uses for per-handler parameters.
		seconds, _ := strconv.Atoi(reasonArg())
		if seconds == 0 {
			seconds, _ = strconv.Atoi(os.Getenv(envHelperSleep))
		}
		time.Sleep(time.Duration(seconds) * time.Second)
		fmt.Print(`{"systemMessage":"睡了很久"}`)
	default:
		return
	}
	os.Exit(0)
}

// reasonArg returns the first argument that is not a go test flag. It is where the
// "reason" mode takes its block reason from and the "sleep" mode its duration — the
// shared accessor is deliberate, since both are "the parameter for this handler"
// and each mode reads only its own.
func reasonArg() string {
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "-test.") {
			return arg
		}
	}
	return ""
}

// selfPath is the absolute path of the test binary, resolved once. Exec form
// resolves its command through PATH, so a bare name would be ambiguous; passing
// the absolute path is what a real config does.
var selfPath = os.Args[0]

// stdoutEnv builds the env entry that makes the "echo_stdout" helper print `text`.
//
// No quoting: the value is placed directly into the child's environment by
// exec.Cmd, so it is never word-split, and adding quotes would make the helper
// print them (an exec-form handler sees no shell to strip them).
func stdoutEnv(text string) string {
	return envHelperStdout + "=" + text
}

// helperMode describes one synthetic handler: which helper behaviour to run and
// the environment that selects it. Bundle the two so a test cannot wire a
// handler to one mode and an engine to another.
type helperMode struct {
	mode   string
	env    []string
	record string
}

// newHelper creates a helper mode plus the temp dir its record file lives in.
// extraEnv entries are appended to the engine's environment, which is how the
// helper script's behaviour is parameterised (stdout text, exit code, sleep).
func newHelper(t *testing.T, mode string, extraEnv ...string) helperMode {
	t.Helper()
	record := filepath.Join(t.TempDir(), "helper-record.txt")
	return helperMode{
		mode:   mode,
		record: record,
		env:    append([]string{envHelperMode + "=" + mode, envHelperFile + "=" + record}, extraEnv...),
	}
}

// handler builds the exec-form command handler that runs this mode.
func (h helperMode) handler() Handler {
	return Handler{
		Type:    HandlerCommand,
		Command: selfPath,
		// -test.run=TestMain keeps the child from running the whole test suite:
		// TestMain is the only entry point the helper needs, and every other test
		// is skipped before it can start.
		Args: []string{"-test.run=TestMain"},
	}
}

// options wires an engine to this mode, with an isolated working directory.
func (h helperMode) options(t *testing.T) Options {
	t.Helper()
	return Options{Dir: t.TempDir(), Env: h.env, MaxTimeout: 10 * time.Second}
}

// read returns what the helper recorded about its argv and stdin.
func (h helperMode) read() string {
	data, err := os.ReadFile(h.record)
	if err != nil {
		return ""
	}
	return string(data)
}

// configFromJSON parses a hooks object the way a caller of ParseConfig does, and
// fails the test on a parse error it did not expect.
func configFromJSON(t *testing.T, raw string) Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfig(%s) 返回错误：%v", raw, err)
	}
	return cfg
}

// groupsFor is a one-event config with one group/frame, for the many tests that
// only need "this event, these handlers".
func groupsFor(event, matcher string, handlers ...Handler) Config {
	raw := fmt.Sprintf(`{"hooks":{%q:[{"matcher":%q,"hooks":[`, event, matcher)
	for i, h := range handlers {
		if i > 0 {
			raw += ","
		}
		raw += mustJSON(h)
	}
	raw += `]}]}}`
	cfg, err := ParseConfig([]byte(raw))
	if err != nil {
		panic(fmt.Sprintf("测试配置解析失败：%v（%s）", err, raw))
	}
	return cfg
}

// mustJSON renders a Handler as JSON for a synthetic config. It exists so the
// tests drive the same ParseConfig path the product does instead of constructing
// structs directly — a difference that would hide exactly the parsing bugs the
// config tests are for.
func mustJSON(h Handler) string {
	var buf strings.Builder
	buf.WriteString(`{"type":"` + h.Type + `"`)
	if h.Command != "" {
		buf.WriteString(`,"command":` + quoteJSON(h.Command))
	}
	if h.Args != nil {
		parts := make([]string, 0, len(h.Args))
		for _, a := range h.Args {
			parts = append(parts, quoteJSON(a))
		}
		buf.WriteString(`,"args":[` + strings.Join(parts, ",") + `]`)
	}
	if h.Timeout != 0 {
		fmt.Fprintf(&buf, `,"timeout":%d`, h.Timeout)
	}
	if h.Async {
		buf.WriteString(`,"async":true`)
	}
	if h.AsyncRewake {
		buf.WriteString(`,"asyncRewake":true`)
	}
	if h.Shell != "" {
		buf.WriteString(`,"shell":` + quoteJSON(h.Shell))
	}
	if h.URL != "" {
		buf.WriteString(`,"url":` + quoteJSON(h.URL))
	}
	if len(h.Headers) > 0 {
		buf.WriteString(`,"headers":{`)
		first := true
		for k, v := range h.Headers {
			if !first {
				buf.WriteString(",")
			}
			first = false
			buf.WriteString(quoteJSON(k) + ":" + quoteJSON(v))
		}
		buf.WriteString(`}`)
	}
	if len(h.AllowedEnvVars) > 0 {
		parts := make([]string, 0, len(h.AllowedEnvVars))
		for _, v := range h.AllowedEnvVars {
			parts = append(parts, quoteJSON(v))
		}
		buf.WriteString(`,"allowedEnvVars":[` + strings.Join(parts, ",") + `]`)
	}
	if h.Server != "" {
		buf.WriteString(`,"server":` + quoteJSON(h.Server))
	}
	if h.Tool != "" {
		buf.WriteString(`,"tool":` + quoteJSON(h.Tool))
	}
	if len(h.Input) > 0 {
		buf.WriteString(`,"input":{`)
		first := true
		for k, v := range h.Input {
			if !first {
				buf.WriteString(",")
			}
			first = false
			buf.WriteString(quoteJSON(k) + ":" + quoteJSON(v))
		}
		buf.WriteString(`}`)
	}
	if h.Prompt != "" {
		buf.WriteString(`,"prompt":` + quoteJSON(h.Prompt))
	}
	if h.Model != "" {
		buf.WriteString(`,"model":` + quoteJSON(h.Model))
	}
	if h.If != "" {
		buf.WriteString(`,"if":` + quoteJSON(h.If))
	}
	if h.StatusMessage != "" {
		buf.WriteString(`,"statusMessage":` + quoteJSON(h.StatusMessage))
	}
	if h.Once {
		buf.WriteString(`,"once":true`)
	}
	buf.WriteString(`}`)
	return buf.String()
}

// quoteJSON quotes a string using encoding/json, so a test fixture with Chinese
// text or backslashes stays valid JSON.
func quoteJSON(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// fakeMCP records the calls an mcp_tool handler made and answers with canned
// output, so the mcp_tool path is exercised without a server.
type fakeMCP struct {
	calls    []mcpCall
	response string
	err      error
}

type mcpCall struct {
	server string
	tool   string
	input  map[string]any
}

func (f *fakeMCP) Call(_ context.Context, server, tool string, input map[string]any) (string, error) {
	// The map is copied: the engine builds a fresh one per call, but a test that
	// asserted on a shared map would pass or fail depending on whether that stays
	// true, which is not what the test is about.
	clone := make(map[string]any, len(input))
	for k, v := range input {
		clone[k] = v
	}
	f.calls = append(f.calls, mcpCall{server: server, tool: tool, input: clone})
	return f.response, f.err
}

// dispatchInDir runs a config in an isolated working directory and returns the
// verdict. The directory matters for command handlers: a test must never run a
// hook in the repository it is testing.
func dispatchInDir(t *testing.T, cfg Config, opts Options, in Input) Verdict {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	return NewEngine(cfg, opts).Dispatch(context.Background(), in)
}

// contains reports whether any string in haystack contains want.
func contains(haystack []string, want string) bool {
	for _, s := range haystack {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
