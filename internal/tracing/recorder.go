// Package tracing records the structure of an agent turn in the agent's own
// database, and reads it back for the admin console.
//
// It is the built-in answer to observability: a turn becomes one trace, every
// model call a GENERATION observation, every tool call a SPAN observation, all
// in the same SQLite file as the conversations they belong to. No external
// service, no network, no account — a fresh install records and displays traces
// with no configuration at all.
//
// Writes are synchronous and bounded. Synchronous, because the remote client
// this replaces buffered only to hide network latency (see internal/langfuse),
// and a local insert has none: buffering would buy nothing while adding a way to
// lose a trace. Bounded, because store.Open pins SQLite to a single connection
// (SetMaxOpenConns(1)), so a write can queue behind another statement.
//
// Every write therefore detaches from the caller's context and carries its own
// short deadline. That is deliberate: a turn's context is cancelled the moment a
// browser disconnects or a run is stopped, and the trace must still be complete,
// exactly as the chat handler persists a partial answer on a fresh context. The
// deadline is what stops that detachment from becoming an unbounded wait — a
// busy database degrades the trace instead of stalling the conversation.
//
// TEMPORARY DEPENDENCY: this package speaks the trace vocabulary that today
// still lives in internal/langfuse (TraceSummary, Observation, TraceDetail,
// Usage, Stats, ErrDisabled). Moving those types into this package is a
// separate, purely mechanical change — the reference count is in the hundreds
// and almost all of it is in that package's own tests — and doing it in the same
// step as the store would bury the behaviour change under a rename. See
// openspec/changes/phase-5c-local-tracing/tasks.md.
package tracing

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/langfuse"
	"github.com/huan/huan-agent/internal/store"
)

// Observation types, as the console's waterfall understands them.
const (
	// typeGeneration marks a model call.
	typeGeneration = "GENERATION"
	// typeSpan marks a tool call.
	typeSpan = "SPAN"
)

// levelError is the observation level the UI colours red. An empty level means
// a normal node, which is what the console's own default already assumes.
const levelError = "ERROR"

const (
	// writeTimeout bounds one store operation. It is deliberately shorter than
	// the driver's busy_timeout (5s, see store.Open) so a lock can never hold a
	// conversation for five seconds. A WAL write takes microseconds, so reaching
	// this bound means something is genuinely wrong, and losing one trace beats
	// stalling a turn.
	writeTimeout = 2 * time.Second
	// maxNameLen caps a recorded label so a runaway name cannot bloat the table
	// or the list rendering.
	maxNameLen = 200
	// stampLayout is the timestamp format served to the console. It is fixed at
	// three fractional digits with a literal Z (every recorded time is UTC)
	// because the browser parses it with `new Date(...)`, and the date-time
	// format browsers are required to accept has exactly that shape.
	stampLayout = "2006-01-02T15:04:05.000Z"
)

// Store is the slice of persistence this package needs. It is declared here
// rather than taken as store.Store so the dependency stays visible and a test
// can substitute it.
type Store interface {
	RecordTrace(ctx context.Context, t store.TraceRow) error
	RecordObservation(ctx context.Context, o store.ObservationRow) error
	EndTrace(ctx context.Context, id, output string, endedAt time.Time) error
	EndObservation(ctx context.Context, id string, end store.ObservationEnd) error
	ListTraces(ctx context.Context, f store.TraceFilter) ([]store.TraceRow, error)
	GetTrace(ctx context.Context, id string) (store.TraceRow, []store.ObservationRow, error)
}

// Recorder writes turns to the store and reads them back. It implements both
// chat.Tracer — the write seam a conversation reports through — and
// server.TraceReader, the read seam the console uses. One type serving both is
// what makes the trace panel work with no second service.
//
// A nil *Recorder, and a Recorder built over a nil Store, are both inert: writes
// do nothing and reads report langfuse.ErrDisabled. That preserves the
// "tracing is optional infrastructure" contract the chat loop relies on, so no
// call site needs a nil check.
type Recorder struct {
	store  Store
	logger *zap.Logger
	// enabled is fixed at construction: it is a property of the wiring, not of
	// the data, so it never has to be re-derived per call.
	enabled bool

	written atomic.Int64
	failed  atomic.Int64
}

// compile-time check that the Recorder is a conversation tracer.
var _ chat.Tracer = (*Recorder)(nil)

// NewRecorder builds a Recorder over a store. A nil store yields a disabled
// recorder rather than a panic, so tracing can be wired optimistically.
func NewRecorder(st Store, logger *zap.Logger) *Recorder {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Recorder{store: st, logger: logger, enabled: st != nil}
}

// Enabled reports whether traces are being recorded.
func (r *Recorder) Enabled() bool { return r != nil && r.enabled }

// Stats reports recording counters in the shape the trace panel renders.
//
// Sent counts rows written (a trace, plus each of its observations) and Failed
// counts writes that did not land. Queued, Dropped and Flushes describe the
// buffered remote client this backend replaces and are always zero.
func (r *Recorder) Stats() langfuse.Stats {
	if r == nil {
		return langfuse.Stats{}
	}
	return langfuse.Stats{
		Enabled: r.enabled,
		Sent:    r.written.Load(),
		Failed:  r.failed.Load(),
	}
}

