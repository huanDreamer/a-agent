// Package claudecode implements Claude Code compatibility mode.
//
// The mode exists because a machine that already runs Claude Code has already
// answered the two questions this agent asks at startup — which endpoint and
// models to use, and which hooks to run around every tool call — in a file that
// Claude Code owns: ~/.claude/settings.json. Rather than asking the operator to
// transcribe those answers into config.yaml (where they would then drift), the
// mode reads that file and runs on it.
//
// The split inside this package follows the split in the file itself:
//
//   - settings.go is the file: a tolerant read of `env`, `hooks` and
//     `disableAllHooks`, plus the env-var catalogue the console renders;
//   - model.go is the `env` block as a model configuration — the same
//     precedence Claude Code applies to it;
//   - service.go is the switch, the hook engine and the provider this mode adds
//     to the model catalog.
//
// Three deliberate limits, all of them visible in 设置 → ClaudeCode rather than
// silent:
//
//   - only ~/.claude/settings.json is read. Claude Code also merges a project's
//     .claude/settings.json and .claude/settings.local.json, which are
//     per-directory facts this process would have to resolve per conversation;
//   - only five events are dispatched (see claudehook.SupportedEvents). The
//     others are shown as configured-but-not-wired;
//   - nothing here writes to the settings file. It is Claude Code's, and a
//     compatibility mode that edited it would be a second author of it.
package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/huan/huan-agent/internal/claudehook"
)

// ProviderID is the provider this mode contributes to the model catalog. It is
// not a stored provider row: the settings file is the source of truth while the
// mode is on, so a database copy would be a second answer that can go stale.
const ProviderID = "claudecode"

// ProviderName is what 设置 → ClaudeCode and the composer show for it.
const ProviderName = "ClaudeCode 兼容"

// DefaultSettingsPath is the file Claude Code itself reads, and the one this
// mode reads when the config does not name another.
func DefaultSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "settings.json")
}

// Settings is one read of a Claude Code settings file.
//
// A failure is carried rather than returned: a missing or malformed file is a
// state the console renders ("找不到 settings.json" / "JSON 解析失败：…"), and the
// rest of the process keeps working — a compatibility mode that refused to
// start the server would be worse than one that reports what it could not read.
type Settings struct {
	// Path is the file that was read.
	Path string `json:"path"`
	// Found reports whether the file exists.
	Found bool `json:"found"`
	// Err is why it could not be read or parsed, empty when fine.
	Err string `json:"error"`
	// MTime is the file's modification time, zero when unknown.
	MTime time.Time `json:"mtime"`
	// LoadedAt is when this read happened.
	LoadedAt time.Time `json:"loaded_at"`

	// Env is the settings file's env block, verbatim.
	Env map[string]string `json:"env"`
	// DisableAllHooks is Claude Code's own kill switch. It is honoured: a mode
	// that ran hooks the settings file explicitly disabled would be ignoring the
	// operator's most direct instruction about them.
	DisableAllHooks bool `json:"disable_all_hooks"`
	// Hooks is the parsed hooks configuration.
	Hooks claudehook.Config `json:"hooks"`
	// HooksErr is why some of the hooks block could not be parsed, empty when
	// fine. Parse failures never discard the groups that did parse.
	HooksErr string `json:"hooks_error"`
}

// settingsFile is the subset of the settings file this mode understands. Every
// other key (permissions, statusLine, model, …) belongs to Claude Code and is
// deliberately not read: guessing at them would be a second implementation of
// Claude Code, not a compatible one.
//
// The hooks section is not decoded here: it is parsed by internal/claudehook,
// which is also the layer that reads disableAllHooks — the two live in the same
// object, and splitting them would mean two parsers of one file agreeing by
// accident.
type settingsFile struct {
	Env map[string]json.RawMessage `json:"env"`
}

// Load reads the settings file at path.
//
// It never returns an error: everything that can go wrong is reported inside
// the returned Settings, because "the file is not there" and "the file is not
// JSON" are both things the console shows rather than failures of the call.
func Load(path string) Settings {
	out := Settings{
		Path:     path,
		LoadedAt: time.Now(),
		Env:      map[string]string{},
	}
	if strings.TrimSpace(path) == "" {
		out.Err = "没有解析出 settings.json 的路径（$HOME 不可用，且 claudecode.settings_path 未设置）"
		return out
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			out.Err = fmt.Sprintf("找不到文件 %s", path)
			return out
		}
		out.Err = fmt.Sprintf("无法读取 %s：%v", path, err)
		return out
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		out.Err = fmt.Sprintf("无法读取 %s：%v", path, err)
		return out
	}
	out.Found = true
	out.MTime = info.ModTime()

	var file settingsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		out.Err = fmt.Sprintf("%s 不是合法的 JSON：%v", path, err)
		return out
	}
	for k, v := range file.Env {
		out.Env[k] = envValueString(v)
	}
	// The whole file is handed to the hook parser, not just its `hooks` value:
	// disableAllHooks is a sibling of `hooks` in the same object, and it is the
	// operator's most direct instruction about whether any hook runs at all.
	cfg, err := claudehook.ParseConfig(raw)
	if err != nil {
		// A malformed group does not discard the groups that parsed: the parser
		// returns both, and the panel shows the error beside the hooks that work.
		out.HooksErr = err.Error()
	}
	out.Hooks = cfg
	out.DisableAllHooks = cfg.DisableAll
	return out
}

