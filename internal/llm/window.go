package llm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Asking a model what its own context window is.
//
// It is a fallback, not the first choice: a provider that publishes the number
// in its /models listing has answered exactly (see ModelInfo.ContextWindow), and
// that answer is what the provider will enforce. But most providers publish
// nothing — deepseek returns ids and nothing else — and the alternative to
// asking is a built-in table that ages badly and cannot know about a model
// released yesterday, let alone a private deployment behind a gateway.
//
// The answer is a hint and is treated as one: it is clamped to a plausible band,
// its source is recorded next to it, and an operator override always wins
// (context.model_windows). A model that confidently states a wrong window
// therefore costs a suboptimal history budget, not a broken turn.

// DefaultWindowAskTimeout bounds one probe. It is generous because a cold model
// on a busy gateway can take seconds to answer anything, and short enough that a
// pass over several models still finishes: a refresh budget of a dozen models
// with a few in flight has to fit inside the pass timeout.
const DefaultWindowAskTimeout = 20 * time.Second

// Window bands. An answer outside them is discarded: 1k is below any model this
// agent could hold a conversation with, and 100M is beyond the largest window
// announced by anyone (1M-10M), so both mean the model answered with something
// that is not a window — a parameter count, a date, a refusal.
const (
	MinPlausibleWindow = 1_024
	MaxPlausibleWindow = 100_000_000
)

// windowAskPrompt is the question. It is shaped for a one-number answer:
// the system message forbids everything else, and the user message names the
// unit and the fallback, because a model that does not know must be able to say
// so rather than invent a number.
const windowAskPrompt = "你的上下文窗口（context window，输入 token 上限）是多少？" +
	"只回答一个整数（token 数），不要单位、不要千分位、不要解释。" +
	"如果你不确定，只回答 0。"

const windowAskSystem = "你是一个模型元数据接口。用户问你的运行参数时，只回答被要求的那个数字，不要任何其他文字。"

// AskContextWindow asks a chat model how large its context window is, as a
// single-number question.
//
// It is the *fallback* to AskModelFacts, and the fallback exists because of what
// the two prompts measurably do: asked for a JSON object with a null escape
// hatch, models hedge and answer `null` for the window; asked for one integer
// and nothing else, the same models commit to a number. A real pair answered
// `null` to the combined question and 128000/200000 to this one, so a model that
// hedged is asked a second time in the shape it finds easier — one extra call,
// only for the models that need it.
//
// It returns 0 and no error when the model does not know (it answered 0), and an
// error only when the call itself failed — the two are different facts and the
// caller records them differently.
func AskContextWindow(ctx context.Context, cm model.BaseChatModel, timeout time.Duration) (int, error) {
	if cm == nil {
		return 0, errors.New("llm: ask context window: no model")
	}
	if timeout <= 0 {
		timeout = DefaultWindowAskTimeout
	}
	askCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := cm.Generate(askCtx, []*schema.Message{
		{Role: schema.System, Content: windowAskSystem},
		{Role: schema.User, Content: windowAskPrompt},
	})
	if err != nil {
		return 0, fmt.Errorf("llm: ask context window: %w", err)
	}
	if out == nil {
		return 0, errors.New("llm: ask context window: empty response")
	}
	return ParseWindowAnswer(out.Content), nil
}

// windowNumber finds the first integer in an answer.
//
// It is not a JSON parse, and that is deliberate: models answer this question in
// prose ("我的上下文窗口是 128000 token"), with units ("128K", "1M"), with
// thousands separators, or wrapped in markdown. Every one of those is a correct
// answer to a badly-behaved question, and refusing them would throw away the
// only source available for a provider that publishes nothing.
var windowNumber = regexp.MustCompile(`\d[\d,_ ]*`)

// windowSuffix recognises the magnitude suffixes a model may attach: 128K means
// 128000, 1M means 1000000.
var windowSuffix = regexp.MustCompile(`(?i)^\s*(k|m|万|亿)`)

// looksLikeADate reports whether an integer could be a year or a YYYYMMDD date
// rather than a token count. Both are common when a model answers a question it
// did not understand, and both would otherwise pass a plausibility band whose
// floor is 1024.
func looksLikeADate(v int) bool {
	if v >= 1900 && v <= 2100 {
		return true
	}
	if v < 19_000_101 || v > 21_001_231 {
		return false
	}
	year, month, day := v/10_000, (v/100)%100, v%100
	return year >= 1900 && year <= 2100 && month >= 1 && month <= 12 && day >= 1 && day <= 31
}

// ParseWindowAnswer extracts a context window from a model's reply. It returns 0
// when the reply does not contain a plausible window.
func ParseWindowAnswer(reply string) int {
	text := strings.TrimSpace(reply)
	if text == "" {
		return 0
	}
	// An explicit "I do not know" is an answer: 0 is what the prompt asks for,
	// and so is a refusal in words.
	if strings.TrimSpace(text) == "0" {
		return 0
	}

	for _, match := range windowNumber.FindAllStringIndex(text, 8) {
		raw := text[match[0]:match[1]]
		digits := strings.Map(func(r rune) rune {
			if unicode.IsDigit(r) {
				return r
			}
			return -1
		}, raw)
		if digits == "" {
			continue
		}
		value, err := strconv.Atoi(digits)
		if err != nil {
			// Overflow: a number that long is not a window.
			continue
		}
		// Scale by a suffix that immediately follows the number.
		if loc := windowSuffix.FindStringSubmatch(text[match[1]:]); loc != nil {
			switch strings.ToLower(loc[1]) {
			case "k":
				value *= 1_000
			case "m":
				value *= 1_000_000
			case "万":
				value *= 10_000
			case "亿":
				value *= 100_000_000
			}
		}
		if looksLikeADate(value) {
			// A model that answers with today's date (or its own version year)
			// is answering something else; the prompt asked for a token count.
			continue
		}
		if value >= MinPlausibleWindow && value <= MaxPlausibleWindow {
			return value
		}
	}
	return 0
}
