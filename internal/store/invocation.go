package store

import (
	"context"
	"fmt"
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
		 (session_id, user_id, tool_name, arguments, result, err, duration_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.SessionID, e.UserID, e.ToolName, e.Arguments, e.Result, e.Err, e.DurationMs,
	)
	if err != nil {
		return fmt.Errorf("invocation insert: %w", err)
	}
	return nil
}

// QueryInvocations returns recent invocations matching the filter, newest
// first. Zero-value filter fields are ignored; Since is an inclusive lower
// bound, Until an exclusive upper bound, and Limit defaults to 100 (max 1000).
func (s *sqliteStore) QueryInvocations(ctx context.Context, f InvocationFilter) ([]InvocationRecord, error) {
	var (
		conds []string
		args  []any
	)
	if f.SessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.UserID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.ToolName != "" {
		conds = append(conds, "tool_name = ?")
		args = append(args, f.ToolName)
	}
	if !f.Since.IsZero() {
		conds = append(conds, "created_at >= ?")
		args = append(args, f.Since.UTC())
	}
	if !f.Until.IsZero() {
		conds = append(conds, "created_at < ?")
		args = append(args, f.Until.UTC())
	}

	q := `SELECT id, session_id, user_id, tool_name, arguments, result, err, duration_ms, created_at
	      FROM tool_invocations`
	q += whereClause(conds)
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, recentLimit(f.Limit))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("invocation query: %w", err)
	}
	defer rows.Close()

	var out []InvocationRecord
	for rows.Next() {
		var r InvocationRecord
		if err := rows.Scan(&r.ID, &r.SessionID, &r.UserID, &r.ToolName, &r.Arguments,
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
