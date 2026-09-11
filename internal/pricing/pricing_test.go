package pricing

import (
	"math"
	"sync"
	"testing"
)

// eps is the tolerance for comparing money values.
const eps = 1e-12

func almostEqual(a, b float64) bool { return math.Abs(a-b) <= eps }

func TestNewTableResolutionOrder(t *testing.T) {
	full := map[string]Rate{
		"deepseek/deepseek-chat": {PromptPer1K: 0.001, CompletionPer1K: 0.002},
		"deepseek-chat":          {PromptPer1K: 0.01, CompletionPer1K: 0.02},
		"deepseek":               {PromptPer1K: 0.1, CompletionPer1K: 0.2},
	}
	noProviderModel := map[string]Rate{
		"deepseek-chat": {PromptPer1K: 0.01, CompletionPer1K: 0.02},
		"deepseek":      {PromptPer1K: 0.1, CompletionPer1K: 0.2},
	}
	providerOnly := map[string]Rate{
		"deepseek": {PromptPer1K: 0.1, CompletionPer1K: 0.2},
	}

	tests := []struct {
		name       string
		models     map[string]Rate
		fallback   Rate
		provider   string
		model      string
		want       Rate
		wantPriced bool
	}{
		{
			name:       "provider and model wins over model",
			models:     full,
			provider:   "deepseek",
			model:      "deepseek-chat",
			want:       Rate{PromptPer1K: 0.001, CompletionPer1K: 0.002},
			wantPriced: true,
		},
		{
			name:       "model wins over provider",
			models:     noProviderModel,
			provider:   "deepseek",
			model:      "deepseek-chat",
			want:       Rate{PromptPer1K: 0.01, CompletionPer1K: 0.02},
			wantPriced: true,
		},
		{
			name:       "provider used when model is not listed",
			models:     providerOnly,
			provider:   "deepseek",
			model:      "deepseek-chat",
			want:       Rate{PromptPer1K: 0.1, CompletionPer1K: 0.2},
			wantPriced: true,
		},
		{
			name:       "model used when provider is empty",
			models:     full,
			provider:   "",
			model:      "deepseek-chat",
			want:       Rate{PromptPer1K: 0.01, CompletionPer1K: 0.02},
			wantPriced: true,
		},
		{
			name:       "provider used when model is empty",
			models:     full,
			provider:   "deepseek",
			model:      "",
			want:       Rate{PromptPer1K: 0.1, CompletionPer1K: 0.2},
			wantPriced: true,
		},
		{
			name:       "both empty goes straight to fallback",
			models:     full,
			fallback:   Rate{PromptPer1K: 1, CompletionPer1K: 2},
			provider:   "",
			model:      "",
			want:       Rate{PromptPer1K: 1, CompletionPer1K: 2},
			wantPriced: false,
		},
		{
			name:       "no match uses fallback and is unpriced",
			models:     full,
			fallback:   Rate{PromptPer1K: 0.5, CompletionPer1K: 1.5},
			provider:   "qwen",
			model:      "qwen-plus",
			want:       Rate{PromptPer1K: 0.5, CompletionPer1K: 1.5},
			wantPriced: false,
		},
		{
			name:       "fallback zero when nothing configured",
			models:     providerOnly,
			provider:   "openai",
			model:      "gpt-4o-mini",
			want:       Rate{},
			wantPriced: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tbl := NewTable(tt.models, tt.fallback)
			got, ok := tbl.Rate(tt.provider, tt.model)
			if got != tt.want {
				t.Errorf("Rate(%q, %q) = %+v, want %+v", tt.provider, tt.model, got, tt.want)
			}
			if ok != tt.wantPriced {
				t.Errorf("Rate(%q, %q) priced = %v, want %v", tt.provider, tt.model, ok, tt.wantPriced)
			}
		})
	}
}

