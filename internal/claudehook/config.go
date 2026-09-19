// Package claudehook implements a Claude Code compatible hook runtime: it parses
// the "hooks" object of a Claude Code settings file and executes the configured
// handlers according to the documented hook protocol.
//
// The authoritative reference for every rule implemented here is
// ~/.claude/hooks/HOOKS-REFERENCE.md (Claude Code v2.1.278). Section numbers in
// the comments below (§N) point at that document. Where the document itself says
// 文档未说明 ("not specified by the documentation"), this package picks the
// conservative reading — the one that cannot make a caller act on a fact the
// protocol never promised — and says so in a comment.
//
// Scope of this build (agreed, deliberately smaller than the protocol):
//
//   - All event names are parsed and displayed, but only the five events in
//     SupportedEvents are dispatched; see IsSupported.
//   - Handler types "command" and "http" are fully implemented, "mcp_tool" is
//     implemented against an injected MCPCaller, and "prompt"/"agent" are
//     reported as not implemented in this build (recorded per handler, never
//     silently ignored, and never an LLM call).
//   - "if" is not evaluated: permission-rule matching is a separate subsystem.
//     A handler carrying "if" is skipped with that reason instead of being run
//     as if it had matched (§3).
package claudehook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Handler types understood by the protocol (§3). The type string is the
// discriminator in the parsed config; an unknown type is carried through ParseConfig
// and skipped at dispatch time, so a config written for a newer Claude Code
// still parses and the operator still sees the rest of the hooks.
const (
	HandlerCommand = "command"
	HandlerHTTP    = "http"
	HandlerMCPTool = "mcp_tool"
	HandlerPrompt  = "prompt"
	HandlerAgent   = "agent"
)

// wireAlias is the set of handler fields whose real Claude Code spelling differs
// from this struct's JSON tag: the tags follow the frozen public API (snake_case)
// while the wire format is camelCase, as §3 and the official reference document it
// ("asyncRewake", "statusMessage", "allowedEnvVars").
//
// UnmarshalJSON accepts both spellings. Accepting neither would be silent and
// costly: an HTTP handler whose allowedEnvVars did not parse would resolve every
// variable in its header templates to the empty string — the failure mode §3 warns
// about — and a lost asyncRewake would make a fire-and-forget hook block the turn.
var wireAlias = struct {
	AsyncRewake    string
	StatusMessage  string
	AllowedEnvVars string
}{
	AsyncRewake:    "asyncRewake",
	StatusMessage:  "statusMessage",
	AllowedEnvVars: "allowedEnvVars",
}

// SupportedEvents are the events this build dispatches. The list is exposed as
// data so the console can render "configured but not dispatched here" without
// hard-coding the names in the UI layer.
var SupportedEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}

// supportedEventSet is the lookup form of SupportedEvents. Derived from the
// slice above so the two can never disagree.
var supportedEventSet = func() map[string]bool {
	m := make(map[string]bool, len(SupportedEvents))
	for _, e := range SupportedEvents {
		m[e] = true
	}
	return m
}()

// IsSupported reports whether this build dispatches the event.
func IsSupported(event string) bool { return supportedEventSet[event] }

