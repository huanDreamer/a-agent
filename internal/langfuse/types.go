package langfuse

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// Defaults for Config.
const (
	// DefaultTimeout bounds one HTTP request.
	DefaultTimeout = 10 * time.Second
	// DefaultFlushInterval is how often buffered events are sent.
	DefaultFlushInterval = 2 * time.Second
	// DefaultBatchSize flushes early once this many events are buffered.
	DefaultBatchSize = 20
	// DefaultMaxQueue bounds the buffer; events beyond it are dropped
	// (oldest first) and counted.
	DefaultMaxQueue = 2048
)

// Config configures the client.
type Config struct {
	// Host is the base URL, e.g. "https://cloud.langfuse.com". Empty disables.
	Host string
	// PublicKey and SecretKey are the project API keys (Basic auth).
	PublicKey string
	SecretKey string
	// Environment is reported on every trace (e.g. "production").
	Environment string
	// Release is reported on every trace (e.g. a git sha).
	Release string
	// Timeout bounds one HTTP request. <=0 uses DefaultTimeout (10s).
	Timeout time.Duration
	// FlushInterval is how often buffered events are sent. <=0 uses
	// DefaultFlushInterval (2s).
	FlushInterval time.Duration
	// BatchSize flushes early once this many events are buffered. <=0 uses
	// DefaultBatchSize (20).
	BatchSize int
	// MaxQueue bounds the buffer; events beyond it are dropped (oldest first)
	// and counted. <=0 uses DefaultMaxQueue (2048).
	MaxQueue int
}

// Enabled reports whether the config has enough to talk to Langfuse.
func (c Config) Enabled() bool {
	return c.host() != "" && c.publicKey() != "" && c.secretKey() != ""
}

// host is the normalized base URL: surrounding space and trailing slashes are
// stripped so paths can be appended verbatim.
func (c Config) host() string { return strings.TrimRight(strings.TrimSpace(c.Host), "/") }

func (c Config) publicKey() string { return strings.TrimSpace(c.PublicKey) }

func (c Config) secretKey() string { return strings.TrimSpace(c.SecretKey) }

// withDefaults returns a copy of c where every unset (<= 0) field is replaced
// by its documented default. Strings are trimmed so that a key pasted with a
// trailing newline still works.
func (c Config) withDefaults() Config {
	out := c
	out.Host = c.host()
	out.PublicKey = c.publicKey()
	out.SecretKey = c.secretKey()
	if out.Timeout <= 0 {
		out.Timeout = DefaultTimeout
	}
	if out.FlushInterval <= 0 {
		out.FlushInterval = DefaultFlushInterval
	}
	if out.BatchSize <= 0 {
		out.BatchSize = DefaultBatchSize
	}
	if out.MaxQueue <= 0 {
		out.MaxQueue = DefaultMaxQueue
	}
	return out
}

// ---------------------------------------------------------------------------
// Reporting counters
// ---------------------------------------------------------------------------

// Stats is a snapshot of delivery counters. Queued is the number of events
// currently buffered (not yet sent); Sent and Failed count events (not HTTP
// requests), Dropped counts events discarded because the buffer was full or
// the client was closed, and Flushes counts flush cycles that had a non-empty
// batch to send.
type Stats struct {
	Queued  int64 `json:"queued"`
	Sent    int64 `json:"sent"`
	Failed  int64 `json:"failed"`
	Dropped int64 `json:"dropped"`
	Flushes int64 `json:"flushes"`
	Enabled bool  `json:"enabled"`
}

// ---------------------------------------------------------------------------
// Reporting input types
// ---------------------------------------------------------------------------

// TraceEvent is the input for starting a trace.
type TraceEvent struct {
	ID        string
	Name      string
	UserID    string
	SessionID string
	Input     any
	Metadata  map[string]any
	Tags      []string
	Timestamp time.Time
}

// SpanEvent is the input for starting a span.
type SpanEvent struct {
	ID                  string
	TraceID             string
	ParentObservationID string
	Name                string
	Input               any
	Metadata            map[string]any
	StartTime           time.Time
}

// GenerationEvent is the input for starting an LLM generation observation.
type GenerationEvent struct {
	ID                  string
	TraceID             string
	ParentObservationID string
	Name                string
	Model               string
	ModelParameters     map[string]any
	Input               any
	Metadata            map[string]any
	StartTime           time.Time
}

