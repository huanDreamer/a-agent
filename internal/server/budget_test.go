package server

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// budgetHarness builds a server whose chat runner has no budget of its own — the
// shape the real server has — so every value a turn runs with has to arrive on
// the request. That makes these tests evidence about the console's control
// rather than about a number captured at construction time.
func budgetHarness(t *testing.T, turns [][]*schema.Message, cfg Config) (*harness, *scriptedModel) {
	t.Helper()

	mdl := &scriptedModel{turns: turns}
	reg := tool.NewRegistry()
	if err := reg.Register(&stubTool{
		name: "noop", desc: "does nothing",
		run: func(context.Context, string) (string, error) { return "ok", nil },
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	runner, err := chat.New(chat.Config{
		Model: mdl, Tools: reg, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	srv, st := buildServerWith(t, buildOpts{
		chat: ChatDeps{Runner: runner, Tools: reg},
	})
	// The defaults under test are the caller's; buildServerWith's own are
	// replaced here so a test can tell "config" from "console".
	srv.cfg.DefaultChatMaxSteps = cfg.DefaultChatMaxSteps
	srv.cfg.DefaultChatMaxTokens = cfg.DefaultChatMaxTokens
	srv.cfg.DefaultChatTurnDeadline = cfg.DefaultChatTurnDeadline
	srv.cfg.ContextMaxTokens = cfg.ContextMaxTokens

	startHarness(t, srv)
	return &harness{
		base:   "http://" + srv.Addr(),
		client: newJar(t),
		srv:    srv,
		store:  st,
	}, mdl
}

// toolCallTurn is one assistant step that asks for a tool, which is how a turn
// keeps looping instead of finishing.
func toolCallTurn(id string) []*schema.Message {
	return []*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
		ID: id, Type: "function",
		Function: schema.FunctionCall{Name: "noop", Arguments: `{}`},
	}}}}
}

// budgetBody is the wire shape of GET /api/chat/budget, limited to what the
// tests assert.
type budgetBody struct {
	MaxSteps          int    `json:"max_steps"`
	TurnMaxTokens     int    `json:"turn_max_tokens"`
	TurnDeadlineSecs  int    `json:"turn_deadline_seconds"`
	DefaultMaxSteps   int    `json:"default_max_steps"`
	SourceMaxSteps    string `json:"source_max_steps"`
	SourceMaxTokens   string `json:"source_turn_max_tokens"`
	SourceDeadline    string `json:"source_turn_deadline_seconds"`
	MaxStepsLimit     int    `json:"max_steps_limit"`
	HasOverride       bool   `json:"has_override"`
	ContextMaxTokens  int    `json:"context_max_tokens"`
	RestartForContext bool   `json:"context_requires_restart"`
}

func TestBudget_RequiresAuth(t *testing.T) {
	// Without a login: the budget decides how many tokens a turn may burn, so it
	// is not a public read.
	h, _ := budgetHarness(t, nil, Config{DefaultChatMaxSteps: 12})
	resp := h.get(t, "/api/chat/budget")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("GET /chat/budget without a login returned 200")
	}
}

func TestBudget_StartsWithConfiguredValues(t *testing.T) {
	h, _ := budgetHarness(t, nil, Config{
		DefaultChatMaxSteps:     12,
		DefaultChatMaxTokens:    400000,
		DefaultChatTurnDeadline: 3 * time.Hour,
	})
	h.login(t)

	var got budgetBody
	h.getJSON(t, "/api/chat/budget", http.StatusOK, &got)

	if got.MaxSteps != 12 || got.TurnMaxTokens != 400000 || got.TurnDeadlineSecs != 10800 {
		t.Errorf("budget = %+v, want the configured 12 / 400000 / 10800", got)
	}
	if got.SourceMaxSteps != budgetFromConfig || got.SourceMaxTokens != budgetFromConfig ||
		got.SourceDeadline != budgetFromConfig {
		t.Errorf("sources = %s / %s / %s, want every one from config",
			got.SourceMaxSteps, got.SourceMaxTokens, got.SourceDeadline)
	}
	if got.HasOverride {
		t.Error("has_override = true with nothing stored")
	}
	if got.DefaultMaxSteps != 12 {
		t.Errorf("default_max_steps = %d, want 12 so 恢复默认 can name it", got.DefaultMaxSteps)
	}
	if got.MaxStepsLimit != MaxConsoleSteps {
		t.Errorf("max_steps_limit = %d, want %d", got.MaxStepsLimit, MaxConsoleSteps)
	}
}

