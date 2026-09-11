// Package langfuse is a small client for the Langfuse observability API.
//
// It writes traces, spans and generations to a Langfuse server through the
// batch ingestion endpoint (POST /api/public/ingestion) and reads them back
// for the admin visualization UI (GET /api/public/traces, .../traces/{id},
// .../observations).
//
// Reporting is asynchronous: [Client.Trace], [Client.Span],
// [Client.Generation] and the End* methods only append to an in-memory buffer
// and return immediately, so they can be called from the chat hot path. A
// background goroutine flushes the buffer when it reaches Config.BatchSize,
// when Config.FlushInterval elapses, or on [Client.Close]. A full buffer drops
// the oldest events and counts them; a failed flush is retried once and then
// counted. Nothing on the reporting path ever blocks the caller on the network.
//
// The package is optional infrastructure. When Config is incomplete,
// [Config.Enabled] and [Client.Enabled] are false, no goroutine is started and
// every reporting method is a no-op returning nil. A nil *Client is also
// usable: reporting methods return nil, read methods return [ErrDisabled] and
// [Client.Stats] returns the zero value.
//
// Field-set tolerance: the exact bodies and responses differ between Langfuse
// versions. Request bodies therefore only carry the fields this client knows
// (all omitempty), and response decoding ignores unknown fields, tolerates
// missing and null fields, accepts both camelCase and snake_case names, and
// never fails a whole response because one optional field is absent.
package langfuse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Langfuse API paths.
const (
	ingestPath = "/api/public/ingestion"
	tracesPath = "/api/public/traces"
)

// Ingestion event types used by this client.
const (
	eventTraceCreate      = "trace-create"
	eventSpanCreate       = "span-create"
	eventSpanUpdate       = "span-update"
	eventGenerationCreate = "generation-create"
	eventGenerationUpdate = "generation-update"
)

// observationLevelError marks a failed observation in Langfuse.
const observationLevelError = "ERROR"

const (
	// maxErrorBody caps how much of an error response body is quoted in an
	// error, so an HTML error page cannot flood the logs or the UI.
	maxErrorBody = 512
	// defaultListLimit is used by ListTraces when TraceFilter.Limit is <= 0.
	defaultListLimit = 50
	// maxRememberedObservations bounds the observation id -> trace id cache
	// used to enrich update events.
	maxRememberedObservations = 256
)

// Client reports traces to Langfuse and reads them back. A nil Client is
// usable: every method is a no-op/zero-value rather than a panic. A Client is
// safe for concurrent use.
type Client struct {
	cfg        Config
	logger     *zap.Logger
	enabled    bool
	baseURL    string
	authHeader string
	httpClient *http.Client

	mu       sync.Mutex
	queue    []ingestEvent
	closed   bool
	obsTrace map[string]string
	obsOrder []string

	wake chan struct{}
	stop chan struct{}
	done chan struct{}

	closeOnce sync.Once

	sent    atomic.Int64
	failed  atomic.Int64
	dropped atomic.Int64
	flushes atomic.Int64
}

// New builds a Client. It returns a non-nil client even when the config is
// incomplete (Enabled() == false), in which case all reporting is a no-op and
// read methods return ErrDisabled. A nil logger falls back to a no-op logger.
func New(cfg Config, logger *zap.Logger) *Client {
	if logger == nil {
		logger = zap.NewNop()
	}
	c := &Client{cfg: cfg.withDefaults(), logger: logger}
	if !c.cfg.Enabled() {
		return c
	}
	c.enabled = true
	c.baseURL = c.cfg.host()
	c.authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.cfg.PublicKey+":"+c.cfg.SecretKey))
	c.httpClient = &http.Client{Timeout: c.cfg.Timeout}
	c.queue = make([]ingestEvent, 0, c.cfg.BatchSize)
	c.obsTrace = make(map[string]string)
	c.wake = make(chan struct{}, 1)
	c.stop = make(chan struct{})
	c.done = make(chan struct{})
	go c.worker()
	return c
}