// envValueString renders one env value as a string.
//
// The settings file is JSON, so a value may be a number or a bool (a port, a
// flag); Claude Code exports those to its hooks as text, and so do we. An
// object or array is kept as its JSON text rather than dropped, so the console
// can show what the file actually says.
func envValueString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return strings.TrimSpace(string(raw))
	}
	switch t := v.(type) {
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		// As written, not re-formatted: 600000 must not become 6e+05 in a
		// table an operator compares against their own file.
		return t.String()
	}
	return strings.TrimSpace(string(raw))
}

// Get returns one env value.
func (s Settings) Get(key string) string {
	if s.Env == nil {
		return ""
	}
	return strings.TrimSpace(s.Env[key])
}

// EnvRow is one line of the 环境变量 table in 设置 → ClaudeCode.
type EnvRow struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
	// Used reports whether this mode actually reads the variable. A CC-only
	// variable is shown (the operator put it there and will look for it) but is
	// marked as not used by this agent, which is the honest answer.
	Used bool   `json:"used"`
	Note string `json:"note"`
}

// envSpec is one known environment variable: whether this build consumes it,
// whether its value is a secret, and what it means.
type envSpec struct {
	used   bool
	secret bool
	note   string
}

// knownEnv is the catalogue of Claude Code environment variables this console
// can explain, in the order it is rendered. The order is deliberate rather than
// alphabetical: the endpoint, the credential and the model are what an operator
// checks first, and the footer rows are the ones that only matter when
// something is wrong.
var knownEnv = []struct {
	key  string
	spec envSpec
}{
	{"ANTHROPIC_BASE_URL", envSpec{true, false, "模型端点：兼容模式下所有对话都打到这里"}},
	{"ANTHROPIC_AUTH_TOKEN", envSpec{true, true, "凭据：以 Authorization: Bearer 发送"}},
	{"ANTHROPIC_API_KEY", envSpec{true, true, "凭据：以 x-api-key 发送"}},
	{"ANTHROPIC_MODEL", envSpec{true, false, "主模型（优先级最高）"}},
	{"ANTHROPIC_DEFAULT_OPUS_MODEL", envSpec{true, false, "Opus 档模型：ANTHROPIC_MODEL 未设置时的主要候选"}},
	{"ANTHROPIC_DEFAULT_SONNET_MODEL", envSpec{true, false, "Sonnet 档模型"}},
	{"ANTHROPIC_DEFAULT_HAIKU_MODEL", envSpec{true, false, "Haiku 档模型：兼容模式用作小模型"}},
	{"ANTHROPIC_SMALL_FAST_MODEL", envSpec{true, false, "旧名的小模型变量：Haiku 档为空时使用"}},
	{"CLAUDE_CODE_SUBAGENT_MODEL", envSpec{false, false, "仅展示：本 agent 的子 agent 随主模型运行，不单独换模型"}},
	{"CLAUDE_CODE_EFFORT_LEVEL", envSpec{true, false, "effort level：随每个 hook payload 一起传给 hook"}},
	{"CLAUDE_CODE_AUTO_COMPACT_WINDOW", envSpec{true, false, "自动压缩窗口：仅展示，本 agent 的窗口按模型目录推导"}},
	{"CLAUDE_CODE_MAX_OUTPUT_TOKENS", envSpec{true, false, "单次回复的 max_tokens"}},
	{"ANTHROPIC_CUSTOM_HEADERS", envSpec{false, false, "自定义请求头：本 agent 未接入"}},
	{"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB", envSpec{false, false, "子进程环境变量剥离：本 agent 未接入"}},
	{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", envSpec{false, false, "关闭非必要请求：本 agent 未接入"}},
	{"CLAUDE_ENV_FILE", envSpec{false, false, "hook 可写入的环境文件：本 agent 未接入"}},
}

// EnvRows returns the env block as the table 设置 → ClaudeCode renders: the
// known variables first in catalogue order, then anything else the file sets,
// alphabetically, so an unrecognised key is visible instead of dropped.
//
// A variable marked secret is masked *here*, not in the console: the credential
// is the one value in this payload that must never leave the process, and doing
// it at the boundary means no later client, log or proxy can receive it by
// forgetting to mask it. The operator still recognises the key from its head and
// tail, which is the whole reason the masked form exists.
func (s Settings) EnvRows() []EnvRow {
	rows := make([]EnvRow, 0, len(s.Env))
	seen := map[string]bool{}
	for _, known := range knownEnv {
		value, ok := s.Env[known.key]
		if !ok {
			continue
		}
		seen[known.key] = true
		if known.spec.secret {
			value = Mask(value)
		}
		rows = append(rows, EnvRow{
			Key:    known.key,
			Value:  value,
			Secret: known.spec.secret,
			Used:   known.spec.used,
			Note:   known.spec.note,
		})
	}
	rest := make([]string, 0, len(s.Env))
	for k := range s.Env {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		rows = append(rows, EnvRow{
			Key:   k,
			Value: s.Env[k],
			// An unknown key is never echoed as a secret and never marked used:
			// guessing either way would be a claim this code cannot support.
			Note: "未识别的变量（原样展示，本 agent 不使用）",
		})
	}
	return rows
}