func TestRateNormalisation(t *testing.T) {
	tests := []struct {
		name     string
		models   map[string]Rate
		provider string
		model    string
		want     Rate
		wantOK   bool
	}{
		{
			name:     "keys are lowercased and trimmed",
			models:   map[string]Rate{"  DeepSeek/DeepSeek-Chat  ": {PromptPer1K: 1, CompletionPer1K: 2}},
			provider: "deepseek",
			model:    "deepseek-chat",
			want:     Rate{PromptPer1K: 1, CompletionPer1K: 2},
			wantOK:   true,
		},
		{
			name:     "lookup arguments are lowercased and trimmed",
			models:   map[string]Rate{"deepseek/deepseek-chat": {PromptPer1K: 1, CompletionPer1K: 2}},
			provider: "  DeepSeek ",
			model:    " DeepSeek-Chat ",
			want:     Rate{PromptPer1K: 1, CompletionPer1K: 2},
			wantOK:   true,
		},
		{
			name:     "whitespace around the slash is ignored",
			models:   map[string]Rate{"deepseek / DeepSeek-Chat": {PromptPer1K: 1, CompletionPer1K: 2}},
			provider: "deepseek",
			model:    "deepseek-chat",
			want:     Rate{PromptPer1K: 1, CompletionPer1K: 2},
			wantOK:   true,
		},
		{
			name:     "trailing slash key equals the plain key",
			models:   map[string]Rate{"Qwen/": {PromptPer1K: 3, CompletionPer1K: 4}},
			provider: "qwen",
			model:    "qwen-plus",
			want:     Rate{PromptPer1K: 3, CompletionPer1K: 4},
			wantOK:   true,
		},
		{
			name:     "blank key never matches",
			models:   map[string]Rate{"   ": {PromptPer1K: 9, CompletionPer1K: 9}, "/": {PromptPer1K: 9, CompletionPer1K: 9}},
			provider: "glm",
			model:    "glm-4-plus",
			want:     Rate{},
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NewTable(tt.models, Rate{}).Rate(tt.provider, tt.model)
			if got != tt.want {
				t.Errorf("Rate(%q, %q) = %+v, want %+v", tt.provider, tt.model, got, tt.want)
			}
			if ok != tt.wantOK {
				t.Errorf("Rate(%q, %q) priced = %v, want %v", tt.provider, tt.model, ok, tt.wantOK)
			}
		})
	}
}

func TestCostArithmetic(t *testing.T) {
	tbl := NewTable(map[string]Rate{
		"deepseek/deepseek-chat": {PromptPer1K: 0.001, CompletionPer1K: 0.002},
		"ollama":                 {PromptPer1K: 0, CompletionPer1K: 0},
	}, Rate{})

	tests := []struct {
		name       string
		provider   string
		model      string
		prompt     int
		completion int
		want       float64
	}{
		{
			name: "known values", provider: "deepseek", model: "deepseek-chat",
			prompt: 1500, completion: 500, want: 0.0025,
		},
		{
			name: "prompt only", provider: "deepseek", model: "deepseek-chat",
			prompt: 1000, completion: 0, want: 0.001,
		},
		{
			name: "completion only", provider: "deepseek", model: "deepseek-chat",
			prompt: 0, completion: 1000, want: 0.002,
		},
		{
			name: "zero tokens cost nothing", provider: "deepseek", model: "deepseek-chat",
			prompt: 0, completion: 0, want: 0,
		},
		{
			name: "negative tokens are treated as zero", provider: "deepseek", model: "deepseek-chat",
			prompt: -1500, completion: -500, want: 0,
		},
		{
			name: "negative prompt does not credit the completion", provider: "deepseek", model: "deepseek-chat",
			prompt: -1500, completion: 500, want: 0.001,
		},
		{
			name: "free local model", provider: "ollama", model: "llama3.2",
			prompt: 100000, completion: 100000, want: 0,
		},
		{
			name: "unpriced model costs nothing with zero fallback", provider: "openai", model: "gpt-4o-mini",
			prompt: 1500, completion: 500, want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tbl.Cost(tt.provider, tt.model, tt.prompt, tt.completion)
			if !almostEqual(got, tt.want) {
				t.Errorf("Cost(%q, %q, %d, %d) = %v, want %v",
					tt.provider, tt.model, tt.prompt, tt.completion, got, tt.want)
			}
		})
	}
}

func TestCostUsesFallbackRate(t *testing.T) {
	tbl := NewTable(map[string]Rate{"deepseek": {PromptPer1K: 0.001, CompletionPer1K: 0.002}},
		Rate{PromptPer1K: 0.01, CompletionPer1K: 0.02})

	got := tbl.CostOf("unknown", "mystery-model", 1000, 1000)
	want := Cost{PromptCost: 0.01, CompletionCost: 0.02, Total: 0.03, Priced: false}
	if got != want {
		t.Errorf("CostOf with fallback = %+v, want %+v", got, want)
	}

	got = tbl.CostOf("deepseek", "deepseek-chat", 1000, 1000)
	want = Cost{PromptCost: 0.001, CompletionCost: 0.002, Total: 0.003, Priced: true}
	if got != want {
		t.Errorf("CostOf explicit = %+v, want %+v", got, want)
	}
}