// Handler is one configured hook handler (the 3rd level of the config).
//
// The JSON tags are the wire names of the Claude Code config, not this build's
// own spelling, so a Handler round-trips through a settings file unchanged.
type Handler struct {
	Type           string            `json:"type"` // "command"|"http"|"mcp_tool"|"prompt"|"agent"
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Timeout        int               `json:"timeout,omitempty"` // seconds
	Async          bool              `json:"async,omitempty"`
	AsyncRewake    bool              `json:"async_rewake,omitempty"`
	Shell          string            `json:"shell,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	AllowedEnvVars []string          `json:"allowed_env_vars,omitempty"`
	Server         string            `json:"server,omitempty"`
	Tool           string            `json:"tool,omitempty"`
	Input          map[string]string `json:"input,omitempty"`
	Prompt         string            `json:"prompt,omitempty"`
	Model          string            `json:"model,omitempty"`
	If             string            `json:"if,omitempty"`
	StatusMessage  string            `json:"status_message,omitempty"`
	// Once is parsed and preserved but has no effect: §1.1 says it is honoured
	// only in skill frontmatter, and this build has no skill frontmatter layer.
	// Running the handler more often than Claude Code would is the harmless
	// direction (the alternative — inventing an unregistration rule — could
	// silently disable a hook for the rest of the session).
	Once bool `json:"once,omitempty"`
	// Matcher is the owning group's matcher, filled in by ParseConfig. It is
	// denormalised onto the handler because that is what a dispatch loop needs:
	// the matcher is evaluated per handler, and a handler that reached execution
	// must be able to report which matcher selected it.
	Matcher string `json:"matcher,omitempty"`
}

// hookHandlerWire mirrors Handler for custom unmarshalling only. It exists so
// UnmarshalJSON can accept the "asyncRewake" alias without a second copy of the
// field list that could drift from Handler's.
type hookHandlerWire Handler

// UnmarshalJSON accepts both the documented camelCase keys and the snake_case
// spellings of this struct's JSON tags, then reports a type mismatch for either as
// a parse error (so ParseConfig can join it with the other problems instead of
// silently dropping the field).
//
// The snake_case acceptance is not nostalgia: the frozen public API of this
// package uses those tags, so a caller that round-trips a Handler through JSON
// must get back what it wrote. The camelCase acceptance is what makes a real
// settings.json work.
func (h *Handler) UnmarshalJSON(data []byte) error {
	wire := (*hookHandlerWire)(h)
	if err := json.Unmarshal(data, wire); err != nil {
		return err
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	alias := struct {
		AsyncRewake    *bool     `json:"asyncRewake"`
		StatusMessage  *string   `json:"statusMessage"`
		AllowedEnvVars *[]string `json:"allowedEnvVars"`
	}{}
	if err := decodeAlias(raw, wireAlias.AsyncRewake, &alias.AsyncRewake); err != nil {
		return err
	}
	if err := decodeAlias(raw, wireAlias.StatusMessage, &alias.StatusMessage); err != nil {
		return err
	}
	if err := decodeAlias(raw, wireAlias.AllowedEnvVars, &alias.AllowedEnvVars); err != nil {
		return err
	}
	if alias.AsyncRewake != nil {
		h.AsyncRewake = *alias.AsyncRewake
	}
	if alias.StatusMessage != nil {
		h.StatusMessage = *alias.StatusMessage
	}
	if alias.AllowedEnvVars != nil {
		h.AllowedEnvVars = *alias.AllowedEnvVars
	}
	return nil
}

// Group is one matcher group (the 2nd level).
type Group struct {
	Matcher  string    `json:"matcher"`
	Handlers []Handler `json:"handlers"`
}

// Config is a parsed "hooks" object.
type Config struct {
	Groups     map[string][]Group `json:"groups"`      // event name -> matcher groups
	EventOrder []string           `json:"event_order"` // stable, sorted
	DisableAll bool               `json:"disable_all"`
}

// HookEvents mirrors the Config slices.
func (c Config) HookEvents() []HookEvent {
	events := make([]HookEvent, 0, len(c.EventOrder))
	for _, name := range c.EventOrder {
		events = append(events, HookEvent{
			Name:   name,
			Groups: c.Groups[name],
		})
	}
	return events
}

// HookEvent is one event with its matcher groups in configured order.
type HookEvent struct {
	Name   string
	Groups []Group
}

// ParseConfig parses the value of the top-level "hooks" key of a Claude Code
// settings file.
//
// A nil/empty raw value yields an empty Config and no error. Malformed groups and
// handlers are collected into one joined error, and the entries that did parse
// are still returned: a single broken handler must not hide the rest of the
// configuration, which is exactly how the operator-facing /hooks menu behaves
// (§1.3 — it lists what it could read rather than refusing to display anything).
func ParseConfig(raw json.RawMessage) (Config, error) {
	cfg := Config{Groups: map[string][]Group{}}
	if isEmptyRaw(raw) {
		return cfg, nil
	}

	var flat struct {
		DisableAllHooks *bool                      `json:"disableAllHooks"`
		Events          map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &flat); err != nil {
		return cfg, fmt.Errorf("解析 hooks 配置：%w", err)
	}
	if flat.DisableAllHooks != nil {
		cfg.DisableAll = *flat.DisableAllHooks
	}

	// Sorting the event names once gives the whole build one stable order: the
	// config's EventOrder, the order dispatch evaluates groups in, and the order
	// skip/failure lines appear in a Verdict. A map iteration order here would
	// make those three outputs differ run to run.
	names := make([]string, 0, len(flat.Events))
	for name := range flat.Events {
		names = append(names, name)
	}
	sort.Strings(names)
	cfg.EventOrder = names

	var problems []error
	for _, name := range names {
		groups, errs := parseEventGroups(name, flat.Events[name])
		problems = append(problems, errs...)
		if len(groups) == 0 {
			continue
		}
		cfg.Groups[name] = groups
	}
	if cfg.Groups == nil {
		cfg.Groups = map[string][]Group{}
	}
	return cfg, errors.Join(problems...)
}

// parseEventGroups decodes one event's value and fills in the owning group's
// matcher on every handler it contains.
//
// The value must be an array of matcher groups (§1). The other JSON shapes a
// hand-edited settings file might contain (a single group object, a bare handler
// array) are rejected rather than guessed at: a config this parser accepted but
// real Claude Code refuses would be the worst possible outcome, because the
// operator would ship hooks that only appear to work here.
func parseEventGroups(event string, raw json.RawMessage) ([]Group, []error) {
	if isEmptyRaw(raw) || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var rawGroups []json.RawMessage
	if err := json.Unmarshal(raw, &rawGroups); err != nil {
		return nil, []error{fmt.Errorf("事件 %s 的值必须是 matcher 组数组：%w", event, err)}
	}

	var problems []error
	groups := make([]Group, 0, len(rawGroups))
	for _, rawGroup := range rawGroups {
		group, err := parseGroup(event, rawGroup)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if len(group.Handlers) == 0 {
			// A group with no handlers selects nothing and runs nothing, so it is
			// carried through (the /hooks menu would show it) but is not reported
			// as a problem.
			continue
		}
		groups = append(groups, group)
	}
	return groups, problems
}

// parseGroup decodes one matcher group. The owning matcher is written onto every
// handler it contains, because the runtime evaluates matchers per handler.
func parseGroup(event string, raw json.RawMessage) (Group, error) {
	var wire struct {
		Matcher json.RawMessage   `json:"matcher"`
		Hooks   []json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Group{}, fmt.Errorf("解析事件 %s 的 matcher 组：%w", event, err)
	}

	group := Group{}
	if matcher, ok, err := decodeOptionalString(wire.Matcher); err != nil {
		return Group{}, fmt.Errorf("事件 %s 的 matcher：%w", event, err)
	} else if ok {
		group.Matcher = matcher
	}

	for _, rawHandler := range wire.Hooks {
		handler, err := decodeHandler(event, group.Matcher, rawHandler)
		if err != nil {
			return Group{}, err
		}
		// A handler with no type is not a handler: the type is what selects the
		// transport, so an entry missing it cannot be classified at all. Rather
		// than fabricate one, report it and move on to the next handler.
		if handler.Type == "" {
			return Group{}, fmt.Errorf("事件 %s 的 handler 缺少 type 字段", event)
		}
		group.Handlers = append(group.Handlers, handler)
	}
	return group, nil
}

// handlerCommonWire is the subset of handler fields this package inspects with
// its own type checking. Every field is a RawMessage (or a pointer) so a field
// with the wrong JSON type is reported as a parse problem instead of aborting the
// whole config decode.
type handlerCommonWire struct {
	Type      json.RawMessage `json:"type"`
	Timeout   json.RawMessage `json:"timeout"`
	Async     json.RawMessage `json:"async"`
	Condition json.RawMessage `json:"if"`
}

// decodeHandler parses one handler. Unknown handler types are NOT errors here:
// they are config that a newer Claude Code understands, and dropping the entry at
// parse time would hide it from the operator. Dispatch is what refuses to run
// them, with a reason that names the type (§3).
func decodeHandler(event, matcher string, raw json.RawMessage) (Handler, error) {
	var handler Handler
	if err := json.Unmarshal(raw, &handler); err != nil {
		return Handler{}, fmt.Errorf("解析事件 %s 的 handler：%w", event, err)
	}
	handler.Matcher = matcher

	var wire handlerCommonWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Handler{}, fmt.Errorf("解析事件 %s 的 handler：%w", event, err)
	}

	typ, _, err := decodeOptionalString(wire.Type)
	if err != nil {
		return Handler{}, fmt.Errorf("事件 %s 的 handler type：%w", event, err)
	}
	handler.Type = typ

	// timeout is documented in seconds (§3). A negative value is not a shorter
	// timeout, it is a typo, and both readings of it ("no time at all" / "wrap
	// around to a huge budget") would behave nothing like the config the operator
	// meant to write — so refuse it.
	if len(bytes.TrimSpace(wire.Timeout)) > 0 {
		var timeout int
		if err := json.Unmarshal(wire.Timeout, &timeout); err != nil {
			return Handler{}, fmt.Errorf("事件 %s 的 handler timeout 必须是秒数：%w", event, err)
		}
		if timeout < 0 {
			return Handler{}, fmt.Errorf("事件 %s 的 handler timeout 不能为负数：%d", event, timeout)
		}
		handler.Timeout = timeout
	}
	if len(bytes.TrimSpace(wire.Async)) > 0 {
		if err := json.Unmarshal(wire.Async, &handler.Async); err != nil {
			return Handler{}, fmt.Errorf("事件 %s 的 handler async 必须是布尔值：%w", event, err)
		}
	}

	// "if" is a permission-rule filter (§3). An empty string is the same as
	// absent; only a non-empty value makes the handler unrunnable in this build.
	condition, present, err := decodeOptionalString(wire.Condition)
	if err != nil {
		return Handler{}, fmt.Errorf("事件 %s 的 handler if：%w", event, err)
	}
	if present {
		handler.If = condition
	}
	return handler, nil
}

// decodeAlias decodes one camelCase alias key into dst, reporting a wrong-typed
// value as an error so ParseConfig can join it with the other problems. A missing
// key leaves dst untouched, which is what keeps the snake_case tag's value.
func decodeAlias(raw map[string]json.RawMessage, key string, dst any) error {
	value, ok := raw[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(value, dst); err != nil {
		return fmt.Errorf("字段 %s：%w", key, err)
	}
	return nil
}

// decodeOptionalString reports a present-but-not-a-string value as an error: the
// difference between "absent" and "written wrong" is exactly what the operator
// needs in the /hooks panel, and the rest of the config must survive it.
func decodeOptionalString(raw json.RawMessage) (string, bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, fmt.Errorf("必须是字符串：%w", err)
	}
	return s, true, nil
}

// isEmptyRaw reports whether raw carries no value at all, which ParseConfig treats
// as "this settings file has no hooks section" rather than as malformed input.
func isEmptyRaw(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || string(trimmed) == "null"
}
