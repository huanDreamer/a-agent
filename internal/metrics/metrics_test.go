package metrics

import (
	"bytes"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// metricNames is the full contract of this package: every name that must show
// up in the exposition, with the labels and the type it must carry.
var metricSpecs = map[string]struct {
	labels []string
	kind   dto.MetricType
}{
	"huan_agent_llm_calls_total":               {[]string{"model", "provider", "status"}, dto.MetricType_COUNTER},
	"huan_agent_llm_call_duration_seconds":     {[]string{"model", "provider"}, dto.MetricType_HISTOGRAM},
	"huan_agent_llm_tokens_total":              {[]string{"kind", "model", "provider"}, dto.MetricType_COUNTER},
	"huan_agent_tool_calls_total":              {[]string{"status", "tool"}, dto.MetricType_COUNTER},
	"huan_agent_tool_call_duration_seconds":    {[]string{"tool"}, dto.MetricType_HISTOGRAM},
	"huan_agent_http_requests_total":           {[]string{"method", "route", "status"}, dto.MetricType_COUNTER},
	"huan_agent_http_request_duration_seconds": {[]string{"method", "route"}, dto.MetricType_HISTOGRAM},
	"huan_agent_rate_limited_total":            {[]string{"component"}, dto.MetricType_COUNTER},
}

// touchAll populates at least one series for each of the eight metrics.
func touchAll(m *Metrics) {
	m.ObserveLLMCall("openai", "gpt-4o", 1200*time.Millisecond, nil, 10, 20)
	m.ObserveToolCall("web_search", 30*time.Millisecond, nil)
	m.ObserveHTTP("GET", "/admin/api/status", 200, 3*time.Millisecond)
	m.IncRateLimited("llm")
}

func gather(t *testing.T, m *Metrics) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := m.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather() returned an error: %v", err)
	}
	out := make(map[string]*dto.MetricFamily, len(families))
	for _, mf := range families {
		out[mf.GetName()] = mf
	}
	return out
}

func mustFamily(t *testing.T, families map[string]*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	mf, ok := families[name]
	if !ok {
		names := make([]string, 0, len(families))
		for n := range families {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("metric family %q not gathered; got %v", name, names)
	}
	return mf
}

func matches(metric *dto.Metric, labels map[string]string) bool {
	if len(metric.GetLabel()) != len(labels) {
		return false
	}
	for _, lp := range metric.GetLabel() {
		want, ok := labels[lp.GetName()]
		if !ok || want != lp.GetValue() {
			return false
		}
	}
	return true
}

func findSeries(t *testing.T, mf *dto.MetricFamily, labels map[string]string) *dto.Metric {
	t.Helper()
	for _, metric := range mf.GetMetric() {
		if matches(metric, labels) {
			return metric
		}
	}
	t.Fatalf("no series of %q with labels %v", mf.GetName(), labels)
	return nil
}

func counterValue(t *testing.T, mf *dto.MetricFamily, labels map[string]string) float64 {
	t.Helper()
	metric := findSeries(t, mf, labels)
	if metric.GetCounter() == nil {
		t.Fatalf("metric %q series %v is not a counter", mf.GetName(), labels)
	}
	return metric.GetCounter().GetValue()
}

func histogramOf(t *testing.T, mf *dto.MetricFamily, labels map[string]string) *dto.Histogram {
	t.Helper()
	metric := findSeries(t, mf, labels)
	if metric.GetHistogram() == nil {
		t.Fatalf("metric %q series %v is not a histogram", mf.GetName(), labels)
	}
	return metric.GetHistogram()
}

func labelNamesOf(mf *dto.MetricFamily) []string {
	metrics := mf.GetMetric()
	if len(metrics) == 0 {
		return nil
	}
	names := make([]string, 0, len(metrics[0].GetLabel()))
	for _, lp := range metrics[0].GetLabel() {
		names = append(names, lp.GetName())
	}
	sort.Strings(names)
	return names
}

func TestNewRegistersEveryMetricWithExpectedTypeAndLabels(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatalf("New() returned an error: %v", err)
	}
	touchAll(m)
	families := gather(t, m)

	for name, spec := range metricSpecs {
		mf := mustFamily(t, families, name)
		if got := mf.GetType(); got != spec.kind {
			t.Errorf("%s: type = %v, want %v", name, got, spec.kind)
		}
		if got := labelNamesOf(mf); !equalStrings(got, spec.labels) {
			t.Errorf("%s: labels = %v, want %v", name, got, spec.labels)
		}
		if mf.GetHelp() == "" {
			t.Errorf("%s: help string is empty", name)
		}
	}
	if len(families) != len(metricSpecs) {
		t.Errorf("gathered %d families, want %d", len(families), len(metricSpecs))
	}
	for name := range families {
		if !strings.HasPrefix(name, "huan_agent_") {
			t.Errorf("metric %q does not use the huan_agent_ prefix", name)
		}
	}
}

