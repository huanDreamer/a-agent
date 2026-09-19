package claudehook

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// preToolUse is the payload shape §10 measured for PreToolUse.
func preToolUse(tool string) Input {
	return Input{
		SessionID:      "61361905-1e0b-4538-b736-601108686a40",
		TranscriptPath: "/private/tmp/capture/session.jsonl",
		Cwd:            "/private/tmp/capture",
		PermissionMode: "default",
		HookEventName:  "PreToolUse",
		ToolName:       tool,
		ToolInput:      json.RawMessage(`{"command":"echo hello","description":"Print hello"}`),
		ToolUseID:      "call_00_jmaTqtGWpKAQGUABqzlm2629",
		Effort:         &Effort{Level: "max"},
	}
}

// TestCommandExecFormArgv covers §3's exec form: command is resolved via PATH, one
// argv element per array entry, and no shell tokenisation happens.
func TestCommandExecFormArgv(t *testing.T) {
	helper := newHelper(t, "echo_system_message")
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d（skipped=%v failures=%v）", verdict.Handlers, verdict.Skipped, verdict.Failures)
	}
	if len(verdict.SystemMessages) != 1 || verdict.SystemMessages[0] != "来自 helper 的提示" {
		t.Errorf("SystemMessages = %v", verdict.SystemMessages)
	}

	argv := argvFromRecord(t, helper.read())
	// The helper saw exactly the one argument the config asked for. The point of
	// the case is that exec form passes each args element through untouched: no
	// shell, no word splitting.
	if len(argv) != 1 || argv[0] != "-test.run=TestMain" {
		t.Errorf("exec form argv = %#v，期望恰好 [\"-test.run=TestMain\"]", argv)
	}
}

// TestCommandExecFormDoesNotSplitOnSpaces is the specific difference from shell
// form: an args element with spaces and shell metacharacters is passed verbatim.
func TestCommandExecFormDoesNotSplitOnSpaces(t *testing.T) {
	helper := newHelper(t, "echo_system_message")
	handler := helper.handler()
	// A single argument that a shell would split into four words and expand.
	handler.Args = append(handler.Args, "one two; echo injected > /tmp/must-not-exist")
	cfg := groupsFor("PreToolUse", "Bash", handler)

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d（failures=%v）", verdict.Handlers, verdict.Failures)
	}
	argv := argvFromRecord(t, helper.read())
	if len(argv) != 2 {
		t.Fatalf("argv = %#v，exec form 不应做任何切分", argv)
	}
	if argv[1] != "one two; echo injected > /tmp/must-not-exist" {
		t.Errorf("argv[1] = %q，参数被改写", argv[1])
	}
	if _, err := os.Stat("/tmp/must-not-exist"); err == nil {
		t.Error("参数中的 shell 重定向被执行了，说明 exec form 走了 shell")
	}
}

// TestCommandShellForm covers §3's shell form: without args the whole string goes
// to sh -c, so pipes and && work.
func TestCommandShellForm(t *testing.T) {
	helper := newHelper(t, "echo_system_message")
	// Shell form: no args, so the field is the entire command string. The helper
	// still writes its record file, which is how the test knows it ran.
	handler := Handler{
		Type:    HandlerCommand,
		Command: selfPath + ` -test.run=TestMain`,
	}
	if handler.Args != nil {
		t.Fatal("shell form 的 Args 必须为 nil")
	}
	cfg := groupsFor("PreToolUse", "Bash", handler)

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d（failures=%v）", verdict.Handlers, verdict.Failures)
	}
	if len(verdict.SystemMessages) != 1 {
		t.Errorf("SystemMessages = %v", verdict.SystemMessages)
	}
	if helper.read() == "" {
		t.Error("shell form 没有运行 helper")
	}
}

// TestCommandShellFormPipesAndOperators covers the shell features §3 says shell
// form keeps available.
func TestCommandShellFormPipesAndOperators(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	handler := Handler{
		Type: HandlerCommand,
		// Two commands joined by &&, with a pipe, redirection and a variable
		// expansion: none of this works in exec form.
		Command: `X=hello && printf '%s' "$X" | tr a-z A-Z > ` + marker + ` && printf '{"systemMessage":"ok"}'`,
	}
	cfg := groupsFor("PreToolUse", "Bash", handler)
	opts := Options{Dir: dir, MaxTimeout: 10 * time.Second}

	verdict := dispatchInDir(t, cfg, opts, preToolUse("Bash"))
	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d（failures=%v）", verdict.Handlers, verdict.Failures)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("重定向没有产生文件：%v", err)
	}
	if string(data) != "HELLO" {
		t.Errorf("管道/变量展开结果 = %q，期望 HELLO", data)
	}
}

// TestCommandArgsEmptySliceIsExecForm covers §9's async example, which writes
// "args": [] precisely to get exec form with no arguments. Treating an empty array
// as "no args" would hand a script path to sh and run the wrong thing.
func TestCommandArgsEmptySliceIsExecForm(t *testing.T) {
	helper := newHelper(t, "echo_system_message")
	handler := helper.handler()
	handler.Args = []string{}
	cfg := groupsFor("PreToolUse", "Bash", handler)

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	// Without the -test.run flag the child runs the whole test suite, which will
	// not produce the helper's systemMessage; the point of the case is simply that
	// the command was spawned directly (its stdin/record still exist) rather than
	// passed to sh.
	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d（failures=%v）", verdict.Handlers, verdict.Failures)
	}
	if helper.read() == "" {
		t.Error("args: [] 应走 exec form 直接 spawn 可执行文件")
	}
}

// TestCommandPlaceholderExpansion covers §3's placeholders: ${CLAUDE_PROJECT_DIR}
// and ~ expand as plain string substitution in every element of the argv.
func TestCommandPlaceholderExpansion(t *testing.T) {
	helper := newHelper(t, "echo_system_message")
	handler := helper.handler()
	// The record file path is an env-provided absolute path, so substituting it
	// into an arg proves the substitution happened in args as well as command.
	handler.Args = append(handler.Args, "${CLAUDE_PROJECT_DIR}/expanded-arg")
	cfg := groupsFor("PreToolUse", "Bash", handler)

	opts := helper.options(t)
	opts.Env = append(opts.Env, "CLAUDE_PROJECT_DIR="+opts.Dir)
	verdict := dispatchInDir(t, cfg, opts, preToolUse("Bash"))
	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d（failures=%v）", verdict.Handlers, verdict.Failures)
	}
	argv := argvFromRecord(t, helper.read())
	if len(argv) != 2 {
		t.Fatalf("argv = %#v", argv)
	}
	if want := opts.Dir + "/expanded-arg"; argv[1] != want {
		t.Errorf("argv[1] = %q，期望 %q（占位符未替换）", argv[1], want)
	}
}

// TestCommandPlaceholderExpansionInCommand covers the substitution in the command
// field itself, and the documented fallback when CLAUDE_PROJECT_DIR is not in the
// environment.
func TestCommandPlaceholderExpansionInCommand(t *testing.T) {
	dir := t.TempDir()
	// A tiny shell script in the working directory, invoked through the
	// placeholder. Writing it from the test keeps the case independent of the
	// user's real hook scripts.
	script := filepath.Join(dir, "hook.sh")
	body := "#!/bin/sh\nprintf '{\"systemMessage\":\"占位符脚本已运行\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("写入脚本失败：%v", err)
	}

	for name, opts := range map[string]Options{
		"环境变量给出项目目录":  {Dir: dir, Env: []string{"CLAUDE_PROJECT_DIR=" + dir}, MaxTimeout: 10 * time.Second},
		"未给出时回退到工作目录": {Dir: dir, MaxTimeout: 10 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := groupsFor("PreToolUse", "Bash", Handler{
				Type:    HandlerCommand,
				Command: "${CLAUDE_PROJECT_DIR}/hook.sh",
			})
			verdict := dispatchInDir(t, cfg, opts, preToolUse("Bash"))
			if verdict.Handlers != 1 {
				t.Fatalf("Handlers = %d（failures=%v）", verdict.Handlers, verdict.Failures)
			}
			if len(verdict.SystemMessages) != 1 || verdict.SystemMessages[0] != "占位符脚本已运行" {
				t.Errorf("SystemMessages = %v", verdict.SystemMessages)
			}
		})
	}
}

// TestExitCodeSemantics walks §5.2's table for the three exit codes that matter.
func TestExitCodeSemantics(t *testing.T) {
	cases := []struct {
		name        string
		exit        string
		event       string
		blockable   bool
		wantBlock   bool
		wantFail    int
		failMustSay string
	}{
		{
			name: "exit 0 无输出", exit: "0", event: "PreToolUse",
			wantBlock: false, wantFail: 0,
		},
		{
			name: "exit 1 空输出不阻塞", exit: "1", event: "PreToolUse",
			wantBlock: false, wantFail: 1, failMustSay: "exit 1",
		},
		{
			name: "exit 2 在可阻塞事件上阻塞", exit: "2", event: "PreToolUse",
			wantBlock: true, wantFail: 0,
		},
		{
			name: "exit 2 在 UserPromptSubmit 上阻塞", exit: "2", event: "UserPromptSubmit",
			wantBlock: true, wantFail: 0,
		},
		{
			name: "exit 2 在 Stop 上阻塞", exit: "2", event: "Stop",
			wantBlock: true, wantFail: 0,
		},
		{
			name: "exit 2 在 PostToolUse 上不阻塞", exit: "2", event: "PostToolUse",
			wantBlock: false, wantFail: 1, failMustSay: "不能阻塞",
		},
		{
			name: "exit 2 在 SessionStart 上不阻塞", exit: "2", event: "SessionStart",
			wantBlock: false, wantFail: 1, failMustSay: "不能阻塞",
		},
		{
			name: "exit 127 是脚本不存在的非阻塞错误", exit: "127", event: "PreToolUse",
			wantBlock: false, wantFail: 1, failMustSay: "exit 127",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			helper := newHelper(t, "exit",
				"CLAUDEHOOK_HELPER_EXIT="+tc.exit,
				// stderr is the documented fallback block reason for exit 2 (§5.2),
				// so the case supplies one.
				"CLAUDEHOOK_HELPER_STDERR=blocked by policy")
			cfg := groupsFor(tc.event, "*", helper.handler())

			verdict := dispatchInDir(t, cfg, helper.options(t), Input{
				SessionID:     "s",
				HookEventName: tc.event,
				ToolName:      "Bash",
				Prompt:        "hi",
			})
			if verdict.Handlers != 1 {
				t.Fatalf("Handlers = %d（skipped=%v）", verdict.Handlers, verdict.Skipped)
			}
			if verdict.Block != tc.wantBlock {
				t.Errorf("Block = %v，期望 %v（block_reason=%q failures=%v）",
					verdict.Block, tc.wantBlock, verdict.BlockReason, verdict.Failures)
			}
			if len(verdict.Failures) != tc.wantFail {
				t.Errorf("Failures 有 %d 条，期望 %d：%v", len(verdict.Failures), tc.wantFail, verdict.Failures)
			}
			if tc.failMustSay != "" && !contains(verdict.Failures, tc.failMustSay) {
				t.Errorf("Failures 中应提到 %q：%v", tc.failMustSay, verdict.Failures)
			}
			if verdict.Continue != true {
				t.Errorf("Continue = %v，exit 语义不应改变 continue", verdict.Continue)
			}
		})
	}
}