func TestBudget_PutStoresPerFieldOverride(t *testing.T) {
	h, _ := budgetHarness(t, nil, Config{
		DefaultChatMaxSteps:  12,
		DefaultChatMaxTokens: 1000,
		ContextMaxTokens:     60000,
	})
	h.login(t)

	// Only the step cap is written. The other two must keep reporting config as
	// their source, because pinning them to a copy of the startup default is how
	// a later edit of config.yaml would stop being visible.
	resp := h.putJSON(t, "/api/chat/budget", map[string]any{"max_steps": 60})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	var got budgetBody
	h.getJSON(t, "/api/chat/budget", http.StatusOK, &got)
	if got.MaxSteps != 60 {
		t.Errorf("max_steps = %d, want 60", got.MaxSteps)
	}
	if got.SourceMaxSteps != budgetFromConsole {
		t.Errorf("source_max_steps = %q, want %q", got.SourceMaxSteps, budgetFromConsole)
	}
	if got.TurnMaxTokens != 1000 || got.SourceMaxTokens != budgetFromConfig {
		t.Errorf("turn_max_tokens = %d from %q, want the untouched config 1000",
			got.TurnMaxTokens, got.SourceMaxTokens)
	}
	if !got.HasOverride {
		t.Error("has_override = false after a successful write")
	}
	if got.ContextMaxTokens != 60000 || got.RestartForContext {
		t.Errorf("context_max_tokens = %d restart=%v, want 60000 and no restart warning",
			got.ContextMaxTokens, got.RestartForContext)
	}
}