func TestGathererExposesAtLeastTheEightMetrics(t *testing.T) {
	m := mustNew(t)
	touchAll(m)

	families, err := m.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather() returned an error: %v", err)
	}
	if len(families) < 8 {
		t.Fatalf("Gatherer() gathered %d families, want >= 8", len(families))
	}
}

func TestObserveLLMCallRecordsStatusAndLatency(t *testing.T) {
	m := mustNew(t)

	m.ObserveLLMCall("openai", "gpt-4o", 1500*time.Millisecond, nil, 0, 0)
	m.ObserveLLMCall("openai", "gpt-4o", 2*time.Second, nil, 0, 0)
	m.ObserveLLMCall("openai", "gpt-4o", 500*time.Millisecond, errors.New("boom"), 0, 0)
	m.ObserveLLMCall("deepseek", "v4", time.Second, errors.New("boom"), 0, 0)

	families := gather(t, m)
	calls := mustFamily(t, families, "huan_agent_llm_calls_total")
	if got := counterValue(t, calls, map[string]string{"provider": "openai", "model": "gpt-4o", "status": StatusOK}); got != 2 {
		t.Errorf("ok calls = %v, want 2", got)
	}
	if got := counterValue(t, calls, map[string]string{"provider": "openai", "model": "gpt-4o", "status": StatusError}); got != 1 {
		t.Errorf("error calls = %v, want 1", got)
	}
	if got := counterValue(t, calls, map[string]string{"provider": "deepseek", "model": "v4", "status": StatusError}); got != 1 {
		t.Errorf("deepseek error calls = %v, want 1", got)
	}

	hist := histogramOf(t, mustFamily(t, families, "huan_agent_llm_call_duration_seconds"),
		map[string]string{"provider": "openai", "model": "gpt-4o"})
	if hist.GetSampleCount() != 3 {
		t.Errorf("latency sample count = %d, want 3", hist.GetSampleCount())
	}
	if hist.GetSampleSum() <= 0 {
		t.Errorf("latency sum = %v, want > 0", hist.GetSampleSum())
	}
	if len(hist.GetBucket()) == 0 {
		t.Error("latency histogram has no buckets")
	}
	last := hist.GetBucket()[len(hist.GetBucket())-1]
	if last.GetCumulativeCount() != 3 {
		t.Errorf("last bucket +Inf count = %d, want 3", last.GetCumulativeCount())
	}
	// 0.25s .. 120s buckets are wide enough to actually observe LLM latency.
	if upper := hist.GetBucket()[len(hist.GetBucket())-2].GetUpperBound(); upper < 90 {
		t.Errorf("largest finite bucket = %v, want >= 90s", upper)
	}
}

func TestObserveLLMCallRecordsTokens(t *testing.T) {
	m := mustNew(t)

	m.ObserveLLMCall("openai", "gpt-4o", time.Second, nil, 100, 250)
	m.ObserveLLMCall("openai", "gpt-4o", time.Second, nil, 5, 7)
	// Zero and negative counts must never be recorded.
	m.ObserveLLMCall("openai", "gpt-4o", time.Second, nil, 0, 0)
	m.ObserveLLMCall("openai", "gpt-4o", time.Second, nil, -3, -9)
	m.ObserveLLMCall("openai", "gpt-4o", time.Second, errors.New("boom"), -1, 4)

	families := gather(t, m)
	tokens := mustFamily(t, families, "huan_agent_llm_tokens_total")
	prompt := counterValue(t, tokens, map[string]string{"provider": "openai", "model": "gpt-4o", "kind": TokenKindPrompt})
	completion := counterValue(t, tokens, map[string]string{"provider": "openai", "model": "gpt-4o", "kind": TokenKindCompletion})
	if prompt != 105 {
		t.Errorf("prompt tokens = %v, want 105", prompt)
	}
	if completion != 261 {
		t.Errorf("completion tokens = %v, want 261", completion)
	}
	for _, metric := range tokens.GetMetric() {
		if got := metric.GetCounter().GetValue(); got < 0 {
			t.Errorf("negative token counter recorded: %v", got)
		}
	}
	if len(tokens.GetMetric()) != 2 {
		t.Errorf("token series = %d, want 2", len(tokens.GetMetric()))
	}
}