// TestExitCode2BlockReasonFromStderr covers §5.2's rule that the block reason comes
// from the JSON decision when there is one and from stderr otherwise. The helper
// exits 2 with no stdout and a message on stderr.
func TestExitCode2BlockReasonFromStderr(t *testing.T) {
	helper := newHelper(t, "echo_stderr_only")
	cfg := groupsFor("PreToolUse", "Bash", handlerWithExit(helper, 2))

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if !verdict.Block {
		t.Fatalf("Block = false（failures=%v）", verdict.Failures)
	}
	if verdict.BlockReason != "写入 stderr 的诊断信息" {
		t.Errorf("BlockReason = %q，期望 stderr 内容（§5.2）", verdict.BlockReason)
	}
	if verdict.PermissionDecision != "deny" {
		t.Errorf("PermissionDecision = %q，期望 deny（§8：exit 2 走 deny 路径）", verdict.PermissionDecision)
	}
}

// TestExitCode2BlockReasonFromJSON covers the JSON branch of the same rule.
func TestExitCode2BlockReasonFromJSON(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"JSON 给出的原因"}}`))
	cfg := groupsFor("PreToolUse", "Bash", handlerWithExit(helper, 2))

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if !verdict.Block {
		t.Fatalf("Block = false（failures=%v）", verdict.Failures)
	}
	if verdict.BlockReason != "JSON 给出的原因" {
		t.Errorf("BlockReason = %q，期望来自 JSON 决策（§5.2）", verdict.BlockReason)
	}
}

// TestExitCode2BeatsAllowingJSON covers §5.2's rule that exit 2 cannot be
// overridden by permissionDecision "allow" on stdout.
func TestExitCode2BeatsAllowingJSON(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"rewritten"}}}`))
	cfg := groupsFor("PreToolUse", "Bash", handlerWithExit(helper, 2))

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if !verdict.Block {
		t.Fatal("exit 2 应阻塞，即使 stdout 的 JSON 写了 permissionDecision: allow（§5.2）")
	}
	if verdict.PermissionDecision != "deny" {
		t.Errorf("PermissionDecision = %q，期望 deny", verdict.PermissionDecision)
	}
}

// TestExitCode2WithInvalidJSONStillBlocks covers §5.2: exit 2 with unparseable
// stdout still blocks, and the failed validation is only recorded.
func TestExitCode2WithInvalidJSONStillBlocks(t *testing.T) {
	helper := newHelper(t, "echo_both",
		stdoutEnv(`{"permissionDecision": `))
	cfg := groupsFor("PreToolUse", "Bash", handlerWithExit(helper, 2))

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if !verdict.Block {
		t.Fatal("exit 2 且 stdout 非法 JSON 时仍应阻塞（§5.2）")
	}
	// The stdout starts with "{" and does not end with "}", so §5.1 makes it plain
	// text; on PreToolUse text has no channel, and the exit-2 path keeps blocking.
	if verdict.BlockReason != "stderr 内容" {
		t.Errorf("BlockReason = %q，期望 stderr", verdict.BlockReason)
	}
}

// TestExitCode2EmptyStdoutAndStderr covers the corner §5.2 leaves implicit: exit 2
// with nothing at all on either stream still blocks, but there is no reason to
// give, and the operator must be told that rather than shown an empty reason.
func TestExitCode2EmptyStdoutAndStderr(t *testing.T) {
	helper := newHelper(t, "exit", "CLAUDEHOOK_HELPER_EXIT=2")
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if !verdict.Block {
		t.Fatal("exit 2 应阻塞")
	}
	if verdict.BlockReason == "" {
		t.Error("BlockReason 不应为空（应有兜底说明）")
	}
	if !contains(verdict.Failures, "没有可用的阻塞原因") {
		t.Errorf("缺原因的 exit 2 应被记录：%v", verdict.Failures)
	}
}

// TestNonZeroExitIgnoresValidJSON covers §5.2's "其他" row: a valid JSON object on
// stdout does not rescue a non-0/non-2 exit code.
func TestNonZeroExitIgnoresValidJSON(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"不该生效"}}`))
	cfg := groupsFor("PreToolUse", "Bash", handlerWithExit(helper, 1))

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if verdict.Block {
		t.Error("exit 1 不应阻塞，即使 stdout 有 deny 决策（§5.2）")
	}
	if verdict.PermissionDecision != "" {
		t.Errorf("PermissionDecision = %q，期望空", verdict.PermissionDecision)
	}
	if !contains(verdict.Failures, "exit 1") {
		t.Errorf("Failures 应说明 exit 1：%v", verdict.Failures)
	}
}

// TestCommandNotFoundIsNonBlocking covers §5.2's note about a wrong path: it fails
// open, and the failure is reported.
func TestCommandNotFoundIsNonBlocking(t *testing.T) {
	cfg := groupsFor("PreToolUse", "Bash", Handler{
		Type:    HandlerCommand,
		Command: filepath.Join(t.TempDir(), "does-not-exist.sh"),
	})
	verdict := dispatchInDir(t, cfg, Options{Dir: t.TempDir()}, preToolUse("Bash"))

	if verdict.Block {
		t.Error("命令不存在不应阻塞（§5.2：静默失效，但本运行时会上报）")
	}
	if len(verdict.Failures) == 0 {
		t.Error("命令不存在应记录失败")
	}
}

// TestJSONDecisionPreToolUsePermissionDecision covers §8's PreToolUse table.
func TestJSONDecisionPreToolUsePermissionDecision(t *testing.T) {
	cases := []struct {
		name       string
		stdout     string
		wantBlock  bool
		wantReason string
		wantDec    string
	}{
		{
			name:      "allow",
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","permissionDecisionReason":"自动放行"}}`,
			wantBlock: false, wantDec: "allow",
		},
		{
			name:      "deny",
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"数据库写入不允许"}}`,
			wantBlock: true, wantReason: "数据库写入不允许", wantDec: "deny",
		},
		{
			name:      "ask",
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask"}}`,
			wantBlock: false, wantDec: "ask",
		},
		{
			name:      "defer",
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"defer"}}`,
			wantBlock: false, wantDec: "defer",
		},
		{
			name:      "废弃值 approve 映射为 allow",
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"approve"}}`,
			wantBlock: false, wantDec: "allow",
		},
		{
			name:      "废弃值 block 映射为 deny",
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"block","permissionDecisionReason":"旧值"}}`,
			wantBlock: true, wantReason: "旧值", wantDec: "deny",
		},
		{
			name:      "无决策字段",
			stdout:    `{"hookSpecificOutput":{"hookEventName":"PreToolUse"}}`,
			wantBlock: false, wantDec: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			helper := newHelper(t, "echo_stdout", stdoutEnv(tc.stdout))
			cfg := groupsFor("PreToolUse", "Bash", helper.handler())

			verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
			if verdict.Block != tc.wantBlock {
				t.Errorf("Block = %v，期望 %v（failures=%v）", verdict.Block, tc.wantBlock, verdict.Failures)
			}
			if verdict.PermissionDecision != tc.wantDec {
				t.Errorf("PermissionDecision = %q，期望 %q", verdict.PermissionDecision, tc.wantDec)
			}
			if tc.wantReason != "" && verdict.BlockReason != tc.wantReason {
				t.Errorf("BlockReason = %q，期望 %q", verdict.BlockReason, tc.wantReason)
			}
		})
	}
}

// TestJSONDecisionPreToolUseUpdatedInput covers §8's updatedInput, which replaces
// the whole tool input object.
func TestJSONDecisionPreToolUseUpdatedInput(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"npm run lint","description":"重新写的命令"}}}`))
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if verdict.PermissionDecision != "allow" {
		t.Fatalf("PermissionDecision = %q", verdict.PermissionDecision)
	}
	var got map[string]any
	if err := json.Unmarshal(verdict.UpdatedInput, &got); err != nil {
		t.Fatalf("UpdatedInput 不是合法 JSON：%v（%s）", err, verdict.UpdatedInput)
	}
	if got["command"] != "npm run lint" {
		t.Errorf("UpdatedInput.command = %v", got["command"])
	}
}