func TestBudget_RejectsValuesItCannotHonour(t *testing.T) {
	h, _ := budgetHarness(t, nil, Config{DefaultChatMaxSteps: 12})
	h.login(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"zero steps has no meaning", map[string]any{"max_steps": 0}},
		{"negative steps", map[string]any{"max_steps": -1}},
		{"steps above the runner's ceiling", map[string]any{"max_steps": MaxConsoleSteps + 1}},
		{"negative token budget", map[string]any{"turn_max_tokens": -1}},
		{"token budget beyond the ceiling", map[string]any{"turn_max_tokens": MaxConsoleTurnTokens + 1}},
		{"negative deadline", map[string]any{"turn_deadline_seconds": -1}},
		{"deadline beyond a day", map[string]any{"turn_deadline_seconds": int(MaxConsoleTurnDeadline/time.Second) + 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.putJSON(t, "/api/chat/budget", tc.body)
			defer func() { _ = resp.Body.Close() }()
			// A rejected value must be a 400 the operator can read, not a silent
			// clamp: a console showing 12 while the runner uses 60 is a worse
			// answer than a message saying the number was refused.
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}

	// None of the rejected bodies was written.
	var got budgetBody
	h.getJSON(t, "/api/chat/budget", http.StatusOK, &got)
	if got.HasOverride || got.MaxSteps != 12 {
		t.Errorf("a rejected write changed the budget: %+v", got)
	}
}

func TestBudget_ResetAllHandsBackToConfig(t *testing.T) {
	h, _ := budgetHarness(t, nil, Config{DefaultChatMaxSteps: 12, DefaultChatMaxTokens: 500})
	h.login(t)

	if resp := h.putJSON(t, "/api/chat/budget", map[string]any{
		"max_steps": 60, "turn_max_tokens": 0, "turn_deadline_seconds": 3600,
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}

	var got budgetBody
	h.getJSON(t, "/api/chat/budget", http.StatusOK, &got)
	// An explicit 0 is a stored choice (unlimited), not an absent key.
	if !got.HasOverride || got.TurnMaxTokens != 0 || got.SourceMaxTokens != budgetFromConsole {
		t.Fatalf("override not stored: %+v", got)
	}

	resp := h.putJSON(t, "/api/chat/budget", map[string]any{"reset_all": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	h.getJSON(t, "/api/chat/budget", http.StatusOK, &got)
	if got.HasOverride {
		t.Error("has_override = true after reset_all")
	}
	if got.MaxSteps != 12 || got.TurnMaxTokens != 500 || got.TurnDeadlineSecs != 0 {
		t.Errorf("after reset = %+v, want the configured 12 / 500 / 0", got)
	}
}

// TestBudget_AppliesToTheNextTurn is the point of the panel: a change made in the
// console has to reach a turn that runs afterwards, with no restart and no
// rebuild of a runner that is already cached. Without this the endpoint would be
// a display that agrees with nothing.
func TestBudget_AppliesToTheNextTurn(t *testing.T) {
	// Every step asks for the same tool, so the loop can only end on its budget.
	turns := make([][]*schema.Message, 8)
	for i := range turns {
		turns[i] = toolCallTurn("c")
	}

	h, _ := budgetHarness(t, turns, Config{DefaultChatMaxSteps: 2})
	h.login(t)
	id := createSession(t, h)

	// The configured 2 steps caps the first turn.
	events := readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages",
		map[string]string{"content": "go"})
	stop, ok := findEvent(events, "budget_stop")
	if !ok {
		t.Fatal("no budget_stop event; the turn did not hit its step cap")
	}
	if reason, _ := stop["reason"].(string); reason != chat.StopSteps {
		t.Fatalf("stop reason = %q, want %q", reason, chat.StopSteps)
	}
	if step := intFromAny(stop["step"]); step != 2 {
		t.Fatalf("stopped at step %d, want the configured 2", step)
	}

	// Raise it in the console and run the same conversation again.
	if resp := h.putJSON(t, "/api/chat/budget", map[string]any{"max_steps": 5}); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}

	events = readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages",
		map[string]string{"content": "继续"})
	stop, ok = findEvent(events, "budget_stop")
	if !ok {
		t.Fatal("no budget_stop event on the second turn")
	}
	// The event carries the step it stopped on, which is how a reader can tell
	// the new ceiling from the old one.
	if step := intFromAny(stop["step"]); step != 5 {
		t.Errorf("stopped at step %v, want the new ceiling of 5", stop["step"])
	}
	// The number the composer shows moved with it.
	var models map[string]any
	h.getJSON(t, "/api/chat/models", http.StatusOK, &models)
	if got := intFromAny(models["max_steps"]); got != 5 {
		t.Errorf("/chat/models max_steps = %v, want 5", models["max_steps"])
	}
}

// TestBudget_TokenBudgetStopsTheTurn covers the second dimension. It needs a
// model that reports usage, because that is the only thing the token budget can
// measure: this test is also the reason the scripted model's silence is
// documented as a limit rather than assumed away.
func TestBudget_TokenBudgetStopsTheTurn(t *testing.T) {
	h := usageReportingHarness(t, Config{DefaultChatMaxSteps: 20, DefaultChatMaxTokens: 1000})
	h.login(t)
	id := createSession(t, h)

	events := readSSE(t, h.client, h.base+"/api/chat/sessions/"+id+"/messages",
		map[string]string{"content": "go"})
	stop, ok := findEvent(events, "budget_stop")
	if !ok {
		t.Fatal("no budget_stop event; the token budget was not enforced")
	}
	if reason, _ := stop["reason"].(string); reason != chat.StopTokens {
		t.Errorf("stop reason = %q, want %q", reason, chat.StopTokens)
	}
	// The budget is checked before the next call, so a 400-token-per-call model
	// against a 1000-token cap stops after the third call, not the fifth.
	if step := intFromAny(stop["step"]); step != 3 {
		t.Errorf("stopped at step %d, want 3", step)
	}
}

// usageReportingHarness is budgetHarness with a model that reports token usage,
// which the token and cost paths need.
func usageReportingHarness(t *testing.T, cfg Config) *harness {
	t.Helper()

	mdl := &usageReportingModel{}
	reg := tool.NewRegistry()
	if err := reg.Register(&stubTool{
		name: "noop", desc: "does nothing",
		run: func(context.Context, string) (string, error) { return "ok", nil },
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	runner, err := chat.New(chat.Config{Model: mdl, Tools: reg, Logger: zap.NewNop()})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	srv, st := buildServerWith(t, buildOpts{chat: ChatDeps{Runner: runner, Tools: reg}})
	srv.cfg.DefaultChatMaxSteps = cfg.DefaultChatMaxSteps
	srv.cfg.DefaultChatMaxTokens = cfg.DefaultChatMaxTokens
	srv.cfg.DefaultChatTurnDeadline = cfg.DefaultChatTurnDeadline
	startHarness(t, srv)
	return &harness{base: "http://" + srv.Addr(), client: newJar(t), srv: srv, store: st}
}

// usageReportingModel always asks for the same tool, so the turn only ends on a
// budget, and reports 400 tokens per call.
type usageReportingModel struct{ calls int }

func (m *usageReportingModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *usageReportingModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, nil
}

func (m *usageReportingModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.calls++
	idx := m.calls
	return schema.StreamReaderFromArray([]*schema.Message{{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID: "c" + strconv.Itoa(idx), Type: "function",
			Function: schema.FunctionCall{Name: "noop", Arguments: `{}`},
		}},
		ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens: 300, CompletionTokens: 100, TotalTokens: 400,
		}},
	}}), nil
}

