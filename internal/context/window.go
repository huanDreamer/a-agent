package context

import (
	"fmt"
	"sort"
	"strings"
)

// This file answers one question: how big is the window this turn may fill?
//
// It used to be answered by a single configured number (context.max_tokens),
// and a fixed number cannot be right for a deployment that lets a conversation
// pick its own model: 60000 throws away three quarters of a 200k window, and the
// same 60000 overflows a 32k one. The window is a property of the model, so it
// is resolved from the model — with three layers, in this order:
//
//  1. the operator's own answer (context.model_windows / context.max_tokens),
//     because a deployment knows things a table cannot: a gateway that truncates
//     earlier than the vendor does, a model behind a proxy with a smaller limit;
//  2. the built-in table below, which is a best-effort record of what the
//     vendors document — useful, never authoritative;
//  3. context.default_window, for a model nobody here has heard of. Guessing a
//     mainstream window size is the right failure: too large costs a
//     context-limit error the next step could retry, too small costs money on
//     every step of every turn.
//
// What comes out is not the window but the budget for *history*: a fraction of
// it (window_ratio) minus a reserve for the model's own output. Filling a window
// to the brim is how a turn dies at the last step with a provider error instead
// of a compression, and the reserve is the space the answer itself needs.

// Defaults for WindowSpec. They are exported because the config package and the
// console both report them, and a default that exists in two places drifts.
const (
	// DefaultWindowRatio is how much of a model's window the history may fill.
	// The remaining third absorbs the system prompt, the tool schemas, the
	// estimate's own error, and the per-step jitter of a sum that is not exact.
	DefaultWindowRatio = 0.7
	// DefaultReserveOutputTokens is held back for the model's own output. A
	// reasoning model spends this on thinking before it writes anything, and a
	// capped generation is a silent truncation rather than an error.
	DefaultReserveOutputTokens = 8192
	// DefaultModelWindow is assumed for a model with no known window.
	DefaultModelWindow = 131072
	// MinWindowCap is the floor for a derived cap. A window so small that the
	// arithmetic lands under this is a misconfiguration; compressing to a window
	// of a few hundred tokens would break the turn in a way that has nothing to
	// do with the task.
	MinWindowCap = 8192
	// DefaultToolResultMaxChars bounds one tool result as the model sees it.
	//
	// 16k characters is about 4k tokens — a large file, a stack trace, a test
	// log — while a command that dumps its whole output is cut back to that.
	// Tool results are what actually fill a long turn's window, so this is the
	// knob with the most leverage over how often compression fires.
	DefaultToolResultMaxChars = 16000
)

// WindowSpec is the configuration a window is resolved from.
type WindowSpec struct {
	// MaxTokens is an explicit cap on the history. >0 wins over everything else
	// here: an operator who wrote a number meant it. <0 disables compression
	// entirely (the history is sent whole).
	MaxTokens int
	// Ratio is how much of the window the history may fill. 0 uses
	// DefaultWindowRatio.
	Ratio float64
	// Reserve is held back from the window for the model's output. 0 uses
	// DefaultReserveOutputTokens; a negative value reserves nothing, which is
	// what a deployment that measures its own headroom asks for.
	Reserve int
	// Default is the window assumed for an unknown model. 0 uses
	// DefaultModelWindow.
	Default int
	// Overrides maps a model id — or a fragment of one, matched
	// case-insensitively as a substring, longest key first — to a window. It is
	// the operator's answer, so it beats everything else.
	Overrides map[string]int
	// Lookup returns the window recorded for a model by the catalog: what the
	// provider published, or what the model answered when asked. Nil means
	// nothing has been recorded — which is the state of a deployment whose
	// providers publish nothing and whose models have never been asked.
	//
	// It is a function rather than a map so the caller decides how fresh the
	// answer has to be; the caller owns the store and the caching.
	Lookup func(model string) int
}

// Window is a resolved window: what the model can hold, and what the history is
// allowed to fill.
type Window struct {
	// Model is the model the window was resolved for.
	Model string
	// Tokens is the context window attributed to the model.
	Tokens int
	// Cap is the token budget for the in-loop history. 0 means compression is
	// off and the history is sent whole.
	Cap int
	// Source says where Tokens came from: WindowSourceConfig, WindowSourceKnown
	// or WindowSourceDefault. It is reported (a log line, the console) because
	// "why is it compressing every four steps" is answered by it.
	Source string
	// Derivation is the arithmetic behind Cap, for the log line: either
	// "fixed" or "0.7 × 131072 − 8192".
	Derivation string
}

// Cap sources, as reported by Window.Source.
const (
	// WindowSourceConfig is an explicit configuration (a fixed max_tokens, or a
	// model_windows entry).
	WindowSourceConfig = "config"
	// WindowSourceModel is what the model's catalog entry records: the provider's
	// published window, or the model's own answer to being asked. It beats the
	// built-in table because it is about *this* deployment's model — a gateway
	// may route to a smaller window than the vendor's headline number.
	WindowSourceModel = "model"
	// WindowSourceKnown is the built-in table.
	WindowSourceKnown = "known"
	// WindowSourceDefault is the fallback for an unknown model.
	WindowSourceDefault = "default"
	// WindowSourceOff marks a window whose compression is switched off.
	WindowSourceOff = "off"
)

