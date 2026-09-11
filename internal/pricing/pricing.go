// Package pricing computes LLM call cost from token counts and a configurable
// price table.
//
// The package is pure computation: no network, no filesystem, no logging, and
// no errors. Unknown models are priced with a fallback rate and reported as
// unpriced, so callers can render the cost as "unknown" instead of "$0.00".
package pricing

import (
	"math"
	"strings"
)

// Rate is the price of one model, quoted per 1000 tokens in USD.
type Rate struct {
	// PromptPer1K is the price per 1000 prompt (input) tokens.
	PromptPer1K float64 `mapstructure:"prompt_per_1k" json:"prompt_per_1k"`
	// CompletionPer1K is the price per 1000 completion (output) tokens.
	CompletionPer1K float64 `mapstructure:"completion_per_1k" json:"completion_per_1k"`
}

// Table resolves the Rate for a (provider, model) pair and computes costs.
//
// A zero Table (Table{}) or a nil *Table is valid and prices everything at
// zero, so callers never need to nil-check.
//
// Lookup order for a (provider="deepseek", model="deepseek-chat") call:
//
//  1. "deepseek/deepseek-chat" (provider/model)
//  2. "deepseek-chat" (model)
//  3. "deepseek" (provider)
//  4. the fallback rate passed to NewTable
//
// Keys and lookup arguments are matched case-insensitively: both are lowercased
// and trimmed, around the whole key and around each "/"-separated component, so
// "DeepSeek/DeepSeek-Chat" in YAML matches a deepseek / deepseek-chat call.
// Empty components are dropped, so "deepseek/" is the same key as "deepseek".
//
// An empty provider or model simply skips the corresponding keys: a call with
// model "" and provider "deepseek" tries "deepseek" and then the fallback. A
// call with both empty goes straight to the fallback.
//
// A Table is immutable once NewTable has built it, so every method is safe for
// concurrent use without locking. NewTable defensively copies the model map:
// mutating the caller's map afterwards cannot change the table.
type Table struct {
	models   map[string]Rate
	fallback Rate
}

// NewTable builds a Table from a model-keyed price map. Keys may be
// "provider/model", "model" or "provider"; the most specific match wins.
// An empty or nil map yields a zero-cost table.
//
// Keys are normalised the same way as lookups (lowercase, trimmed), so the
// price table can be written in any casing in YAML. Non-finite or negative
// rates are treated as 0 so a bad config entry can never produce a NaN or a
// negative cost.
func NewTable(models map[string]Rate, fallback Rate) *Table {
	t := &Table{
		models:   make(map[string]Rate, len(models)),
		fallback: sanitizeRate(fallback),
	}
	for key, r := range models {
		k := normalizeKey(key)
		if k == "" {
			// A key made of whitespace/slashes can never match a lookup.
			continue
		}
		t.models[k] = sanitizeRate(r)
	}
	return t
}

// Rate returns the rate for a provider/model pair and whether an explicit
// entry was found. When no key matches, the fallback rate is returned with
// false.
//
// A nil receiver is valid and reports a zero rate that was not explicitly
// priced.
func (t *Table) Rate(provider, model string) (Rate, bool) {
	if t == nil {
		return Rate{}, false
	}
	p := normalizeKey(provider)
	m := normalizeKey(model)
	// 1. provider/model — most specific.
	if p != "" && m != "" {
		if r, ok := t.models[p+"/"+m]; ok {
			return r, true
		}
	}
	// 2. model only.
	if m != "" {
		if r, ok := t.models[m]; ok {
			return r, true
		}
	}
	// 3. provider only.
	if p != "" {
		if r, ok := t.models[p]; ok {
			return r, true
		}
	}
	// 4. Fallback: priced with the default rate, but not explicitly matched.
	return t.fallback, false
}

// Cost returns the cost in USD for the given token counts:
//
//	promptTokens/1000*promptPer1K + completionTokens/1000*completionPer1K
//
// Negative token counts count as 0, so the result is never negative.
func (t *Table) Cost(provider, model string, promptTokens, completionTokens int) float64 {
	return t.CostOf(provider, model, promptTokens, completionTokens).Total
}

// CostOf is Cost plus a breakdown, for reporting.
func (t *Table) CostOf(provider, model string, promptTokens, completionTokens int) Cost {
	r, priced := t.Rate(provider, model)
	promptCost := tokenCost(promptTokens, r.PromptPer1K)
	completionCost := tokenCost(completionTokens, r.CompletionPer1K)
	return Cost{
		PromptCost:     promptCost,
		CompletionCost: completionCost,
		Total:          promptCost + completionCost,
		Priced:         priced,
	}
}

// Cost is a computed cost with its inputs, so an API can show the working.
type Cost struct {
	PromptCost     float64 `json:"prompt_cost"`
	CompletionCost float64 `json:"completion_cost"`
	Total          float64 `json:"total"`
	// Priced reports whether an explicit rate matched (false = fallback used,
	// so the total is likely 0 and should be shown as "unknown").
	Priced bool `json:"priced"`
}

// tokenCost prices a single token bucket. Non-positive token counts and
// non-positive rates cost nothing.
func tokenCost(tokens int, per1K float64) float64 {
	if tokens <= 0 || per1K <= 0 {
		return 0
	}
	return float64(tokens) / 1000 * per1K
}

// sanitizeRate clears rates that would poison the arithmetic.
func sanitizeRate(r Rate) Rate {
	return Rate{
		PromptPer1K:     sanitizePrice(r.PromptPer1K),
		CompletionPer1K: sanitizePrice(r.CompletionPer1K),
	}
}

// sanitizePrice maps a non-finite or negative price to 0.
func sanitizePrice(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// normalizeKey lowercases and trims a price-table key or lookup argument.
// Each "/"-separated component is trimmed and lowercased independently so that
// " DeepSeek / DeepSeek-Chat " and "deepseek/deepseek-chat" are the same key;
// empty components are dropped.
func normalizeKey(s string) string {
	parts := strings.Split(s, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, "/")
}
