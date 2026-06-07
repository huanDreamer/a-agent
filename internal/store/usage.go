package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const usageInsertSchema = `
INSERT INTO usage_logs
  (session_id, provider, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

// RecordUsage inserts a single usage event.
func (s *sqliteStore) RecordUsage(ctx context.Context, e UsageEvent) error {
	created := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, usageInsertSchema,
		e.SessionID, e.Provider, e.Model,
		e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.DurationMs,
		created,
	); err != nil {
		return fmt.Errorf("insert usage: %w", err)
	}
	return nil
}

// QueryUsage returns usage records matching the filter.
func (s *sqliteStore) QueryUsage(ctx context.Context, f UsageFilter) ([]UsageRecord, error) {
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
	if f.Provider != "" {
		conds = append(conds, "provider = ?")
		args = append(args, f.Provider)
	}

	q := "SELECT id, session_id, provider, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, created_at FROM usage_logs"
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UsageRecord
	for rows.Next() {
		var r UsageRecord
		var created sql.NullString
		if err := rows.Scan(
			&r.ID, &r.SessionID, &r.Provider, &r.Model,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.DurationMs,
			&created,
		); err != nil {
			return nil, fmt.Errorf("scan usage: %w", err)
		}
		if created.Valid {
			r.CreatedAt = created.String
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage: %w", err)
	}
	return out, nil
}
