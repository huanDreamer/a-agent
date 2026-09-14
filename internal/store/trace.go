package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TraceRow is one recorded agent turn.
//
// EndedAt is the zero time while the turn is still running. That state is
// stored as SQL NULL rather than as a zero timestamp so a reader can tell an
// unfinished trace from one that finished at the Unix epoch — which matters
// because the trace list and the waterfall both render the distinction.
type TraceRow struct {
	ID        string
	Name      string
	SessionID string
	UserID    string
	// Input and Output are arbitrary JSON documents, already marshalled: the
	// column is text and the shape is whatever the caller recorded.
	Input     string
	Output    string
	StartedAt time.Time
	EndedAt   time.Time
}

// Running reports whether the trace has no end yet.
func (t TraceRow) Running() bool { return t.EndedAt.IsZero() }

// ObservationRow is one node of a trace: a GENERATION (one model call) or a
// SPAN (one tool call).
//
// Type is stored as the string the UI already understands, ParentID nests the
// node (the trace id itself for a node attached directly to the turn), and the
// token columns are only meaningful for a generation.
type ObservationRow struct {
	ID       string
	TraceID  string
	ParentID string
	// Type is the observation kind: a model call or a tool call.
	Type  string
	Name  string
	Model string
	// Step is the 1-based model iteration the observation belongs to, so a
	// reader can group the nodes of one ReAct round.
	Step          int
	Input         string
	Output        string
	Level         string
	StatusMessage string
	// PromptTokens, CompletionTokens and TotalTokens are zero for a span.
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	StartedAt        time.Time
	EndedAt          time.Time
}

// Observation end payload.
type ObservationEnd struct {
	Output           string
	Level            string
	StatusMessage    string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	EndedAt          time.Time
}

// TraceFilter narrows a trace listing. Zero-value fields are ignored.
type TraceFilter struct {
	SessionID string
	UserID    string
	// Name is matched as a substring, because the panel is a search box rather
	// than an exact lookup.
	Name   string
	Limit  int
	Offset int
}

const (
	// defaultTraceLimit is used when TraceFilter.Limit is <= 0.
	defaultTraceLimit = 50
	// maxTraceLimit caps a single page so a hand-written request cannot ask for
	// the whole table.
	maxTraceLimit = 500
)

// RecordTrace inserts a trace. Recording the same id twice is an error rather
// than a silent overwrite: a duplicate id means the caller generated one that
// was already used, and quietly merging two turns would be worse than losing
// the second.
func (s *sqliteStore) RecordTrace(ctx context.Context, t TraceRow) error {
	if strings.TrimSpace(t.ID) == "" {
		return errors.New("store: trace id is required")
	}
	if t.StartedAt.IsZero() {
		t.StartedAt = time.Now().UTC()
	}
	var ended any
	if !t.EndedAt.IsZero() {
		ended = t.EndedAt
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO traces (id, name, session_id, user_id, input, output, started_at, ended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.SessionID, t.UserID, t.Input, t.Output, t.StartedAt, ended); err != nil {
		return fmt.Errorf("insert trace: %w", err)
	}
	return nil
}

// RecordObservation inserts an observation node. An empty ParentID attaches it
// to the trace itself, matching how the waterfall nests a node whose parent it
// cannot know.
func (s *sqliteStore) RecordObservation(ctx context.Context, o ObservationRow) error {
	if strings.TrimSpace(o.ID) == "" {
		return errors.New("store: observation id is required")
	}
	if strings.TrimSpace(o.TraceID) == "" {
		return errors.New("store: observation needs a trace id")
	}
	if o.StartedAt.IsZero() {
		o.StartedAt = time.Now().UTC()
	}
	if o.ParentID == "" {
		o.ParentID = o.TraceID
	}
	var ended any
	if !o.EndedAt.IsZero() {
		ended = o.EndedAt
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO observations
		  (id, trace_id, parent_id, type, name, model, step, input, output,
		   level, status_message, prompt_tokens, completion_tokens, total_tokens,
		   started_at, ended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.TraceID, o.ParentID, o.Type, o.Name, o.Model, o.Step, o.Input, o.Output,
		o.Level, o.StatusMessage, o.PromptTokens, o.CompletionTokens, o.TotalTokens,
		o.StartedAt, ended); err != nil {
		return fmt.Errorf("insert observation: %w", err)
	}
	return nil
}

// EndTrace closes a trace with its final output.
//
// Only the fields a caller may legitimately know at the end are updated:
// overwriting started_at here is exactly the bug the Langfuse client hit once,
// and leaving the output column untouched when the caller has nothing to say
// keeps a recorded input from being blanked.
func (s *sqliteStore) EndTrace(ctx context.Context, id, output string, endedAt time.Time) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("store: trace id is required")
	}
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE traces SET ended_at = ?, output = CASE WHEN ? = '' THEN output ELSE ? END WHERE id = ?",
		endedAt, output, output, id)
	if err != nil {
		return fmt.Errorf("end trace: %w", err)
	}
	return affectedOrNotFound(res, "trace", id)
}