func TestTokenCountersAreNotCreatedForNonPositiveCounts(t *testing.T) {
	m := mustNew(t)

	m.ObserveLLMCall("openai", "gpt-4o", time.Second, nil, 0, -12)

	// No child was ever created, so the family is not exposed at all: a caller
	// that never reports usage sees no token series instead of zeroes.
	if _, ok := gather(t, m)["huan_agent_llm_tokens_total"]; ok {
		t.Fatal("token counter family must be absent when no positive token count is recorded")
	}
}

func TestObserveToolCallOKAndError(t *testing.T) {
	m := mustNew(t)

	m.ObserveToolCall("web_search", 20*time.Millisecond, nil)
	m.ObserveToolCall("web_search", 40*time.Millisecond, nil)
	m.ObserveToolCall("web_search", 10*time.Millisecond, errors.New("timeout"))
	m.ObserveToolCall("read_file", time.Millisecond, nil)

	families := gather(t, m)
	calls := mustFamily(t, families, "huan_agent_tool_calls_total")
	if got := counterValue(t, calls, map[string]string{"tool": "web_search", "status": StatusOK}); got != 2 {
		t.Errorf("ok tool calls = %v, want 2", got)
	}
	if got := counterValue(t, calls, map[string]string{"tool": "web_search", "status": StatusError}); got != 1 {
		t.Errorf("error tool calls = %v, want 1", got)
	}
	if got := counterValue(t, calls, map[string]string{"tool": "read_file", "status": StatusOK}); got != 1 {
		t.Errorf("read_file calls = %v, want 1", got)
	}

	hist := histogramOf(t, mustFamily(t, families, "huan_agent_tool_call_duration_seconds"), map[string]string{"tool": "web_search"})
	if hist.GetSampleCount() != 3 {
		t.Errorf("tool latency count = %d, want 3", hist.GetSampleCount())
	}
	if hist.GetSampleSum() <= 0 {
		t.Errorf("tool latency sum = %v, want > 0", hist.GetSampleSum())
	}
}

func TestObserveHTTPRecordsMethodRouteAndStatus(t *testing.T) {
	m := mustNew(t)

	m.ObserveHTTP("GET", "/admin/api/status", 200, 2*time.Millisecond)
	m.ObserveHTTP("GET", "/admin/api/status", 200, 4*time.Millisecond)
	m.ObserveHTTP("GET", "/admin/api/status", 500, 9*time.Millisecond)
	m.ObserveHTTP("POST", "/admin/api/config", 204, time.Millisecond)

	families := gather(t, m)
	requests := mustFamily(t, families, "huan_agent_http_requests_total")
	if got := counterValue(t, requests, map[string]string{"method": "GET", "route": "/admin/api/status", "status": "200"}); got != 2 {
		t.Errorf("GET 200 requests = %v, want 2", got)
	}
	if got := counterValue(t, requests, map[string]string{"method": "GET", "route": "/admin/api/status", "status": "500"}); got != 1 {
		t.Errorf("GET 500 requests = %v, want 1", got)
	}
	if got := counterValue(t, requests, map[string]string{"method": "POST", "route": "/admin/api/config", "status": "204"}); got != 1 {
		t.Errorf("POST 204 requests = %v, want 1", got)
	}

	hist := histogramOf(t, mustFamily(t, families, "huan_agent_http_request_duration_seconds"),
		map[string]string{"method": "GET", "route": "/admin/api/status"})
	if hist.GetSampleCount() != 3 {
		t.Errorf("http latency count = %d, want 3", hist.GetSampleCount())
	}
	if hist.GetSampleSum() <= 0 {
		t.Errorf("http latency sum = %v, want > 0", hist.GetSampleSum())
	}
}