// Usage is token usage for a generation.
type Usage struct {
	Input  int `json:"input,omitempty"`
	Output int `json:"output,omitempty"`
	Total  int `json:"total,omitempty"`
}

// UnmarshalJSON accepts the several spellings Langfuse has used for token
// usage across versions: input/output/total (v2+), promptTokens/
// completionTokens/totalTokens (v1) and numeric strings. Unknown fields are
// ignored and a value that cannot be understood leaves the field at zero
// rather than failing the whole response.
func (u *Usage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Input      json.RawMessage `json:"input"`
		Output     json.RawMessage `json:"output"`
		Total      json.RawMessage `json:"total"`
		Prompt     json.RawMessage `json:"promptTokens"`
		Completion json.RawMessage `json:"completionTokens"`
		TotalAlt   json.RawMessage `json:"totalTokens"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*u = Usage{
		Input:  flexInt(raw.Input),
		Output: flexInt(raw.Output),
		Total:  flexInt(raw.Total),
	}
	if u.Input == 0 {
		u.Input = flexInt(raw.Prompt)
	}
	if u.Output == 0 {
		u.Output = flexInt(raw.Completion)
	}
	if u.Total == 0 {
		u.Total = flexInt(raw.TotalAlt)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Read side
// ---------------------------------------------------------------------------

// ErrDisabled is returned by read methods when Langfuse is not configured.
var ErrDisabled = errors.New("langfuse: not configured")

// TraceSummary is one row of the trace list.
//
// The JSON tags are the shape the admin UI consumes (snake_case); the Langfuse
// API itself answers with camelCase (userId, sessionId, totalCost), so the
// custom UnmarshalJSON below accepts both spellings. Every field is optional:
// a missing or unknown field never fails the response.
type TraceSummary struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp,omitempty"`
	Name      string          `json:"name,omitempty"`
	UserID    string          `json:"user_id,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Tags      []string        `json:"tags,omitempty"`
	Latency   float64         `json:"latency,omitempty"`
	TotalCost float64         `json:"total_cost,omitempty"`
}

// UnmarshalJSON decodes a trace object tolerantly. It accepts the snake_case
// aliases above as well as Langfuse's camelCase names, and it reads numbers
// that a version may have rendered as strings. Anything it cannot understand
// is ignored instead of aborting the whole list.
func (t *TraceSummary) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID        string          `json:"id"`
		Timestamp json.RawMessage `json:"timestamp"`
		Name      string          `json:"name"`
		UserID    string          `json:"user_id"`
		SessionID string          `json:"session_id"`
		Input     json.RawMessage `json:"input"`
		Output    json.RawMessage `json:"output"`
		Tags      []string        `json:"tags"`
		Latency   json.RawMessage `json:"latency"`
		TotalCost json.RawMessage `json:"total_cost"`

		UserIDAlt    string          `json:"userId"`
		SessionIDAlt string          `json:"sessionId"`
		TotalCostAlt json.RawMessage `json:"totalCost"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := TraceSummary{
		ID:        raw.ID,
		Timestamp: flexString(raw.Timestamp),
		Name:      raw.Name,
		UserID:    raw.UserID,
		SessionID: raw.SessionID,
		Input:     raw.Input,
		Output:    raw.Output,
		Tags:      raw.Tags,
		Latency:   flexFloat(raw.Latency),
		TotalCost: flexFloat(raw.TotalCost),
	}
	if out.UserID == "" {
		out.UserID = raw.UserIDAlt
	}
	if out.SessionID == "" {
		out.SessionID = raw.SessionIDAlt
	}
	if out.TotalCost == 0 {
		out.TotalCost = flexFloat(raw.TotalCostAlt)
	}
	*t = out
	return nil
}