// Host returns the base URL of an external trace UI, for deep links. The
// built-in store has none, so this is empty and the console renders no link.
func (r *Recorder) Host() string { return "" }

// ---------------------------------------------------------------- writing --

// StartTrace opens a turn and returns its id.
//
// The caller's context is accepted for the interface but deliberately unused;
// see the package comment for why every write detaches.
func (r *Recorder) StartTrace(_ context.Context, info chat.TraceInfo) string {
	if !r.Enabled() {
		return ""
	}
	id := strings.TrimSpace(info.ID)
	if id == "" {
		id = langfuse.NewID()
	}
	wctx, cancel := newWriteCtx()
	defer cancel()
	err := r.store.RecordTrace(wctx, store.TraceRow{
		ID:        id,
		Name:      clip(info.Name),
		SessionID: info.SessionID,
		UserID:    info.UserID,
		Input:     encodeJSON(info.Input),
		StartedAt: time.Now().UTC(),
	})
	r.count(err, "record trace", zap.String("trace_id", id))
	return id
}

// EndTrace closes a turn with its final output.
func (r *Recorder) EndTrace(_ context.Context, traceID string, output any) {
	if !r.Enabled() || strings.TrimSpace(traceID) == "" {
		return
	}
	wctx, cancel := newWriteCtx()
	defer cancel()
	err := r.store.EndTrace(wctx, traceID, encodeJSON(output), time.Now().UTC())
	r.count(err, "end trace", zap.String("trace_id", traceID))
}

// StartSpan opens a tool call.
func (r *Recorder) StartSpan(_ context.Context, info chat.SpanInfo) string {
	if !r.Enabled() || strings.TrimSpace(info.TraceID) == "" {
		return ""
	}
	id := strings.TrimSpace(info.ID)
	if id == "" {
		id = langfuse.NewID()
	}
	wctx, cancel := newWriteCtx()
	defer cancel()
	// The parent is the trace: the conversation loop has no deeper nesting to
	// report, and the waterfall nests by parent id, so attaching to the trace is
	// what puts a tool call under the turn that ran it.
	err := r.store.RecordObservation(wctx, store.ObservationRow{
		ID:        id,
		TraceID:   info.TraceID,
		ParentID:  info.TraceID,
		Type:      typeSpan,
		Name:      clip(info.Name),
		Input:     encodeJSON(info.Input),
		StartedAt: time.Now().UTC(),
	})
	r.count(err, "record span", zap.String("observation_id", id))
	return id
}

// EndSpan closes a tool call. A non-empty errMsg marks it failed, which is what
// colours the node red in the waterfall instead of hiding the failure.
func (r *Recorder) EndSpan(_ context.Context, spanID string, output any, errMsg string) {
	if !r.Enabled() || strings.TrimSpace(spanID) == "" {
		return
	}
	wctx, cancel := newWriteCtx()
	defer cancel()
	err := r.store.EndObservation(wctx, spanID, store.ObservationEnd{
		Output:        encodeJSON(output),
		Level:         levelFor(errMsg),
		StatusMessage: errMsg,
		EndedAt:       time.Now().UTC(),
	})
	r.count(err, "end span", zap.String("observation_id", spanID))
}

// StartGeneration opens a model call.
func (r *Recorder) StartGeneration(_ context.Context, info chat.GenInfo) string {
	if !r.Enabled() || strings.TrimSpace(info.TraceID) == "" {
		return ""
	}
	id := strings.TrimSpace(info.ID)
	if id == "" {
		id = langfuse.NewID()
	}
	wctx, cancel := newWriteCtx()
	defer cancel()
	err := r.store.RecordObservation(wctx, store.ObservationRow{
		ID:        id,
		TraceID:   info.TraceID,
		ParentID:  info.TraceID,
		Type:      typeGeneration,
		Name:      clip(info.Name),
		Model:     clip(info.Model),
		Step:      info.Step,
		Input:     encodeJSON(info.Input),
		StartedAt: time.Now().UTC(),
	})
	r.count(err, "record generation", zap.String("observation_id", id))
	return id
}

// EndGeneration closes a model call with its output and token usage.
func (r *Recorder) EndGeneration(_ context.Context, genID string, output any, usage chat.Usage, errMsg string) {
	if !r.Enabled() || strings.TrimSpace(genID) == "" {
		return
	}
	wctx, cancel := newWriteCtx()
	defer cancel()
	err := r.store.EndObservation(wctx, genID, store.ObservationEnd{
		Output:           encodeJSON(output),
		Level:            levelFor(errMsg),
		StatusMessage:    errMsg,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		EndedAt:          time.Now().UTC(),
	})
	r.count(err, "end generation", zap.String("observation_id", genID))
}

// ----------------------------------------------------------------- reading --

