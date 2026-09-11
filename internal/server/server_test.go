package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huan/huan-agent/internal/store"
)

// adminPassword is the password used by the test harness.
const adminPassword = "correct-horse-battery-staple"

// harness is a running admin server with a cookie-aware client.
type harness struct {
	base   string
	client *http.Client
	srv    *Server
	store  store.Store
}

// newHarness starts a server on an ephemeral port with one admin account and an
// optional usage fixture.
func newHarness(t *testing.T, seed func(store.Store)) *harness {
	t.Helper()
	return newHarnessWithSkillsAndSeed(t, "", seed)
}

// newHarnessWithSkills starts a server whose skills API reads skillsDir.
func newHarnessWithSkills(t *testing.T, skillsDir string) *harness {
	t.Helper()
	return newHarnessWithSkillsAndSeed(t, skillsDir, nil)
}

// newHarnessWithSkillsAndSeed is the shared constructor.
func newHarnessWithSkillsAndSeed(t *testing.T, skillsDir string, seed func(store.Store)) *harness {
	t.Helper()
	statePath := ""
	if skillsDir != "" {
		statePath = filepath.Join(t.TempDir(), "skills.json")
	}
	srv, st := buildServer(t, skillsDir, statePath, seed)
	startHarness(t, srv)
	return &harness{
		base:   "http://" + srv.Addr(),
		client: newJar(t),
		srv:    srv,
		store:  st,
	}
}

// login authenticates the harness client and requires success.
func (h *harness) login(t *testing.T) {
	t.Helper()
	resp := h.postJSON(t, "/api/login", map[string]string{"password": adminPassword})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
}

