package langfuse

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// recorded is one request seen by the fake Langfuse server.
type recorded struct {
	method string
	path   string
	query  url.Values
	auth   string
	ctype  string
	body   []byte
}

// reply computes the response for request number n (0-based).
type reply func(n int, r *http.Request) (status int, body string)

// fake is a minimal in-process Langfuse stand-in. Ingestion answers 207 (the
// multi-status Langfuse really returns) unless reply says otherwise.
type fake struct {
	mu       sync.Mutex
	got      []recorded
	reply    reply
	requests int
}

func newFake(t *testing.T, r reply) (*fake, *httptest.Server) {
	t.Helper()
	f := &fake{reply: r}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.mu.Lock()
	n := len(f.got)
	f.got = append(f.got, recorded{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.Query(),
		auth:   r.Header.Get("Authorization"),
		ctype:  r.Header.Get("Content-Type"),
		body:   body,
	})
	f.requests++
	rep := f.reply
	f.mu.Unlock()

	status, payload := 0, ""
	if rep != nil {
		status, payload = rep(n, r)
	}
	if status == 0 {
		status = http.StatusMultiStatus
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload != "" {
		_, _ = io.WriteString(w, payload)
	}
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func (f *fake) request(i int) recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.got) {
		return recorded{}
	}
	return f.got[i]
}

// waitFor polls cond until it holds or the timeout elapses. It never sleeps a
// fixed long time.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// envelope is one decoded batch item.
type envelope struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Body      json.RawMessage `json:"body"`
}

// decodeBatch decodes the ingestion body of request i.
func decodeBatch(t *testing.T, f *fake, i int) []envelope {
	t.Helper()
	req := f.request(i)
	if req.method != http.MethodPost {
		t.Fatalf("request %d: method = %q, want POST", i, req.method)
	}
	if req.path != ingestPath {
		t.Fatalf("request %d: path = %q, want %q", i, req.path, ingestPath)
	}
	if got := req.ctype; !strings.HasPrefix(got, "application/json") {
		t.Fatalf("request %d: content-type = %q, want application/json", i, got)
	}
	var decoded struct {
		Batch []envelope `json:"batch"`
	}
	if err := json.Unmarshal(req.body, &decoded); err != nil {
		t.Fatalf("request %d: decode batch: %v (body=%s)", i, err, req.body)
	}
	if len(decoded.Batch) == 0 {
		t.Fatalf("request %d: empty batch", i)
	}
	return decoded.Batch
}

// bodyMap decodes an event body as a generic map so tests can assert on the
// keys Langfuse actually receives, mirroring the column names of the API.
func bodyMap(t *testing.T, e envelope) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Body, &m); err != nil {
		t.Fatalf("decode body of %s: %v (body=%s)", e.Type, err, e.Body)
	}
	return m
}

// find returns the first envelope of the given type.
func find(t *testing.T, batch []envelope, typ string) envelope {
	t.Helper()
	for _, e := range batch {
		if e.Type == typ {
			return e
		}
	}
	t.Fatalf("no %s event in batch %v", typ, typesOf(batch))
	return envelope{}
}

// findLast returns the last envelope of the given type (an update may be
// preceded by the create event of the same type).
func findLast(t *testing.T, batch []envelope, typ string) envelope {
	t.Helper()
	var out envelope
	found := false
	for _, e := range batch {
		if e.Type == typ {
			out, found = e, true
		}
	}
	if !found {
		t.Fatalf("no %s event in batch %v", typ, typesOf(batch))
	}
	return out
}

func typesOf(batch []envelope) []string {
	out := make([]string, 0, len(batch))
	for _, e := range batch {
		out = append(out, e.Type)
	}
	return out
}

func wantString(t *testing.T, m map[string]any, key, want string) {
	t.Helper()
	got, _ := m[key].(string)
	if got != want {
		t.Errorf("body[%q] = %v, want %q (body=%v)", key, m[key], want, m)
	}
}

func wantAbsent(t *testing.T, m map[string]any, key string) {
	t.Helper()
	if v, ok := m[key]; ok {
		t.Errorf("body[%q] = %v, want absent", key, v)
	}
}

// disabledConfig returns a config that is missing the API keys.
func disabledConfig(host string) Config {
	return Config{Host: host}
}

