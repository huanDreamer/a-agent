package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// usageSelectCols is the projection shared by every usage_logs reader, so
// QueryUsage and QueryUsageRecent scan identically.
const usageSelectCols = `id, session_id, user_id, provider, model,
	prompt_tokens, completion_tokens, total_tokens, duration_ms, created_at`

const usageInsertSchema = `
INSERT INTO usage_logs
  (session_id, user_id, provider, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

// recentLimit applies the record-listing convention shared by QueryUsage,
// QueryUsageRecent and QueryInvocations: default 100, max 1000.
func recentLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

// RecordUsage inserts a single usage event.
func (s *sqliteStore) RecordUsage(ctx context.Context, e UsageEvent) error {
	created := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, usageInsertSchema,
		e.SessionID, e.UserID, e.Provider, e.Model,
		e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.DurationMs,
		created,
	); err != nil {
		return fmt.Errorf("insert usage: %w", err)
	}
	return nil
}

// QueryUsage returns usage records matching the filter, newest first.
// SessionID, UserID, Provider, Since and Until narrow the result; Limit
// defaults to 100 and is capped at 1000.
func (s *sqliteStore) QueryUsage(ctx context.Context, f UsageFilter) ([]UsageRecord, error) {
	var (
		conds []string
		args  []any
	)
	if f.SessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, f.SessionID)
	}
	// UserID/Provider/Since/Until share the aggregation window helper so that
	// listing and aggregation filter identically.
	wConds, wArgs := usageWhereConds(UsageWindow{
		Since:    f.Since,
		Until:    f.Until,
		UserID:   f.UserID,
		Provider: f.Provider,
	})
	conds = append(conds, wConds...)
	args = append(args, wArgs...)

	q := "SELECT " + usageSelectCols + " FROM usage_logs" + whereClause(conds) + " ORDER BY id DESC LIMIT ?"
	args = append(args, recentLimit(f.Limit))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanUsageRows(rows)
}

// scanUsageRows drains a usageSelectCols result set. It leaves the caller to
// close the rows.
func scanUsageRows(rows *sql.Rows) ([]UsageRecord, error) {
	var out []UsageRecord
	for rows.Next() {
		var r UsageRecord
		var created sql.NullString
		if err := rows.Scan(
			&r.ID, &r.SessionID, &r.UserID, &r.Provider, &r.Model,
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