// get issues a GET and returns the response (body left open for the caller).
func (h *harness) get(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := h.client.Get(h.base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// getJSON issues a GET, requires the status code and decodes the body.
func (h *harness) getJSON(t *testing.T, path string, want int, v any) {
	t.Helper()
	resp := h.get(t, path)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		t.Fatalf("GET %s status = %d, want %d (body: %s)", path, resp.StatusCode, want, buf.String())
	}
	if v == nil {
		return
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("GET %s decode: %v", path, err)
	}
}

// postJSON issues a POST with a JSON body.
func (h *harness) postJSON(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	resp, err := h.client.Post(h.base+path, "application/json", &buf)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// seedUsage inserts a small deterministic fixture.
func seedUsage(st store.Store) {
	ctx := context.Background()
	rows := []store.UsageEvent{
		{UserID: "ou_alice", SessionID: "s1", Provider: "deepseek", Model: "deepseek-chat",
			PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, DurationMs: 100},
		{UserID: "ou_alice", SessionID: "s1", Provider: "deepseek", Model: "deepseek-chat",
			PromptTokens: 500, CompletionTokens: 250, TotalTokens: 750, DurationMs: 80},
		{UserID: "ou_bob", SessionID: "s2", Provider: "qwen", Model: "qwen-plus",
			PromptTokens: 2000, CompletionTokens: 1000, TotalTokens: 3000, DurationMs: 200},
	}
	for _, e := range rows {
		if err := st.RecordUsage(ctx, e); err != nil {
			panic(err)
		}
	}
	if err := st.RecordInvocation(ctx, store.InvocationEvent{
		UserID: "ou_alice", SessionID: "s1", ToolName: "time", Arguments: `{"tz":"UTC"}`,
		Result: "2026-09-06T00:00:00Z", DurationMs: 3,
	}); err != nil {
		panic(err)
	}
	if err := st.RecordInvocation(ctx, store.InvocationEvent{
		UserID: "ou_bob", SessionID: "s2", ToolName: "echo", Arguments: `{}`,
		Err: "boom", DurationMs: 7,
	}); err != nil {
		panic(err)
	}
}

// ---- auth ----

func TestLogin_SuccessAndSessionCookie(t *testing.T) {
	h := newHarness(t, nil)

	resp := h.postJSON(t, "/api/login", map[string]string{"password": adminPassword})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Assert on the raw header: Go's cookiejar deliberately drops the HttpOnly
	// and SameSite attributes when storing, so reading them back from the jar
	// would always look empty.
	raw := resp.Header.Get("Set-Cookie")
	if raw == "" {
		t.Fatal("no Set-Cookie header was sent")
	}
	// Hertz lowercases the attribute names, so compare case-insensitively.
	lower := strings.ToLower(raw)
	for _, want := range []string{
		strings.ToLower(SessionCookieName + "="), "httponly", "samesite=lax", "path=/",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("Set-Cookie = %q, want it to contain %q", raw, want)
		}
	}
	// Secure must NOT be set over plain HTTP, or a localhost login would never
	// send the cookie back.
	if strings.Contains(lower, "secure") {
		t.Errorf("Set-Cookie = %q, must not be Secure over plain HTTP", raw)
	}

	// The cookie must have been stored, and /api/me must accept the session.
	u := mustParseURL(t, h.base+"/api/login")
	stored := false
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name == SessionCookieName && c.Value != "" {
			stored = true
		}
	}
	if !stored {
		t.Error("the session cookie was not stored by the client")
	}

	var me struct {
		Authenticated bool   `json:"authenticated"`
		Username      string `json:"username"`
	}
	h.getJSON(t, "/api/me", http.StatusOK, &me)
	if !me.Authenticated || me.Username != "admin" {
		t.Errorf("/api/me = %+v, want authenticated admin", me)
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.postJSON(t, "/api/login", map[string]string{"password": "wrong"})
	requireStatus(t, resp, http.StatusUnauthorized)

	var me struct {
		Authenticated bool `json:"authenticated"`
	}
	h.getJSON(t, "/api/me", http.StatusOK, &me)
	if me.Authenticated {
		t.Error("a failed login must not create a session")
	}
}

func TestLogin_MalformedBody(t *testing.T) {
	h := newHarness(t, nil)
	resp, err := h.client.Post(h.base+"/api/login", "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	requireStatus(t, resp, http.StatusBadRequest)
}

func TestLogin_ThrottledAfterRepeatedFailures(t *testing.T) {
	h := newHarness(t, nil)
	for i := 0; i < loginMaxFailures; i++ {
		resp := h.postJSON(t, "/api/login", map[string]string{"password": "wrong"})
		requireStatus(t, resp, http.StatusUnauthorized)
	}
	// Even the correct password is refused once throttled.
	resp := h.postJSON(t, "/api/login", map[string]string{"password": adminPassword})
	requireStatus(t, resp, http.StatusUnauthorized)
}

func TestLogout_RevokesSession(t *testing.T) {
	h := newHarness(t, nil)
	h.login(t)

	resp := h.postJSON(t, "/api/logout", nil)
	requireStatus(t, resp, http.StatusOK)

	var me struct {
		Authenticated bool `json:"authenticated"`
	}
	h.getJSON(t, "/api/me", http.StatusOK, &me)
	if me.Authenticated {
		t.Error("session should be revoked after logout")
	}
	// And a protected endpoint must reject the caller again.
	h.getJSON(t, "/api/usage/summary", http.StatusUnauthorized, nil)
}

func TestProtectedEndpoints_RequireSession(t *testing.T) {
	h := newHarness(t, nil)
	protected := []string{
		"/api/meta",
		"/api/usage/summary",
		"/api/usage/by-model",
		"/api/usage/by-provider",
		"/api/usage/by-user",
		"/api/usage/by-day",
		"/api/usage/recent",
		"/api/audit",
		"/api/skills",
	}
	for _, p := range protected {
		t.Run(p, func(t *testing.T) {
			resp := h.get(t, p)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			var body map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["error"] != "unauthorized" {
				t.Errorf("error = %q, want unauthorized", body["error"])
			}
		})
	}
}

// ---- public endpoints ----

func TestHealthAndMeta(t *testing.T) {
	h := newHarness(t, nil)

	var health struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Uptime  int64  `json:"uptime_s"`
	}
	h.getJSON(t, "/api/health", http.StatusOK, &health)
	if !health.OK || health.Version != "test-version" {
		t.Errorf("health = %+v", health)
	}
	if health.Uptime < 0 {
		t.Errorf("uptime = %d, want >= 0", health.Uptime)
	}

	h.login(t)
	var meta struct {
		Provider      string `json:"provider"`
		Model         string `json:"model"`
		MetricsEnable bool   `json:"metrics_enabled"`
	}
	h.getJSON(t, "/api/meta", http.StatusOK, &meta)
	if meta.Provider != "deepseek" || meta.Model != "deepseek-chat" || !meta.MetricsEnable {
		t.Errorf("meta = %+v", meta)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newHarness(t, nil)

	resp := h.get(t, "/metrics")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", ct)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	body := buf.String()
	// With no metrics instance wired, the endpoint still answers usefully.
	if strings.TrimSpace(body) == "" {
		t.Error("metrics body is empty")
	}
}

// ---- usage API ----

func TestUsageSummary(t *testing.T) {
	h := newHarness(t, seedUsage)
	h.login(t)

	var got struct {
		Calls            int64 `json:"calls"`
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		Users            int64 `json:"users"`
		Sessions         int64 `json:"sessions"`
		Cost             struct {
			Total  float64 `json:"total"`
			Priced bool    `json:"priced"`
		} `json:"cost"`
	}
	h.getJSON(t, "/api/usage/summary", http.StatusOK, &got)

	if got.Calls != 3 {
		t.Errorf("calls = %d, want 3", got.Calls)
	}
	if got.PromptTokens != 3500 || got.CompletionTokens != 1750 || got.TotalTokens != 5250 {
		t.Errorf("tokens = %d/%d/%d, want 3500/1750/5250",
			got.PromptTokens, got.CompletionTokens, got.TotalTokens)
	}
	if got.Users != 2 {
		t.Errorf("users = %d, want 2", got.Users)
	}
	if got.Sessions != 2 {
		t.Errorf("sessions = %d, want 2", got.Sessions)
	}
	// The default provider/model is priced, so cost must be non-zero and known.
	if !got.Cost.Priced {
		t.Error("cost.priced = false, want the default model to be priced")
	}
	if got.Cost.Total <= 0 {
		t.Errorf("cost.total = %v, want > 0", got.Cost.Total)
	}
}

func TestUsageByModelAndProvider(t *testing.T) {
	h := newHarness(t, seedUsage)
	h.login(t)

	var byModel struct {
		Rows []struct {
			Key         string `json:"key"`
			TotalTokens int64  `json:"total_tokens"`
			Cost        struct {
				Total float64 `json:"total"`
			} `json:"cost"`
		} `json:"rows"`
	}
	h.getJSON(t, "/api/usage/by-model", http.StatusOK, &byModel)
	if len(byModel.Rows) != 2 {
		t.Fatalf("by-model rows = %d, want 2", len(byModel.Rows))
	}
	// Descending by total tokens: qwen-plus (3000) before deepseek-chat (2250).
	if byModel.Rows[0].Key != "qwen-plus" {
		t.Errorf("first model = %q, want qwen-plus", byModel.Rows[0].Key)
	}
	if byModel.Rows[0].TotalTokens != 3000 {
		t.Errorf("qwen total = %d, want 3000", byModel.Rows[0].TotalTokens)
	}

	var byProvider struct {
		Rows []struct {
			Key  string `json:"key"`
			Cost struct {
				Total float64 `json:"total"`
			} `json:"cost"`
		} `json:"rows"`
	}
	h.getJSON(t, "/api/usage/by-provider", http.StatusOK, &byProvider)
	if len(byProvider.Rows) != 2 {
		t.Fatalf("by-provider rows = %d, want 2", len(byProvider.Rows))
	}
	// deepseek is priced by the table, so its bucket must carry a real cost.
	var deepseekCost float64
	for _, r := range byProvider.Rows {
		if r.Key == "deepseek" {
			deepseekCost = r.Cost.Total
		}
	}
	if deepseekCost <= 0 {
		t.Errorf("deepseek cost = %v, want > 0 (the price table has an entry)", deepseekCost)
	}
}

func TestUsageByUser(t *testing.T) {
	h := newHarness(t, seedUsage)
	h.login(t)

	var got struct {
		Rows []struct {
			Key      string `json:"key"`
			Calls    int64  `json:"calls"`
			Sessions int64  `json:"sessions"`
		} `json:"rows"`
	}
	h.getJSON(t, "/api/usage/by-user", http.StatusOK, &got)
	if len(got.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(got.Rows))
	}
	byKey := map[string]int64{}
	for _, r := range got.Rows {
		byKey[r.Key] = r.Calls
	}
	if byKey["ou_alice"] != 2 || byKey["ou_bob"] != 1 {
		t.Errorf("calls by user = %v, want alice=2 bob=1", byKey)
	}
}

func TestUsageByDay(t *testing.T) {
	h := newHarness(t, seedUsage)
	h.login(t)

	var got struct {
		Rows []struct {
			Day         string `json:"day"`
			Calls       int64  `json:"calls"`
			TotalTokens int64  `json:"total_tokens"`
		} `json:"rows"`
	}
	h.getJSON(t, "/api/usage/by-day", http.StatusOK, &got)
	if len(got.Rows) == 0 {
		t.Fatal("trend is empty; the day bucket did not match driver-written timestamps")
	}
	total := int64(0)
	for _, r := range got.Rows {
		total += r.TotalTokens
		if len(r.Day) != 10 {
			t.Errorf("day = %q, want YYYY-MM-DD", r.Day)
		}
	}
	if total != 5250 {
		t.Errorf("trend token total = %d, want 5250 (all rows must land in a bucket)", total)
	}
}

func TestUsageRecent(t *testing.T) {
	h := newHarness(t, seedUsage)
	h.login(t)

	var got struct {
		Rows []struct {
			ID        int64  `json:"id"`
			UserID    string `json:"user_id"`
			Provider  string `json:"provider"`
			CreatedAt string `json:"created_at"`
		} `json:"rows"`
	}
	h.getJSON(t, "/api/usage/recent?limit=2", http.StatusOK, &got)
	if len(got.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (limit applied)", len(got.Rows))
	}
	if got.Rows[0].ID < got.Rows[1].ID {
		t.Error("recent rows should be newest first")
	}
	for _, r := range got.Rows {
		if r.CreatedAt == "" {
			t.Error("created_at is empty")
		}
		if r.UserID == "" {
			t.Error("user_id is empty; attribution was not persisted")
		}
	}
}

func TestUsageFilters(t *testing.T) {
	h := newHarness(t, seedUsage)
	h.login(t)

	// Filtering by user must narrow the totals.
	var got struct {
		Calls       int64 `json:"calls"`
		TotalTokens int64 `json:"total_tokens"`
	}
	h.getJSON(t, "/api/usage/summary?user=ou_bob", http.StatusOK, &got)
	if got.Calls != 1 || got.TotalTokens != 3000 {
		t.Errorf("summary for ou_bob = %+v, want 1 call / 3000 tokens", got)
	}

	// A window in the distant past must return nothing.
	h.getJSON(t, "/api/usage/summary?since=2000-01-01T00:00:00Z&until=2000-01-02T00:00:00Z",
		http.StatusOK, &got)
	if got.Calls != 0 {
		t.Errorf("calls in a past window = %d, want 0", got.Calls)
	}

	// A malformed timestamp is a client error, not a 500.
	resp := h.get(t, "/api/usage/summary?since=not-a-time")
	requireStatus(t, resp, http.StatusBadRequest)
}

func TestUsageRecent_InvalidLimitFallsBackToDefault(t *testing.T) {
	h := newHarness(t, seedUsage)
	h.login(t)

	var got struct {
		Rows []json.RawMessage `json:"rows"`
	}
	h.getJSON(t, "/api/usage/recent?limit=abc", http.StatusOK, &got)
	if len(got.Rows) != 3 {
		t.Errorf("rows = %d, want all 3 with the default limit", len(got.Rows))
	}
	// A limit above the cap is clamped, not rejected.
	h.getJSON(t, "/api/usage/recent?limit=99999", http.StatusOK, &got)
	if len(got.Rows) != 3 {
		t.Errorf("rows = %d, want 3", len(got.Rows))
	}
}

// ---- audit ----

func TestAudit(t *testing.T) {
	h := newHarness(t, seedUsage)
	h.login(t)

	var got struct {
		Rows []struct {
			ToolName   string `json:"tool_name"`
			UserID     string `json:"user_id"`
			Err        string `json:"err"`
			DurationMs int64  `json:"duration_ms"`
		} `json:"rows"`
	}
	h.getJSON(t, "/api/audit", http.StatusOK, &got)
	if len(got.Rows) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(got.Rows))
	}

	// Filtering by tool narrows the log.
	h.getJSON(t, "/api/audit?tool=time", http.StatusOK, &got)
	if len(got.Rows) != 1 || got.Rows[0].ToolName != "time" {
		t.Errorf("audit filter rows = %+v, want only the time tool", got.Rows)
	}

	// Filtering by user works too.
	h.getJSON(t, "/api/audit?user=ou_bob", http.StatusOK, &got)
	if len(got.Rows) != 1 || got.Rows[0].UserID != "ou_bob" {
		t.Errorf("audit by user = %+v, want only ou_bob", got.Rows)
	}
	if got.Rows[0].Err == "" {
		t.Error("the recorded tool error was not returned")
	}
}

