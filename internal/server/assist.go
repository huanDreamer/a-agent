package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

// draftTimeout bounds one AI-assist call. Writing a config from a description
// is one model call over a short prompt, so a model that has not answered in
// two minutes is not going to; the request must not hang forever holding the
// console's spinner.
const draftTimeout = 120 * time.Second

// draft is one AI-generated configuration object plus provenance: which model
// produced it and what it said the result was.
//
// Nothing here is persisted by the drafting path. The console shows the draft
// for review and only a separate save call writes it, so a model that
// misunderstands the request cannot change a working configuration.
type draft struct {
	// Fields is the parsed JSON object the model returned.
	Fields map[string]any `json:"fields"`
	// Raw is the model's answer, kept so the UI can show what it actually said
	// when parsing fails or a field looks wrong.
	Raw string `json:"raw"`
	// Provider and Model name what produced the draft.
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Notes is the model's own one-line summary of the result, when it gave one.
	Notes string `json:"notes,omitempty"`
}

// draftModel resolves the model an AI-assist call runs on: the same default a
// new conversation uses, so drafting never needs its own configuration.
func (s *Server) draftModel(ctx context.Context) (einomodel.BaseChatModel, string, string, error) {
	if s.chat.Builder == nil {
		return nil, "", "", errors.New("配置助手需要一个对话模型：请先在 设置 → 模型 里添加 provider 并启用一个对话模型")
	}
	built, err := s.chat.Builder.Build(ctx, "", "")
	if err != nil {
		return nil, "", "", err
	}
	cm, ok := built.(einomodel.BaseChatModel)
	if !ok {
		return nil, "", "", fmt.Errorf("配置助手需要一个对话模型，但当前默认模型是 %T", built)
	}
	provider, model := "", ""
	cat := s.chat.Builder.Catalog(ctx)
	for _, m := range cat.Models {
		if m.Default {
			provider, model = m.Provider, m.Model
			break
		}
	}
	return cm, provider, model, nil
}

// draftJSON runs one AI-assist call and parses its JSON answer.
func (s *Server) draftJSON(ctx context.Context, system, user string) (*draft, error) {
	cm, provider, model, err := s.draftModel(ctx)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, draftTimeout)
	defer cancel()

	started := time.Now()
	out, err := cm.Generate(callCtx, []*schema.Message{
		{Role: schema.System, Content: system},
		{Role: schema.User, Content: user},
	})
	if err != nil {
		return nil, fmt.Errorf("配置助手调用模型失败: %w", err)
	}
	if out == nil {
		return nil, errors.New("配置助手没有得到模型回复")
	}
	raw := strings.TrimSpace(out.Content)
	if raw == "" {
		return nil, errors.New("配置助手得到了空回复（模型可能触发了内容过滤）")
	}

	fields, err := extractJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%w；模型原始回复：%s", err, truncateForMessage(raw, 500))
	}

	s.logger.Info("config draft generated",
		zapString("provider", provider), zapString("model", model),
		zap.Int64("duration_ms", time.Since(started).Milliseconds()))

	d := &draft{Fields: fields, Raw: raw, Provider: provider, Model: model}
	if notes, ok := fields["summary"].(string); ok {
		d.Notes = strings.TrimSpace(notes)
	}
	return d, nil
}

// extractJSONObject pulls the first JSON object out of a model answer.
//
// Models wrap JSON in prose or a ```json fence often enough that demanding
// "JSON only" is not sufficient on its own, and a strict decode would throw
// away an otherwise perfectly usable configuration. The scan is brace-aware and
// string-aware, so a brace inside a string value does not end the object early.
func extractJSONObject(raw string) (map[string]any, error) {
	text := strings.TrimSpace(raw)
	// Drop a fenced block's markers, keeping the content.
	if strings.HasPrefix(text, "```") {
		if nl := strings.IndexByte(text, '\n'); nl >= 0 {
			text = text[nl+1:]
		}
		if end := strings.LastIndex(text, "```"); end >= 0 {
			text = text[:end]
		}
	}

	start := strings.IndexByte(text, '{')
	if start < 0 {
		return nil, errors.New("配置助手的回复里没有 JSON 对象")
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inString:
			escaped = true
		case ch == '"':
			inString = !inString
		case inString:
			// Nothing else is structural inside a string.
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				var out map[string]any
				if err := json.Unmarshal([]byte(text[start:i+1]), &out); err != nil {
					return nil, fmt.Errorf("配置助手返回的 JSON 无法解析: %w", err)
				}
				return out, nil
			}
		}
	}
	return nil, errors.New("配置助手返回的 JSON 不完整（缺少右花括号）")
}

// stringField reads one string field from a draft, tolerating a number or a
// bool (a model that answers `"port": 3000` when asked for a string should not
// fail the whole draft).
func stringField(fields map[string]any, key string) string {
	v, ok := fields[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	case float64:
		// A model that answers `"port": 3000` is giving the useful value; only
		// the rendering has to avoid "3000.000000".
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

// stringListField reads a list-of-strings field, accepting a single string as a
// one-element list. Entries that are not strings are dropped rather than
// rendered as "[object Object]" into a command line.
//
// Entries are returned untrimmed on purpose: a header value that ends in a
// space ("Authorization: Bearer ") is how the model marks a value the operator
// still has to fill in, and trimming here would erase that signal before the
// caller can notice it. Callers that store the list trim it themselves.
func stringListField(fields map[string]any, key string) []string {
	v, ok := fields[key]
	if !ok || v == nil {
		return nil
	}
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) != "" {
			return []string{t}
		}
		return nil
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			switch s := item.(type) {
			case string:
				if strings.TrimSpace(s) != "" {
					out = append(out, s)
				}
			case float64:
				out = append(out, strconv.FormatFloat(s, 'f', -1, 64))
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}

// truncateForMessage bounds a string included in an error message, on a rune
// boundary so the message stays valid UTF-8.
func truncateForMessage(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