// TestJSONDecisionPostToolUseUpdatedToolOutput covers §5.5's PostToolUse row:
// updatedToolOutput replaces the tool result.
func TestJSONDecisionPostToolUseUpdatedToolOutput(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"PostToolUse","updatedToolOutput":"替换后的工具输出"}}`))
	cfg := groupsFor("PostToolUse", "Bash", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), Input{
		SessionID:     "s",
		HookEventName: "PostToolUse",
		ToolName:      "Bash",
		ToolInput:     json.RawMessage(`{"command":"ls"}`),
		ToolResponse:  json.RawMessage(`{"type":"text"}`),
		ToolUseID:     "call_1",
	})
	if !verdict.HasUpdatedOutput {
		t.Fatalf("HasUpdatedOutput = false（failures=%v）", verdict.Failures)
	}
	if verdict.UpdatedToolOutput != "替换后的工具输出" {
		t.Errorf("UpdatedToolOutput = %q", verdict.UpdatedToolOutput)
	}
	// PostToolUse cannot block (§5.3/§5.5).
	if verdict.Block {
		t.Error("PostToolUse 不应阻塞")
	}
}

// TestJSONDecisionPostToolUseEmptyUpdatedOutput covers the reason
// HasUpdatedOutput exists: an empty replacement string is a legal value and must
// stay distinguishable from "no replacement".
func TestJSONDecisionPostToolUseEmptyUpdatedOutput(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"PostToolUse","updatedToolOutput":""}}`))
	cfg := groupsFor("PostToolUse", "Bash", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), Input{
		SessionID:     "s",
		HookEventName: "PostToolUse",
		ToolName:      "Bash",
	})
	if !verdict.HasUpdatedOutput {
		t.Error("updatedToolOutput 为空串时 HasUpdatedOutput 应为 true")
	}
	if verdict.UpdatedToolOutput != "" {
		t.Errorf("UpdatedToolOutput = %q，期望空串", verdict.UpdatedToolOutput)
	}
}

// TestJSONDecisionBlock covers the top-level decision:"block" shape §5.5 gives
// UserPromptSubmit, PostToolUse and Stop.
func TestJSONDecisionBlock(t *testing.T) {
	cases := []struct {
		event      string
		in         Input
		wantBlock  bool
		wantReason string
	}{
		{
			event: "UserPromptSubmit", in: Input{SessionID: "s", HookEventName: "UserPromptSubmit", Prompt: "hi"},
			wantBlock: true, wantReason: "这个 prompt 不允许",
		},
		{
			event: "Stop", in: Input{SessionID: "s", HookEventName: "Stop", LastAssistant: "done"},
			wantBlock: true, wantReason: "还有任务没做完",
		},
		{
			// §5.5 lists PostToolUse under the top-level decision mode too, and
			// §5.3 says it cannot block — so the decision is recorded but the tool
			// has already run.
			event: "PostToolUse", in: Input{SessionID: "s", HookEventName: "PostToolUse", ToolName: "Bash"},
			wantBlock: true, wantReason: "结果需要复核",
		},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			helper := newHelper(t, "echo_stdout",
				stdoutEnv(`{"decision":"block","reason":"`+tc.wantReason+`"}`))
			cfg := groupsFor(tc.event, "*", helper.handler())

			verdict := dispatchInDir(t, cfg, helper.options(t), tc.in)
			if verdict.Block != tc.wantBlock {
				t.Errorf("Block = %v，期望 %v（failures=%v）", verdict.Block, tc.wantBlock, verdict.Failures)
			}
			if verdict.BlockReason != tc.wantReason {
				t.Errorf("BlockReason = %q，期望 %q", verdict.BlockReason, tc.wantReason)
			}
		})
	}
}

// TestJSONDecisionInvalidBlockValue covers §5.5's note that "block" is the only
// legal value: anything else must not be treated as a block.
func TestJSONDecisionInvalidBlockValue(t *testing.T) {
	helper := newHelper(t, "echo_stdout", stdoutEnv(`{"decision":"reject","reason":"拼错的值"}`))
	cfg := groupsFor("Stop", "*", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), Input{SessionID: "s", HookEventName: "Stop"})
	if verdict.Block {
		t.Error("decision 取 " + `"reject"` + " 不应阻塞")
	}
	if !contains(verdict.Failures, "无效") {
		t.Errorf("无效 decision 取值应被记录：%v", verdict.Failures)
	}
}

// TestJSONDecisionContinueFalse covers §5.4: continue:false stops processing
// entirely and takes precedence over the event's own decision fields.
func TestJSONDecisionContinueFalse(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"continue":false,"stopReason":"用户账单已超限","hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`))
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if verdict.Continue {
		t.Error("Continue = true，期望 false（§5.4）")
	}
	if verdict.StopReason != "用户账单已超限" {
		t.Errorf("StopReason = %q", verdict.StopReason)
	}
}

// TestJSONDecisionAdditionalContext covers §5.4's additionalContext for all five
// dispatched events, including the events whose own decision mode is not
// hookSpecificOutput: §5.4 documents the field as arriving with the payload at the
// hook's position for every event.
func TestJSONDecisionAdditionalContext(t *testing.T) {
	for _, event := range SupportedEvents {
		t.Run(event, func(t *testing.T) {
			helper := newHelper(t, "echo_stdout",
				stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"`+event+`","additionalContext":"部署目标是生产环境"}}`))
			cfg := groupsFor(event, "*", helper.handler())

			verdict := dispatchInDir(t, cfg, helper.options(t), Input{
				SessionID:     "s",
				HookEventName: event,
				ToolName:      "Bash",
				Prompt:        "hi",
				Source:        "startup",
			})
			if len(verdict.AdditionalContext) != 1 || verdict.AdditionalContext[0] != "部署目标是生产环境" {
				t.Errorf("AdditionalContext = %v（failures=%v）", verdict.AdditionalContext, verdict.Failures)
			}
		})
	}
}

// TestPermissionDecisionReasonWithoutDecision covers the orphan reason: §8 reads
// permissionDecisionReason per decision, so a reason with no decision has nothing to
// route to. It is kept as the block reason rather than dropped, because it is the
// only explanation the handler gave, and it surfaces when something else blocks —
// here the handler's own exit code 2, which has no other reason to report.
func TestPermissionDecisionReasonWithoutDecision(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecisionReason":"只给了原因"}}`))
	cfg := groupsFor("PreToolUse", "Bash", handlerWithExit(helper, 2))

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if !verdict.Block {
		t.Fatalf("exit 2 应阻塞（failures=%v）", verdict.Failures)
	}
	if verdict.BlockReason != "只给了原因" {
		t.Errorf("BlockReason = %q，期望保留 handler 给出的原因", verdict.BlockReason)
	}
	// With no decision the verdict must not claim one.
	if verdict.PermissionDecision != "deny" {
		t.Errorf("PermissionDecision = %q", verdict.PermissionDecision)
	}
}

// TestHookSpecificEventNameMismatch covers §5.4's mandatory hookEventName: a
// mismatched or missing value invalidates the whole nested object. This is a
// documented compatibility point, and the failure must be reported rather than
// silently dropping the decision.
func TestHookSpecificEventNameMismatch(t *testing.T) {
	cases := map[string]string{
		"完全不同的值": `{"hookSpecificOutput":{"hookEventName":"PostToolUse","permissionDecision":"deny","permissionDecisionReason":"不该生效"}}`,
		"缺少字段":   `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"不该生效"}}`,
		"空字符串":   `{"hookSpecificOutput":{"hookEventName":"","permissionDecision":"deny"}}`,
		"类型错误":   `{"hookSpecificOutput":{"hookEventName":42,"permissionDecision":"deny"}}`,
	}
	for name, stdout := range cases {
		t.Run(name, func(t *testing.T) {
			helper := newHelper(t, "echo_stdout", stdoutEnv(stdout))
			cfg := groupsFor("PreToolUse", "Bash", helper.handler())

			verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
			if verdict.Block {
				t.Error("hookEventName 不匹配时 hookSpecificOutput 应被拒绝，不应阻塞")
			}
			if verdict.PermissionDecision != "" {
				t.Errorf("PermissionDecision = %q，期望空", verdict.PermissionDecision)
			}
			if !contains(verdict.Failures, "hookSpecificOutput 被拒绝") {
				t.Errorf("拒绝应被记录：%v", verdict.Failures)
			}
		})
	}
}

// TestHookSpecificOutputMatchedEventNameSucceeds is the control for the case above:
// the same object with the right name does take effect.
func TestHookSpecificOutputMatchedEventNameSucceeds(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"应该生效"}}`))
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if !verdict.Block || verdict.BlockReason != "应该生效" {
		t.Errorf("Block = %v，BlockReason = %q（failures=%v）", verdict.Block, verdict.BlockReason, verdict.Failures)
	}
}

// TestTopLevelFields covers §5.4's general fields: systemMessage, terminalSequence
// (accepted and ignored) and suppressOutput (accepted and ignored).
func TestTopLevelFields(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"systemMessage":"界面提示","terminalSequence":"\u001b]9;通知\u0007","suppressOutput":true}`))
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if len(verdict.SystemMessages) != 1 || verdict.SystemMessages[0] != "界面提示" {
		t.Errorf("SystemMessages = %v", verdict.SystemMessages)
	}
	// terminalSequence and suppressOutput are terminal-only / no-ops (§5.4): they
	// must neither fail the hook nor leak into any verdict field.
	if len(verdict.Failures) != 0 {
		t.Errorf("terminalSequence/suppressOutput 应被接受并忽略：%v", verdict.Failures)
	}
}

// TestPlainTextStdoutSessionStart covers §5.1/§8: plain text on SessionStart
// reaches the model as context. This build reports it as AdditionalContext.
func TestPlainTextStdoutSessionStart(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv("当前项目使用 Go 1.26 与 Eino 框架"))
	cfg := groupsFor("SessionStart", "*", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), Input{
		SessionID:     "s",
		HookEventName: "SessionStart",
		Source:        "startup",
	})
	if len(verdict.AdditionalContext) != 1 {
		t.Fatalf("AdditionalContext = %v（failures=%v）", verdict.AdditionalContext, verdict.Failures)
	}
	if verdict.AdditionalContext[0] != "当前项目使用 Go 1.26 与 Eino 框架" {
		t.Errorf("AdditionalContext[0] = %q", verdict.AdditionalContext[0])
	}
	if verdict.Block {
		t.Error("SessionStart 的纯文本 stdout 不应阻塞")
	}
}

