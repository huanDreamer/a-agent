package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/store"
)

// Asking a model what it can do.
//
// The catalog knows a model's capabilities from three sources, and this file is
// the third: what a provider publishes about its models (supported_endpoints and
// whatever else it chooses to say), what the model's *name* implies, and what the
// model answers when asked. The name is the weakest — "gpt-4o" in a model id is a
// family, not a promise — and for a gateway the operator added themselves there
// may be nothing else at all.
//
// The answer is a hint and is treated as one. It is recorded with its source, it
// can only ever be as good as the model's self-knowledge (which is imperfect: a
// model asked about its own modality support will sometimes describe its family
// rather than itself), an operator's own toggle always wins, and the prompt below
// asks for "null" more often than it asks for a guess, because an unrecorded
// capability costs a wasted file read while a wrong one costs a broken turn.

// DefaultFactsTimeout bounds one probe.
const DefaultFactsTimeout = 25 * time.Second

// factsSystemPrompt makes the shape of the answer non-negotiable: one JSON
// object, no prose. Every extra sentence a model adds is a chance to break the
// parse, and the answer is four booleans and a number — nothing about it needs
// prose.
const factsSystemPrompt = "你是一个模型元数据接口：只输出一个 JSON 对象，不要任何解释、前后缀或 Markdown 代码块。" +
	"不知道的字段写 null，绝对不要猜测。"

// factsUserPrompt is the question. It is written to make three things as hard as
// possible to get wrong:
//
//  1. *which* model is being described. A model asked "do you support vision?"
//     tends to answer on behalf of its family ("Claude supports vision"), which
//     is how a text-only sibling ends up marked as seeing images. Hence the
//     emphasis on 你自己, and on not extrapolating from the family.
//  2. how to say "I do not know". Without an explicit escape hatch every answer
//     becomes a confident boolean, and a wrong `false` is as bad as a wrong
//     `true` — it would take away a capability the model actually has. `null` is
//     named as the preferred answer when unsure.
//  3. what "support" means, per field. "multimodal" and "audio" mean different
//     things to different vendors, so each field says what it is in terms of
//     input and output.
const factsUserPrompt = `请如实回答**你自己**（正在生成这段回答的这个模型，而不是你所属的产品系列、同系列的其他模型，也不是你的 API 网关）的以下事实。只输出一个 JSON 对象：

{
  "context_window": <整数：你的输入上下文窗口有多少 token。必须给出你最好的估计，例如 128000；只有完全无法估计时才写 null>,
  "window_confidence": <"high" 或 "low"：你对上面这个数字的信心>,
  "text": <true/false：能否进行文本对话>,
  "vision": <true/false：能否直接看图，即图像作为输入>,
  "tools": <true/false：是否支持函数调用 / 工具调用（function calling）>,
  "image_gen": <true/false：能否根据文字生成图片>,
  "asr": <true/false：能否把语音转写成文字，即音频作为输入>,
  "tts": <true/false：能否把文字合成为语音，即输出音频>,
  "unsure": <数组：上面你无法确定的字段名，例如 ["vision","tools"]>
}

规则：
- 只描述你自己这一份权重的能力。不要因为"同系列有别的模型支持"就写 true。
- context_window 必须是数字（不要单位、不要千分位）。估计可以不准，但要给出来，信心写进 window_confidence。
- 能力字段：确定支持写 true，确定不支持写 false，不确定写 null，并把字段名放进 unsure。
- 不要输出 JSON 之外的任何字符。`

// factsAliases maps the field names a model may use to the capability they mean.
// Models answer with the names from the prompt most of the time and with their
// own vocabulary the rest of it — "stt" for transcription, "function_calling"
// for tools — and refusing a correct answer because of a synonym would throw away
// the only source available for a model nobody publishes anything about.
var factsAliases = map[string]store.Capability{
	"text":             store.CapChat,
	"chat":             store.CapChat,
	"text_chat":        store.CapChat,
	"vision":           store.CapVision,
	"image_input":      store.CapVision,
	"multimodal":       store.CapVision,
	"image_gen":        store.CapImageGen,
	"image_generation": store.CapImageGen,
	"text_to_image":    store.CapImageGen,
	"tools":            store.CapTools,
	"tool_calling":     store.CapTools,
	"function_calling": store.CapTools,
	"asr":              store.CapAudioTranscribe,
	"stt":              store.CapAudioTranscribe,
	"transcribe":       store.CapAudioTranscribe,
	"audio_transcribe": store.CapAudioTranscribe,
	"speech_to_text":   store.CapAudioTranscribe,
	"audio_input":      store.CapAudioTranscribe,
	"tts":              store.CapAudioSpeech,
	"audio_speech":     store.CapAudioSpeech,
	"text_to_speech":   store.CapAudioSpeech,
	"speech":           store.CapAudioSpeech,
	"audio_output":     store.CapAudioSpeech,
	"embedding":        store.CapEmbedding,
	"embeddings":       store.CapEmbedding,
}