func TestCostOfBreakdownIsConsistent(t *testing.T) {
	tbl := NewTable(map[string]Rate{
		"deepseek/deepseek-chat": {PromptPer1K: 0.001, CompletionPer1K: 0.002},
		"qwen":                   {PromptPer1K: 0.4, CompletionPer1K: 1.2},
	}, Rate{PromptPer1K: 0.05, CompletionPer1K: 0.15})

	tests := []struct {
		provider           string
		model              string
		prompt, completion int
		wantPriced         bool
		wantTotal          float64
	}{
		{"deepseek", "deepseek-chat", 1500, 500, true, 0.0025},
		{"qwen", "qwen-plus", 2500, 100, true, 1.12},
		{"glm", "glm-4-plus", 1000, 1000, false, 0.2},
		{"", "", 0, 0, false, 0},
		{"deepseek", "deepseek-chat", -10, -10, true, 0},
	}

	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.model, func(t *testing.T) {
			c := tbl.CostOf(tt.provider, tt.model, tt.prompt, tt.completion)
			if c.Total != c.PromptCost+c.CompletionCost {
				t.Errorf("Total %v != PromptCost %v + CompletionCost %v",
					c.Total, c.PromptCost, c.CompletionCost)
			}
			if !almostEqual(c.Total, tt.wantTotal) {
				t.Errorf("Total = %v, want %v", c.Total, tt.wantTotal)
			}
			if c.Priced != tt.wantPriced {
				t.Errorf("Priced = %v, want %v", c.Priced, tt.wantPriced)
			}
			if got := tbl.Cost(tt.provider, tt.model, tt.prompt, tt.completion); !almostEqual(got, c.Total) {
				t.Errorf("Cost = %v, CostOf.Total = %v", got, c.Total)
			}
			if c.PromptCost < 0 || c.CompletionCost < 0 || c.Total < 0 {
				t.Errorf("negative cost: %+v", c)
			}
		})
	}
}

func TestNonFiniteAndNegativeRates(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	negInf := math.Inf(-1)

	tests := []struct {
		name   string
		models map[string]Rate
		want   Rate
	}{
		{
			name:   "negative rates are zero",
			models: map[string]Rate{"deepseek": {PromptPer1K: -0.001, CompletionPer1K: -1}},
			want:   Rate{},
		},
		{
			name:   "NaN rates are zero",
			models: map[string]Rate{"deepseek": {PromptPer1K: nan, CompletionPer1K: nan}},
			want:   Rate{},
		},
		{
			name:   "infinite rates are zero",
			models: map[string]Rate{"deepseek": {PromptPer1K: inf, CompletionPer1K: negInf}},
			want:   Rate{},
		},
		{
			name:   "mixed finite and broken rates keep the finite one",
			models: map[string]Rate{"deepseek": {PromptPer1K: 0.001, CompletionPer1K: nan}},
			want:   Rate{PromptPer1K: 0.001},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tbl := NewTable(tt.models, Rate{})
			got, ok := tbl.Rate("deepseek", "deepseek-chat")
			if !ok {
				t.Fatal("expected an explicit rate match")
			}
			if got != tt.want {
				t.Fatalf("Rate = %+v, want %+v", got, tt.want)
			}
			c := tbl.CostOf("deepseek", "deepseek-chat", 1500, 500)
			if math.IsNaN(c.Total) || math.IsInf(c.Total, 0) || c.Total < 0 {
				t.Errorf("CostOf produced a bad total: %+v", c)
			}
		})
	}

	t.Run("non-finite fallback is zero", func(t *testing.T) {
		tbl := NewTable(nil, Rate{PromptPer1K: nan, CompletionPer1K: inf})
		got, ok := tbl.Rate("nothing", "here")
		if ok || got != (Rate{}) {
			t.Errorf("Rate = %+v, %v; want zero rate, false", got, ok)
		}
		if c := tbl.CostOf("nothing", "here", 1000, 1000); c.Total != 0 {
			t.Errorf("Total = %v, want 0", c.Total)
		}
	})
}