// Observation is one node in a trace tree.
//
// As with TraceSummary, the JSON tags are the UI-facing shape (snake_case)
// while the Langfuse API uses camelCase (traceId, parentObservationId,
// startTime, statusMessage); UnmarshalJSON accepts both. All fields are
// optional and unknown fields are ignored.
type Observation struct {
	ID            string          `json:"id"`
	TraceID       string          `json:"trace_id,omitempty"`
	ParentID      string          `json:"parent_id,omitempty"`
	Type          string          `json:"type,omitempty"`
	Name          string          `json:"name,omitempty"`
	Model         string          `json:"model,omitempty"`
	StartTime     string          `json:"start_time,omitempty"`
	EndTime       string          `json:"end_time,omitempty"`
	Input         json.RawMessage `json:"input,omitempty"`
	Output        json.RawMessage `json:"output,omitempty"`
	Level         string          `json:"level,omitempty"`
	StatusMessage string          `json:"status_message,omitempty"`
	Latency       float64         `json:"latency,omitempty"`
	Usage         *Usage          `json:"usage,omitempty"`
}

// UnmarshalJSON decodes an observation tolerantly; see TraceSummary.
func (o *Observation) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID            string          `json:"id"`
		TraceID       string          `json:"trace_id"`
		ParentID      string          `json:"parent_id"`
		Type          string          `json:"type"`
		Name          string          `json:"name"`
		Model         string          `json:"model"`
		StartTime     json.RawMessage `json:"start_time"`
		EndTime       json.RawMessage `json:"end_time"`
		Input         json.RawMessage `json:"input"`
		Output        json.RawMessage `json:"output"`
		Level         string          `json:"level"`
		StatusMessage string          `json:"status_message"`
		Latency       json.RawMessage `json:"latency"`
		Usage         *Usage          `json:"usage"`

		TraceIDAlt   string `json:"traceId"`
		ParentIDAlt  string `json:"parentObservationId"`
		StartTimeAlt string `json:"startTime"`
		EndTimeAlt   string `json:"endTime"`
		StatusMsgAlt string `json:"statusMessage"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := Observation{
		ID:            raw.ID,
		TraceID:       raw.TraceID,
		ParentID:      raw.ParentID,
		Type:          raw.Type,
		Name:          raw.Name,
		Model:         raw.Model,
		StartTime:     flexString(raw.StartTime),
		EndTime:       flexString(raw.EndTime),
		Input:         raw.Input,
		Output:        raw.Output,
		Level:         raw.Level,
		StatusMessage: raw.StatusMessage,
		Latency:       flexFloat(raw.Latency),
		Usage:         raw.Usage,
	}
	if out.TraceID == "" {
		out.TraceID = raw.TraceIDAlt
	}
	if out.ParentID == "" {
		out.ParentID = raw.ParentIDAlt
	}
	if out.StartTime == "" {
		out.StartTime = raw.StartTimeAlt
	}
	if out.EndTime == "" {
		out.EndTime = raw.EndTimeAlt
	}
	if out.StatusMessage == "" {
		out.StatusMessage = raw.StatusMsgAlt
	}
	*o = out
	return nil
}

// TraceDetail is a trace plus its observation tree.
type TraceDetail struct {
	TraceSummary
	Observations []Observation `json:"observations"`
}

// UnmarshalJSON decodes the summary fields through TraceSummary's tolerant
// decoder (an embedded struct's UnmarshalJSON is otherwise bypassed by
// encoding/json) and the observation tree alongside it.
func (d *TraceDetail) UnmarshalJSON(data []byte) error {
	var summary TraceSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return err
	}
	var rest struct {
		Observations []Observation `json:"observations"`
	}
	if err := json.Unmarshal(data, &rest); err != nil {
		return err
	}
	d.TraceSummary = summary
	d.Observations = rest.Observations
	return nil
}

// TraceFilter narrows ListTraces. Zero-value fields are ignored.
type TraceFilter struct {
	UserID    string
	SessionID string
	Name      string
	Page      int
	Limit     int
}

// ---------------------------------------------------------------------------
// Ingestion wire types
//
// These mirror the request bodies documented at
// https://api.reference.langfuse.com/. Langfuse accepts partial bodies for
// update events and ignores unknown keys, so every field is omitempty and the
// client only sends what it knows.
// ---------------------------------------------------------------------------

// ingestRequest is the body of POST {host}/api/public/ingestion.
type ingestRequest struct {
	Batch []ingestEvent `json:"batch"`
}

// ingestEvent is one envelope in the batch.
type ingestEvent struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Body      any    `json:"body"`
}

// traceBody is the body of a trace-create event. It is also reused to finish a
// trace: Langfuse upserts the trace identified by id.
type traceBody struct {
	ID          string         `json:"id,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
	Name        string         `json:"name,omitempty"`
	UserID      string         `json:"userId,omitempty"`
	SessionID   string         `json:"sessionId,omitempty"`
	Input       any            `json:"input,omitempty"`
	Output      any            `json:"output,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Release     string         `json:"release,omitempty"`
	Version     string         `json:"version,omitempty"`
	Environment string         `json:"environment,omitempty"`
}

// spanBody is the body of a span-create / span-update event.
type spanBody struct {
	ID                  string         `json:"id,omitempty"`
	TraceID             string         `json:"traceId,omitempty"`
	ParentObservationID string         `json:"parentObservationId,omitempty"`
	Name                string         `json:"name,omitempty"`
	StartTime           string         `json:"startTime,omitempty"`
	EndTime             string         `json:"endTime,omitempty"`
	Input               any            `json:"input,omitempty"`
	Output              any            `json:"output,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
	Level               string         `json:"level,omitempty"`
	StatusMessage       string         `json:"statusMessage,omitempty"`
}