func enabledConfig(host string) Config {
	return Config{
		Host:          host,
		PublicKey:     "public",
		SecretKey:     "secret",
		Environment:   "test",
		Release:       "sha-test",
		FlushInterval: 10 * time.Second,
		BatchSize:     512,
		MaxQueue:      4096,
	}
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

func TestConfigEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"empty", Config{}, false},
		{"host only", Config{Host: "https://cloud.langfuse.com"}, false},
		{"missing secret", Config{Host: "https://x", PublicKey: "p"}, false},
		{"missing public", Config{Host: "https://x", SecretKey: "s"}, false},
		{"blank host", Config{Host: "   ", PublicKey: "p", SecretKey: "s"}, false},
		{"blank keys", Config{Host: "https://x", PublicKey: " ", SecretKey: "\n"}, false},
		{"complete", Config{Host: "https://x", PublicKey: "p", SecretKey: "s"}, true},
		{"trailing slash", Config{Host: "https://x/", PublicKey: "p", SecretKey: "s"}, true},
		{"surrounding space", Config{Host: " https://x ", PublicKey: "p", SecretKey: "s"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Enabled(); got != tc.want {
				t.Fatalf("Config.Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", got.Timeout, DefaultTimeout)
	}
	if got.FlushInterval != DefaultFlushInterval {
		t.Errorf("FlushInterval = %v, want %v", got.FlushInterval, DefaultFlushInterval)
	}
	if got.BatchSize != DefaultBatchSize {
		t.Errorf("BatchSize = %d, want %d", got.BatchSize, DefaultBatchSize)
	}
	if got.MaxQueue != DefaultMaxQueue {
		t.Errorf("MaxQueue = %d, want %d", got.MaxQueue, DefaultMaxQueue)
	}
	if host := (Config{Host: "https://x///"}).host(); host != "https://x" {
		t.Errorf("host() = %q, want %q", host, "https://x")
	}
	kept := Config{Timeout: time.Second, FlushInterval: time.Minute, BatchSize: 3, MaxQueue: 9}.withDefaults()
	if kept.Timeout != time.Second || kept.FlushInterval != time.Minute || kept.BatchSize != 3 || kept.MaxQueue != 9 {
		t.Errorf("withDefaults clobbered explicit values: %+v", kept)
	}
}

// ---------------------------------------------------------------------------
// Disabled and nil clients
// ---------------------------------------------------------------------------

func TestDisabledClientDoesNothing(t *testing.T) {
	f, srv := newFake(t, nil)
	c := New(disabledConfig(srv.URL), zap.NewNop())
	if c == nil {
		t.Fatal("New returned nil")
	}
	if c.Enabled() {
		t.Fatal("Enabled() = true for an incomplete config")
	}

	ctx := context.Background()
	for name, err := range map[string]error{
		"Trace":      c.Trace(ctx, TraceEvent{Name: "t"}),
		"Span":       c.Span(ctx, SpanEvent{Name: "s"}),
		"Generation": c.Generation(ctx, GenerationEvent{Name: "g"}),
		"EndSpan":    c.EndSpan(ctx, "sp", nil, "", time.Time{}),
		"EndGen":     c.EndGeneration(ctx, "gen", nil, Usage{Input: 1}, "", time.Time{}),
		"EndTrace":   c.EndTrace(ctx, "tr", "out"),
	} {
		if err != nil {
			t.Errorf("%s on disabled client = %v, want nil", name, err)
		}
	}
	if got := c.Stats(); got != (Stats{}) {
		t.Errorf("Stats() = %+v, want zero value", got)
	}
	if err := c.Close(ctx); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Errorf("second Close() = %v, want nil", err)
	}
	if n := f.count(); n != 0 {
		t.Errorf("disabled client made %d HTTP requests, want 0", n)
	}
	if _, err := c.ListTraces(ctx, TraceFilter{}); !errors.Is(err, ErrDisabled) {
		t.Errorf("ListTraces on disabled client = %v, want ErrDisabled", err)
	}
}

func TestNilClient(t *testing.T) {
	var c *Client
	ctx := context.Background()

	if c.Enabled() {
		t.Error("nil Client.Enabled() = true")
	}
	if got := c.Stats(); got != (Stats{}) {
		t.Errorf("nil Client.Stats() = %+v, want zero value", got)
	}
	if err := c.Close(ctx); err != nil {
		t.Errorf("nil Client.Close() = %v, want nil", err)
	}
	for name, err := range map[string]error{
		"Trace":      c.Trace(ctx, TraceEvent{Name: "t"}),
		"Span":       c.Span(ctx, SpanEvent{Name: "s"}),
		"Generation": c.Generation(ctx, GenerationEvent{Name: "g"}),
		"EndSpan":    c.EndSpan(ctx, "sp", "out", "err", time.Now()),
		"EndGen":     c.EndGeneration(ctx, "gen", "out", Usage{Input: 1}, "", time.Now()),
		"EndTrace":   c.EndTrace(ctx, "tr", "out"),
	} {
		if err != nil {
			t.Errorf("nil Client.%s = %v, want nil", name, err)
		}
	}
	traces, err := c.ListTraces(ctx, TraceFilter{})
	if traces != nil || !errors.Is(err, ErrDisabled) {
		t.Errorf("nil Client.ListTraces() = (%v, %v), want (nil, ErrDisabled)", traces, err)
	}
	detail, err := c.GetTrace(ctx, "abc")
	if detail != nil || !errors.Is(err, ErrDisabled) {
		t.Errorf("nil Client.GetTrace() = (%v, %v), want (nil, ErrDisabled)", detail, err)
	}
}

func TestReadMethodsDisabledWithoutHost(t *testing.T) {
	c := New(Config{PublicKey: "p", SecretKey: "s"}, nil) // no host
	if c.Enabled() {
		t.Fatal("Enabled() = true without a host")
	}
	if _, err := c.ListTraces(context.Background(), TraceFilter{Page: 1}); !errors.Is(err, ErrDisabled) {
		t.Errorf("ListTraces = %v, want ErrDisabled", err)
	}
	if _, err := c.GetTrace(context.Background(), "t1"); !errors.Is(err, ErrDisabled) {
		t.Errorf("GetTrace = %v, want ErrDisabled", err)
	}
}

// ---------------------------------------------------------------------------
// Reporting payloads
// ---------------------------------------------------------------------------

func TestIngestionPayloadForCreates(t *testing.T) {
	f, srv := newFake(t, nil)
	cfg := enabledConfig(srv.URL)
	c := New(cfg, zap.NewNop())
	ctx := context.Background()

	start := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	if err := c.Trace(ctx, TraceEvent{
		ID:        "trace-1",
		Name:      "chat.turn",
		UserID:    "user-1",
		SessionID: "sess-1",
		Input:     map[string]any{"question": "hi"},
		Metadata:  map[string]any{"model": "deepseek"},
		Tags:      []string{"prod", "chat"},
		Timestamp: start,
	}); err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if err := c.Span(ctx, SpanEvent{
		ID:                  "span-1",
		TraceID:             "trace-1",
		ParentObservationID: "trace-1",
		Name:                "tool.search",
		Input:               "query",
		Metadata:            map[string]any{"k": "v"},
		StartTime:           start,
	}); err != nil {
		t.Fatalf("Span: %v", err)
	}
	if err := c.Generation(ctx, GenerationEvent{
		ID:                  "gen-1",
		TraceID:             "trace-1",
		ParentObservationID: "span-1",
		Name:                "llm.call",
		Model:               "deepseek-chat",
		ModelParameters:     map[string]any{"temperature": 0.2},
		Input:               []any{map[string]any{"role": "user"}},
		Metadata:            map[string]any{"provider": "deepseek"},
		StartTime:           start,
	}); err != nil {
		t.Fatalf("Generation: %v", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if n := f.count(); n != 1 {
		t.Fatalf("got %d requests, want a single ingestion POST", n)
	}
	batch := decodeBatch(t, f, 0)
	if len(batch) != 3 {
		t.Fatalf("batch has %d events, want 3: %v", len(batch), typesOf(batch))
	}
	idShape := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for i, e := range batch {
		if !idShape.MatchString(e.ID) {
			t.Errorf("event %d envelope id = %q, want 32 lowercase hex chars", i, e.ID)
		}
		if _, err := time.Parse(time.RFC3339, e.Timestamp); err != nil {
			t.Errorf("event %d timestamp %q is not RFC3339: %v", i, e.Timestamp, err)
		}
	}

	// trace-create
	trace := find(t, batch, "trace-create")
	tb := bodyMap(t, trace)
	wantString(t, tb, "id", "trace-1")
	wantString(t, tb, "name", "chat.turn")
	wantString(t, tb, "userId", "user-1")
	wantString(t, tb, "sessionId", "sess-1")
	wantString(t, tb, "release", "sha-test")
	wantString(t, tb, "environment", "test")
	wantString(t, tb, "timestamp", "2026-02-01T10:00:00Z")
	if tags, ok := tb["tags"].([]any); !ok || len(tags) != 2 || tags[0] != "prod" {
		t.Errorf("trace tags = %v, want [prod chat]", tb["tags"])
	}
	if in, ok := tb["input"].(map[string]any); !ok || in["question"] != "hi" {
		t.Errorf("trace input = %v, want {question: hi}", tb["input"])
	}
	if md, ok := tb["metadata"].(map[string]any); !ok || md["model"] != "deepseek" {
		t.Errorf("trace metadata = %v", tb["metadata"])
	}

	// span-create
	span := bodyMap(t, find(t, batch, "span-create"))
	wantString(t, span, "id", "span-1")
	wantString(t, span, "traceId", "trace-1")
	wantString(t, span, "parentObservationId", "trace-1")
	wantString(t, span, "name", "tool.search")
	wantString(t, span, "startTime", "2026-02-01T10:00:00Z")
	wantString(t, span, "input", "query")
	wantAbsent(t, span, "endTime")

	// generation-create
	gen := bodyMap(t, find(t, batch, "generation-create"))
	wantString(t, gen, "id", "gen-1")
	wantString(t, gen, "traceId", "trace-1")
	wantString(t, gen, "parentObservationId", "span-1")
	wantString(t, gen, "model", "deepseek-chat")
	wantString(t, gen, "startTime", "2026-02-01T10:00:00Z")
	if params, ok := gen["modelParameters"].(map[string]any); !ok || params["temperature"] != 0.2 {
		t.Errorf("modelParameters = %v, want temperature 0.2", gen["modelParameters"])
	}
	wantAbsent(t, gen, "usage")
}

func TestIngestionPayloadForUpdates(t *testing.T) {
	f, srv := newFake(t, nil)
	c := New(enabledConfig(srv.URL), zap.NewNop())
	ctx := context.Background()

	start := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 2, 1, 10, 0, 5, 500000000, time.UTC)

	// Create first so the update events can carry the trace id.
	_ = c.Trace(ctx, TraceEvent{ID: "trace-1", Name: "chat", Timestamp: start})
	_ = c.Span(ctx, SpanEvent{ID: "span-1", TraceID: "trace-1", Name: "tool", StartTime: start})
	_ = c.Generation(ctx, GenerationEvent{ID: "gen-1", TraceID: "trace-1", Name: "llm", Model: "m", StartTime: start})
	_ = c.EndSpan(ctx, "span-1", map[string]any{"answer": 42}, "", end)
	_ = c.EndGeneration(ctx, "gen-1", "the answer", Usage{Input: 10, Output: 5, Total: 15}, "provider exploded", end)
	_ = c.EndTrace(ctx, "trace-1", "final answer")

	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := f.count(); n != 1 {
		t.Fatalf("got %d requests, want 1", n)
	}
	batch := decodeBatch(t, f, 0)
	if len(batch) != 6 {
		t.Fatalf("batch has %d events, want 6: %v", len(batch), typesOf(batch))
	}

	spanUpdate := bodyMap(t, find(t, batch, "span-update"))
	wantString(t, spanUpdate, "id", "span-1")
	wantString(t, spanUpdate, "traceId", "trace-1")
	wantString(t, spanUpdate, "endTime", "2026-02-01T10:00:05.5Z")
	if out, ok := spanUpdate["output"].(map[string]any); !ok || out["answer"] != float64(42) {
		t.Errorf("span output = %v", spanUpdate["output"])
	}
	wantAbsent(t, spanUpdate, "level")
	wantAbsent(t, spanUpdate, "statusMessage")

	genUpdate := bodyMap(t, find(t, batch, "generation-update"))
	wantString(t, genUpdate, "id", "gen-1")
	wantString(t, genUpdate, "traceId", "trace-1")
	wantString(t, genUpdate, "endTime", "2026-02-01T10:00:05.5Z")
	wantString(t, genUpdate, "output", "the answer")
	wantString(t, genUpdate, "level", "ERROR")
	wantString(t, genUpdate, "statusMessage", "provider exploded")
	usage, ok := genUpdate["usage"].(map[string]any)
	if !ok {
		t.Fatalf("generation usage = %v, want an object", genUpdate["usage"])
	}
	if usage["input"] != float64(10) || usage["output"] != float64(5) || usage["total"] != float64(15) {
		t.Errorf("usage = %v, want input 10 output 5 total 15", usage)
	}
	if usage["unit"] != "TOKENS" {
		t.Errorf("usage unit = %v, want TOKENS", usage["unit"])
	}

	trace := bodyMap(t, findLast(t, batch, "trace-create"))
	wantString(t, trace, "id", "trace-1")
	wantString(t, trace, "output", "final answer")
}

func TestUsageTotalIsDerived(t *testing.T) {
	if u := newUsageBody(Usage{}); u != nil {
		t.Errorf("empty usage = %+v, want nil", u)
	}
	u := newUsageBody(Usage{Input: 3, Output: 4})
	if u == nil || u.Total != 7 || u.Unit != usageUnitTokens {
		t.Fatalf("derived usage = %+v, want total 7 with unit TOKENS", u)
	}
	if got := newUsageBody(Usage{Total: 9}); got.Total != 9 {
		t.Errorf("explicit total = %d, want 9", got.Total)
	}
}

func TestBasicAuthHeader(t *testing.T) {
	f, srv := newFake(t, nil)
	c := New(Config{
		Host:          srv.URL + "/",
		PublicKey:     "pk-123",
		SecretKey:     "sk-456",
		BatchSize:     1,
		FlushInterval: 10 * time.Second,
		MaxQueue:      8,
	}, zap.NewNop())

	_ = c.Trace(context.Background(), TraceEvent{Name: "auth"})
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-123:sk-456"))
	if got := f.request(0).auth; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	// The trailing slash on Host must not produce a double slash in the path.
	if got := f.request(0).path; got != ingestPath {
		t.Errorf("path = %q, want %q", got, ingestPath)
	}
}

func TestZeroTimesDefaultToNow(t *testing.T) {
	f, srv := newFake(t, nil)
	c := New(enabledConfig(srv.URL), zap.NewNop())
	before := time.Now().Add(-time.Second)
	_ = c.Trace(context.Background(), TraceEvent{Name: "no-time"})
	_ = c.Close(context.Background())

	tb := bodyMap(t, decodeBatch(t, f, 0)[0])
	ts, _ := tb["timestamp"].(string)
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("timestamp %q: %v", ts, err)
	}
	if parsed.Before(before) || parsed.After(time.Now().Add(time.Second)) {
		t.Errorf("timestamp %q is not close to now", ts)
	}
	if parsed.Location() != time.UTC {
		t.Errorf("timestamp %q is not UTC", ts)
	}
}

func TestEmptyIDsAreFilledAndEmptyEndIDsAreNoops(t *testing.T) {
	f, srv := newFake(t, nil)
	c := New(enabledConfig(srv.URL), zap.NewNop())
	if err := c.Trace(context.Background(), TraceEvent{Name: "generated"}); err != nil {
		t.Fatalf("Trace: %v", err)
	}
	// Empty observation ids cannot address anything: they must be ignored.
	if err := c.EndSpan(context.Background(), "", nil, "", time.Now()); err != nil {
		t.Fatalf("EndSpan with empty id: %v", err)
	}
	if err := c.EndGeneration(context.Background(), "", nil, Usage{}, "", time.Now()); err != nil {
		t.Fatalf("EndGeneration with empty id: %v", err)
	}
	if err := c.EndTrace(context.Background(), "", nil); err != nil {
		t.Fatalf("EndTrace with empty id: %v", err)
	}
	_ = c.Close(context.Background())

	batch := decodeBatch(t, f, 0)
	if len(batch) != 1 || batch[0].Type != "trace-create" {
		t.Fatalf("batch = %v, want a single trace-create", typesOf(batch))
	}
	tb := bodyMap(t, batch[0])
	id, _ := tb["id"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Errorf("generated trace id = %q, want 32 lowercase hex chars", id)
	}
}

// ---------------------------------------------------------------------------
// Batching, flushing, failures
// ---------------------------------------------------------------------------