// TestBudget_StoreErrorFallsBackToConfig covers a failing read: a broken
// database must not stop a conversation, so the turn keeps the configured budget
// and the console keeps answering.
func TestBudget_StoreErrorFallsBackToConfig(t *testing.T) {
	srv := &Server{cfg: Config{DefaultChatMaxSteps: 30}, logger: zap.NewNop()}
	srv.store = failingBudgetStore{}

	got := srv.effectiveBudget(context.Background())
	if got.maxSteps != 30 {
		t.Errorf("maxSteps = %d, want the configured 30 after a store failure", got.maxSteps)
	}
}

// failingBudgetStore fails every accessor, standing in for a database that went
// away mid-session, and embeds the interface so only the methods a test needs
// have to be written out. Every method it does not override panics on the nil
// interface, which is what a double nobody expects to reach should do.
type failingBudgetStore struct {
	store.Store
}

func (failingBudgetStore) GetTurnBudgetOverride(context.Context) (store.TurnBudgetOverride, error) {
	return store.TurnBudgetOverride{}, errStoreUnavailable
}

func (failingBudgetStore) SetTurnBudgetOverride(context.Context, store.TurnBudgetOverride) error {
	return errStoreUnavailable
}

// QueryInvocationTotals fails the same way, so the conversation stats have to
// survive a store that stopped answering.
func (failingBudgetStore) QueryInvocationTotals(context.Context, string) (store.InvocationTotals, error) {
	return store.InvocationTotals{}, errStoreUnavailable
}

var errStoreUnavailable = &storeUnavailableError{}

type storeUnavailableError struct{}

func (*storeUnavailableError) Error() string { return "store unavailable" }

// TestBudget_NilStoreKeepsConfigValues is the degraded path for a server built
// without a store: the resolver must still answer, from config alone.
func TestBudget_NilStoreKeepsConfigValues(t *testing.T) {
	srv := &Server{cfg: Config{DefaultChatMaxSteps: 12}, logger: zap.NewNop()}
	if bs := srv.turnBudgetStore(); bs != nil {
		t.Fatal("a nil store must not produce a budget store")
	}
	b := srv.effectiveBudget(context.Background())
	if b.maxSteps != 12 || b.maxTokens != 0 || b.deadline != 0 {
		t.Errorf("budget = %+v, want 12 / unlimited / unlimited", b)
	}
}

// intFromAny reads a JSON number that may have decoded as float64.
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return -1
}
