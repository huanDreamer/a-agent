package context

import "testing"

// derived is the cap the defaults produce for a window, spelled out in the test
// rather than recomputed through the code under test wherever it can be: the
// arithmetic is part of the contract.
func derived(window int) int {
	return int(float64(window)*DefaultWindowRatio) - DefaultReserveOutputTokens
}

func TestWindowSpecResolveDerivesCapFromModelWindow(t *testing.T) {
	cases := []struct {
		name       string
		spec       WindowSpec
		model      string
		wantTokens int
		wantCap    int
		wantSource string
	}{
		{
			name:       "known family",
			spec:       WindowSpec{},
			model:      "deepseek/deepseek-v4.1-flash",
			wantTokens: 131072,
			wantCap:    derived(131072),
			wantSource: WindowSourceKnown,
		},
		{
			name:       "longest pattern wins",
			spec:       WindowSpec{},
			model:      "openai/gpt-4o-mini",
			wantTokens: 128000,
			wantCap:    derived(128000),
			wantSource: WindowSourceKnown,
		},
		{
			name:       "more specific pattern beats the family",
			spec:       WindowSpec{},
			model:      "openai/gpt-4-turbo-preview",
			wantTokens: 128000,
			wantCap:    derived(128000),
			wantSource: WindowSourceKnown,
		},
		{
			name:       "unknown model falls back to the default window",
			spec:       WindowSpec{},
			model:      "acme/mystery-1",
			wantTokens: DefaultModelWindow,
			wantCap:    derived(DefaultModelWindow),
			wantSource: WindowSourceDefault,
		},
		{
			name:       "operator override wins over the table",
			spec:       WindowSpec{Overrides: map[string]int{"deepseek": 65536}},
			model:      "deepseek/deepseek-v4.1-flash",
			wantTokens: 65536,
			wantCap:    derived(65536),
			wantSource: WindowSourceConfig,
		},
		{
			name:       "the longest override key wins",
			spec:       WindowSpec{Overrides: map[string]int{"deepseek": 65536, "deepseek-v4.1": 32768}},
			model:      "deepseek/deepseek-v4.1-flash",
			wantTokens: 32768,
			wantCap:    derived(32768),
			wantSource: WindowSourceConfig,
		},
		{
			name:       "fixed cap is used as written",
			spec:       WindowSpec{MaxTokens: 60000},
			model:      "deepseek/deepseek-v4.1-flash",
			wantTokens: 131072,
			wantCap:    60000,
			wantSource: WindowSourceConfig,
		},
		{
			name:       "a fixed cap still reports the real window",
			spec:       WindowSpec{MaxTokens: 60000},
			model:      "acme/mystery-1",
			wantTokens: DefaultModelWindow,
			wantCap:    60000,
			wantSource: WindowSourceConfig,
		},
		{
			name:       "negative max_tokens disables compression",
			spec:       WindowSpec{MaxTokens: -1},
			model:      "deepseek/deepseek-v4.1-flash",
			wantTokens: 131072,
			wantCap:    0,
			wantSource: WindowSourceOff,
		},
		{
			name:       "ratio and reserve are honoured",
			spec:       WindowSpec{Ratio: 0.5, Reserve: 1024},
			model:      "deepseek/deepseek-v4.1-flash",
			wantTokens: 131072,
			wantCap:    int(float64(131072)*0.5) - 1024,
			wantSource: WindowSourceKnown,
		},
		{
			name:       "a window too small for the floor gets half of itself",
			spec:       WindowSpec{Overrides: map[string]int{"tiny": 8000}},
			model:      "tiny/model",
			wantTokens: 8000,
			// Half, not MinWindowCap: a cap above the model's window is not a
			// bound, and returning the floor here meant the condenser never fired
			// on a small local model.
			wantCap:    4000,
			wantSource: WindowSourceConfig,
		},
		{
			name:       "the floor applies when the window can afford it",
			spec:       WindowSpec{Overrides: map[string]int{"small": 20000}},
			model:      "small/model",
			wantTokens: 20000,
			wantCap:    MinWindowCap,
			wantSource: WindowSourceConfig,
		},
		{
			name:       "a ratio above 1 is clamped to the whole window",
			spec:       WindowSpec{Ratio: 1.5, Reserve: 0},
			model:      "tiny/model",
			wantTokens: DefaultModelWindow,
			wantCap:    DefaultModelWindow - DefaultReserveOutputTokens,
			wantSource: WindowSourceDefault,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.Resolve(tc.model)
			if got.Tokens != tc.wantTokens {
				t.Fatalf("Tokens = %d, want %d", got.Tokens, tc.wantTokens)
			}
			if got.Cap != tc.wantCap {
				t.Fatalf("Cap = %d, want %d", got.Cap, tc.wantCap)
			}
			if got.Source != tc.wantSource {
				t.Fatalf("Source = %q, want %q", got.Source, tc.wantSource)
			}
			if got.Derivation == "" {
				t.Fatal("Derivation is empty: the cap must be explainable")
			}
		})
	}
}

// TestWindowSpecResolveNeverExceedsTheWindow: the whole point of deriving the cap
// is to stay inside the model's window, so a derived cap under the defaults may
// not exceed it even in the pathological configuration.
func TestWindowSpecResolveNeverExceedsTheWindow(t *testing.T) {
	models := []string{"", "claude-sonnet-4", "gpt-3.5-turbo", "acme/mystery-1", "gpt-4.1"}
	specs := []WindowSpec{
		{},
		{Ratio: 1, Reserve: 0},
		{Ratio: 1, Reserve: -1},
		{Ratio: 1.5, Reserve: -1},
		{Ratio: 0.1, Reserve: 100000},
		{Overrides: map[string]int{"claude": 200000}},
		// The small windows where the floor used to win: these are the cases a
		// gateway override exists for, and the ones the old property test never
		// reached.
		{Overrides: map[string]int{"claude": 8000}},
		{Overrides: map[string]int{"claude": 4096}},
		{Overrides: map[string]int{"claude": 1024}},
		{Overrides: map[string]int{"claude": 200}},
		{Overrides: map[string]int{"claude": 1}},
	}
	for _, spec := range specs {
		for _, model := range models {
			got := spec.Resolve(model)
			if got.Cap > got.Tokens {
				t.Fatalf("model %q: Cap %d exceeds window %d (%s)", model, got.Cap, got.Tokens, got.Derivation)
			}
		}
	}
}

func TestKnownWindowsIsACopy(t *testing.T) {
	rows := KnownWindows()
	if len(rows) == 0 {
		t.Fatal("the built-in table is empty")
	}
	rows[0].Tokens = -1
	for _, row := range KnownWindows() {
		if row.Tokens <= 0 {
			t.Fatalf("a caller mutating the returned slice changed the table: %+v", row)
		}
	}
}