func TestBatchSizeTriggersOneFlush(t *testing.T) {
	f, srv := newFake(t, nil)
	cfg := enabledConfig(srv.URL)
	cfg.BatchSize = 3
	cfg.FlushInterval = time.Hour // only the batch trigger may fire
	c := New(cfg, zap.NewNop())

	for i := 0; i < cfg.BatchSize; i++ {
		if err := c.Trace(context.Background(), TraceEvent{Name: fmt.Sprintf("e%d", i)}); err != nil {
			t.Fatalf("Trace: %v", err)
		}
	}
	waitFor(t, 2*time.Second, "one flush", func() bool { return c.Stats().Flushes == 1 })

	// Give a would-be second flush a chance to appear, then assert there was none.
	time.Sleep(50 * time.Millisecond)
	st := c.Stats()
	if st.Flushes != 1 {
		t.Errorf("Flushes = %d, want 1", st.Flushes)
	}
	if st.Sent != int64(cfg.BatchSize) {
		t.Errorf("Sent = %d, want %d", st.Sent, cfg.BatchSize)
	}
	if st.Queued != 0 {
		t.Errorf("Queued = %d, want 0", st.Queued)
	}
	if n := f.count(); n != 1 {
		t.Fatalf("got %d requests, want 1", n)
	}
	if batch := decodeBatch(t, f, 0); len(batch) != cfg.BatchSize {
		t.Errorf("batch has %d events, want %d", len(batch), cfg.BatchSize)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := f.count(); n != 1 {
		t.Errorf("Close added a request: got %d, want 1", n)
	}
}

func TestCloseFlushesRemaining(t *testing.T) {
	f, srv := newFake(t, nil)
	c := New(enabledConfig(srv.URL), zap.NewNop())

	_ = c.Trace(context.Background(), TraceEvent{Name: "one"})
	_ = c.Trace(context.Background(), TraceEvent{Name: "two"})
	if n := f.count(); n != 0 {
		t.Fatalf("got %d early requests, want 0 before Close", n)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := f.count(); n != 1 {
		t.Fatalf("got %d requests, want 1", n)
	}
	if batch := decodeBatch(t, f, 0); len(batch) != 2 {
		t.Errorf("batch has %d events, want 2", len(batch))
	}
	// Idempotent.
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n := f.count(); n != 1 {
		t.Errorf("second Close sent more: got %d requests, want 1", n)
	}
	// Reporting after Close is a counted no-op, never a panic.
	if err := c.Trace(context.Background(), TraceEvent{Name: "late"}); err != nil {
		t.Fatalf("Trace after Close: %v", err)
	}
	if got := c.Stats().Dropped; got != 1 {
		t.Errorf("Dropped after Close = %d, want 1", got)
	}
}

func TestFlushIntervalTriggersFlush(t *testing.T) {
	f, srv := newFake(t, nil)
	cfg := enabledConfig(srv.URL)
	cfg.BatchSize = 100
	cfg.FlushInterval = 20 * time.Millisecond
	c := New(cfg, zap.NewNop())

	_ = c.Trace(context.Background(), TraceEvent{Name: "interval"})
	waitFor(t, 2*time.Second, "interval flush", func() bool { return f.count() == 1 })
	if got := c.Stats().Sent; got != 1 {
		t.Errorf("Sent = %d, want 1", got)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestQueueOverflowDropsOldest(t *testing.T) {
	f, srv := newFake(t, nil)
	cfg := enabledConfig(srv.URL)
	cfg.MaxQueue = 3
	cfg.BatchSize = 100 // never triggered
	cfg.FlushInterval = time.Hour
	c := New(cfg, nil) // nil logger exercises the defaulting path

	for i := 0; i < 5; i++ {
		if err := c.Trace(context.Background(), TraceEvent{Name: fmt.Sprintf("drop-%d", i)}); err != nil {
			t.Fatalf("Trace: %v", err)
		}
	}
	st := c.Stats()
	if st.Queued != 3 {
		t.Errorf("Queued = %d, want 3", st.Queued)
	}
	if st.Dropped != 2 {
		t.Errorf("Dropped = %d, want 2", st.Dropped)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	batch := decodeBatch(t, f, 0)
	if len(batch) != 3 {
		t.Fatalf("batch has %d events, want 3", len(batch))
	}
	var names []string
	for _, e := range batch {
		m := bodyMap(t, e)
		name, _ := m["name"].(string)
		if name == "drop-0" || name == "drop-1" {
			t.Errorf("oldest event %q survived, want it dropped", name)
		}
		names = append(names, name)
	}
	if strings.Join(names, ",") != "drop-2,drop-3,drop-4" {
		t.Errorf("kept %v, want [drop-2 drop-3 drop-4]", names)
	}
}

func TestIngestFailureCountsFailedAndRecovers(t *testing.T) {
	f, srv := newFake(t, func(n int, _ *http.Request) (int, string) {
		if n < 2 { // first attempt plus the single retry both fail
			return http.StatusInternalServerError, `{"error":"boom"}`
		}
		return http.StatusMultiStatus, ""
	})
	cfg := enabledConfig(srv.URL)
	cfg.BatchSize = 2
	c := New(cfg, zap.NewNop())
	ctx := context.Background()

	_ = c.Trace(ctx, TraceEvent{Name: "bad-1"})
	_ = c.Trace(ctx, TraceEvent{Name: "bad-2"})
	// Wait for the flush to FINISH, not merely to start: the flush counter is
	// incremented before the HTTP request is issued, so waiting on it would
	// observe a half-done flush and read the request counter too early.
	waitFor(t, 2*time.Second, "failed flush", func() bool { return c.Stats().Failed == 2 })

	st := c.Stats()
	if st.Failed != 2 {
		t.Errorf("Failed = %d, want 2", st.Failed)
	}
	if st.Sent != 0 {
		t.Errorf("Sent = %d, want 0", st.Sent)
	}
	if n := f.count(); n != 2 {
		t.Fatalf("got %d requests, want 2 (one attempt + one retry)", n)
	}

	// A later flush must still work.
	_ = c.Trace(ctx, TraceEvent{Name: "good"})
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	final := c.Stats()
	if final.Sent != 1 {
		t.Errorf("Sent = %d, want 1", final.Sent)
	}
	if n := f.count(); n != 3 {
		t.Fatalf("got %d requests, want 3", n)
	}
	if batch := decodeBatch(t, f, 2); len(batch) != 1 {
		t.Errorf("recovery batch has %d events, want 1", len(batch))
	}
}

func TestUnencodableEventCountsFailed(t *testing.T) {
	f, srv := newFake(t, nil)
	cfg := enabledConfig(srv.URL)
	cfg.BatchSize = 1
	c := New(cfg, zap.NewNop())

	// A channel cannot be JSON encoded, so the flush fails without I/O.
	if err := c.Trace(context.Background(), TraceEvent{Name: "bad", Input: make(chan int)}); err != nil {
		t.Fatalf("Trace: %v", err)
	}
	waitFor(t, 2*time.Second, "failed flush", func() bool { return c.Stats().Flushes == 1 })
	if got := c.Stats().Failed; got != 1 {
		t.Errorf("Failed = %d, want 1", got)
	}
	if n := f.count(); n != 0 {
		t.Errorf("got %d requests, want 0", n)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestIngestNetworkFailureIsCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
	}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	cfg := enabledConfig(url)
	cfg.BatchSize = 1
	c := New(cfg, zap.NewNop())
	_ = c.Trace(context.Background(), TraceEvent{Name: "unreachable"})
	waitFor(t, 2*time.Second, "failed flush", func() bool { return c.Stats().Failed == 1 })
	if get, want := c.Stats().Sent, int64(0); get != want {
		t.Errorf("Sent = %d, want %d", get, want)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCloseHonoursContext(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusMultiStatus)
	}))
	t.Cleanup(func() {
		unblock()
		srv.Close()
	})

	cfg := enabledConfig(srv.URL)
	cfg.BatchSize = 1
	c := New(cfg, zap.NewNop())
	_ = c.Trace(context.Background(), TraceEvent{Name: "slow"})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := c.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close with an expired context = %v, want context.DeadlineExceeded", err)
	}

	unblock()
	// Once the sender can finish, the same Close (and a second one) succeeds.
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

func TestCloseWithNilContext(t *testing.T) {
	_, srv := newFake(t, nil)
	c := New(enabledConfig(srv.URL), zap.NewNop())
	_ = c.Trace(context.Background(), TraceEvent{Name: "nil-ctx"})
	// A nil context must be tolerated rather than panicking.
	if err := c.Close(nil); err != nil { //nolint:staticcheck // nil ctx is part of the defensive contract
		t.Fatalf("Close(nil) = %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestConcurrentTraceIsRaceFree(t *testing.T) {
	f, srv := newFake(t, nil)
	cfg := enabledConfig(srv.URL)
	cfg.BatchSize = 4096
	cfg.MaxQueue = 4096
	cfg.FlushInterval = time.Hour
	c := New(cfg, zap.NewNop())

	const goroutines, perGoroutine = 16, 25
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*perGoroutine)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				ctx := context.Background()
				if err := c.Trace(ctx, TraceEvent{ID: NewID(), Name: fmt.Sprintf("g%d-%d", g, i)}); err != nil {
					errCh <- err
				}
				if err := c.Span(ctx, SpanEvent{ID: NewID(), TraceID: NewID(), Name: "s"}); err != nil {
					errCh <- err
				}
				if err := c.EndSpan(ctx, NewID(), "out", "", time.Now()); err != nil {
					errCh <- err
				}
				_ = c.Stats()
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent reporting error: %v", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st := c.Stats()
	if st.Dropped != 0 {
		t.Errorf("Dropped = %d, want 0", st.Dropped)
	}
	want := int64(goroutines * perGoroutine * 3)
	if st.Sent != want {
		t.Errorf("Sent = %d, want %d", st.Sent, want)
	}
	if n := f.count(); n != 1 {
		t.Errorf("got %d requests, want 1", n)
	}
	if batch := decodeBatch(t, f, 0); int64(len(batch)) != want {
		t.Errorf("batch has %d events, want %d", len(batch), want)
	}
}

// ---------------------------------------------------------------------------
// NewID
// ---------------------------------------------------------------------------

func TestNewID(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9a-f]{32}$`)
	seen := make(map[string]struct{}, 2000)
	for i := 0; i < 2000; i++ {
		id := NewID()
		if !shape.MatchString(id) {
			t.Fatalf("NewID() = %q, want 32 lowercase hex chars", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = struct{}{}
	}
}

// brokenReader always fails, standing in for an unavailable system RNG.
type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestNewIDFallsBackWhenEntropyIsUnavailable(t *testing.T) {
	original := idReader
	idReader = brokenReader{}
	t.Cleanup(func() { idReader = original })

	shape := regexp.MustCompile(`^[0-9a-f]{32}$`)
	seen := make(map[string]struct{}, 50)
	for i := 0; i < 50; i++ {
		id := NewID()
		if !shape.MatchString(id) {
			t.Fatalf("fallback NewID() = %q, want 32 lowercase hex chars", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("fallback NewID() repeated %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestTolerantDecodersRejectGarbage(t *testing.T) {
	var summary TraceSummary
	if err := json.Unmarshal([]byte(`{"id":`), &summary); err == nil {
		t.Error("TraceSummary decoded invalid JSON, want an error")
	}
	var observation Observation
	if err := json.Unmarshal([]byte(`nonsense`), &observation); err == nil {
		t.Error("Observation decoded invalid JSON, want an error")
	}
	var detail TraceDetail
	if err := json.Unmarshal([]byte(`[1,2,3]`), &detail); err == nil {
		t.Error("TraceDetail decoded an array, want an error")
	}
}

// ---------------------------------------------------------------------------
// Read side
// ---------------------------------------------------------------------------

const tracesPayload = `{
  "data": [
    {
      "id": "t-1",
      "timestamp": "2026-02-01T10:00:00.000Z",
      "name": "chat.turn",
      "userId": "u-1",
      "sessionId": "s-1",
      "input": {"question": "hi"},
      "output": "hello",
      "tags": ["prod"],
      "latency": 1.25,
      "totalCost": 0.0012,
      "metadata": {"ignored": true},
      "unknownFutureField": {"nested": [1,2,3]}
    },
    {
      "id": "t-2",
      "timestamp": "2026-02-01T11:00:00.000Z",
      "name": "chat.turn",
      "user_id": "u-2",
      "session_id": "s-2",
      "latency": "2.5",
      "total_cost": "0.5",
      "input": null,
      "output": null
    },
    {"id": "t-3"}
  ],
  "meta": {"page": 2, "limit": 7, "totalItems": 3, "totalPages": 1}
}`

func TestListTraces(t *testing.T) {
	f, srv := newFake(t, func(_ int, r *http.Request) (int, string) {
		if r.URL.Path != tracesPath {
			return http.StatusNotFound, `{"error":"not found"}`
		}
		return http.StatusOK, tracesPayload
	})
	c := New(enabledConfig(srv.URL), zap.NewNop())

	traces, err := c.ListTraces(context.Background(), TraceFilter{
		UserID:    "u-1",
		SessionID: "s-1",
		Name:      "chat.turn",
		Page:      2,
		Limit:     7,
	})
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	req := f.request(0)
	if req.method != http.MethodGet {
		t.Errorf("method = %q, want GET", req.method)
	}
	for key, want := range map[string]string{
		"page": "2", "limit": "7", "userId": "u-1", "sessionId": "s-1", "name": "chat.turn",
	} {
		if got := req.query.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
	if req.auth == "" {
		t.Error("read request is missing the Authorization header")
	}

	if len(traces) != 3 {
		t.Fatalf("got %d traces, want 3", len(traces))
	}
	if traces[0].ID != "t-1" || traces[0].Name != "chat.turn" {
		t.Errorf("trace 0 = %+v", traces[0])
	}
	if traces[0].UserID != "u-1" || traces[0].SessionID != "s-1" {
		t.Errorf("camelCase ids not decoded: %+v", traces[0])
	}
	if traces[0].TotalCost != 0.0012 || traces[0].Latency != 1.25 {
		t.Errorf("numbers not decoded: %+v", traces[0])
	}
	if len(traces[0].Tags) != 1 || traces[0].Tags[0] != "prod" {
		t.Errorf("tags = %v", traces[0].Tags)
	}
	var in map[string]any
	if err := json.Unmarshal(traces[0].Input, &in); err != nil || in["question"] != "hi" {
		t.Errorf("input = %s (err %v)", traces[0].Input, err)
	}
	if string(traces[0].Output) != `"hello"` {
		t.Errorf("output = %s", traces[0].Output)
	}
	// snake_case aliases and string-encoded numbers must work too.
	if traces[1].UserID != "u-2" || traces[1].SessionID != "s-2" {
		t.Errorf("snake_case ids not decoded: %+v", traces[1])
	}
	if traces[1].Latency != 2.5 || traces[1].TotalCost != 0.5 {
		t.Errorf("string numbers not decoded: %+v", traces[1])
	}
	// A row with only an id must not fail the whole response.
	if traces[2].ID != "t-3" || traces[2].Name != "" {
		t.Errorf("sparse row = %+v", traces[2])
	}
}

func TestListTracesOmitsEmptyFilters(t *testing.T) {
	f, srv := newFake(t, func(_ int, _ *http.Request) (int, string) {
		return http.StatusOK, `{"data":[],"meta":{"page":1,"limit":50,"totalItems":0,"totalPages":0}}`
	})
	c := New(enabledConfig(srv.URL), zap.NewNop())

	traces, err := c.ListTraces(context.Background(), TraceFilter{})
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	if len(traces) != 0 {
		t.Errorf("traces = %v, want an empty result for an empty data array", traces)
	}
	q := f.request(0).query
	if q.Get("page") != "1" || q.Get("limit") != strconv.Itoa(defaultListLimit) {
		t.Errorf("default paging = %v", q)
	}
	for _, key := range []string{"userId", "sessionId", "name", "fromTimestamp", "toTimestamp"} {
		if _, ok := q[key]; ok {
			t.Errorf("query unexpectedly contains %s=%q", key, q.Get(key))
		}
	}
}

func TestListTracesErrors(t *testing.T) {
	long := strings.Repeat("Z", 4000)
	tests := []struct {
		name    string
		status  int
		body    string
		wantSub string
	}{
		{"server error", http.StatusInternalServerError, `{"error":"kaboom"}`, "500"},
		{"html error page", http.StatusBadGateway, "<html>" + long + "</html>", "502"},
		{"empty body", http.StatusForbidden, "", "403"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newFake(t, func(_ int, _ *http.Request) (int, string) {
				return tc.status, tc.body
			})
			c := New(enabledConfig(srv.URL), zap.NewNop())
			traces, err := c.ListTraces(context.Background(), TraceFilter{})
			if err == nil {
				t.Fatalf("ListTraces = (%v, nil), want an error", traces)
			}
			if traces != nil {
				t.Errorf("traces = %v, want nil on error", traces)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention status %s", err, tc.wantSub)
			}
			if len(err.Error()) > maxErrorBody+120 {
				t.Errorf("error message is %d bytes, want the body truncated near %d", len(err.Error()), maxErrorBody)
			}
			if strings.Contains(err.Error(), strings.Repeat("Z", maxErrorBody+1)) {
				t.Error("error message contains an untruncated body")
			}
		})
	}
}

func TestListTracesDecodeError(t *testing.T) {
	_, srv := newFake(t, func(_ int, _ *http.Request) (int, string) {
		return http.StatusOK, "not json at all"
	})
	c := New(enabledConfig(srv.URL), zap.NewNop())
	if _, err := c.ListTraces(context.Background(), TraceFilter{}); err == nil {
		t.Fatal("ListTraces with a non-JSON body = nil, want a decode error")
	}
}

func TestListTracesNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := New(enabledConfig(url), zap.NewNop())
	if _, err := c.ListTraces(context.Background(), TraceFilter{}); err == nil {
		t.Fatal("ListTraces against a dead server = nil, want an error")
	}
}

const traceDetailPayload = `{
  "id": "t-9",
  "timestamp": "2026-02-01T12:00:00.000Z",
  "name": "chat.turn",
  "userId": "u-9",
  "sessionId": "s-9",
  "input": {"question": "hi"},
  "output": {"answer": "hello"},
  "tags": ["prod"],
  "latency": 3.5,
  "totalCost": 0.02,
  "observations": [
    {
      "id": "o-1",
      "traceId": "t-9",
      "type": "SPAN",
      "name": "tool.search",
      "startTime": "2026-02-01T12:00:00.100Z",
      "endTime": "2026-02-01T12:00:00.200Z",
      "output": "results",
      "latency": 0.1,
      "futureField": true
    },
    {
      "id": "o-2",
      "traceId": "t-9",
      "parentObservationId": "o-1",
      "type": "GENERATION",
      "name": "llm.call",
      "model": "deepseek-chat",
      "startTime": "2026-02-01T12:00:01Z",
      "endTime": "2026-02-01T12:00:03Z",
      "level": "ERROR",
      "statusMessage": "provider exploded",
      "usage": {"input": 12, "output": 3, "total": 15, "unit": "TOKENS", "inputCost": 0.001},
      "latency": 2
    },
    {"id": "o-3", "parent_id": "o-2", "trace_id": "t-9", "start_time": "2026-02-01T12:00:04Z"}
  ]
}`

func TestGetTrace(t *testing.T) {
	f, srv := newFake(t, func(_ int, r *http.Request) (int, string) {
		if r.URL.Path != tracesPath+"/t-9" {
			return http.StatusNotFound, `{"error":"no such trace"}`
		}
		return http.StatusOK, traceDetailPayload
	})
	c := New(enabledConfig(srv.URL), zap.NewNop())

	detail, err := c.GetTrace(context.Background(), "t-9")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if got := f.request(0).path; got != tracesPath+"/t-9" {
		t.Errorf("path = %q", got)
	}
	if detail.ID != "t-9" || detail.UserID != "u-9" || detail.SessionID != "s-9" {
		t.Errorf("summary not decoded: %+v", detail.TraceSummary)
	}
	if detail.TotalCost != 0.02 || detail.Latency != 3.5 {
		t.Errorf("summary numbers not decoded: %+v", detail.TraceSummary)
	}
	if len(detail.Observations) != 3 {
		t.Fatalf("got %d observations, want 3", len(detail.Observations))
	}
	span := detail.Observations[0]
	if span.Type != "SPAN" || span.TraceID != "t-9" || span.Name != "tool.search" {
		t.Errorf("span = %+v", span)
	}
	if span.StartTime != "2026-02-01T12:00:00.100Z" || span.EndTime != "2026-02-01T12:00:00.200Z" {
		t.Errorf("span times = %q / %q", span.StartTime, span.EndTime)
	}
	if span.Latency != 0.1 {
		t.Errorf("span latency = %v", span.Latency)
	}
	gen := detail.Observations[1]
	if gen.ParentID != "o-1" || gen.Model != "deepseek-chat" {
		t.Errorf("generation = %+v", gen)
	}
	if gen.Level != "ERROR" || gen.StatusMessage != "provider exploded" {
		t.Errorf("generation level/status = %q / %q", gen.Level, gen.StatusMessage)
	}
	if gen.Usage == nil {
		t.Fatal("generation usage is nil")
	}
	if gen.Usage.Input != 12 || gen.Usage.Output != 3 || gen.Usage.Total != 15 {
		t.Errorf("usage = %+v", *gen.Usage)
	}
	// snake_case aliases on the last observation.
	last := detail.Observations[2]
	if last.ParentID != "o-2" || last.TraceID != "t-9" || last.StartTime != "2026-02-01T12:00:04Z" {
		t.Errorf("snake_case observation = %+v", last)
	}
}

func TestGetTraceErrors(t *testing.T) {
	_, srv := newFake(t, func(_ int, _ *http.Request) (int, string) {
		return http.StatusNotFound, `{"error":"no such trace"}`
	})
	c := New(enabledConfig(srv.URL), zap.NewNop())
	detail, err := c.GetTrace(context.Background(), "missing")
	if err == nil {
		t.Fatalf("GetTrace = (%v, nil), want an error", detail)
	}
	if detail != nil {
		t.Errorf("detail = %v, want nil on error", detail)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not mention the status", err)
	}
	if _, err := c.GetTrace(context.Background(), ""); err == nil {
		t.Error("GetTrace with an empty id = nil, want an error")
	}
}

func TestUsageTolerantUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Usage
	}{
		{"v2 shape", `{"input":1,"output":2,"total":3,"unit":"TOKENS"}`, Usage{1, 2, 3}},
		{"v1 shape", `{"promptTokens":4,"completionTokens":5,"totalTokens":9}`, Usage{4, 5, 9}},
		{"string numbers", `{"input":"6","output":"7","total":"13"}`, Usage{6, 7, 13}},
		{"empty object", `{}`, Usage{}},
		{"nulls", `{"input":null,"output":2}`, Usage{Output: 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got Usage
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Usage = %+v, want %+v", got, tc.want)
			}
		})
	}
	var bad Usage
	if err := json.Unmarshal([]byte(`"not an object"`), &bad); err == nil {
		t.Error("unmarshalling a string into Usage = nil, want an error")
	}
}

func TestFlexHelpers(t *testing.T) {
	if got := flexString(json.RawMessage(`"x"`)); got != "x" {
		t.Errorf("flexString quoted = %q", got)
	}
	if got := flexString(json.RawMessage(`null`)); got != "" {
		t.Errorf("flexString null = %q", got)
	}
	if got := flexString(nil); got != "" {
		t.Errorf("flexString empty = %q", got)
	}
	if got := flexString(json.RawMessage(`12`)); got != "12" {
		t.Errorf("flexString number = %q", got)
	}
	if got := flexString(json.RawMessage(`"\u00e9"`)); got != "é" {
		t.Errorf("flexString escape = %q", got)
	}
	if got := flexFloat(json.RawMessage(`"1.5"`)); got != 1.5 {
		t.Errorf("flexFloat = %v", got)
	}
	if got := flexFloat(json.RawMessage(`"abc"`)); got != 0 {
		t.Errorf("flexFloat invalid = %v", got)
	}
	if got := flexInt(json.RawMessage(`"7"`)); got != 7 {
		t.Errorf("flexInt = %v", got)
	}
	if got := flexInt(json.RawMessage(`2.9`)); got != 2 {
		t.Errorf("flexInt float = %v", got)
	}
	if got := flexInt(json.RawMessage(`"x"`)); got != 0 {
		t.Errorf("flexInt invalid = %v", got)
	}
}

func TestObservationCacheIsBounded(t *testing.T) {
	c := New(enabledConfig("http://127.0.0.1:1"), nil)
	for i := 0; i < maxRememberedObservations*2; i++ {
		c.remember(fmt.Sprintf("id-%d", i), fmt.Sprintf("trace-%d", i))
	}
	c.mu.Lock()
	size := len(c.obsTrace)
	order := len(c.obsOrder)
	c.mu.Unlock()
	if size != maxRememberedObservations || order != maxRememberedObservations {
		t.Fatalf("cache grew to %d entries (%d ordered), want %d", size, order, maxRememberedObservations)
	}
	if got := c.forget("id-0"); got != "" {
		t.Errorf("evicted id still remembered: %q", got)
	}
	last := fmt.Sprintf("id-%d", maxRememberedObservations*2-1)
	if got := c.forget(last); got != fmt.Sprintf("trace-%d", maxRememberedObservations*2-1) {
		t.Errorf("forget(%s) = %q", last, got)
	}
	if got := c.forget(last); got != "" {
		t.Errorf("second forget(%s) = %q, want empty", last, got)
	}
	if got := c.forget(""); got != "" {
		t.Errorf("forget(\"\") = %q, want empty", got)
	}
	c.remember("", "trace-x") // must not panic or store anything
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