// ListTraces returns recent traces, newest first.
func (r *Recorder) ListTraces(ctx context.Context, f langfuse.TraceFilter) ([]langfuse.TraceSummary, error) {
	if !r.Enabled() {
		return nil, langfuse.ErrDisabled
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	page := f.Page
	if page < 1 {
		page = 1
	}
	rows, err := r.store.ListTraces(ctx, store.TraceFilter{
		SessionID: f.SessionID,
		UserID:    f.UserID,
		Name:      f.Name,
		Limit:     limit,
		Offset:    (page - 1) * limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]langfuse.TraceSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, summaryOf(row))
	}
	return out, nil
}

// GetTrace returns one trace with its observation tree.
func (r *Recorder) GetTrace(ctx context.Context, traceID string) (*langfuse.TraceDetail, error) {
	if !r.Enabled() {
		return nil, langfuse.ErrDisabled
	}
	if strings.TrimSpace(traceID) == "" {
		return nil, langfuse.ErrDisabled
	}
	row, obs, err := r.store.GetTrace(ctx, traceID)
	if err != nil {
		return nil, err
	}
	detail := &langfuse.TraceDetail{
		TraceSummary: summaryOf(row),
		Observations: make([]langfuse.Observation, 0, len(obs)),
	}
	for _, o := range obs {
		detail.Observations = append(detail.Observations, observationOf(o))
	}
	return detail, nil
}

// ------------------------------------------------------------- conversions --

// summaryOf maps a stored trace onto the shape the console already consumes.
//
// TotalCost stays zero: costing a turn needs a pricing table, which lives with
// the CLI's configuration rather than in the store, and reporting nothing is
// better than reporting a wrong number.
func summaryOf(row store.TraceRow) langfuse.TraceSummary {
	out := langfuse.TraceSummary{
		ID:        row.ID,
		Name:      row.Name,
		UserID:    row.UserID,
		SessionID: row.SessionID,
		Input:     rawJSON(row.Input),
		Output:    rawJSON(row.Output),
	}
	if !row.StartedAt.IsZero() {
		out.Timestamp = stamp(row.StartedAt)
	}
	if !row.EndedAt.IsZero() && !row.StartedAt.IsZero() {
		out.Latency = row.EndedAt.Sub(row.StartedAt).Seconds()
	}
	return out
}

// observationOf maps a stored node onto the console's observation shape.
//
// An observation with no end gets no EndTime and no latency, which is how the
// waterfall knows to draw it as still running rather than as an instant.
func observationOf(o store.ObservationRow) langfuse.Observation {
	out := langfuse.Observation{
		ID:            o.ID,
		TraceID:       o.TraceID,
		ParentID:      o.ParentID,
		Type:          o.Type,
		Name:          o.Name,
		Model:         o.Model,
		Input:         rawJSON(o.Input),
		Output:        rawJSON(o.Output),
		Level:         o.Level,
		StatusMessage: o.StatusMessage,
	}
	if !o.StartedAt.IsZero() {
		out.StartTime = stamp(o.StartedAt)
	}
	if !o.EndedAt.IsZero() {
		out.EndTime = stamp(o.EndedAt)
		if !o.StartedAt.IsZero() {
			out.Latency = o.EndedAt.Sub(o.StartedAt).Seconds()
		}
	}
	if o.TotalTokens > 0 || o.PromptTokens > 0 || o.CompletionTokens > 0 {
		out.Usage = &langfuse.Usage{
			Input:  o.PromptTokens,
			Output: o.CompletionTokens,
			Total:  o.TotalTokens,
		}
	}
	return out
}

// ------------------------------------------------------------------ helpers --

// newWriteCtx detaches one store write from the caller's context and bounds it.
func newWriteCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), writeTimeout)
}

// count records the outcome of one write.
//
// A failure is logged and counted, never returned: tracing must not be able to
// fail a conversation, so the caller has no error to handle.
func (r *Recorder) count(err error, what string, fields ...zap.Field) {
	if err == nil {
		r.written.Add(1)
		return
	}
	r.failed.Add(1)
	if r.logger != nil {
		r.logger.Warn("trace "+what+" failed", append(fields, zap.Error(err))...)
	}
}

// levelFor turns a failure message into the observation level the UI colours.
func levelFor(errMsg string) string {
	if strings.TrimSpace(errMsg) == "" {
		return ""
	}
	return levelError
}

// encodeJSON renders an arbitrary input or output as text safe to store and to
// serve. A plain string is encoded as a JSON string rather than passed through,
// so whatever is recorded is always a valid JSON document.
func encodeJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// rawJSON turns a stored document into a json.RawMessage, or nothing.
//
// A RawMessage is emitted verbatim, so returning text that is not valid JSON
// would corrupt the whole response and leave the console unable to parse the
// trace at all. Anything undecodable is therefore re-encoded as a JSON string.
func rawJSON(s string) json.RawMessage {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return b
}

// clip bounds a free-form label without splitting a multi-byte character.
func clip(s string) string {
	if len(s) <= maxNameLen {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxNameLen {
		return s
	}
	return string(runes[:maxNameLen])
}

// stamp formats a recorded time for the console.
func stamp(t time.Time) string { return t.UTC().Format(stampLayout) }