// TestPlainTextStdoutUserPromptSubmit covers the same rule for UserPromptSubmit
// (§5.1: this event's plain stdout reaches the model).
func TestPlainTextStdoutUserPromptSubmit(t *testing.T) {
	helper := newHelper(t, "echo_stdout", stdoutEnv("今天生产环境在维护窗口内"))
	cfg := groupsFor("UserPromptSubmit", "*", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), Input{
		SessionID:     "s",
		HookEventName: "UserPromptSubmit",
		Prompt:        "部署吧",
	})
	if len(verdict.AdditionalContext) != 1 || verdict.AdditionalContext[0] != "今天生产环境在维护窗口内" {
		t.Errorf("AdditionalContext = %v（failures=%v）", verdict.AdditionalContext, verdict.Failures)
	}
}

// TestPlainTextStdoutOnOtherEventsIsReported covers the events §5.5 gives no text
// channel: the output has no effect, and saying so is better than dropping it
// silently — a hook author who prints a warning on PreToolUse expects it to appear.
func TestPlainTextStdoutOnOtherEventsIsReported(t *testing.T) {
	for _, event := range []string{"PreToolUse", "PostToolUse", "Stop"} {
		t.Run(event, func(t *testing.T) {
			helper := newHelper(t, "echo_stdout", stdoutEnv("这行文字不会生效"))
			cfg := groupsFor(event, "*", helper.handler())

			verdict := dispatchInDir(t, cfg, helper.options(t), Input{
				SessionID:     "s",
				HookEventName: event,
				ToolName:      "Bash",
			})
			if len(verdict.AdditionalContext) != 0 {
				t.Errorf("AdditionalContext = %v，该事件没有纯文本通道", verdict.AdditionalContext)
			}
			if !contains(verdict.Failures, "不支持纯文本 stdout") {
				t.Errorf("应以非阻塞错误记录：%v", verdict.Failures)
			}
		})
	}
}

// TestJSONStartingButNotEndingIsPlainText covers §5.1's second rule.
func TestJSONStartingButNotEndingIsPlainText(t *testing.T) {
	helper := newHelper(t, "echo_stdout", stdoutEnv(`{"decision":"block" 这行被截断了`))
	cfg := groupsFor("Stop", "*", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), Input{SessionID: "s", HookEventName: "Stop"})
	if verdict.Block {
		t.Error("以 { 开头但不以 } 结尾的 stdout 是纯文本，不应阻塞（§5.1）")
	}
	if !contains(verdict.Failures, "不支持纯文本 stdout") {
		t.Errorf("应记录纯文本无通道：%v", verdict.Failures)
	}
}

// TestJSONArrayIsPlainText covers §5.1's "其他任何字符开头" row: a JSON array is
// plain text, not a decision.
func TestJSONArrayIsPlainText(t *testing.T) {
	helper := newHelper(t, "echo_stdout", stdoutEnv(`["decision","block"]`))
	cfg := groupsFor("Stop", "*", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), Input{SessionID: "s", HookEventName: "Stop"})
	if verdict.Block {
		t.Error("JSON 数组应被当作纯文本（§5.1）")
	}
}

// TestLengthCaps covers §5.8: each of the three capped strings is truncated at
// 10,000 characters and the truncation is recorded. This build must not write the
// overflow to a file, so the caller simply gets less.
func TestLengthCaps(t *testing.T) {
	long := strings.Repeat("长", maxContextChars+2500)
	cases := []struct {
		name   string
		stdout string
		check  func(t *testing.T, verdict Verdict)
	}{
		{
			name:   "additionalContext",
			stdout: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"` + long + `"}}`,
			check: func(t *testing.T, v Verdict) {
				if len(v.AdditionalContext) != 1 {
					t.Fatalf("AdditionalContext = %d 条", len(v.AdditionalContext))
				}
				assertRuneLen(t, "additionalContext", v.AdditionalContext[0], maxContextChars)
				if !contains(v.Failures, "additionalContext") {
					t.Errorf("截断应被记录：%v", v.Failures)
				}
			},
		},
		{
			name:   "systemMessage",
			stdout: `{"systemMessage":"` + long + `"}`,
			check: func(t *testing.T, v Verdict) {
				if len(v.SystemMessages) != 1 {
					t.Fatalf("SystemMessages = %d 条", len(v.SystemMessages))
				}
				assertRuneLen(t, "systemMessage", v.SystemMessages[0], maxContextChars)
				if !contains(v.Failures, "systemMessage") {
					t.Errorf("截断应被记录：%v", v.Failures)
				}
			},
		},
		{
			name:   "initialUserMessage",
			stdout: `{"hookSpecificOutput":{"hookEventName":"SessionStart","initialUserMessage":"` + long + `"}}`,
			check: func(t *testing.T, v Verdict) {
				assertRuneLen(t, "initialUserMessage", v.InitialUserMessage, maxContextChars)
				if !contains(v.Failures, "initialUserMessage") {
					t.Errorf("截断应被记录：%v", v.Failures)
				}
			},
		},
		{
			// §5.8 also caps plain stdout, which is the channel SessionStart uses.
			name:   "纯文本 stdout",
			stdout: long,
			check: func(t *testing.T, v Verdict) {
				if len(v.AdditionalContext) != 1 {
					t.Fatalf("AdditionalContext = %d 条", len(v.AdditionalContext))
				}
				assertRuneLen(t, "stdout 文本", v.AdditionalContext[0], maxContextChars)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			helper := newHelper(t, "echo_stdout", stdoutEnv(tc.stdout))
			event := "PreToolUse"
			if strings.Contains(tc.stdout, "SessionStart") || tc.name == "纯文本 stdout" {
				event = "SessionStart"
			}
			cfg := groupsFor(event, "*", helper.handler())

			verdict := dispatchInDir(t, cfg, helper.options(t), Input{
				SessionID:     "s",
				HookEventName: event,
				ToolName:      "Bash",
			})
			tc.check(t, verdict)
		})
	}
}

// TestLengthCapUnderLimitIsUntouched is the control for the caps: a value at the
// limit is passed through whole.
func TestLengthCapUnderLimitIsUntouched(t *testing.T) {
	exact := strings.Repeat("a", maxContextChars)
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"`+exact+`"}}`))
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if len(verdict.AdditionalContext) != 1 || len([]rune(verdict.AdditionalContext[0])) != maxContextChars {
		t.Fatalf("恰好 10000 字符不应被截断：%d", len([]rune(strings.Join(verdict.AdditionalContext, ""))))
	}
	if contains(verdict.Failures, "截断") {
		t.Errorf("未超限不应记录截断：%v", verdict.Failures)
	}
}

// TestTimeout covers §5.7: a handler that exceeds its timeout is cancelled, its
// output is discarded, and on PreToolUse that means "no block".
func TestTimeout(t *testing.T) {
	helper := newHelper(t, "sleep", "CLAUDEHOOK_HELPER_SLEEP=5")
	handler := helper.handler()
	handler.Timeout = 1 // seconds
	cfg := groupsFor("PreToolUse", "Bash", handler)

	opts := helper.options(t)
	opts.MaxTimeout = 10 * time.Second

	start := time.Now()
	verdict := dispatchInDir(t, cfg, opts, preToolUse("Bash"))
	elapsed := time.Since(start)

	if elapsed > 4*time.Second {
		t.Errorf("超时未生效，耗时 %v", elapsed)
	}
	if verdict.Block {
		t.Error("PreToolUse 的 command hook 超时不阻塞（§5.7）")
	}
	if !contains(verdict.Failures, "输出已丢弃") {
		t.Errorf("超时应被记录并说明输出被丢弃：%v", verdict.Failures)
	}
	if verdict.Handlers != 1 {
		t.Errorf("Handlers = %d，超时的 handler 仍然算运行过（有记录可查）", verdict.Handlers)
	}
	// The record must exist and show the abnormal termination.
	records := NewEngine(cfg, opts).Recent(10)
	_ = records
	if len(verdict.SystemMessages) != 0 {
		t.Errorf("超时后输出应被丢弃，SystemMessages = %v", verdict.SystemMessages)
	}
}

// TestTimeoutClampedByMaxTimeout covers this build's documented clamp: a config
// asking for more than Options.MaxTimeout gets MaxTimeout, and the handler is
// cancelled accordingly. §3 allows up to 600s; a chat turn cannot wait for it.
func TestTimeoutClampedByMaxTimeout(t *testing.T) {
	helper := newHelper(t, "sleep", "CLAUDEHOOK_HELPER_SLEEP=30")
	handler := helper.handler()
	handler.Timeout = 600 // what the config asks for
	cfg := groupsFor("PreToolUse", "Bash", handler)

	opts := helper.options(t)
	opts.MaxTimeout = 500 * time.Millisecond

	start := time.Now()
	verdict := dispatchInDir(t, cfg, opts, preToolUse("Bash"))
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("MaxTimeout 未生效，耗时 %v", elapsed)
	}
	if !contains(verdict.Failures, "输出已丢弃") {
		t.Errorf("被 MaxTimeout 取消应记录：%v", verdict.Failures)
	}
}

// TestDefaultTimeoutIsClampedForUnspecifiedHandlers covers the default path: a
// handler with no timeout field gets the §3 default (600s), then this build's
// clamp, so a hung default-configured hook cannot stall a turn for ten minutes.
func TestDefaultTimeoutIsClampedForUnspecifiedHandlers(t *testing.T) {
	engine := NewEngine(Config{}, Options{})
	if got := engine.handlerTimeout("PreToolUse", Handler{}); got != DefaultMaxTimeout {
		t.Errorf("默认超时 = %v，期望被钳到 %v", got, DefaultMaxTimeout)
	}
	// §3's shorter default for UserPromptSubmit survives the clamp (30s < 60s).
	if got := engine.handlerTimeout("UserPromptSubmit", Handler{}); got != 30*time.Second {
		t.Errorf("UserPromptSubmit 默认超时 = %v，期望 30s（§3）", got)
	}
	// An explicit longer timeout is clamped rather than rejected.
	if got := engine.handlerTimeout("PreToolUse", Handler{Timeout: 600}); got != DefaultMaxTimeout {
		t.Errorf("600s 超时 = %v，期望钳到 %v", got, DefaultMaxTimeout)
	}
	// An explicit shorter one is honoured.
	if got := engine.handlerTimeout("PreToolUse", Handler{Timeout: 5}); got != 5*time.Second {
		t.Errorf("5s 超时 = %v，期望 5s", got)
	}
}