// generationBody is the body of a generation-create / generation-update event.
type generationBody struct {
	ID                  string         `json:"id,omitempty"`
	TraceID             string         `json:"traceId,omitempty"`
	ParentObservationID string         `json:"parentObservationId,omitempty"`
	Name                string         `json:"name,omitempty"`
	StartTime           string         `json:"startTime,omitempty"`
	EndTime             string         `json:"endTime,omitempty"`
	Model               string         `json:"model,omitempty"`
	ModelParameters     map[string]any `json:"modelParameters,omitempty"`
	Input               any            `json:"input,omitempty"`
	Output              any            `json:"output,omitempty"`
	Usage               *usageBody     `json:"usage,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
	Level               string         `json:"level,omitempty"`
	StatusMessage       string         `json:"statusMessage,omitempty"`
}

// usageBody is the ingestion shape of token usage.
type usageBody struct {
	Input  int    `json:"input,omitempty"`
	Output int    `json:"output,omitempty"`
	Total  int    `json:"total,omitempty"`
	Unit   string `json:"unit,omitempty"`
}

// usageUnitTokens is Langfuse's unit for token counts.
const usageUnitTokens = "TOKENS"

// newUsageBody converts usage for the wire, returning nil when there is
// nothing to report. Total is filled in from Input+Output when the caller left
// it at zero, because Langfuse displays the total verbatim.
func newUsageBody(u Usage) *usageBody {
	total := u.Total
	if total == 0 {
		total = u.Input + u.Output
	}
	if u.Input == 0 && u.Output == 0 && total == 0 {
		return nil
	}
	return &usageBody{Input: u.Input, Output: u.Output, Total: total, Unit: usageUnitTokens}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// idFallbackCounter keeps fallback ids unique if crypto/rand ever fails.
var idFallbackCounter atomic.Uint64

// idReader is the entropy source for NewID. It is a variable so tests can
// exercise the fallback path.
var idReader io.Reader = rand.Reader

// NewID returns a new random observation/trace id (32 hex chars, no dashes) so
// that ids fit Langfuse's own id shape and need no external dependency.
func NewID() string {
	var buf [16]byte
	if _, err := io.ReadFull(idReader, buf[:]); err != nil {
		// Only reachable if the platform RNG is broken; stay unique anyway.
		seed := strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatUint(idFallbackCounter.Add(1), 10)
		sum := sha256.Sum256([]byte(seed))
		copy(buf[:], sum[:16])
	}
	return hex.EncodeToString(buf[:])
}

// formatTime renders t as RFC3339 with nanoseconds in UTC. The zero time is
// replaced by the current time.
func formatTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// flexString decodes a JSON value that should be a string but may arrive as a
// number or null. Unparseable values become "".
func flexString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	switch s {
	case "", "null":
		return ""
	}
	if s[0] == '"' {
		var out string
		if err := json.Unmarshal(raw, &out); err != nil {
			return ""
		}
		return out
	}
	return s
}

// flexFloat decodes a JSON number that may arrive as a string. Unparseable
// values become 0.
func flexFloat(raw json.RawMessage) float64 {
	s := flexString(raw)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// flexInt decodes a JSON number that may arrive as a string or a float.
// Unparseable values become 0.
func flexInt(raw json.RawMessage) int {
	s := flexString(raw)
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f)
	}
	return 0
}