// EndObservation closes an observation with its output, outcome and usage.
func (s *sqliteStore) EndObservation(ctx context.Context, id string, end ObservationEnd) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("store: observation id is required")
	}
	if end.EndedAt.IsZero() {
		end.EndedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE observations
		SET ended_at          = ?,
		    output            = CASE WHEN ? = '' THEN output ELSE ? END,
		    level             = ?,
		    status_message    = ?,
		    prompt_tokens     = ?,
		    completion_tokens = ?,
		    total_tokens      = ?
		WHERE id = ?`,
		end.EndedAt, end.Output, end.Output, end.Level, end.StatusMessage,
		end.PromptTokens, end.CompletionTokens, end.TotalTokens, id)
	if err != nil {
		return fmt.Errorf("end observation: %w", err)
	}
	return affectedOrNotFound(res, "observation", id)
}

// affectedOrNotFound turns a zero-row UPDATE into ErrNotFound, so a caller
// learns that the row it meant to close does not exist instead of believing the
// write succeeded.
func affectedOrNotFound(res sql.Result, kind, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return nil // the statement ran; the count is only a diagnostic
	}
	if n == 0 {
		return fmt.Errorf("%s %q: %w", kind, id, ErrNotFound)
	}
	return nil
}

// ListTraces returns traces newest first.
func (s *sqliteStore) ListTraces(ctx context.Context, f TraceFilter) ([]TraceRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultTraceLimit
	}
	if limit > maxTraceLimit {
		limit = maxTraceLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	q := `SELECT id, name, session_id, user_id, input, output, started_at, ended_at
	      FROM traces`
	var where []string
	var args []any
	if f.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.UserID != "" {
		where = append(where, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.Name != "" {
		where = append(where, "name LIKE ?")
		args = append(args, "%"+f.Name+"%")
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	// The id breaks ties: two turns began in the same millisecond would
	// otherwise come back in an arbitrary order and make paging unstable.
	q += " ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list traces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]TraceRow, 0, 16)
	for rows.Next() {
		t, err := scanTrace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate traces: %w", err)
	}
	return out, nil
}

// GetTrace returns one trace with its observation tree, oldest node first.
func (s *sqliteStore) GetTrace(ctx context.Context, id string) (TraceRow, []ObservationRow, error) {
	var t TraceRow
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, session_id, user_id, input, output, started_at, ended_at
		FROM traces WHERE id = ?`, id)
	t, err := scanTrace(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TraceRow{}, nil, fmt.Errorf("trace %q: %w", id, ErrNotFound)
		}
		return TraceRow{}, nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, trace_id, parent_id, type, name, model, step, input, output,
		       level, status_message, prompt_tokens, completion_tokens, total_tokens,
		       started_at, ended_at
		FROM observations WHERE trace_id = ? ORDER BY started_at ASC, id ASC`, id)
	if err != nil {
		return TraceRow{}, nil, fmt.Errorf("list observations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	obs := make([]ObservationRow, 0, 8)
	for rows.Next() {
		var o ObservationRow
		var started, ended sql.NullTime
		if err := rows.Scan(&o.ID, &o.TraceID, &o.ParentID, &o.Type, &o.Name, &o.Model,
			&o.Step, &o.Input, &o.Output, &o.Level, &o.StatusMessage,
			&o.PromptTokens, &o.CompletionTokens, &o.TotalTokens, &started, &ended); err != nil {
			return TraceRow{}, nil, fmt.Errorf("scan observation: %w", err)
		}
		o.StartedAt = nullTime(started)
		o.EndedAt = nullTime(ended)
		obs = append(obs, o)
	}
	if err := rows.Err(); err != nil {
		return TraceRow{}, nil, fmt.Errorf("iterate observations: %w", err)
	}
	return t, obs, nil
}

// scanTrace reads one trace row, keeping an unfinished trace's end NULL rather
// than decoding it as a zero time in the database's own terms. It shares the
// `scanner` interface declared alongside the usage helpers in analytics.go.
func scanTrace(src scanner) (TraceRow, error) {
	var t TraceRow
	var started, ended sql.NullTime
	if err := src.Scan(&t.ID, &t.Name, &t.SessionID, &t.UserID, &t.Input, &t.Output,
		&started, &ended); err != nil {
		return TraceRow{}, err
	}
	t.StartedAt = nullTime(started)
	t.EndedAt = nullTime(ended)
	return t, nil
}