// TestParallelExecution covers §1/§7-13: all matching handlers of one dispatch run in
// parallel, not serially.
//
// The test measures a SERIAL pair of dispatches of the same handler in the same
// process and asserts the parallel one came in well under, instead of asserting an
// absolute duration. A fixed threshold would be a bet on machine speed, and one tight
// enough to catch a regression on a fast machine fails on a slow one — which is
// exactly what the race detector, with its large slowdown, demonstrated.
func TestParallelExecution(t *testing.T) {
	const sleep = 1

	// Two handlers of 1s each in one dispatch: parallel should cost about 1s.
	parallelCfg := groupsFor("PreToolUse", "Bash", sleepHandler(sleep), sleepHandler(sleep))
	// One handler of 1s, dispatched twice: the serial baseline, which includes the
	// same process start-up cost twice.
	serialCfg := groupsFor("PreToolUse", "Bash", sleepHandler(sleep))

	opts := Options{Dir: t.TempDir(), MaxTimeout: 30 * time.Second, Env: sleepModeEnv()}

	engine := NewEngine(parallelCfg, opts)
	started := time.Now()
	verdict := engine.Dispatch(t.Context(), preToolUse("Bash"))
	parallel := time.Since(started)
	if verdict.Handlers != 2 {
		t.Fatalf("Handlers = %d，期望 2（skipped=%v failures=%v）", verdict.Handlers, verdict.Skipped, verdict.Failures)
	}

	engine = NewEngine(serialCfg, opts)
	started = time.Now()
	engine.Dispatch(t.Context(), preToolUse("Bash"))
	first := time.Since(started)
	started = time.Now()
	engine.Dispatch(t.Context(), preToolUse("Bash"))
	serial := first + time.Since(started)

	// One sleep is the floor for either shape, so the parallel run should be near
	// one sleep and the serial baseline near two. Three quarters of the baseline is
	// the midpoint, which separates them with room on both sides.
	if parallel >= serial*3/4 {
		t.Errorf("并行耗时 %v，串行两次耗时 %v：看起来是串行执行（§1：所有命中 handler 并行）",
			parallel, serial)
	}
}

// sleepHandler builds an exec-form helper handler that sleeps for whole seconds. The
// duration travels in argv because two handlers of one dispatch must be able to
// differ, and an Engine has a single Env.
func sleepHandler(seconds int) Handler {
	return Handler{
		Type:    HandlerCommand,
		Command: selfPath,
		Args:    []string{"-test.run=TestMain", strconv.Itoa(seconds)},
		Timeout: 30,
	}
}

// TestParallelExecutionAcrossGroups covers §1's "一个事件的多个 matcher group 都会
// 被求值": handlers reached through different groups run too, and all of them in
// parallel. It uses the same serial-baseline comparison as TestParallelExecution.
func TestParallelExecutionAcrossGroups(t *testing.T) {
	const sleep = 1
	// Three groups, all selecting Bash, one handler each.
	threeGroups := configFromJSON(t, `{"hooks": {"PreToolUse": [
		{"matcher": "Bash", "hooks": [`+mustJSON(sleepHandler(sleep))+`]},
		{"matcher": "*", "hooks": [`+mustJSON(sleepHandler(sleep))+`]},
		{"matcher": "^Bash$", "hooks": [`+mustJSON(sleepHandler(sleep))+`]}
	]}}`)
	oneHandler := groupsFor("PreToolUse", "Bash", sleepHandler(sleep))

	opts := Options{Dir: t.TempDir(), MaxTimeout: 30 * time.Second, Env: sleepModeEnv()}

	engine := NewEngine(threeGroups, opts)
	started := time.Now()
	verdict := engine.Dispatch(t.Context(), preToolUse("Bash"))
	parallel := time.Since(started)
	if verdict.Handlers != 3 {
		t.Fatalf("Handlers = %d，期望 3（skipped=%v failures=%v）", verdict.Handlers, verdict.Skipped, verdict.Failures)
	}

	// Baseline: the same handler dispatched three times in sequence.
	engine = NewEngine(oneHandler, opts)
	var serial time.Duration
	for i := 0; i < 3; i++ {
		started = time.Now()
		engine.Dispatch(t.Context(), preToolUse("Bash"))
		serial += time.Since(started)
	}

	// Three sleeps in parallel cost about one; three serial ones cost about three.
	// Halfway is a safe midpoint at this count.
	if parallel >= serial/2 {
		t.Errorf("三个 handler 并行耗时 %v，串行三次耗时 %v：看起来没有并行", parallel, serial)
	}
}

// sleepModeEnv selects the "sleep" helper mode for an engine whose handlers are built
// by sleepHandler. The duration itself is per handler; this only names the mode.
func sleepModeEnv() []string {
	return []string{envHelperMode + "=sleep"}
}

// TestBlockWinsAmongHandlers covers §5.5's aggregation: Block wins if any handler
// blocks, the first blocking reason is kept, and the second is still reported.
//
// The two handlers get different reasons through argv rather than the environment:
// an Engine has exactly one Env, so per-handler differences must travel in args
// (which is also what a real exec-form config does).
func TestBlockWinsAmongHandlers(t *testing.T) {
	helper := newHelper(t, "reason")
	cfg := groupsFor("PreToolUse", "Bash",
		Handler{
			Type: HandlerCommand, Command: selfPath,
			Args: []string{"-test.run=TestMain", "第一条拒绝原因"}, Timeout: 10,
		},
		Handler{
			Type: HandlerCommand, Command: selfPath,
			Args: []string{"-test.run=TestMain", "第二条拒绝原因"}, Timeout: 10,
		})

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if verdict.Handlers != 2 {
		t.Fatalf("Handlers = %d（failures=%v）", verdict.Handlers, verdict.Failures)
	}
	if !verdict.Block {
		t.Fatalf("Block = false（failures=%v）", verdict.Failures)
	}
	if verdict.BlockReason != "第一条拒绝原因" {
		t.Errorf("BlockReason = %q，期望按配置顺序取第一条", verdict.BlockReason)
	}
	if !contains(verdict.Failures, "另一个 hook 也阻止了本次调用") {
		t.Errorf("第二条阻塞原因应被记录：%v", verdict.Failures)
	}
	if !contains(verdict.Failures, "第二条拒绝原因") {
		t.Errorf("第二条拒绝原因本身应出现在记录里：%v", verdict.Failures)
	}
}

// TestDenyOverridesAllow covers §5.5's precedence for a real dispatch of each half:
// an allow keeps its updatedInput, a deny the other way around is not resurrected
// by it. The ordering half of the rule (deny beats a LATER allow too) is covered by
// TestPreToolUseDecisionPrecedence, which can hold both decisions in one dispatch.
func TestDenyOverridesAllow(t *testing.T) {
	allow := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"被允许的输入"}}}`))
	cfgAllow := groupsFor("PreToolUse", "Bash", handlerWithExitTimeout(allow, 10))

	verdict := dispatchInDir(t, cfgAllow, allow.options(t), preToolUse("Bash"))
	if verdict.PermissionDecision != "allow" {
		t.Fatalf("PermissionDecision = %q，期望 allow（failures=%v）", verdict.PermissionDecision, verdict.Failures)
	}
	if len(verdict.UpdatedInput) == 0 {
		t.Fatal("allow 的 updatedInput 应被保留")
	}
	if verdict.Block {
		t.Error("allow 不应阻塞")
	}

	deny := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"稍后的拒绝"}}`))
	cfgDeny := groupsFor("PreToolUse", "Bash", handlerWithExitTimeout(deny, 10))

	verdict = dispatchInDir(t, cfgDeny, deny.options(t), preToolUse("Bash"))
	if verdict.PermissionDecision != "deny" {
		t.Fatalf("PermissionDecision = %q，期望 deny", verdict.PermissionDecision)
	}
	if !verdict.Block || verdict.BlockReason != "稍后的拒绝" {
		t.Errorf("Block = %v，BlockReason = %q", verdict.Block, verdict.BlockReason)
	}
	if len(verdict.UpdatedInput) != 0 {
		t.Errorf("deny 不应带 updatedInput：%s", verdict.UpdatedInput)
	}
}

// TestPreToolUseDecisionPrecedence drives aggregate directly, which is the only way
// to hold two different handler decisions in one dispatch (a real dispatch shares
// one environment, and therefore cannot produce two different stdout texts).
func TestPreToolUseDecisionPrecedence(t *testing.T) {
	allowInput := json.RawMessage(`{"command":"允许后的输入"}`)
	cases := []struct {
		name        string
		decisions   []string
		wantDec     string
		wantUpdated bool
		wantBlock   bool
	}{
		{
			name:      "deny 覆盖 allow",
			decisions: []string{"allow", "deny"},
			wantDec:   "deny", wantUpdated: false, wantBlock: true,
		},
		{
			name:      "allow 不能覆盖 deny",
			decisions: []string{"deny", "allow"},
			wantDec:   "deny", wantUpdated: false, wantBlock: true,
		},
		{
			name:      "ask 优先级高于 allow",
			decisions: []string{"allow", "ask"},
			wantDec:   "ask", wantUpdated: true,
		},
		{
			name:      "defer 优先级高于 ask",
			decisions: []string{"ask", "defer"},
			wantDec:   "defer", wantUpdated: false,
		},
		{
			name:      "allow 单独保留 updatedInput",
			decisions: []string{"allow"},
			wantDec:   "allow", wantUpdated: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := NewEngine(Config{}, Options{})
			outcomes := make([]handlerOutcome, 0, len(tc.decisions))
			for _, decision := range tc.decisions {
				out := parsedOutput{PermissionDecision: decision}
				if decision == "allow" || decision == "ask" {
					out.UpdatedInput = allowInput
				}
				if decision == "deny" {
					out.Block = true
					out.BlockReason = "被拒绝"
				}
				outcomes = append(outcomes, handlerOutcome{ran: true, output: out})
			}
			verdict := engine.aggregate(Verdict{Continue: true}, outcomes)

			if verdict.PermissionDecision != tc.wantDec {
				t.Errorf("PermissionDecision = %q，期望 %q", verdict.PermissionDecision, tc.wantDec)
			}
			if verdict.Block != tc.wantBlock {
				t.Errorf("Block = %v，期望 %v", verdict.Block, tc.wantBlock)
			}
			hasUpdated := len(verdict.UpdatedInput) > 0
			if hasUpdated != tc.wantUpdated {
				t.Errorf("UpdatedInput 存在 = %v，期望 %v（%s）", hasUpdated, tc.wantUpdated, verdict.UpdatedInput)
			}
		})
	}
}