func TestZeroValueAndNilTables(t *testing.T) {
	zero := Table{}
	var nilTable *Table

	for _, tc := range []struct {
		name string
		tbl  *Table
	}{
		{"zero value", &zero},
		{"nil pointer", nilTable},
		{"NewTable(nil, zero)", NewTable(nil, Rate{})},
		{"NewTable(empty, zero)", NewTable(map[string]Rate{}, Rate{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tbl.Cost("deepseek", "deepseek-chat", 1500, 500); got != 0 {
				t.Errorf("Cost = %v, want 0", got)
			}
			c := tc.tbl.CostOf("deepseek", "deepseek-chat", 1500, 500)
			if c != (Cost{}) {
				t.Errorf("CostOf = %+v, want zero Cost with Priced false", c)
			}
			r, ok := tc.tbl.Rate("deepseek", "deepseek-chat")
			if ok || r != (Rate{}) {
				t.Errorf("Rate = %+v, %v; want zero rate, false", r, ok)
			}
		})
	}
}

func TestInputMapIsCopied(t *testing.T) {
	models := map[string]Rate{
		"deepseek/deepseek-chat": {PromptPer1K: 0.001, CompletionPer1K: 0.002},
	}
	tbl := NewTable(models, Rate{})

	// Mutate the caller's map in every way that could leak through.
	models["deepseek/deepseek-chat"] = Rate{PromptPer1K: 99, CompletionPer1K: 99}
	models["qwen"] = Rate{PromptPer1K: 42, CompletionPer1K: 42}
	delete(models, "deepseek/deepseek-chat")

	got, ok := tbl.Rate("deepseek", "deepseek-chat")
	if !ok || got != (Rate{PromptPer1K: 0.001, CompletionPer1K: 0.002}) {
		t.Errorf("Rate after caller mutation = %+v, %v; want the original rate", got, ok)
	}
	if got := tbl.Cost("deepseek", "deepseek-chat", 1500, 500); !almostEqual(got, 0.0025) {
		t.Errorf("Cost after caller mutation = %v, want 0.0025", got)
	}
	if _, ok := tbl.Rate("qwen", "qwen-plus"); ok {
		t.Error("a key added to the caller's map leaked into the table")
	}
}

func TestConcurrentCostCalls(t *testing.T) {
	tbl := NewTable(map[string]Rate{
		"deepseek/deepseek-chat": {PromptPer1K: 0.001, CompletionPer1K: 0.002},
		"qwen-plus":              {PromptPer1K: 0.0008, CompletionPer1K: 0.002},
	}, Rate{PromptPer1K: 0.01, CompletionPer1K: 0.02})

	const (
		goroutines = 16
		iterations = 200
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				switch i % 3 {
				case 0:
					if got := tbl.Cost("deepseek", "deepseek-chat", 1500, 500); !almostEqual(got, 0.0025) {
						t.Errorf("goroutine %d: Cost = %v, want 0.0025", g, got)
					}
				case 1:
					if got := tbl.Cost("qwen", "qwen-plus", 2000, 1000); !almostEqual(got, 0.0036) {
						t.Errorf("goroutine %d: Cost = %v, want 0.0036", g, got)
					}
				default:
					c := tbl.CostOf("glm", "glm-4-plus", 1000, 0)
					if c.Priced || !almostEqual(c.Total, 0.01) {
						t.Errorf("goroutine %d: CostOf = %+v, want fallback 0.01 unpriced", g, c)
					}
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestNormalizeKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"deepseek", "deepseek"},
		{"  DeepSeek  ", "deepseek"},
		{"DeepSeek/DeepSeek-Chat", "deepseek/deepseek-chat"},
		{" deepseek / qwen-plus ", "deepseek/qwen-plus"},
		{"deepseek/", "deepseek"},
		{"/deepseek", "deepseek"},
		{"", ""},
		{"   ", ""},
		{"///", ""},
	}
	for _, tt := range tests {
		if got := normalizeKey(tt.in); got != tt.want {
			t.Errorf("normalizeKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSanitizePrice(t *testing.T) {
	tests := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		{0.001, 0.001},
		{-0.001, 0},
		{-1, 0},
		{math.NaN(), 0},
		{math.Inf(1), 0},
		{math.Inf(-1), 0},
	}
	for _, tt := range tests {
		if got := sanitizePrice(tt.in); got != tt.want {
			t.Errorf("sanitizePrice(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