// ---- skills ----

func TestSkills_ListAndToggle(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "daily-summary", "Summarise the day")
	writeSkill(t, dir, "translator", "Translate text")

	h := newHarnessWithSkills(t, dir)
	h.login(t)

	var list struct {
		Skills []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"skills"`
	}
	h.getJSON(t, "/api/skills", http.StatusOK, &list)
	if len(list.Skills) != 2 {
		t.Fatalf("skills = %d, want 2: %+v", len(list.Skills), list.Skills)
	}
	if list.Skills[0].Name != "daily-summary" {
		t.Errorf("skills should be sorted by name, got %q first", list.Skills[0].Name)
	}
	for _, s := range list.Skills {
		if !s.Enabled {
			t.Errorf("skill %q should start enabled", s.Name)
		}
	}

	// Disable one and confirm it sticks.
	resp := h.postJSON(t, "/api/skills/daily-summary", map[string]bool{"enabled": false})
	requireStatus(t, resp, http.StatusOK)
	h.getJSON(t, "/api/skills", http.StatusOK, &list)
	for _, s := range list.Skills {
		if s.Name == "daily-summary" && s.Enabled {
			t.Error("daily-summary should be disabled")
		}
		if s.Name == "translator" && !s.Enabled {
			t.Error("translator should still be enabled")
		}
	}

	// Re-enable it.
	resp = h.postJSON(t, "/api/skills/daily-summary", map[string]bool{"enabled": true})
	requireStatus(t, resp, http.StatusOK)
	h.getJSON(t, "/api/skills", http.StatusOK, &list)
	for _, s := range list.Skills {
		if !s.Enabled {
			t.Errorf("skill %q should be enabled again", s.Name)
		}
	}
}

func TestSkills_UnknownAndBadBody(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "known", "d")

	h := newHarnessWithSkills(t, dir)
	h.login(t)

	resp := h.postJSON(t, "/api/skills/nope", map[string]bool{"enabled": false})
	requireStatus(t, resp, http.StatusNotFound)

	// A body without "enabled" is rejected.
	resp = h.postJSON(t, "/api/skills/known", map[string]string{"other": "x"})
	requireStatus(t, resp, http.StatusBadRequest)
}

// writeSkill creates a minimal skill markdown file.
func writeSkill(t *testing.T, dir, name, desc string) {
	t.Helper()
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\nBody for %s.\n", name, desc, name)
	if err := writeFile(filepath.Join(dir, name+".md"), content); err != nil {
		t.Fatalf("write skill: %v", err)
	}
}

func TestUsageSummary_SumsCostAcrossProviderModelPairs(t *testing.T) {
	// The fixture mixes deepseek-chat (priced) and qwen-plus (not in the price
	// table), so the total must equal the deepseek spend exactly — not a single
	// provider/model lookup, and not zero.
	h := newHarness(t, seedUsage)
	h.login(t)

	var got struct {
		TotalTokens int64 `json:"total_tokens"`
		Cost        struct {
			PromptCost     float64 `json:"prompt_cost"`
			CompletionCost float64 `json:"completion_cost"`
			Total          float64 `json:"total"`
			Priced         bool    `json:"priced"`
		} `json:"cost"`
	}
	h.getJSON(t, "/api/usage/summary", http.StatusOK, &got)

	if !got.Cost.Priced {
		t.Fatal("cost.priced = false; the deepseek-chat rows are in the price table")
	}
	// deepseek-chat totals: 1500 prompt, 750 completion.
	// 1500/1000*0.001 = 0.0015, 750/1000*0.002 = 0.0015.
	const wantPrompt, wantCompletion = 0.0015, 0.0015
	if !almostEqual(got.Cost.PromptCost, wantPrompt) {
		t.Errorf("prompt_cost = %v, want %v", got.Cost.PromptCost, wantPrompt)
	}
	if !almostEqual(got.Cost.CompletionCost, wantCompletion) {
		t.Errorf("completion_cost = %v, want %v", got.Cost.CompletionCost, wantCompletion)
	}
	if !almostEqual(got.Cost.Total, wantPrompt+wantCompletion) {
		t.Errorf("total = %v, want %v", got.Cost.Total, wantPrompt+wantCompletion)
	}
}

func TestUsageSummary_EmptyWindowIsPricedZero(t *testing.T) {
	// With no rows there is nothing to price: the cost must read as a known
	// zero rather than an unpriced "—".
	h := newHarness(t, nil)
	h.login(t)

	var got struct {
		Calls int64 `json:"calls"`
		Cost  struct {
			Total  float64 `json:"total"`
			Priced bool    `json:"priced"`
		} `json:"cost"`
	}
	h.getJSON(t, "/api/usage/summary", http.StatusOK, &got)
	if got.Calls != 0 || got.Cost.Total != 0 {
		t.Errorf("got %+v, want zero calls and zero cost", got)
	}
	if !got.Cost.Priced {
		t.Error("an empty window should report a known zero cost, not unpriced")
	}
}

func TestUsageSummary_UnpricedModelIsNotCounted(t *testing.T) {
	h := newHarness(t, func(st store.Store) {
		if err := st.RecordUsage(context.Background(), store.UsageEvent{
			UserID: "ou_x", SessionID: "s", Provider: "unknown-provider", Model: "mystery-model",
			PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000,
		}); err != nil {
			panic(err)
		}
	})
	h.login(t)

	var got struct {
		Cost struct {
			Total  float64 `json:"total"`
			Priced bool    `json:"priced"`
		} `json:"cost"`
	}
	h.getJSON(t, "/api/usage/summary", http.StatusOK, &got)
	if got.Cost.Priced {
		t.Error("priced = true, but nothing in the window has a configured price")
	}
	if got.Cost.Total != 0 {
		t.Errorf("total = %v, want 0 (nothing priced)", got.Cost.Total)
	}
}

// almostEqual compares floats with a tolerance suitable for money maths.
func almostEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