// TestContinueFalseBeatsEverything drives the aggregate directly to check §5.4's
// precedence rule without needing two different handler outputs.
func TestContinueFalseBeatsEverything(t *testing.T) {
	engine := NewEngine(Config{}, Options{})
	no := false
	outcomes := []handlerOutcome{
		{ran: true, output: parsedOutput{Block: true, BlockReason: "先阻塞", PermissionDecision: "deny"}},
		{ran: true, output: parsedOutput{Continue: &no, StopReason: "停止处理"}},
	}
	verdict := engine.aggregate(Verdict{Continue: true}, outcomes)

	if verdict.Continue {
		t.Error("Continue = true，期望 false（§5.4：优先级高于事件自身的决策字段）")
	}
	if verdict.StopReason != "停止处理" {
		t.Errorf("StopReason = %q", verdict.StopReason)
	}
	if !verdict.Block {
		t.Error("Block 仍应为 true，continue:false 不取消已发生的阻塞")
	}
}

// TestSessionStartOnlyFields covers §8's SessionStart-specific outputs.
func TestSessionStartOnlyFields(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"恢复了 3 天前的会话","initialUserMessage":"先总结一下昨天做了什么","sessionTitle":"恢复的会话","watchPaths":["/tmp/a","/tmp/b"],"reloadSkills":true}}`))
	cfg := groupsFor("SessionStart", "*", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), Input{
		SessionID:     "s",
		HookEventName: "SessionStart",
		Source:        "resume",
	})
	if len(verdict.AdditionalContext) != 1 || verdict.AdditionalContext[0] != "恢复了 3 天前的会话" {
		t.Errorf("AdditionalContext = %v", verdict.AdditionalContext)
	}
	if verdict.InitialUserMessage != "先总结一下昨天做了什么" {
		t.Errorf("InitialUserMessage = %q", verdict.InitialUserMessage)
	}
	if verdict.SessionTitle != "恢复的会话" {
		t.Errorf("SessionTitle = %q", verdict.SessionTitle)
	}
	if len(verdict.WatchPaths) != 2 || verdict.WatchPaths[0] != "/tmp/a" {
		t.Errorf("WatchPaths = %v", verdict.WatchPaths)
	}
	if !verdict.ReloadSkills {
		t.Error("ReloadSkills = false")
	}
}

// TestPermissionRequestDecisionShape covers §5.5's PermissionRequest shape, which
// this build parses even though it does not dispatch that event: a config that
// relies on it must produce a reported outcome rather than an unexplained no-op.
func TestPermissionRequestDecisionShape(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","decision":{"behavior":"deny","message":"需要人工审批"}}}`))
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if !verdict.Block {
		t.Fatalf("decision.behavior=deny 应阻塞（failures=%v）", verdict.Failures)
	}
	if verdict.BlockReason != "需要人工审批" {
		t.Errorf("BlockReason = %q", verdict.BlockReason)
	}
}

// TestWrongTypedFieldsAreReported covers the "reject, do not coerce" policy: a
// decision field of the wrong type is reported, because silently dropping it is how
// a policy hook appears to work while doing nothing.
func TestWrongTypedFieldsAreReported(t *testing.T) {
	helper := newHelper(t, "echo_stdout",
		stdoutEnv(`{"systemMessage":42,"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","watchPaths":"/tmp/a","reloadSkills":"yes"}}`))
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	// The deny itself still takes effect: one bad sibling field does not void the
	// decisions that did parse.
	if !verdict.Block {
		t.Error("permissionDecision=deny 应生效，即使同一对象里有类型错误的字段")
	}
	for _, field := range []string{"systemMessage", "watchPaths", "reloadSkills"} {
		if !contains(verdict.Failures, field) {
			t.Errorf("类型错误的 %s 应被记录：%v", field, verdict.Failures)
		}
	}
}

// TestUnsupportedEventIsSkipped covers the agreed scope: every event name parses
// and is displayed, but only SupportedEvents are dispatched.
func TestUnsupportedEventIsSkipped(t *testing.T) {
	helper := newHelper(t, "echo_system_message")
	cfg := groupsFor("SessionEnd", "*", helper.handler())

	verdict := dispatchInDir(t, cfg, helper.options(t), Input{
		SessionID:     "s",
		HookEventName: "SessionEnd",
		Extra:         map[string]any{"reason": "clear"},
	})
	if verdict.Ran || verdict.Handlers != 0 {
		t.Errorf("不支持的事件不应运行 handler：Ran=%v Handlers=%d", verdict.Ran, verdict.Handlers)
	}
	if !contains(verdict.Skipped, "SessionEnd") || !contains(verdict.Skipped, "不派发") {
		t.Errorf("Skipped 应说明事件名与原因：%v", verdict.Skipped)
	}
	if helper.read() != "" {
		t.Error("不支持的事件不应启动任何进程")
	}
	if !verdict.Continue {
		t.Error("Continue 应为 true：没有决策")
	}
}

// TestDisableAllHooks covers §1.3.
func TestDisableAllHooks(t *testing.T) {
	helper := newHelper(t, "echo_system_message")
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())
	cfg.DisableAll = true

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if verdict.Ran || verdict.Handlers != 0 {
		t.Errorf("disableAllHooks 为 true 时不应运行：%+v", verdict)
	}
	if !contains(verdict.Skipped, "disableAllHooks") {
		t.Errorf("Skipped 应说明原因：%v", verdict.Skipped)
	}
	if helper.read() != "" {
		t.Error("disableAllHooks 为 true 时不应启动任何进程（§1.3：配置保留但全部禁用）")
	}
}

// TestZeroConfigRunsNothing covers NewEngine's documented contract.
func TestZeroConfigRunsNothing(t *testing.T) {
	engine := NewEngine(Config{}, Options{Logger: zap.NewNop()})
	verdict := engine.Dispatch(t.Context(), preToolUse("Bash"))
	if verdict.Ran || verdict.Handlers != 0 {
		t.Errorf("零配置应什么都不跑：%+v", verdict)
	}
	if !verdict.Continue {
		t.Error("Continue 应为 true")
	}
	if len(verdict.Skipped) != 0 || len(verdict.Failures) != 0 {
		t.Errorf("零配置不应产生 skip/failure：%+v", verdict)
	}
}

// TestSkippedPaths covers every configured reason a handler does not run. Each one
// must name the handler and say why, because "the hook did not fire" is the
// question this runtime exists to answer.
func TestSkippedPaths(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		handler Handler
		// rawConfig, when set, is used instead of building a config from handler.
		rawConfig   *Config
		in          Input
		opts        Options
		wantSkipped string
	}{
		{
			name: "prompt 类型未实现", event: "Stop",
			handler:     Handler{Type: HandlerPrompt, Prompt: "Evaluate $ARGUMENTS"},
			in:          Input{SessionID: "s", HookEventName: "Stop"},
			wantSkipped: "prompt 类型 handler 本版本未实现",
		},
		{
			name: "agent 类型未实现", event: "Stop",
			handler:     Handler{Type: HandlerAgent, Prompt: "Verify $ARGUMENTS"},
			in:          Input{SessionID: "s", HookEventName: "Stop"},
			wantSkipped: "agent 类型 handler 本版本未实现",
		},
		{
			name: "未知 handler 类型", event: "Stop",
			handler:     Handler{Type: "hologram"},
			in:          Input{SessionID: "s", HookEventName: "Stop"},
			wantSkipped: "本版本不支持",
		},
		{
			name: "带 if 的 handler", event: "PreToolUse",
			handler:     Handler{Type: HandlerCommand, Command: "/bin/echo", If: "Bash(git *)"},
			in:          preToolUse("Bash"),
			wantSkipped: "未实现权限规则求值",
		},
		{
			name: "command 缺少 command", event: "Stop",
			handler:     Handler{Type: HandlerCommand},
			in:          Input{SessionID: "s", HookEventName: "Stop"},
			wantSkipped: "缺少 command 字段",
		},
		{
			name: "http 缺少 url", event: "Stop",
			handler:     Handler{Type: HandlerHTTP},
			in:          Input{SessionID: "s", HookEventName: "Stop"},
			wantSkipped: "缺少 url 字段",
		},
		{
			name: "SessionStart 不支持 http", event: "SessionStart",
			handler:     Handler{Type: HandlerHTTP, URL: "https://example.test"},
			in:          Input{SessionID: "s", HookEventName: "SessionStart", Source: "startup"},
			wantSkipped: "SessionStart 不支持 http",
		},
		{
			name: "mcp_tool 缺少 caller", event: "PostToolUse",
			handler:     Handler{Type: HandlerMCPTool, Server: "demo", Tool: "scan"},
			in:          Input{SessionID: "s", HookEventName: "PostToolUse", ToolName: "Write"},
			wantSkipped: "未接入 MCP caller",
		},
		{
			name: "mcp_tool 缺少 server/tool", event: "PostToolUse",
			handler:     Handler{Type: HandlerMCPTool, Server: "demo"},
			in:          Input{SessionID: "s", HookEventName: "PostToolUse", ToolName: "Write"},
			opts:        Options{MCP: &fakeMCP{}},
			wantSkipped: "缺少 server 或 tool",
		},
		{
			name: "SessionStart 上 MCP 未就绪", event: "SessionStart",
			handler:     Handler{Type: HandlerMCPTool, Server: "demo", Tool: "load"},
			in:          Input{SessionID: "s", HookEventName: "SessionStart", Source: "startup"},
			opts:        Options{MCP: &fakeMCP{}},
			wantSkipped: "MCP server 尚未就绪",
		},
		{
			name: "不支持的 shell", event: "Stop",
			handler:     Handler{Type: HandlerCommand, Command: "/bin/echo", Shell: "powershell"},
			in:          Input{SessionID: "s", HookEventName: "Stop"},
			wantSkipped: "powershell",
		},
		{
			// ParseConfig refuses a handler with no type, so this case builds the
			// Config by hand: the dispatch-level reason is the defence for a caller
			// that constructed a Config itself, and it must not fall through to
			// "ran but did nothing".
			name: "handler 缺少 type", event: "Stop",
			rawConfig: &Config{Groups: map[string][]Group{
				"Stop": {{Matcher: "*", Handlers: []Handler{{}}}},
			}},
			in:          Input{SessionID: "s", HookEventName: "Stop"},
			wantSkipped: "缺少 type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if tc.rawConfig != nil {
				cfg = *tc.rawConfig
			} else {
				cfg = groupsFor(tc.event, "*", tc.handler)
			}
			opts := tc.opts
			opts.Dir = t.TempDir()
			opts.MaxTimeout = 5 * time.Second

			verdict := dispatchInDir(t, cfg, opts, tc.in)
			if verdict.Ran || verdict.Handlers != 0 {
				t.Errorf("应跳过而不是运行：Ran=%v Handlers=%d failures=%v",
					verdict.Ran, verdict.Handlers, verdict.Failures)
			}
			if !contains(verdict.Skipped, tc.wantSkipped) {
				t.Errorf("Skipped 中找不到 %q：%v", tc.wantSkipped, verdict.Skipped)
			}
		})
	}
}

