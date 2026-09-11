// Package metrics exposes Prometheus instrumentation for huan-agent.
//
// The package is self contained: it owns a private [prometheus.Registry] and
// never touches the global default registerer, so a process (or a test) can
// build any number of independent [Metrics] instances without them seeing each
// other's series.
//
// Observability is optional by design. Callers may hand a nil *Metrics to code
// that knows how to instrument itself: every Observe*/Inc* method is a safe
// no-op on a nil receiver, and [Metrics.Gatherer] returns an empty gatherer
// instead of nil.
//
// The HTTP layer is deliberately not implemented here: [Metrics.Gatherer] and
// [WriteText] expose the exposition, so the admin server (Hertz) can pick its
// own response writer without this package depending on any server library.
package metrics

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// Metric names, all using the huan_agent_ namespace prefix.
const (
	metricLLMCallsTotal    = "huan_agent_llm_calls_total"
	metricLLMDuration      = "huan_agent_llm_call_duration_seconds"
	metricLLMTokensTotal   = "huan_agent_llm_tokens_total"
	metricToolCallsTotal   = "huan_agent_tool_calls_total"
	metricToolDuration     = "huan_agent_tool_call_duration_seconds"
	metricHTTPRequests     = "huan_agent_http_requests_total"
	metricHTTPDuration     = "huan_agent_http_request_duration_seconds"
	metricRateLimitedTotal = "huan_agent_rate_limited_total"
	metricBuildInfo        = "huan_agent_build_info"
)

// Values used for the status and kind labels.
const (
	// StatusOK labels a call that returned without error.
	StatusOK = "ok"
	// StatusError labels a call that failed.
	StatusError = "error"

	// TokenKindPrompt labels prompt (input) token usage.
	TokenKindPrompt = "prompt"
	// TokenKindCompletion labels completion (output) token usage.
	TokenKindCompletion = "completion"
)

// ContentType is the MIME type of the exposition format produced by
// [WriteText]: "text/plain; version=0.0.4; charset=utf-8".
const ContentType = string(expfmt.FmtText)

// Histogram buckets. LLM calls are slow and highly variable, tool calls are
// usually sub-second, and the admin HTTP surface should be fast, so each
// histogram gets buckets tuned to its own latency profile.
var (
	llmDurationBuckets  = []float64{0.25, 0.5, 1, 2, 3, 5, 8, 13, 21, 34, 55, 90, 120}
	toolDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	httpDurationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
)

// Metrics holds the collectors and their registry. The zero value is not
// usable; build one with [New]. A nil *Metrics is a valid, inert value.
type Metrics struct {
	registry *prometheus.Registry

	llmCalls    *prometheus.CounterVec
	llmDuration *prometheus.HistogramVec
	llmTokens   *prometheus.CounterVec

	toolCalls    *prometheus.CounterVec
	toolDuration *prometheus.HistogramVec

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec

	rateLimited *prometheus.CounterVec
	buildInfo   *prometheus.GaugeVec
}

// New builds the collectors and registers them on a fresh registry. It returns
// a usable Metrics even if registration partially fails (see the error): the
// collectors that did register keep working, and the returned error wraps the
// failure — including prometheus.AlreadyRegisteredError, which is tolerated
// rather than panicking.
func New() (*Metrics, error) {
	return newWithRegistry(prometheus.NewRegistry())
}

// newWithRegistry is New with an explicit registry. It exists so tests can
// drive the duplicate-registration path; production code should call New.
func newWithRegistry(reg *prometheus.Registry) (*Metrics, error) {
	m := newCollectors()
	var errs []error
	for _, c := range m.collectors() {
		err := reg.Register(c)
		if err == nil {
			continue
		}
		// Never fatal and never a panic: an AlreadyRegisteredError (the same
		// descriptor registered twice on one registry) leaves the previously
		// registered collector serving, and any other error only means the
		// remaining collectors still work. The caller is told through the
		// returned error.
		errs = append(errs, fmt.Errorf("metrics: register collector: %w", err))
	}
	m.registry = reg
	return m, errors.Join(errs...)
}

