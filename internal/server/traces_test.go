package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/langfuse"
	"github.com/huan/huan-agent/internal/llm"
	"github.com/huan/huan-agent/internal/store"
)

// ---- model builder ----

// testRegistry builds an llm.Registry with two providers.
func testRegistry() *llm.Registry {
	return llm.NewRegistry(map[string]llm.Provider{
		"deepseek": {Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-x", Model: "deepseek-chat"},
		"local":    {Name: "local", BaseURL: "http://localhost:11434/v1", Model: "llama3.2"},
	}, "deepseek")
}

func TestModelBuilder_Build(t *testing.T) {
	b := NewModelBuilder(testRegistry())

	t.Run("explicit provider and model", func(t *testing.T) {
		got, err := b.Build("deepseek", "deepseek-reasoner")
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if _, ok := got.(model.BaseChatModel); !ok {
			t.Errorf("Build returned %T, want a chat model", got)
		}
	})
	t.Run("empty provider uses the default", func(t *testing.T) {
		if _, err := b.Build("", ""); err != nil {
			t.Errorf("Build with empty provider: %v", err)
		}
	})
	t.Run("unknown provider errors", func(t *testing.T) {
		if _, err := b.Build("nope", ""); err == nil {
			t.Error("want an error for an unknown provider")
		}
	})
	t.Run("nil registry errors", func(t *testing.T) {
		nb := NewModelBuilder(nil)
		if _, err := nb.Build("deepseek", ""); err == nil {
			t.Error("want an error when no registry is configured")
		}
		if got := nb.Catalog(); got != nil {
			t.Errorf("Catalog = %+v, want nil", got)
		}
	})
}

func TestModelBuilder_Catalog(t *testing.T) {
	cat := NewModelBuilder(testRegistry()).Catalog()
	if len(cat) != 2 {
		t.Fatalf("catalog = %d entries, want 2", len(cat))
	}
	// Sorted by provider name, and the default is marked.
	if cat[0].Provider != "deepseek" || cat[1].Provider != "local" {
		t.Errorf("catalog order = %s/%s, want deepseek/local", cat[0].Provider, cat[1].Provider)
	}
	if !cat[0].Default {
		t.Error("deepseek should be marked as the default provider")
	}
	if cat[0].Model != "deepseek-chat" {
		t.Errorf("model = %q", cat[0].Model)
	}
	// has_api_key lets the UI avoid offering a provider that cannot work.
	if !cat[0].HasAPIKey {
		t.Error("deepseek has a key and should report it")
	}
	if cat[1].HasAPIKey {
		t.Error("local has no key and should report has_api_key=false")
	}
}

// ---- per-session runner ----

func TestRunnerFor_UsesSessionModel(t *testing.T) {
	builder := NewModelBuilder(testRegistry())
	base, err := chat.New(chat.Config{
		Model: &scriptedModel{}, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	s := &Server{
		cfg:    Config{ChatMaxSteps: 3},
		logger: zap.NewNop(),
		chat:   ChatDeps{Runner: base, Builder: builder},
		tracer: nopTracer{},
	}

	// Two different model choices must yield two runners and be cached.
	r1, err := s.runnerFor(store.ChatSession{Provider: "deepseek", Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("runnerFor: %v", err)
	}
	r2, err := s.runnerFor(store.ChatSession{Provider: "deepseek", Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("runnerFor: %v", err)
	}
	if r1 != r2 {
		t.Error("the same model choice should reuse a cached runner")
	}

	r3, err := s.runnerFor(store.ChatSession{Provider: "local", Model: "llama3.2"})
	if err != nil {
		t.Fatalf("runnerFor: %v", err)
	}
	if r3 == r1 {
		t.Error("a different model choice must produce a different runner")
	}

	// An unknown provider surfaces as an error the handler can report.
	if _, err := s.runnerFor(store.ChatSession{Provider: "nope"}); err == nil {
		t.Error("want an error for an unknown provider")
	}
}

func TestRunnerFor_NoBuilderUsesDefaultRunner(t *testing.T) {
	base, _ := chat.New(chat.Config{Model: &scriptedModel{}, Logger: zap.NewNop()})
	s := &Server{cfg: Config{}, logger: zap.NewNop(), chat: ChatDeps{Runner: base}, tracer: nopTracer{}}

	got, err := s.runnerFor(store.ChatSession{})
	if err != nil {
		t.Fatalf("runnerFor: %v", err)
	}
	if got != base {
		t.Error("without a builder the base runner must be reused")
	}
}

func TestRunnerFor_ChatDisabled(t *testing.T) {
	s := &Server{cfg: Config{}, logger: zap.NewNop(), tracer: nopTracer{}}
	if _, err := s.runnerFor(store.ChatSession{}); err == nil {
		t.Error("want an error when chat is not enabled")
	}
}

func TestRunnerCache_EvictsWhenFull(t *testing.T) {
	var c runnerCache
	for i := 0; i < maxRunnerCache+5; i++ {
		key := "k" + strconvItoa(i)
		c.put(key, &chat.Runner{})
		if _, ok := c.get(key); !ok {
			t.Fatalf("entry %s was not stored", key)
		}
	}
	// The cache must stay bounded no matter how many models a caller names.
	if len(c.m) > maxRunnerCache {
		t.Errorf("cache holds %d entries, want at most %d", len(c.m), maxRunnerCache)
	}
}

// ---- defaults and helpers ----

func TestChatPromptAndLimits(t *testing.T) {
	t.Run("default prompt", func(t *testing.T) {
		s := &Server{cfg: Config{}}
		if got := s.chatPrompt(); got != defaultSystemPrompt {
			t.Errorf("chatPrompt = %q, want the default", got)
		}
	})
	t.Run("configured prompt wins", func(t *testing.T) {
		s := &Server{cfg: Config{}, chat: ChatDeps{SystemPrompt: "  custom prompt  "}}
		if got := s.chatPrompt(); got != "custom prompt" {
			t.Errorf("chatPrompt = %q, want the trimmed custom prompt", got)
		}
	})
	t.Run("whitespace prompt falls back", func(t *testing.T) {
		s := &Server{cfg: Config{}, chat: ChatDeps{SystemPrompt: "   "}}
		if got := s.chatPrompt(); got != defaultSystemPrompt {
			t.Errorf("chatPrompt = %q, want the default", got)
		}
	})
	t.Run("max steps", func(t *testing.T) {
		if got := (&Server{cfg: Config{}}).chatMaxSteps(); got != chat.DefaultMaxSteps {
			t.Errorf("chatMaxSteps = %d, want the default", got)
		}
		if got := (&Server{cfg: Config{ChatMaxSteps: 7}}).chatMaxSteps(); got != 7 {
			t.Errorf("chatMaxSteps = %d, want 7", got)
		}
	})
	t.Run("history limit", func(t *testing.T) {
		if got := (&Server{cfg: Config{}}).chatHistoryLimit(); got <= 0 {
			t.Errorf("chatHistoryLimit = %d, want a positive default", got)
		}
		if got := (&Server{cfg: Config{ChatHistoryLimit: 5}}).chatHistoryLimit(); got != 5 {
			t.Errorf("chatHistoryLimit = %d, want 5", got)
		}
	})
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"single line", "hello", 40, "hello"},
		{"takes first line only", "first\nsecond", 40, "first"},
		{"carriage return", "first\r\nsecond", 40, "first"},
		{"trims", "  padded  ", 40, "padded"},
		{"truncates long", strings.Repeat("a", 50), 10, strings.Repeat("a", 10) + "…"},
		{"empty", "", 10, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstLine(tc.in, tc.max); got != tc.want {
				t.Errorf("firstLine(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

func TestFirstLine_IsRuneSafe(t *testing.T) {
	// Cutting mid-rune would emit invalid UTF-8 into a title.
	got := firstLine(strings.Repeat("中", 30), 10)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation, got %q", got)
	}
	if strings.Contains(got, "\uFFFD") {
		t.Errorf("truncation produced a replacement character: %q", got)
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Errorf("invalid rune in %q", got)
		}
	}
}

func TestToolNames(t *testing.T) {
	if got := (&Server{}).toolNames(); len(got) != 0 {
		t.Errorf("toolNames with no registry = %v, want empty", got)
	}
}

func TestParseUsageAndToolRuns(t *testing.T) {
	t.Run("empty is zero", func(t *testing.T) {
		if got := parseUsage(""); got.TotalTokens != 0 {
			t.Errorf("parseUsage(\"\") = %+v", got)
		}
	})
	t.Run("malformed is ignored rather than fatal", func(t *testing.T) {
		if got := parseUsage("not json"); got.TotalTokens != 0 {
			t.Errorf("parseUsage(malformed) = %+v", got)
		}
	})
	t.Run("round trip", func(t *testing.T) {
		got := parseUsage(`{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"duration_ms":4}`)
		if got.PromptTokens != 1 || got.CompletionTokens != 2 || got.TotalTokens != 3 || got.DurationMs != 4 {
			t.Errorf("parseUsage = %+v", got)
		}
	})
	t.Run("tool runs", func(t *testing.T) {
		if got := toolRunsFromJSON(""); got != nil {
			t.Errorf("empty blob = %+v, want nil", got)
		}
		if got := toolRunsFromJSON("garbage"); got != nil {
			t.Errorf("malformed blob = %+v, want nil (ignored, not fatal)", got)
		}
		got := toolRunsFromJSON(`[{"id":"c1","name":"time","args":"{}","result":"ok"}]`)
		if len(got) != 1 || got[0].Name != "time" || got[0].Result != "ok" {
			t.Errorf("decoded runs = %+v", got)
		}
	})
}

// ---- nop tracer ----

func TestNopTracer_IsInertAndDisabled(t *testing.T) {
	var tr nopTracer
	ctx := context.Background()
	if tr.Enabled() {
		t.Error("nopTracer must report disabled so the UI can say tracing is off")
	}
	if id := tr.StartTrace(ctx, chat.TraceInfo{}); id != "" {
		t.Errorf("StartTrace = %q, want empty", id)
	}
	if id := tr.StartSpan(ctx, chat.SpanInfo{}); id != "" {
		t.Errorf("StartSpan = %q, want empty", id)
	}
	if id := tr.StartGeneration(ctx, chat.GenInfo{}); id != "" {
		t.Errorf("StartGeneration = %q, want empty", id)
	}
	// None of these may panic.
	tr.EndTrace(ctx, "", nil)
	tr.EndSpan(ctx, "", nil, "")
	tr.EndGeneration(ctx, "", nil, chat.Usage{}, "")
}

// ---- hertz logger adapter ----

func TestHertzZapLogger_ForwardsAndNeverPanics(t *testing.T) {
	l := &hertzZapLogger{logger: zap.NewNop()}
	// Every method must exist and be callable; Hertz requires the full interface.
	l.Trace("a")
	l.Debug("b")
	l.Info("c")
	l.Notice("d")
	l.Warn("e")
	l.Error("f")
	l.Fatal("g")
	l.Tracef("%s", "a")
	l.Debugf("%s", "b")
	l.Infof("%s", "c")
	l.Noticef("%s", "d")
	l.Warnf("%s", "e")
	l.Errorf("%s", "f")
	l.Fatalf("%s", "g")
	ctx := context.Background()
	l.CtxTracef(ctx, "%s", "a")
	l.CtxDebugf(ctx, "%s", "b")
	l.CtxInfof(ctx, "%s", "c")
	l.CtxNoticef(ctx, "%s", "d")
	l.CtxWarnf(ctx, "%s", "e")
	l.CtxErrorf(ctx, "%s", "f")
	l.CtxFatalf(ctx, "%s", "g")
	// Level/output are owned by zap, so these are deliberate no-ops.
	l.SetLevel(0)
	l.SetOutput(nil)
}

// ---- traces ----

// fakeTraceReader is a configurable TraceReader.
type fakeTraceReader struct {
	enabled bool
	host    string
	listErr error
	getErr  error
	detail  *langfuse.TraceDetail
	traces  []langfuse.TraceSummary
}

func (f *fakeTraceReader) Enabled() bool { return f.enabled }
func (f *fakeTraceReader) Stats() langfuse.Stats {
	return langfuse.Stats{Enabled: f.enabled, Sent: 5, Failed: 1}
}
func (f *fakeTraceReader) Host() string { return f.host }
func (f *fakeTraceReader) ListTraces(context.Context, langfuse.TraceFilter) ([]langfuse.TraceSummary, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.traces, nil
}
func (f *fakeTraceReader) GetTrace(context.Context, string) (*langfuse.TraceDetail, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.detail, nil
}

// newTraceHarness builds a server with a fake trace reader.
func newTraceHarness(t *testing.T, reader TraceReader) *harness {
	t.Helper()
	srv, st := buildServerWith(t, buildOpts{})
	srv.traces = reader
	startHarness(t, srv)
	return &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
}

func TestTraceStatus(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		h := newTraceHarness(t, &fakeTraceReader{enabled: true, host: "https://lf.example"})
		h.login(t)

		var got struct {
			Enabled bool   `json:"enabled"`
			Host    string `json:"host"`
			Stats   struct {
				Sent   int64 `json:"sent"`
				Failed int64 `json:"failed"`
			} `json:"stats"`
		}
		h.getJSON(t, "/api/traces/status", http.StatusOK, &got)
		if !got.Enabled || got.Host != "https://lf.example" {
			t.Errorf("status = %+v", got)
		}
		if got.Stats.Sent != 5 || got.Stats.Failed != 1 {
			t.Errorf("stats = %+v, want the delivery counters", got.Stats)
		}
	})
	t.Run("no reader", func(t *testing.T) {
		h := newTraceHarness(t, nil)
		h.login(t)
		var got struct {
			Enabled bool `json:"enabled"`
		}
		h.getJSON(t, "/api/traces/status", http.StatusOK, &got)
		if got.Enabled {
			t.Error("without a reader tracing must report disabled")
		}
	})
}

func TestTraceList(t *testing.T) {
	reader := &fakeTraceReader{
		enabled: true,
		host:    "https://lf.example",
		traces: []langfuse.TraceSummary{
			{ID: "t1", Name: "chat.turn", UserID: "admin", Latency: 1.2},
			{ID: "t2", Name: "chat.turn", Latency: 0.4},
		},
	}
	h := newTraceHarness(t, reader)
	h.login(t)

	var got struct {
		Enabled bool                    `json:"enabled"`
		Host    string                  `json:"host"`
		Traces  []langfuse.TraceSummary `json:"traces"`
	}
	h.getJSON(t, "/api/traces", http.StatusOK, &got)
	if len(got.Traces) != 2 {
		t.Fatalf("traces = %d, want 2", len(got.Traces))
	}
	if got.Traces[0].ID != "t1" || !got.Enabled {
		t.Errorf("list = %+v", got)
	}
}

func TestTraceList_DisabledExplainsWhy(t *testing.T) {
	h := newTraceHarness(t, &fakeTraceReader{enabled: false})
	h.login(t)

	var got struct {
		Enabled bool   `json:"enabled"`
		Message string `json:"message"`
	}
	h.getJSON(t, "/api/traces", http.StatusOK, &got)
	if got.Enabled {
		t.Error("must not report enabled")
	}
	// The UI needs to tell the operator what to configure.
	if !strings.Contains(got.Message, "langfuse") {
		t.Errorf("message = %q, want it to name the config to set", got.Message)
	}
}

func TestTraceList_BackendFailureIsDistinct(t *testing.T) {
	h := newTraceHarness(t, &fakeTraceReader{enabled: true, listErr: errors.New("connection refused")})
	h.login(t)

	// A Langfuse outage must not look like a broken admin server.
	resp := h.get(t, "/api/traces")
	requireStatus(t, resp, http.StatusBadGateway)
}

func TestTraceList_ErrDisabledIsNotAnError(t *testing.T) {
	h := newTraceHarness(t, &fakeTraceReader{enabled: true, listErr: langfuse.ErrDisabled})
	h.login(t)

	var got struct {
		Enabled bool `json:"enabled"`
	}
	h.getJSON(t, "/api/traces", http.StatusOK, &got)
	if got.Enabled {
		t.Error("ErrDisabled should report tracing as disabled, not as a success")
	}
}

func TestTraceDetail(t *testing.T) {
	reader := &fakeTraceReader{
		enabled: true,
		detail: &langfuse.TraceDetail{
			TraceSummary: langfuse.TraceSummary{ID: "t1", Name: "chat.turn"},
			Observations: []langfuse.Observation{
				{ID: "o1", Name: "step-1", Type: "GENERATION", Model: "deepseek-chat"},
				{ID: "o2", Name: "tool.time", Type: "SPAN", ParentID: "o1"},
			},
		},
	}
	h := newTraceHarness(t, reader)
	h.login(t)

	var got struct {
		Trace struct {
			ID           string                 `json:"id"`
			Observations []langfuse.Observation `json:"observations"`
		} `json:"trace"`
	}
	h.getJSON(t, "/api/traces/t1", http.StatusOK, &got)
	if got.Trace.ID != "t1" {
		t.Errorf("trace id = %q", got.Trace.ID)
	}
	if len(got.Trace.Observations) != 2 {
		t.Fatalf("observations = %d, want 2", len(got.Trace.Observations))
	}
	// The waterfall needs parent links to build the tree.
	if got.Trace.Observations[1].ParentID != "o1" {
		t.Errorf("parent link lost: %+v", got.Trace.Observations[1])
	}
}

func TestTraceDetail_MissingAndDisabled(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		h := newTraceHarness(t, &fakeTraceReader{enabled: true, detail: nil})
		h.login(t)
		h.getJSON(t, "/api/traces/nope", http.StatusNotFound, nil)
	})
	t.Run("tracing disabled", func(t *testing.T) {
		h := newTraceHarness(t, &fakeTraceReader{enabled: false})
		h.login(t)
		var got struct {
			Enabled bool `json:"enabled"`
		}
		h.getJSON(t, "/api/traces/any", http.StatusOK, &got)
		if got.Enabled {
			t.Error("must report disabled rather than erroring")
		}
	})
	t.Run("backend error", func(t *testing.T) {
		h := newTraceHarness(t, &fakeTraceReader{enabled: true, getErr: errors.New("boom")})
		h.login(t)
		resp := h.get(t, "/api/traces/t1")
		requireStatus(t, resp, http.StatusBadGateway)
	})
}

func TestTraceEndpoints_RequireAuth(t *testing.T) {
	h := newTraceHarness(t, &fakeTraceReader{enabled: true})
	for _, p := range []string{"/api/traces", "/api/traces/t1", "/api/traces/status"} {
		resp := h.get(t, p)
		requireStatus(t, resp, http.StatusUnauthorized)
	}
}

// strconvItoa is a tiny local helper to avoid another import.
func strconvItoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
