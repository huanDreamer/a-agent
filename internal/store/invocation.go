package store

import (
	"context"
	"fmt"
	"strings"
)

// RecordInvocation persists a tool-invocation audit row. Errors MUST
// be returned; callers decide whether to log-and-continue or fail.
func (s *sqliteStore) RecordInvocation(ctx context.Context, e InvocationEvent) error {
	if e.ToolName == "" {
		return fmt.Errorf("invocation: tool_name is required")
	}
	if e.SessionID == "" {
		return fmt.Errorf("invocation: session_id is required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tool_invocations
		 (session_id, tool_name, arguments, result, err, duration_ms)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		e.SessionID, e.ToolName, e.Arguments, e.Result, e.Err, e.DurationMs,
	)
	if err != nil {
		return fmt.Errorf("invocation insert: %w", err)
	}
	return nil
}

// QueryInvocations returns recent invocations matching the filter.
// Zero-value filter fields are ignored; Limit defaults to 100.
func (s *sqliteStore) QueryInvocations(ctx context.Context, f InvocationFilter) ([]InvocationRecord, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	var (
		conds []string
		args  []any
	)
	if f.SessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.ToolName != "" {
		conds = append(conds, "tool_name = ?")
		args = append(args, f.ToolName)
	}

	q := `SELECT id, session_id, tool_name, arguments, result, err, duration_ms, created_at
	      FROM tool_invocations`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("invocation query: %w", err)
	}
	defer rows.Close()

	var out []InvocationRecord
	for rows.Next() {
		var r InvocationRecord
		if err := rows.Scan(&r.ID, &r.SessionID, &r.ToolName, &r.Arguments,
			&r.Result, &r.Err, &r.DurationMs, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("invocation scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("invocation iterate: %w", err)
	}
	return out, nil
}