// newCollectors builds every collector without registering anything.
func newCollectors() *Metrics {
	return &Metrics{
		llmCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricLLMCallsTotal,
			Help: "Total number of LLM calls, partitioned by provider, model and outcome status (ok or error).",
		}, []string{"provider", "model", "status"}),
		llmDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricLLMDuration,
			Help:    "Latency of LLM calls in seconds, partitioned by provider and model.",
			Buckets: llmDurationBuckets,
		}, []string{"provider", "model"}),
		llmTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricLLMTokensTotal,
			Help: "Total number of LLM tokens consumed, partitioned by provider, model and token kind (prompt or completion).",
		}, []string{"provider", "model", "kind"}),
		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricToolCallsTotal,
			Help: "Total number of tool invocations, partitioned by tool name and outcome status (ok or error).",
		}, []string{"tool", "status"}),
		toolDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricToolDuration,
			Help:    "Latency of tool invocations in seconds, partitioned by tool name.",
			Buckets: toolDurationBuckets,
		}, []string{"tool"}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricHTTPRequests,
			Help: "Total number of HTTP requests served by the admin server, partitioned by method, route and numeric status code.",
		}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricHTTPDuration,
			Help:    "Latency of HTTP requests served by the admin server in seconds, partitioned by method and route.",
			Buckets: httpDurationBuckets,
		}, []string{"method", "route"}),
		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricRateLimitedTotal,
			Help: "Total number of outbound calls dropped or delayed by a rate limiter, partitioned by component.",
		}, []string{"component"}),
		// A gauge that is always present with a value of 1. Prometheus omits
		// label vectors that have no series yet, so without this a freshly
		// started process would expose an empty /metrics response and an
		// operator could not tell "no traffic yet" from "broken".
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: metricBuildInfo,
			Help: "Build information; the value is always 1 and the version label identifies the running binary.",
		}, []string{"version"}),
	}
}

// SetBuildInfo publishes the running version. It is safe to call repeatedly and
// on a nil receiver.
func (m *Metrics) SetBuildInfo(version string) {
	if m == nil || m.buildInfo == nil {
		return
	}
	if version == "" {
		version = "unknown"
	}
	m.buildInfo.WithLabelValues(version).Set(1)
}

// collectors returns every collector in a stable order.
func (m *Metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.llmCalls,
		m.llmDuration,
		m.llmTokens,
		m.toolCalls,
		m.toolDuration,
		m.httpRequests,
		m.httpDuration,
		m.rateLimited,
		m.buildInfo,
	}
}

// Gatherer exposes the registry so the HTTP layer can serve /metrics without
// importing prometheus itself. The result is never nil: a nil *Metrics yields
// an empty gatherer, which gathers zero families.
func (m *Metrics) Gatherer() prometheus.Gatherer {
	if m == nil || m.registry == nil {
		return prometheus.Gatherers(nil)
	}
	return m.registry
}

// WriteText gathers g and writes every metric family in the Prometheus text
// exposition format (Content-Type: text/plain; version=0.0.4; charset=utf-8,
// i.e. [ContentType]) to w. A nil gatherer or writer is a no-op.
func WriteText(g prometheus.Gatherer, w io.Writer) error {
	if g == nil || w == nil {
		return nil
	}
	families, err := g.Gather()
	if err != nil {
		return fmt.Errorf("metrics: gather: %w", err)
	}
	if len(families) == 0 {
		return nil
	}
	enc := expfmt.NewEncoder(w, expfmt.FmtText)
	for _, mf := range families {
		if err := enc.Encode(mf); err != nil {
			return fmt.Errorf("metrics: encode %q: %w", mf.GetName(), err)
		}
	}
	return nil
}

// ObserveLLMCall records one LLM call: its latency, outcome and token usage.
// err == nil means success. A nil *Metrics is a no-op. Token counts that are
// zero or negative are ignored, so a provider that does not report usage never
// contributes bogus series.
func (m *Metrics) ObserveLLMCall(provider, model string, d time.Duration, err error, promptTokens, completionTokens int) {
	if m == nil {
		return
	}
	status := StatusOK
	if err != nil {
		status = StatusError
	}
	m.llmCalls.WithLabelValues(provider, model, status).Inc()
	m.llmDuration.WithLabelValues(provider, model).Observe(d.Seconds())
	if promptTokens > 0 {
		m.llmTokens.WithLabelValues(provider, model, TokenKindPrompt).Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		m.llmTokens.WithLabelValues(provider, model, TokenKindCompletion).Add(float64(completionTokens))
	}
}

// ObserveToolCall records one tool invocation. A nil *Metrics is a no-op.
func (m *Metrics) ObserveToolCall(tool string, d time.Duration, err error) {
	if m == nil {
		return
	}
	status := StatusOK
	if err != nil {
		status = StatusError
	}
	m.toolCalls.WithLabelValues(tool, status).Inc()
	m.toolDuration.WithLabelValues(tool).Observe(d.Seconds())
}

// ObserveHTTP records one HTTP request served by the admin server, with the
// numeric status code rendered as a string label. A nil *Metrics is a no-op.
func (m *Metrics) ObserveHTTP(method, route string, status int, d time.Duration) {
	if m == nil {
		return
	}
	m.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

// IncRateLimited counts an outbound call dropped/waited by a rate limiter.
// A nil *Metrics is a no-op.
func (m *Metrics) IncRateLimited(component string) {
	if m == nil {
		return
	}
	m.rateLimited.WithLabelValues(component).Inc()
}