// knownWindow is one row of the built-in table.
//
// Patterns are matched as case-insensitive substrings of the model id, and the
// longest match wins — which is how "gpt-4.1" beats "gpt-4", and why the table
// can carry both a family and its exceptions. The model id is what the provider
// was asked for, so it usually looks like "deepseek/deepseek-v4.1-flash" or
// "claude-sonnet-4-20250514": matching on the bare name rather than on an exact
// id is what makes one row cover the dated and prefixed spellings.
var knownWindows = []struct {
	pattern string
	tokens  int
	// note records where the number comes from, for the maintainer. It is not
	// shown to anyone at runtime, and it is deliberately short: the vendors
	// change these, and a stale table is fixed by editing one line.
	note string
}{
	// Anthropic
	{"claude-2", 100_000, "Anthropic claude-2 (100k)"},
	{"claude", 200_000, "Anthropic claude-3+ (200k)"},
	// OpenAI
	{"gpt-3.5", 16_385, "OpenAI gpt-3.5-turbo (16k)"},
	{"gpt-4.1", 1_047_576, "OpenAI gpt-4.1 (1M)"},
	{"gpt-4o", 128_000, "OpenAI gpt-4o (128k)"},
	{"gpt-4-turbo", 128_000, "OpenAI gpt-4-turbo (128k)"},
	{"gpt-4-32k", 32_768, "OpenAI gpt-4-32k"},
	{"gpt-4", 8_192, "OpenAI gpt-4 (8k)"},
	{"gpt-5", 400_000, "OpenAI gpt-5 (400k)"},
	{"o1", 200_000, "OpenAI o1 (200k)"},
	{"o3", 200_000, "OpenAI o3 (200k)"},
	{"o4", 200_000, "OpenAI o4-mini (200k)"},
	// Google
	{"gemini", 1_048_576, "Google gemini 1.5+ (1M)"},
	{"gemma", 131_072, "Google gemma 3 (128k)"},
	// DeepSeek
	{"deepseek", 131_072, "DeepSeek V3/R1 (128k)"},
	// Alibaba
	{"qwen", 131_072, "Qwen 2.5/3 (128k)"},
	{"qwq", 131_072, "QwQ (128k)"},
	// Moonshot / Kimi
	{"kimi-k2", 262_144, "Moonshot kimi-k2 (256k)"},
	{"kimi", 131_072, "Moonshot kimi (128k)"},
	{"moonshot", 131_072, "Moonshot (128k)"},
	// Zhipu
	{"glm", 131_072, "Zhipu GLM-4 (128k)"},
	{"chatglm", 131_072, "Zhipu ChatGLM (128k)"},
	// Meta
	{"llama-4", 1_048_576, "Meta llama-4 (1M)"},
	{"llama", 131_072, "Meta llama-3 (128k)"},
	// Mistral
	{"codestral", 262_144, "Mistral codestral (256k)"},
	{"mistral-large", 131_072, "Mistral large (128k)"},
	{"mixtral", 32_768, "Mistral mixtral (32k)"},
	{"mistral", 32_768, "Mistral (32k)"},
	// xAI
	{"grok-4", 262_144, "xAI grok-4 (256k)"},
	{"grok", 131_072, "xAI grok (128k)"},
	// Others worth naming rather than guessing
	{"yi-", 200_000, "01.AI yi (200k)"},
	{"doubao", 131_072, "ByteDance doubao (128k)"},
	{"abab", 245_760, "MiniMax abab (240k)"},
	{"minimax", 245_760, "MiniMax (240k)"},
	{"hunyuan", 131_072, "Tencent hunyuan (128k)"},
	{"ernie", 131_072, "Baidu ernie (128k)"},
	{"step-", 131_072, "StepFun step (128k)"},
	{"command-r", 131_072, "Cohere command-r (128k)"},
	{"nova", 300_000, "Amazon nova (300k)"},
	{"sonar", 131_072, "Perplexity sonar (128k)"},
	{"longcat", 131_072, "Meituan longcat (128k)"},
}

// Resolve answers what the model's window is and how much of it the history may
// fill.
func (s WindowSpec) Resolve(model string) Window {
	out := Window{Model: model}

	switch {
	case s.MaxTokens < 0:
		// Compression off, deliberately. The window is still reported, because
		// "off" next to a 200k window reads very differently from "off" next to
		// an 8k one.
		out.Tokens = s.window(model)
		out.Source = WindowSourceOff
		out.Derivation = "compression off"
		return out
	case s.MaxTokens > 0:
		out.Tokens = s.window(model)
		out.Source = WindowSourceConfig
		out.Cap = s.MaxTokens
		out.Derivation = "fixed"
		return out
	}

	tokens, source := s.lookup(model)
	out.Tokens = tokens
	out.Source = source
	out.Cap = s.derivedCap(tokens)
	out.Derivation = s.derivation(tokens, out.Cap)
	return out
}

// WithLookup returns the spec with the catalog lookup attached. It exists so a
// caller can hand the same resolver to several places (the turn condenser, the
// console panel, a session's memory window) without rebuilding the config half
// three times.
func (s WindowSpec) WithLookup(fn func(model string) int) WindowSpec {
	s.Lookup = fn
	return s
}

// window returns just the window size, for the paths that report it without
// deriving a cap from it.
func (s WindowSpec) window(model string) int {
	tokens, _ := s.lookup(model)
	return tokens
}

// lookup resolves the window size and where it came from.
func (s WindowSpec) lookup(model string) (int, string) {
	if n := matchWindow(s.Overrides, model); n > 0 {
		return n, WindowSourceConfig
	}
	if s.Lookup != nil {
		if n := s.Lookup(model); n > 0 {
			return n, WindowSourceModel
		}
	}
	if n := matchKnown(model); n > 0 {
		return n, WindowSourceKnown
	}
	if s.Default > 0 {
		return s.Default, WindowSourceDefault
	}
	return DefaultModelWindow, WindowSourceDefault
}

// derivedCap turns a window into a history budget.
//
// The floor matters more than it looks: ratio × window − reserve is a small
// number for a small window, and a cap under MinWindowCap would make the runner
// compress on nearly every step — the pathology this file exists to remove,
// only faster.
func (s WindowSpec) derivedCap(tokens int) int {
	ratio := s.Ratio
	if ratio <= 0 {
		ratio = DefaultWindowRatio
	}
	if ratio > 1 {
		// A ratio above 1 is a request to overflow the window. Clamped rather
		// than refused: it is a knob, and the clamp is the behaviour the
		// operator was reaching for.
		ratio = 1
	}
	reserve := s.reserve()
	cap := int(float64(tokens)*ratio) - reserve
	if cap > tokens {
		// A cap above the window is not a bound. It is reachable with a reserve
		// small enough and a ratio close to 1, and it would turn "compress before
		// the window overflows" into "compress when the provider already refused".
		cap = tokens
	}
	if cap < MinWindowCap {
		if 2*MinWindowCap <= tokens {
			// The floor is affordable: the window can hold the floor plus a
			// prompt and an answer.
			return MinWindowCap
		}
		// The window is too small for the floor. Half of it is the honest answer:
		// the alternative — returning the floor — produced a cap LARGER than the
		// model's window, so the condenser never fired and the history was resent
		// whole, which is the failure this whole file exists to remove.
		cap = tokens / 2
	}
	if cap < 1 {
		cap = 1
	}
	return cap
}

// derivation renders the arithmetic for the log line, so a surprising cap can be
// read back without recomputing it.
func (s WindowSpec) derivation(tokens, cap int) string {
	ratio := s.Ratio
	if ratio <= 0 {
		ratio = DefaultWindowRatio
	}
	if ratio > 1 {
		ratio = 1
	}
	reserve := s.reserve()
	if cap == MinWindowCap && int(float64(tokens)*ratio)-reserve < MinWindowCap {
		return fmt.Sprintf("floored at %d", MinWindowCap)
	}
	return fmt.Sprintf("%.2g × %d − %d", ratio, tokens, reserve)
}

// reserve is the output headroom the spec asks for: the configured value, the
// default when unset, or nothing when the operator explicitly asked for nothing.
func (s WindowSpec) reserve() int {
	if s.Reserve == 0 {
		return DefaultReserveOutputTokens
	}
	if s.Reserve < 0 {
		return 0
	}
	return s.Reserve
}

// matchWindow matches a model id against an operator-supplied table. The longest
// key that appears in the model id wins, so "deepseek-v4" can override
// "deepseek" without an exact id being required.
func matchWindow(overrides map[string]int, model string) int {
	if len(overrides) == 0 || strings.TrimSpace(model) == "" {
		return 0
	}
	id := strings.ToLower(model)
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		key := strings.ToLower(strings.TrimSpace(k))
		if key == "" {
			continue
		}
		if strings.Contains(id, key) && overrides[k] > 0 {
			return overrides[k]
		}
	}
	return 0
}

// matchKnown matches the built-in table, longest pattern first.
func matchKnown(model string) int {
	id := strings.ToLower(strings.TrimSpace(model))
	if id == "" {
		return 0
	}
	best, bestLen := 0, 0
	for _, row := range knownWindows {
		if !strings.Contains(id, row.pattern) {
			continue
		}
		if len(row.pattern) > bestLen {
			best, bestLen = row.tokens, len(row.pattern)
		}
	}
	return best
}

// KnownWindows reports the built-in table, sorted by pattern, for the console
// and the docs. It returns a copy: the table is package state and a caller that
// mutated it would change every later resolution.
func KnownWindows() []struct {
	Pattern string
	Tokens  int
	Note    string
} {
	out := make([]struct {
		Pattern string
		Tokens  int
		Note    string
	}, 0, len(knownWindows))
	for _, row := range knownWindows {
		out = append(out, struct {
			Pattern string
			Tokens  int
			Note    string
		}{row.pattern, row.tokens, row.note})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	return out
}