// TestMatcherMismatchIsSilent covers the deliberate asymmetry: a matcher that did
// not select the handler produces no skip line at all. Every non-matching group of
// every event would otherwise bury the real skips — the live settings file has
// three PreToolUse groups and only one of them matches a Bash call.
func TestMatcherMismatchIsSilent(t *testing.T) {
	helper := newHelper(t, "echo_system_message")
	cfg := configFromJSON(t, `{"hooks": {"PreToolUse": [
		{"matcher": "Edit|Write", "hooks": [`+mustJSON(helper.handler())+`]},
		{"matcher": "^Notebook", "hooks": [`+mustJSON(helper.handler())+`]},
		{"matcher": "Bash", "hooks": [`+mustJSON(helper.handler())+`]}
	]}}`)

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	if verdict.Handlers != 1 {
		t.Errorf("Handlers = %d，期望只跑匹配 Bash 的那一个", verdict.Handlers)
	}
	if len(verdict.Skipped) != 0 {
		t.Errorf("matcher 未命中不应产生 skip 记录：%v", verdict.Skipped)
	}
}

// TestAsyncHandlerDoesNotBlock covers §9: async:true starts the process and returns
// immediately, the record says so, and the decision fields are never read.
func TestAsyncHandlerDoesNotBlock(t *testing.T) {
	helper := newHelper(t, "sleep", "CLAUDEHOOK_HELPER_SLEEP=3")
	handler := handlerWithExitTimeout(helper, 10)
	handler.Async = true
	cfg := groupsFor("PreToolUse", "Bash", handler)

	engine := NewEngine(cfg, helper.options(t))
	start := time.Now()
	verdict := engine.Dispatch(t.Context(), preToolUse("Bash"))
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("async handler 阻塞了 %v，应立刻返回（§9）", elapsed)
	}
	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d（skipped=%v）", verdict.Handlers, verdict.Skipped)
	}

	records := engine.Recent(10)
	if len(records) != 1 {
		t.Fatalf("Recent 返回 %d 条，期望 1", len(records))
	}
	rec := records[0]
	if rec.ExitCode != asyncRecordExitCode {
		t.Errorf("ExitCode = %d，期望 %d（async 没有退出码可报）", rec.ExitCode, asyncRecordExitCode)
	}
	if !strings.Contains(rec.Reason, "async: 已后台启动，不参与决策") {
		t.Errorf("Reason = %q，应说明已后台启动", rec.Reason)
	}
	// §9: an async hook cannot control Claude, so its output must not produce a
	// decision — the sleep helper would have printed a systemMessage.
	if len(verdict.SystemMessages) != 0 {
		t.Errorf("async handler 的决策不应参与聚合：%v", verdict.SystemMessages)
	}
}

// TestAsyncRewakeTreatedAsAsync covers the agreed scope: asyncRewake is out of
// scope, so it behaves as async and the record says which one it was.
func TestAsyncRewakeTreatedAsAsync(t *testing.T) {
	helper := newHelper(t, "sleep", "CLAUDEHOOK_HELPER_SLEEP=3")
	handler := handlerWithExitTimeout(helper, 10)
	handler.AsyncRewake = true
	cfg := groupsFor("PreToolUse", "Bash", handler)

	engine := NewEngine(cfg, helper.options(t))
	start := time.Now()
	engine.Dispatch(t.Context(), preToolUse("Bash"))
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("asyncRewake 阻塞了 %v，应立刻返回", elapsed)
	}
	records := engine.Recent(10)
	if len(records) != 1 {
		t.Fatalf("Recent 返回 %d 条", len(records))
	}
	if !strings.Contains(records[0].Reason, "asyncRewake 本版本不支持") {
		t.Errorf("Reason = %q，应说明 asyncRewake 按 async 处理", records[0].Reason)
	}
	if records[0].ExitCode != asyncRecordExitCode {
		t.Errorf("ExitCode = %d，期望 %d", records[0].ExitCode, asyncRecordExitCode)
	}
}

// TestAsyncRecordIsWrittenImmediately covers §9's "record at start" requirement:
// the log shows the hook the moment it fires, not when it eventually finishes.
func TestAsyncRecordIsWrittenImmediately(t *testing.T) {
	helper := newHelper(t, "sleep", "CLAUDEHOOK_HELPER_SLEEP=5")
	handler := handlerWithExitTimeout(helper, 10)
	handler.Async = true
	cfg := groupsFor("PreToolUse", "Bash", handler)

	engine := NewEngine(cfg, helper.options(t))
	engine.Dispatch(t.Context(), preToolUse("Bash"))

	if got := len(engine.Recent(10)); got != 1 {
		t.Fatalf("dispatch 返回时应有 1 条记录，得到 %d", got)
	}
}

// TestRecentBoundsAndOrder covers the event log's contract: newest first, bounded
// by limit, and bounded by MaxRecords.
func TestRecentBoundsAndOrder(t *testing.T) {
	engine := NewEngine(Config{}, Options{MaxRecords: 5})
	base := time.Date(2026, 9, 19, 15, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		engine.writeRecords([]Record{{At: at, Event: "Stop", Command: "cmd" + string(rune('a'+i))}}, false)
	}

	all := engine.Recent(0)
	if len(all) != 5 {
		t.Fatalf("Recent(0) 返回 %d 条，期望被 MaxRecords=5 限制", len(all))
	}
	// Newest first.
	if all[0].Command != "cmdh" {
		t.Errorf("Recent[0] = %q，期望最新的 cmdh", all[0].Command)
	}
	if all[4].Command != "cmdd" {
		t.Errorf("Recent[4] = %q，期望 cmdd（最早的 3 条已被挤掉）", all[4].Command)
	}
	for i := 1; i < len(all); i++ {
		if !all[i-1].At.After(all[i].At) {
			t.Errorf("Recent 未按时间倒序：%v 在 %v 之前", all[i-1].At, all[i].At)
		}
	}

	// limit is honoured and is an upper bound, not an exact count.
	if got := len(engine.Recent(2)); got != 2 {
		t.Errorf("Recent(2) 返回 %d 条", got)
	}
	if got := len(engine.Recent(100)); got != 5 {
		t.Errorf("Recent(100) 返回 %d 条，应返回全部 5 条", got)
	}
	if got := len(engine.Recent(-1)); got != 5 {
		t.Errorf("Recent(-1) 返回 %d 条，应按「全部」处理", got)
	}
}

// TestRecentConcurrent covers the concurrency contract of the log: dispatch from
// many goroutines while reading it must not race, and every record must be
// accounted for. Run with -race this is the assertion that matters.
func TestRecentConcurrent(t *testing.T) {
	helper := newHelper(t, "echo_system_message")
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())
	engine := NewEngine(cfg, helper.options(t))

	const workers = 8
	const perWorker = 4
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				engine.Dispatch(context.Background(), preToolUse("Bash"))
			}
		}()
	}
	// Concurrent readers, including one that keeps calling Recent while records
	// are being appended.
	stop := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = engine.Recent(3)
			}
		}
	}()

	wg.Wait()
	close(stop)
	readerWG.Wait()

	if got := len(engine.Recent(0)); got != workers*perWorker {
		t.Errorf("记录数 = %d，期望 %d", got, workers*perWorker)
	}
}

// TestUpdateIsConcurrentSafe covers hot reload: Update while dispatching must not
// race, and the active config must be one of the two valid ones.
func TestUpdateIsConcurrentSafe(t *testing.T) {
	first := newHelper(t, "echo_system_message")
	second := newHelper(t, "echo_system_message")

	cfgFirst := groupsFor("PreToolUse", "Bash", first.handler())
	cfgSecond := groupsFor("PreToolUse", "Bash", second.handler())
	engine := NewEngine(cfgFirst, first.options(t))

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				if n%2 == 0 {
					engine.Dispatch(context.Background(), preToolUse("Bash"))
					continue
				}
				engine.Update(cfgSecond)
				engine.Update(cfgFirst)
			}
		}(i)
	}
	wg.Wait()

	if got := engine.Config(); len(got.Groups["PreToolUse"]) != 1 {
		t.Errorf("Update 后配置异常：%+v", got)
	}
}