// Enabled reports whether the client will actually send data.
func (c *Client) Enabled() bool { return c != nil && c.enabled }

// Close flushes pending events and stops the background sender. It is
// idempotent: calling it again (or concurrently) is safe and returns nil once
// the sender has stopped. A nil or disabled Client returns nil immediately.
func (c *Client) Close(ctx context.Context) error {
	if c == nil || !c.enabled {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.stop)
	})
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("langfuse: close: %w", ctx.Err())
	}
}

// Stats reports counters, for the admin UI.
func (c *Client) Stats() Stats {
	if c == nil || !c.enabled {
		return Stats{}
	}
	c.mu.Lock()
	queued := int64(len(c.queue))
	c.mu.Unlock()
	return Stats{
		Queued:  queued,
		Sent:    c.sent.Load(),
		Failed:  c.failed.Load(),
		Dropped: c.dropped.Load(),
		Flushes: c.flushes.Load(),
		Enabled: true,
	}
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

// Trace starts a trace. The returned error is always nil: reporting failures
// are logged and counted by the background sender and never reach the caller's
// main flow.
func (c *Client) Trace(ctx context.Context, e TraceEvent) error {
	if c == nil || !c.enabled {
		return nil
	}
	id := e.ID
	if id == "" {
		id = NewID()
	}
	ts := formatTime(e.Timestamp)
	body := traceBody{
		ID:          id,
		Timestamp:   ts,
		Name:        e.Name,
		UserID:      e.UserID,
		SessionID:   e.SessionID,
		Input:       e.Input,
		Metadata:    e.Metadata,
		Tags:        e.Tags,
		Release:     c.cfg.Release,
		Environment: c.cfg.Environment,
	}
	c.enqueue(ingestEvent{ID: NewID(), Type: eventTraceCreate, Timestamp: ts, Body: body})
	return nil
}

// Span starts a span.
func (c *Client) Span(ctx context.Context, e SpanEvent) error {
	if c == nil || !c.enabled {
		return nil
	}
	id := e.ID
	if id == "" {
		id = NewID()
	}
	start := formatTime(e.StartTime)
	body := spanBody{
		ID:                  id,
		TraceID:             e.TraceID,
		ParentObservationID: e.ParentObservationID,
		Name:                e.Name,
		StartTime:           start,
		Input:               e.Input,
		Metadata:            e.Metadata,
	}
	c.remember(id, e.TraceID)
	c.enqueue(ingestEvent{ID: NewID(), Type: eventSpanCreate, Timestamp: start, Body: body})
	return nil
}

// Generation starts a generation observation.
func (c *Client) Generation(ctx context.Context, e GenerationEvent) error {
	if c == nil || !c.enabled {
		return nil
	}
	id := e.ID
	if id == "" {
		id = NewID()
	}
	start := formatTime(e.StartTime)
	body := generationBody{
		ID:                  id,
		TraceID:             e.TraceID,
		ParentObservationID: e.ParentObservationID,
		Name:                e.Name,
		Model:               e.Model,
		ModelParameters:     e.ModelParameters,
		StartTime:           start,
		Input:               e.Input,
		Metadata:            e.Metadata,
	}
	c.remember(id, e.TraceID)
	c.enqueue(ingestEvent{ID: NewID(), Type: eventGenerationCreate, Timestamp: start, Body: body})
	return nil
}

// EndSpan finishes a span. output may be nil; errMsg marks the span as ERROR.
func (c *Client) EndSpan(ctx context.Context, spanID string, output any, errMsg string, end time.Time) error {
	if c == nil || !c.enabled || spanID == "" {
		return nil
	}
	endTS := formatTime(end)
	body := spanBody{
		ID:            spanID,
		TraceID:       c.forget(spanID),
		EndTime:       endTS,
		Output:        output,
		Level:         levelFor(errMsg),
		StatusMessage: errMsg,
	}
	c.enqueue(ingestEvent{ID: NewID(), Type: eventSpanUpdate, Timestamp: endTS, Body: body})
	return nil
}

// EndGeneration finishes a generation with its output, usage and optional
// error.
func (c *Client) EndGeneration(ctx context.Context, generationID string, output any, usage Usage, errMsg string, end time.Time) error {
	if c == nil || !c.enabled || generationID == "" {
		return nil
	}
	endTS := formatTime(end)
	body := generationBody{
		ID:            generationID,
		TraceID:       c.forget(generationID),
		EndTime:       endTS,
		Output:        output,
		Usage:         newUsageBody(usage),
		Level:         levelFor(errMsg),
		StatusMessage: errMsg,
	}
	c.enqueue(ingestEvent{ID: NewID(), Type: eventGenerationUpdate, Timestamp: endTS, Body: body})
	return nil
}

// EndTrace finishes a trace by writing its output.
//
// The body deliberately omits Timestamp: Langfuse upserts by id, so sending a
// timestamp here would overwrite the trace's ORIGINAL start time with the
// finish time, corrupting both the trace list ordering and any date filter. The
// omitempty tag means the field is left untouched on the server.
func (c *Client) EndTrace(ctx context.Context, traceID string, output any) error {
	if c == nil || !c.enabled || traceID == "" {
		return nil
	}
	ts := formatTime(time.Now())
	body := traceBody{ID: traceID, Output: output}
	c.enqueue(ingestEvent{ID: NewID(), Type: eventTraceCreate, Timestamp: ts, Body: body})
	return nil
}

// levelFor maps an error message to a Langfuse observation level.
func levelFor(errMsg string) string {
	if errMsg == "" {
		return ""
	}
	return observationLevelError
}

// ---------------------------------------------------------------------------
// Read side
// ---------------------------------------------------------------------------

// ListTraces returns recent traces, newest first as Langfuse returns them.
// Only non-empty filter fields become query parameters. A missing data array
// yields an empty slice, never an error.
func (c *Client) ListTraces(ctx context.Context, f TraceFilter) ([]TraceSummary, error) {
	if c == nil || !c.enabled {
		return nil, ErrDisabled
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("limit", strconv.Itoa(limit))
	if f.UserID != "" {
		q.Set("userId", f.UserID)
	}
	if f.SessionID != "" {
		q.Set("sessionId", f.SessionID)
	}
	if f.Name != "" {
		q.Set("name", f.Name)
	}
	var out struct {
		Data []TraceSummary `json:"data"`
	}
	if err := c.getJSON(ctx, "list traces", tracesPath+"?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// GetTrace returns one trace with its observations. A trace without an
// observations array yields an empty slice.
func (c *Client) GetTrace(ctx context.Context, traceID string) (*TraceDetail, error) {
	if c == nil || !c.enabled {
		return nil, ErrDisabled
	}
	if traceID == "" {
		return nil, fmt.Errorf("langfuse: get trace: empty trace id")
	}
	var out TraceDetail
	if err := c.getJSON(ctx, "get trace", tracesPath+"/"+url.PathEscape(traceID), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// getJSON performs a GET against path (which must start with "/") and decodes
// the JSON body into out.
func (c *Client) getJSON(ctx context.Context, op, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("langfuse: %s: %w", op, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", c.authHeader)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("langfuse: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return statusError(op, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("langfuse: %s: decode response: %w", op, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Background sender
// ---------------------------------------------------------------------------

// worker drains the buffer: immediately when it is full, on the flush
// interval, and once more before exiting.
func (c *Client) worker() {
	defer close(c.done)
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			c.flush(context.Background())
			return
		case <-ticker.C:
			c.flush(context.Background())
		case <-c.wake:
			if c.pending() >= c.cfg.BatchSize {
				c.flush(context.Background())
			}
		}
	}
}

// enqueue buffers an event without ever blocking. When the buffer is over
// MaxQueue the oldest events are discarded and counted.
func (c *Client) enqueue(ev ingestEvent) {
	if c == nil || !c.enabled {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.dropped.Add(1)
		c.logger.Debug("langfuse: dropping event, client is closed",
			zap.String("type", ev.Type))
		return
	}
	c.queue = append(c.queue, ev)
	var dropped int
	if extra := len(c.queue) - c.cfg.MaxQueue; extra > 0 {
		dropped = extra
		c.queue = c.queue[:copy(c.queue, c.queue[extra:])]
	}
	full := len(c.queue) >= c.cfg.BatchSize
	c.mu.Unlock()

	if dropped > 0 {
		c.dropped.Add(int64(dropped))
		c.logger.Warn("langfuse: queue full, dropping oldest events",
			zap.Int("dropped", dropped),
			zap.Int("max_queue", c.cfg.MaxQueue))
	}
	if full {
		c.signal()
	}
}

// signal wakes the worker without blocking (a pending wake-up is enough).
func (c *Client) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// pending reports how many events are buffered.
func (c *Client) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// take removes and returns every buffered event as one batch.
func (c *Client) take() []ingestEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		return nil
	}
	batch := c.queue
	c.queue = make([]ingestEvent, 0, c.cfg.BatchSize)
	return batch
}

// flush sends the buffered events in a single ingestion request, retrying once
// on failure. Failures are counted and logged; events are not re-queued.
func (c *Client) flush(ctx context.Context) {
	batch := c.take()
	if len(batch) == 0 {
		return
	}
	c.flushes.Add(1)
	err := c.send(ctx, batch)
	if err != nil {
		// One immediate retry, no backoff, no re-queue.
		err = c.send(ctx, batch)
	}
	if err != nil {
		c.failed.Add(int64(len(batch)))
		c.logger.Warn("langfuse: ingestion failed, dropping events",
			zap.Int("events", len(batch)),
			zap.Error(err))
		return
	}
	c.sent.Add(int64(len(batch)))
}

// send posts one batch to the ingestion endpoint.
func (c *Client) send(ctx context.Context, batch []ingestEvent) error {
	payload, err := json.Marshal(ingestRequest{Batch: batch})
	if err != nil {
		return fmt.Errorf("langfuse: encode batch: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+ingestPath, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("langfuse: ingest: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.authHeader)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("langfuse: ingest: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError("ingest", resp)
	}
	// Drain a little so the connection can be reused; the payload is ignored.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// statusError turns a non-2xx response into an error carrying the status and a
// truncated body.
func statusError(op string, resp *http.Response) error {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
	if readErr != nil {
		body = nil
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > maxErrorBody {
		msg = strings.ToValidUTF8(msg[:maxErrorBody], "") + "..."
	}
	if msg == "" {
		return fmt.Errorf("langfuse: %s: unexpected status %d", op, resp.StatusCode)
	}
	return fmt.Errorf("langfuse: %s: unexpected status %d: %s", op, resp.StatusCode, msg)
}

// ---------------------------------------------------------------------------
// Observation -> trace id cache
//
// Langfuse versions differ on whether an update event must repeat traceId.
// Remembering it at create time lets update events carry it when it is known,
// and keeps update bodies minimal when it is not.
// ---------------------------------------------------------------------------

// remember associates an observation id with its trace id.
func (c *Client) remember(id, traceID string) {
	if id == "" || traceID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.obsTrace[id]; !seen {
		c.obsOrder = append(c.obsOrder, id)
		if extra := len(c.obsOrder) - maxRememberedObservations; extra > 0 {
			for _, old := range c.obsOrder[:extra] {
				delete(c.obsTrace, old)
			}
			c.obsOrder = c.obsOrder[:copy(c.obsOrder, c.obsOrder[extra:])]
		}
	}
	c.obsTrace[id] = traceID
}

// forget returns the remembered trace id for an observation and drops it.
func (c *Client) forget(id string) string {
	if id == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	traceID, ok := c.obsTrace[id]
	if !ok {
		return ""
	}
	delete(c.obsTrace, id)
	for i, v := range c.obsOrder {
		if v == id {
			c.obsOrder = c.obsOrder[:copy(c.obsOrder[i:], c.obsOrder[i+1:])]
			break
		}
	}
	return traceID
}

// Host returns the configured Langfuse base URL, for building deep links from
// the admin UI. It is empty when the client is disabled.
func (c *Client) Host() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}