func TestIncRateLimitedCountsByComponent(t *testing.T) {
	m := mustNew(t)

	m.IncRateLimited("llm")
	m.IncRateLimited("llm")
	m.IncRateLimited("feishu")

	family := mustFamily(t, gather(t, m), "huan_agent_rate_limited_total")
	if got := counterValue(t, family, map[string]string{"component": "llm"}); got != 2 {
		t.Errorf("llm rate limited = %v, want 2", got)
	}
	if got := counterValue(t, family, map[string]string{"component": "feishu"}); got != 1 {
		t.Errorf("feishu rate limited = %v, want 1", got)
	}
}

func TestNilReceiverIsSafe(t *testing.T) {
	var m *Metrics

	// None of these may panic.
	m.ObserveLLMCall("openai", "gpt-4o", time.Second, nil, 10, 20)
	m.ObserveLLMCall("openai", "gpt-4o", time.Second, errors.New("boom"), -1, -1)
	m.ObserveToolCall("web_search", time.Millisecond, nil)
	m.ObserveToolCall("web_search", time.Millisecond, errors.New("boom"))
	m.ObserveHTTP("GET", "/metrics", 200, time.Millisecond)
	m.IncRateLimited("llm")

	g := m.Gatherer()
	if g == nil {
		t.Fatal("Gatherer() on a nil *Metrics must not return a nil interface")
	}
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("empty Gatherer().Gather() returned an error: %v", err)
	}
	if len(families) != 0 {
		t.Fatalf("nil *Metrics gathered %d families, want 0", len(families))
	}

	var buf bytes.Buffer
	if err := WriteText(g, &buf); err != nil {
		t.Fatalf("WriteText(empty gatherer) returned an error: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("WriteText(empty gatherer) wrote %q, want nothing", buf.String())
	}
}

func TestNewInstancesUseIndependentRegistries(t *testing.T) {
	first := mustNew(t)
	second := mustNew(t)

	first.ObserveLLMCall("openai", "gpt-4o", time.Second, nil, 1, 1)
	first.IncRateLimited("llm")

	firstFamilies := gather(t, first)
	if got := counterValue(t, mustFamily(t, firstFamilies, "huan_agent_llm_calls_total"),
		map[string]string{"provider": "openai", "model": "gpt-4o", "status": StatusOK}); got != 1 {
		t.Errorf("first metrics ok calls = %v, want 1", got)
	}

	// Nothing observed on the first instance may leak into the second.
	secondFamilies := gather(t, second)
	if len(secondFamilies) != 0 {
		t.Fatalf("second instance gathered %d families, want 0 (shared global state?)", len(secondFamilies))
	}

	second.ObserveLLMCall("openai", "gpt-4o", time.Second, nil, 1, 1)
	if got := counterValue(t, mustFamily(t, gather(t, second), "huan_agent_llm_calls_total"),
		map[string]string{"provider": "openai", "model": "gpt-4o", "status": StatusOK}); got != 1 {
		t.Errorf("second metrics ok calls = %v, want 1", got)
	}
	if got := counterValue(t, mustFamily(t, gather(t, first), "huan_agent_llm_calls_total"),
		map[string]string{"provider": "openai", "model": "gpt-4o", "status": StatusOK}); got != 1 {
		t.Errorf("first metrics ok calls changed to %v, want 1", got)
	}
}

func TestNewWithRegistryToleratesDuplicateRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()

	first, err := newWithRegistry(reg)
	if err != nil {
		t.Fatalf("first newWithRegistry() returned an error: %v", err)
	}
	if first.Gatherer() != prometheus.Gatherer(reg) {
		t.Error("Gatherer() must expose the registry that was passed in")
	}

	// Registering the same descriptors again must not panic; it reports a
	// wrapped prometheus.AlreadyRegisteredError and still returns a usable value.
	second, err := newWithRegistry(reg)
	if err == nil {
		t.Fatal("second newWithRegistry() on the same registry must report the clash")
	}
	if second == nil {
		t.Fatal("a partially failing registration must still return a usable *Metrics")
	}
	var already prometheus.AlreadyRegisteredError
	if !errors.As(err, &already) {
		t.Fatalf("error %v does not wrap prometheus.AlreadyRegisteredError", err)
	}
	if !strings.Contains(err.Error(), "metrics: register collector") {
		t.Errorf("error %q is not wrapped with package context", err)
	}

	// The first instance keeps working and keeps serving its series.
	first.IncRateLimited("llm")
	if got := counterValue(t, mustFamily(t, gather(t, first), "huan_agent_rate_limited_total"),
		map[string]string{"component": "llm"}); got != 1 {
		t.Errorf("rate limited = %v, want 1", got)
	}
	// The second instance is inert but must not panic when used.
	second.IncRateLimited("llm")
	second.ObserveLLMCall("openai", "gpt-4o", time.Second, nil, 1, 1)
	second.ObserveToolCall("web_search", time.Millisecond, nil)
	second.ObserveHTTP("GET", "/metrics", 200, time.Millisecond)
}