// TestConfigRoundTrip covers Config() returning what Update stored, including the
// derived EventOrder for a Config built by hand.
func TestConfigRoundTrip(t *testing.T) {
	engine := NewEngine(Config{
		Groups: map[string][]Group{
			"Stop":         {{Matcher: "*", Handlers: []Handler{{Type: HandlerCommand, Command: "/bin/true"}}}},
			"PreToolUse":   {{Matcher: "Bash", Handlers: []Handler{{Type: HandlerCommand, Command: "/bin/true"}}}},
			"SessionStart": {{Matcher: "*", Handlers: []Handler{{Type: HandlerCommand, Command: "/bin/true"}}}},
		},
	}, Options{})

	got := engine.Config()
	want := []string{"PreToolUse", "SessionStart", "Stop"}
	if len(got.EventOrder) != len(want) {
		t.Fatalf("EventOrder = %v，期望 %v", got.EventOrder, want)
	}
	for i, name := range want {
		if got.EventOrder[i] != name {
			t.Errorf("EventOrder[%d] = %q，期望 %q", i, got.EventOrder[i], name)
		}
	}
	if got.Groups["PreToolUse"][0].Matcher != "Bash" {
		t.Errorf("Config() 丢失了配置：%+v", got)
	}
}

// TestConfigIsIsolated covers the deep copy: the caller's Config must not be shared
// with the engine's live state in either direction, because Dispatch reads that state
// from many goroutines while the console reads it through Config().
func TestConfigIsIsolated(t *testing.T) {
	original := Config{
		Groups: map[string][]Group{
			"PreToolUse": {{
				Matcher: "Bash",
				Handlers: []Handler{{
					Type:           HandlerCommand,
					Command:        "/bin/true",
					Args:           []string{"a"},
					Headers:        map[string]string{"X": "1"},
					Input:          map[string]string{"k": "v"},
					AllowedEnvVars: []string{"A"},
				}},
			}},
		},
	}
	engine := NewEngine(original, Options{})

	// Mutating the caller's copy must not reach the engine.
	original.Groups["PreToolUse"][0].Matcher = "MUTATED"
	original.Groups["PreToolUse"][0].Handlers[0].Args[0] = "MUTATED"
	original.Groups["PreToolUse"][0].Handlers[0].Headers["X"] = "MUTATED"
	original.Groups["PreToolUse"][0].Handlers[0].Input["k"] = "MUTATED"
	original.Groups["PreToolUse"][0].Handlers[0].AllowedEnvVars[0] = "MUTATED"
	original.Groups["NewEvent"] = nil

	got := engine.Config()
	h := got.Groups["PreToolUse"][0].Handlers[0]
	if got.Groups["PreToolUse"][0].Matcher != "Bash" || h.Args[0] != "a" ||
		h.Headers["X"] != "1" || h.Input["k"] != "v" || h.AllowedEnvVars[0] != "A" {
		t.Fatalf("调用方修改原 Config 影响了引擎：%+v", got.Groups["PreToolUse"][0])
	}
	if _, ok := got.Groups["NewEvent"]; ok {
		t.Error("调用方新增的事件出现在了引擎配置里")
	}

	// And the other direction: mutating what Config() returned must not reach the
	// engine either.
	got.Groups["PreToolUse"][0].Matcher = "MUTATED"
	got.Groups["PreToolUse"][0].Handlers[0].Args[0] = "MUTATED"
	got.Groups["PreToolUse"][0].Handlers[0].Headers["X"] = "MUTATED"

	again := engine.Config()
	if again.Groups["PreToolUse"][0].Matcher != "Bash" ||
		again.Groups["PreToolUse"][0].Handlers[0].Args[0] != "a" ||
		again.Groups["PreToolUse"][0].Handlers[0].Headers["X"] != "1" {
		t.Fatalf("修改 Config() 的返回值影响了引擎：%+v", again.Groups["PreToolUse"][0])
	}
}

// TestDispatchWithNilContext covers the defensive path: a nil context must not
// panic, because Dispatch is called from a console handler that may not have one.
func TestDispatchWithNilContext(t *testing.T) {
	helper := newHelper(t, "echo_system_message")
	cfg := groupsFor("PreToolUse", "Bash", helper.handler())
	engine := NewEngine(cfg, helper.options(t))

	// The nil is passed through a variable on purpose: staticcheck's SA1012 is right
	// that passing a literal nil context is a bug, but here the nil IS the input
	// under test — the point is that a caller which has no context does not panic.
	var noContext context.Context
	verdict := engine.Dispatch(noContext, preToolUse("Bash"))
	if verdict.Handlers != 1 {
		t.Errorf("Handlers = %d（failures=%v）", verdict.Handlers, verdict.Failures)
	}
}

// TestDispatchContextCancellation covers the caller cancelling: the running handler
// is killed and recorded as a non-blocking cancellation, not a crash.
func TestDispatchContextCancellation(t *testing.T) {
	helper := newHelper(t, "sleep", "CLAUDEHOOK_HELPER_SLEEP=10")
	handler := helper.handler()
	handler.Timeout = 30
	cfg := groupsFor("PreToolUse", "Bash", handler)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	verdict := dispatchInDir(t, cfg, helper.options(t), preToolUse("Bash"))
	_ = ctx
	if verdict.Block {
		t.Error("取消不应阻塞")
	}
	if !contains(verdict.Failures, "已丢弃") {
		t.Errorf("取消应被记录：%v", verdict.Failures)
	}
}

// TestEnvIsInherited covers §1.2: the handler inherits the process environment plus
// Options.Env. Both halves matter — a hook that runs a normal script needs PATH,
// and a hook that needs the project directory needs the caller's addition.
func TestEnvIsInherited(t *testing.T) {
	helper := newHelper(t, "env")
	cfg := groupsFor("Stop", "*", helper.handler())
	opts := helper.options(t)
	opts.Env = append(opts.Env, "MARKER=from-options")

	verdict := dispatchInDir(t, cfg, opts, Input{SessionID: "s", HookEventName: "Stop"})
	if verdict.Handlers != 1 {
		t.Fatalf("Handlers = %d（failures=%v）", verdict.Handlers, verdict.Failures)
	}
	if len(verdict.SystemMessages) != 1 {
		t.Fatalf("SystemMessages = %v", verdict.SystemMessages)
	}
	got := verdict.SystemMessages[0]
	if !strings.Contains(got, "MARKER=from-options") {
		t.Errorf("Options.Env 未传给子进程：%q", got)
	}
	if !strings.Contains(got, "PATH_SET=true") {
		t.Errorf("进程环境未被子进程继承：%q", got)
	}
}

// TestEnvOptionsOverrideProcess covers the merge rule: Options.Env wins for a name
// that is also in the process environment, because the caller's value is the one
// that describes this runtime's session.
func TestEnvOptionsOverrideProcess(t *testing.T) {
	t.Setenv("MARKER", "from-process")
	helper := newHelper(t, "env")
	cfg := groupsFor("Stop", "*", helper.handler())
	opts := helper.options(t)
	opts.Env = append(opts.Env, "MARKER=from-options")

	verdict := dispatchInDir(t, cfg, opts, Input{SessionID: "s", HookEventName: "Stop"})
	if len(verdict.SystemMessages) != 1 || !strings.Contains(verdict.SystemMessages[0], "MARKER=from-options") {
		t.Errorf("Options.Env 应覆盖进程环境：%v", verdict.SystemMessages)
	}
}

// handlerWithExitTimeout copies a helper handler and sets its timeout, so a case
// can pin a short timeout without repeating the exec-form wiring.
func handlerWithExitTimeout(h helperMode, seconds int) Handler {
	handler := h.handler()
	handler.Timeout = seconds
	return handler
}

// handlerWithExit builds a helper handler that runs the helper and then exits with
// the requested code, so a case can pin §5.2's exit-code semantics.
//
// Shell form is deliberate — "run this, then exit N" is the shell operator a real
// operator would write — and that choice has a consequence the wiring must respect:
// the command string is a plain string handed to sh, so the handler cannot rely on
// the engine's Env to identify itself. The helper's whole environment is therefore
// inlined into the command, quoted, exactly as an operator would have to write it.
// Forwarding all of h.env rather than a hard-coded pair is what keeps this helper
// working when a case adds another variable (the stdout text, say).
func handlerWithExit(h helperMode, code int) Handler {
	parts := make([]string, 0, len(h.env))
	for _, kv := range h.env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		parts = append(parts, name+"="+shellQuote(value))
	}
	return Handler{
		Type: HandlerCommand,
		Command: strings.Join(parts, " ") + " " + shellQuote(selfPath) +
			" -test.run=TestMain; exit " + strconv.Itoa(code),
	}
}

// shellQuote single-quotes a value for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// assertRuneLen fails unless s is exactly want runes long. §5.8's cap counts
// characters, not bytes, so a byte-based truncation that split a Chinese character
// would be a bug this catches.
func assertRuneLen(t *testing.T, what, s string, want int) {
	t.Helper()
	if got := len([]rune(s)); got != want {
		t.Errorf("%s 长度 = %d 字符，期望 %d", what, got, want)
	}
}

// argvFromRecord reads back the argv the helper recorded, one %q-quoted element per
// line. strconv.Unquote is the decoder because %q is Go's own quoting, which is
// exactly the format the helper wrote.
func argvFromRecord(t *testing.T, record string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(record, "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(name, "argv") {
			continue
		}
		decoded, err := strconv.Unquote(value)
		if err != nil {
			t.Fatalf("读取 %s 失败（值 %q）：%v", name, value, err)
		}
		out = append(out, decoded)
	}
	if len(out) == 0 && !strings.Contains(record, "argv0=") {
		t.Fatalf("记录文件没有 argv 段：%s", record)
	}
	return out
}