// factsWindowKeys are the field names that carry the context window.
var factsWindowKeys = []string{
	"context_window", "context_length", "max_context_tokens", "max_input_tokens",
	"window", "context_tokens", "max_tokens",
}

// ModelFacts is what a model said about itself.
type ModelFacts struct {
	// ContextWindow is the window it claimed, or 0 when it did not say.
	ContextWindow int
	// Capabilities holds only the capabilities the model answered *definitely*
	// about: a key present with true or false is an answer, a missing key is
	// "unknown" and must not be read as false. That distinction is the whole
	// point of the struct: applying an unknown as false would silently strip a
	// model of something it can do.
	Capabilities map[store.Capability]bool
	// Answered reports whether the reply was a parseable JSON object. It is what
	// decides whether "checked" is recorded: a probe that failed to produce an
	// answer should be retried, one that produced "I do not know" should not.
	Answered bool
	// windowFound records that a window was extracted, whatever its source, so
	// the caller can tell "nothing at all came back" from "the answer was prose".
	windowFound bool

	// WindowConfidence is how sure the model said it was about ContextWindow:
	// "high", "low", or "" when it did not say. It is a self-assessment and is
	// reported rather than acted on — the number is bounded by the plausible
	// band either way — but a low-confidence claim next to a high-confidence one
	// is worth knowing when a window looks wrong.
	WindowConfidence string
	// Unsure lists the fields the model named as ones it could not answer. It
	// carries no information the missing capabilities do not already carry; it
	// is kept for the log line, where "the model said it does not know" and "the
	// model ignored the question" are different stories.
	Unsure []string
}

// Fact returns the model's answer for one capability: true, false, or unknown
// (the second result is false when nothing was answered).
func (f ModelFacts) Fact(c store.Capability) (bool, bool) {
	v, ok := f.Capabilities[c]
	return v, ok
}

// AskModelFacts asks a chat model what it is.
//
// It asks everything in one call — the window and every capability — because the
// cost is one round trip either way, and a model that will answer one question
// about itself will answer five.
func AskModelFacts(ctx context.Context, cm model.BaseChatModel, timeout time.Duration) (ModelFacts, error) {
	if cm == nil {
		return ModelFacts{}, errors.New("llm: ask model facts: no model")
	}
	if timeout <= 0 {
		timeout = DefaultFactsTimeout
	}
	askCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := cm.Generate(askCtx, []*schema.Message{
		{Role: schema.System, Content: factsSystemPrompt},
		{Role: schema.User, Content: factsUserPrompt},
	})
	if err != nil {
		return ModelFacts{}, fmt.Errorf("llm: ask model facts: %w", err)
	}
	if out == nil {
		return ModelFacts{}, errors.New("llm: ask model facts: empty response")
	}
	facts := ParseModelFacts(out.Content)
	if !facts.Answered && !facts.windowFound {
		// A reasoning model can spend its whole budget thinking and return an
		// empty answer, or leave the object in the reasoning trace instead of the
		// content. Both are worth reading.
		if fromReasoning := ParseModelFacts(out.ReasoningContent); fromReasoning.Answered || fromReasoning.windowFound {
			facts = fromReasoning
		}
	}
	return facts, nil
}

// jsonObject finds the first JSON object in a reply, tolerating the shapes models
// actually produce: a bare object, one inside a ```json fence, or one with a
// sentence of preamble in front of it.
func jsonObject(reply string) (string, bool) {
	start := strings.IndexByte(reply, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for i, r := range reply[start:] {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inString = !inString
		case inString:
		case r == '{':
			depth++
		case r == '}':
			depth--
			if depth == 0 {
				return reply[start : start+i+1], true
			}
		}
	}
	return "", false
}

// ParseModelFacts reads a reply into facts. It never guesses: a field that is
// absent, null, or unreadable is simply not part of the answer.
func ParseModelFacts(reply string) ModelFacts {
	out := ModelFacts{Capabilities: map[store.Capability]bool{}}
	// The window is worth extracting even from a reply that ignored the format:
	// a model that answers "我的窗口是 200000 token" in prose has still answered,
	// and the numeric scan is the same one the window-only probe used.
	if window := ParseWindowAnswer(reply); window > 0 {
		out.ContextWindow = window
		out.windowFound = true
	}

	raw, ok := jsonObject(reply)
	if !ok {
		return out
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return out
	}
	out.Answered = true

	for key, value := range fields {
		name := strings.ToLower(strings.TrimSpace(key))
		if cap, isCap := factsAliases[name]; isCap {
			if v, definite := truthy(value); definite {
				out.Capabilities[cap] = v
			}
			continue
		}
		if !containsString(factsWindowKeys, name) {
			continue
		}
		if window := windowFromValue(value); window > 0 {
			out.ContextWindow = window
			out.windowFound = true
		}
	}
	if raw, ok := fields["window_confidence"].(string); ok {
		out.WindowConfidence = strings.ToLower(strings.TrimSpace(raw))
	}
	if list, ok := fields["unsure"].([]any); ok {
		for _, item := range list {
			if name, isStr := item.(string); isStr && strings.TrimSpace(name) != "" {
				out.Unsure = append(out.Unsure, strings.TrimSpace(name))
			}
		}
	}
	sort.Strings(out.Unsure)
	return out
}

// windowFromValue reads a context window out of a JSON value, which may be a
// number, a numeric string, or a string with a suffix.
func windowFromValue(v any) int {
	switch t := v.(type) {
	case float64:
		n := int(t)
		if n >= MinPlausibleWindow && n <= MaxPlausibleWindow {
			return n
		}
	case string:
		return ParseWindowAnswer(t)
	case json.Number:
		if n, err := t.Int64(); err == nil && n >= MinPlausibleWindow && n <= MaxPlausibleWindow {
			return int(n)
		}
	}
	return 0
}

// truthy reads a tri-state answer: (value, definite). A null, a missing field, or
// anything unrecognised is not an answer — a model that wrote "maybe" has not
// told us anything, and recording it as false would be inventing a fact.
func truthy(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case float64:
		// Some models answer 1/0.
		if t == 1 {
			return true, true
		}
		if t == 0 {
			return false, true
		}
	case string:
		return parseBoolWord(t)
	}
	return false, false
}

var boolWords = map[string]bool{
	"true": true, "yes": true, "y": true, "是": true, "支持": true, "可以": true, "能": true, "1": true,
	"false": false, "no": false, "n": false, "否": false, "不支持": false, "不可以": false, "不能": false, "0": false,
}

func parseBoolWord(s string) (bool, bool) {
	word := strings.ToLower(strings.TrimSpace(s))
	word = strings.Trim(word, `"'.。，,`)
	if v, ok := boolWords[word]; ok {
		return v, true
	}
	return false, false
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// CapabilitiesFromEndpoints maps a provider's supported_endpoints to the
// capabilities they imply.
//
// It only ever *adds*: a listing that mentions /chat/completions proves the model
// can chat, while a listing that omits /audio/speech does not prove it cannot
// (the endpoint may simply not be enumerated). Absence is not evidence, which is
// the same reason a probe's "unknown" is not recorded as false.
func CapabilitiesFromEndpoints(endpoints []string) store.Capabilities {
	var out store.Capabilities
	seen := map[store.Capability]bool{}
	add := func(c store.Capability) {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, raw := range endpoints {
		e := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case strings.Contains(e, "chat/completions"), strings.Contains(e, "messages"),
			strings.Contains(e, "responses"), strings.Contains(e, "completions"):
			add(store.CapChat)
		case strings.Contains(e, "images"), strings.Contains(e, "image"):
			add(store.CapImageGen)
		case strings.Contains(e, "audio/transcriptions"), strings.Contains(e, "transcription"):
			add(store.CapAudioTranscribe)
		case strings.Contains(e, "audio/speech"), strings.Contains(e, "audio/generations"):
			add(store.CapAudioSpeech)
		case strings.Contains(e, "embeddings"):
			add(store.CapEmbedding)
		}
	}
	return out
}