func TestWriteTextExposition(t *testing.T) {
	if want := "text/plain; version=0.0.4; charset=utf-8"; ContentType != want {
		t.Fatalf("ContentType = %q, want %q", ContentType, want)
	}

	m := mustNew(t)
	touchAll(m)

	var buf bytes.Buffer
	if err := WriteText(m.Gatherer(), &buf); err != nil {
		t.Fatalf("WriteText() returned an error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"# TYPE huan_agent_llm_calls_total counter",
		"# TYPE huan_agent_llm_call_duration_seconds histogram",
		`huan_agent_llm_calls_total{model="gpt-4o",provider="openai",status="ok"} 1`,
		"huan_agent_rate_limited_total",
		"huan_agent_http_requests_total",
		"huan_agent_tool_calls_total",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition does not contain %q:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "\n") {
		t.Error("exposition must end with a newline")
	}

	// A nil gatherer or a nil writer is a no-op.
	if err := WriteText(nil, &buf); err != nil {
		t.Errorf("WriteText(nil gatherer) returned an error: %v", err)
	}
	if err := WriteText(m.Gatherer(), nil); err != nil {
		t.Errorf("WriteText(nil writer) returned an error: %v", err)
	}
}

func TestWriteTextPropagatesErrors(t *testing.T) {
	boom := errors.New("boom")

	if err := WriteText(failingGatherer{err: boom}, io.Discard); !errors.Is(err, boom) {
		t.Errorf("WriteText(failing gatherer) = %v, want it to wrap %v", err, boom)
	}

	m := mustNew(t)
	touchAll(m)
	if err := WriteText(m.Gatherer(), failingWriter{}); !errors.Is(err, errWriteFailed) {
		t.Errorf("WriteText(failing writer) = %v, want it to wrap %v", err, errWriteFailed)
	}
}

func TestMetricsAreSafeForConcurrentUse(t *testing.T) {
	m := mustNew(t)

	const goroutines, iterations = 8, 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				m.ObserveLLMCall("openai", "gpt-4o", time.Second, nil, 1, 2)
				m.ObserveToolCall("web_search", time.Millisecond, nil)
				m.ObserveHTTP("GET", "/metrics", 200, time.Millisecond)
				m.IncRateLimited("llm")
				if err := WriteText(m.Gatherer(), io.Discard); err != nil {
					t.Errorf("WriteText() returned an error: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	families := gather(t, m)
	if got := counterValue(t, mustFamily(t, families, "huan_agent_llm_calls_total"),
		map[string]string{"provider": "openai", "model": "gpt-4o", "status": StatusOK}); got != goroutines*iterations {
		t.Errorf("llm calls = %v, want %d", got, goroutines*iterations)
	}
	if got := counterValue(t, mustFamily(t, families, "huan_agent_llm_tokens_total"),
		map[string]string{"provider": "openai", "model": "gpt-4o", "kind": TokenKindCompletion}); got != 2*goroutines*iterations {
		t.Errorf("completion tokens = %v, want %d", got, 2*goroutines*iterations)
	}
	if got := counterValue(t, mustFamily(t, families, "huan_agent_rate_limited_total"),
		map[string]string{"component": "llm"}); got != goroutines*iterations {
		t.Errorf("rate limited = %v, want %d", got, goroutines*iterations)
	}
	hist := histogramOf(t, mustFamily(t, families, "huan_agent_http_request_duration_seconds"),
		map[string]string{"method": "GET", "route": "/metrics"})
	if want := uint64(goroutines * iterations); hist.GetSampleCount() != want {
		t.Errorf("http latency count = %d, want %d", hist.GetSampleCount(), want)
	}
}

func mustNew(t *testing.T) *Metrics {
	t.Helper()
	m, err := New()
	if err != nil {
		t.Fatalf("New() returned an error: %v", err)
	}
	return m
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var errWriteFailed = errors.New("write failed")

type failingGatherer struct{ err error }

func (g failingGatherer) Gather() ([]*dto.MetricFamily, error) { return nil, g.err }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWriteFailed }
