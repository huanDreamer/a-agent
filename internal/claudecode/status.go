package claudecode

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/huan/huan-agent/internal/claudehook"
	"github.com/huan/huan-agent/internal/store"
)

// Status is everything 设置 → ClaudeCode renders, in one object.
//
// One payload rather than five: the panel draws a switch, a model block, an env
// table, the hook table and the run log together, and five requests would let
// them disagree with each other for as long as they took to arrive — which is
// exactly the state a reader would misread as "the mode changed by itself".
//
// The model block carries ModelConfig, which holds the credential in an
// unexported JSON field: the console only ever sees MaskedToken.
type Status struct {
	Mode      string `json:"mode"`
	Compat    bool   `json:"compat"`
	Available bool   `json:"available"`
	// Source is "console" when the switch was flipped here, "config" when
	// config.yaml still governs.
	Source string `json:"source"`

	SettingsPath  string     `json:"settings_path"`
	SettingsFound bool       `json:"settings_found"`
	SettingsError string     `json:"settings_error"`
	SettingsMTime *time.Time `json:"settings_mtime,omitempty"`
	LoadedAt      time.Time  `json:"loaded_at"`

	ProviderID string              `json:"provider_id"`
	Provider   string              `json:"provider_name"`
	Model      ModelConfig         `json:"model"`
	Env        []EnvRow            `json:"env"`
	Hooks      HooksStatus         `json:"hooks"`
	HookLog    []claudehook.Record `json:"hook_log"`

	// Native is what the deployment runs on when the mode is off. The server
	// fills it in: this package deliberately does not know, because the native
	// target comes from the catalog it is only a contributor to.
	Native *NativeTarget `json:"native,omitempty"`

	// Notes are the mode's limits, stated where an operator will read them
	// rather than only in this file's comments.
	Notes []string `json:"notes"`
}

// NativeTarget is the (provider, model) the mode is *not* using.
type NativeTarget struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// HooksStatus is the hook half of the panel.
type HooksStatus struct {
	// Enabled reports whether handlers actually run right now.
	Enabled bool `json:"enabled"`
	// Reason says why not, when Enabled is false.
	Reason string `json:"reason"`
	// SettingsDisableAll is Claude Code's own disableAllHooks flag.
	SettingsDisableAll bool `json:"settings_disable_all"`
	// SupportedEvents are the events this agent can fire.
	SupportedEvents  []string        `json:"supported_events"`
	ConfiguredEvents int             `json:"configured_events"`
	TotalHandlers    int             `json:"total_handlers"`
	Events           []HookEventView `json:"events"`
	// UnsupportedConfigured names the events the settings file configures and
	// this agent never fires, so "nothing happened" has a visible explanation.
	UnsupportedConfigured []string `json:"unsupported_configured"`
}

// HookEventView is one event's configuration.
type HookEventView struct {
	Event     string          `json:"event"`
	Supported bool            `json:"supported"`
	Groups    []HookGroupView `json:"groups"`
	// Meaning is what the event corresponds to in this agent, so the table says
	// where it fires rather than only naming it.
	Meaning string `json:"meaning"`
}

// HookGroupView is one matcher group.
type HookGroupView struct {
	Matcher  string            `json:"matcher"`
	Handlers []HookHandlerView `json:"handlers"`
}

// HookHandlerView is one configured handler.
type HookHandlerView struct {
	Type    string   `json:"type"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Timeout int      `json:"timeout,omitempty"`
	Async   bool     `json:"async,omitempty"`
	If      string   `json:"if,omitempty"`
	URL     string   `json:"url,omitempty"`
	Server  string   `json:"mcp_server,omitempty"`
	Tool    string   `json:"mcp_tool,omitempty"`
	Summary string   `json:"summary"`
	// Unsupported is why this handler will not run, empty when it will.
	Unsupported string `json:"unsupported,omitempty"`
}

// eventMeaning explains, in the console's words, where each supported event
// fires in this agent. The five sentences are the contract the panel shows, and
// they are kept next to the views rather than in the panel so the Go side and
// the UI cannot describe the same event differently.
var eventMeaning = map[string]string{
	"SessionStart":     "新建对话、继续上一轮、清空对话时",
	"UserPromptSubmit": "用户消息提交前（可阻止这条消息）",
	"PreToolUse":       "每次工具调用前（可阻止该次调用、可改写入参）",
	"PostToolUse":      "每次工具调用后（可改写工具结果、可追加提示）",
	"Stop":             "一轮回答结束时（可让模型继续做）",
}

// unsupportedEventMeaning is what the panel says for an event this agent has no
// trigger point for. It is a sentence rather than an empty string: "配置了但不会
// 运行" and "nothing configured" are different facts, and only one of them is a
// reason to go looking for a bug.
const unsupportedEventMeaning = "本 agent 没有这个事件的触发点：配置会被读取并展示，但不会运行"

// Status assembles the panel payload. hookLimit bounds the run log it carries;
// a non-positive value uses the console's default of 50.
func (s *Service) Status(hookLimit int) Status {
	settings := s.Settings()
	model := settings.Model()
	compat := s.Compat()

	if hookLimit <= 0 {
		hookLimit = 50
	}

	log := s.Recent(hookLimit)
	if log == nil {
		// An empty array, not null: the console renders "no hook has fired yet",
		// and a null would make it distinguish two spellings of the same fact.
		log = []claudehook.Record{}
	}

	out := Status{
		Mode:          store.ClaudeCodeModeNative,
		Compat:        compat,
		Source:        s.Source(),
		SettingsPath:  settings.Path,
		SettingsFound: settings.Found,
		SettingsError: settings.Err,
		LoadedAt:      settings.LoadedAt,
		ProviderID:    ProviderID,
		Provider:      ProviderName,
		Model:         model,
		Env:           settings.EnvRows(),
		Hooks:         s.hooksStatus(settings, compat),
		HookLog:       log,
		Notes: []string{
			"只读取 ~/.claude/settings.json；Claude Code 还会合并项目级 .claude/settings.json，本 agent 未接入。",
			"只派发 SessionStart / UserPromptSubmit / PreToolUse / PostToolUse / Stop 五个事件，其余事件只展示配置。",
			"handler 支持 command / http / mcp_tool；prompt 与 agent 类型（需要 LLM 判定）本 agent 未接入，运行时记为跳过。",
			"兼容模式开启时，所有对话都跑 CC 的模型，覆盖会话里选的模型；关闭后恢复本机模型。",
			"CLAUDE_CODE_SUBAGENT_MODEL 仅展示：本 agent 的子 agent 与主 agent 同模型运行。",
			"工具名按 Claude Code 的叫法翻译后匹配（bash→Bash、read_file→Read、edit_file→Edit、write_file→Write、glob→Grep…），所以 matcher 写成 Bash/Edit|Write 这类名字可以生效；tool_input 仍是本 agent 自己的字段。",
		},
	}
	if compat {
		out.Mode = store.ClaudeCodeModeCompat
	}
	if !settings.MTime.IsZero() {
		at := settings.MTime
		out.SettingsMTime = &at
	}
	// Available is "this switch can be turned on and mean something": the
	// settings file parsed and names a usable endpoint, token and model. A mode
	// with nothing behind it would be a switch that changes the label and
	// nothing else.
	out.Available = model.Ready
	return out
}

// hooksStatus renders the hook half.
func (s *Service) hooksStatus(settings Settings, compat bool) HooksStatus {
	out := HooksStatus{
		Enabled:            s.HooksActive(),
		SettingsDisableAll: settings.DisableAllHooks,
		SupportedEvents:    append([]string(nil), claudehook.SupportedEvents...),
		ConfiguredEvents:   len(settings.Hooks.Groups),
		// Non-nil so the payload always carries an array: the console lists the
		// events this agent cannot fire, and "none configured" and "the field was
		// left out" must not be the same JSON.
		UnsupportedConfigured: []string{},
	}

	// The order of these reasons is the order an operator can act on them: turn
	// the mode on, turn the hooks switch on, fix the file.
	switch {
	case !compat:
		out.Reason = "兼容模式未开启：hook 只在兼容模式下运行"
	case !s.cfg.HooksOr():
		out.Reason = "claudecode.hooks 为 false：只切换模型，不运行 hook"
	case settings.DisableAllHooks:
		out.Reason = "settings.json 里 disableAllHooks 为 true：Claude Code 自己也禁用了全部 hook"
	case len(settings.Hooks.Groups) == 0:
		out.Reason = "settings.json 里没有配置任何 hook"
	case settings.Err != "":
		out.Reason = "settings.json 读取失败：" + settings.Err
	}
	if settings.HooksErr != "" {
		out.Reason = strings.TrimSpace(out.Reason + " " + settings.HooksErr)
	}

	events := make([]string, 0, len(settings.Hooks.Groups))
	for event := range settings.Hooks.Groups {
		events = append(events, event)
	}
	sort.Strings(events)
	// Non-nil for the same reason as the list above: the panel renders the
	// configured hooks as a table, and an absent field is not a table.
	out.Events = []HookEventView{}

	for _, event := range events {
		view := HookEventView{
			Event:     event,
			Supported: claudehook.IsSupported(event),
			Meaning:   eventMeaning[event],
		}
		if !view.Supported {
			// Every event gets a meaning line, including the ones that never
			// fire: the panel's job is to say which of the operator's hooks are
			// dead here, and a blank cell reads like a rendering bug rather than
			// like a fact about this agent.
			view.Meaning = unsupportedEventMeaning
		}
		for _, g := range settings.Hooks.Groups[event] {
			gv := HookGroupView{Matcher: g.Matcher}
			for _, h := range g.Handlers {
				hv := hookHandlerView(h)
				gv.Handlers = append(gv.Handlers, hv)
				out.TotalHandlers++
			}
			view.Groups = append(view.Groups, gv)
		}
		out.Events = append(out.Events, view)
		if !view.Supported {
			out.UnsupportedConfigured = append(out.UnsupportedConfigured, event)
		}
	}
	return out
}

// hookHandlerView renders one handler, including whether this build can run it.
func hookHandlerView(h claudehook.Handler) HookHandlerView {
	view := HookHandlerView{
		Type:    h.Type,
		Command: h.Command,
		Args:    append([]string(nil), h.Args...),
		Timeout: h.Timeout,
		Async:   h.Async || h.AsyncRewake,
		If:      h.If,
		URL:     h.URL,
		Server:  h.Server,
		Tool:    h.Tool,
	}
	view.Summary = handlerSummary(h)
	view.Unsupported = handlerProblem(h)
	return view
}

// handlerSummary is the one-line description in the hook table.
func handlerSummary(h claudehook.Handler) string {
	var target string
	switch h.Type {
	case "command":
		target = h.Command
		if len(h.Args) > 0 {
			target += " " + strings.Join(h.Args, " ")
		}
	case "http":
		target = h.URL
	case "mcp_tool":
		target = strings.TrimPrefix(h.Server+"."+h.Tool, ".")
	default:
		target = h.Type
	}
	parts := []string{target}
	if h.Timeout > 0 {
		parts = append(parts, fmt.Sprintf("%ds", h.Timeout))
	}
	if h.Async || h.AsyncRewake {
		parts = append(parts, "后台")
	}
	return strings.Join(parts, " · ")
}

// handlerProblem says why this build will not run a handler, empty when it
// will. It is the same set of reasons the engine records at runtime, stated
// ahead of time so the table never shows a hook as working that cannot be.
func handlerProblem(h claudehook.Handler) string {
	switch h.Type {
	case "command":
		if strings.TrimSpace(h.Command) == "" {
			return "command 为空，运行时跳过"
		}
	case "http":
		if strings.TrimSpace(h.URL) == "" {
			return "url 为空，运行时跳过"
		}
	case "mcp_tool":
		if strings.TrimSpace(h.Server) == "" || strings.TrimSpace(h.Tool) == "" {
			return "server/tool 为空，运行时跳过"
		}
	case "prompt", "agent":
		return "本 agent 暂未接入 " + h.Type + " 类型（需要 LLM 判定），运行时记为跳过"
	default:
		return "未知的 handler 类型 " + h.Type + "，运行时跳过"
	}
	if strings.TrimSpace(h.If) != "" {
		return "带 if 条件（权限规则语法）本 agent 未求值，运行时跳过"
	}
	return ""
}
