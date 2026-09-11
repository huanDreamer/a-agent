package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Aggregation limits (Phase 5 admin API contract).
const (
	defaultGroupLimit = 50  // grouped queries: limit <= 0
	maxGroupLimit     = 500 // grouped queries: hard cap
	defaultTrendDays  = 30  // QueryUsageByDay: days <= 0
	maxTrendDays      = 366 // QueryUsageByDay: hard cap
)

// usageAggregateCols is the aggregate projection shared by every usage
// aggregation, so the meaning of a bucket (and of UsageTotals) cannot drift
// between "totals" and "grouped" queries.
//
// NULLIF(user_id, ”) collapses unattributed rows so that
// COUNT(DISTINCT NULLIF(user_id, ”)) ignores them.
const usageAggregateCols = `
	COUNT(*)                             AS calls,
	COALESCE(SUM(prompt_tokens), 0)      AS prompt_tokens,
	COALESCE(SUM(completion_tokens), 0)  AS completion_tokens,
	COALESCE(SUM(total_tokens), 0)       AS total_tokens,
	COALESCE(SUM(duration_ms), 0)        AS duration_ms,
	COUNT(DISTINCT NULLIF(user_id, ''))  AS users,
	COUNT(DISTINCT session_id)           AS sessions`

// usageDayExpr buckets created_at into a UTC calendar day (YYYY-MM-DD).
//
// RecordUsage writes created_at as a Go time.Time bound parameter, which the
// modernc.org/sqlite driver renders with time.Time.String()
// ("2024-05-01 23:59:59 +0000 UTC"): a UTC instant, but a shape SQLite's
// date() cannot parse (it yields NULL). date(created_at) is still tried first
// so that SQLite-native formats (e.g. the CURRENT_TIMESTAMP default) are
// handled canonically; the substr() fallback reads the leading "YYYY-MM-DD",
// which is the UTC day for every instant this package writes.
const usageDayExpr = `COALESCE(date(created_at), date(substr(created_at, 1, 10)))`

// UsageWindow narrows an aggregation over usage_logs. Zero-valued fields are
// ignored (no constraint).
type UsageWindow struct {
	Since    time.Time // inclusive lower bound, zero = unbounded
	Until    time.Time // exclusive upper bound, zero = unbounded
	UserID   string    // empty = all users
	Provider string
	Model    string
}

// UsageTotals is an aggregate over usage_logs.
type UsageTotals struct {
	Calls            int64 `json:"calls"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	DurationMs       int64 `json:"duration_ms"`
	Users            int64 `json:"users"`    // COUNT(DISTINCT user_id), ignores empty user_id
	Sessions         int64 `json:"sessions"` // COUNT(DISTINCT session_id)
}

// UsageGroupRow is one bucket of a grouped aggregate.
type UsageGroupRow struct {
	Key string `json:"key"`
	UsageTotals
}

// UsageDayRow is one day of the usage trend.
type UsageDayRow struct {
	Day string `json:"day"` // YYYY-MM-DD in UTC
	UsageTotals
}

// UsageWindow filter helpers ------------------------------------------------

// usageWhereConds builds the WHERE conditions for a UsageWindow. It is the
// single source of truth for window filtering: every aggregation query and
// QueryUsage go through it, so their filtering cannot drift apart.
//
// Zero-valued window fields contribute nothing. Time bounds are compared
// against created_at as text, which is chronological because the column holds a
// fixed-width UTC instant ("YYYY-MM-DD HH:MM:SS[.fff] +0000 UTC"): the trimmed
// fractional part only ever appears after a '.' and ' ' < '.', so a whole
// second sorts before its own fractions.
func usageWhereConds(w UsageWindow) ([]string, []any) {
	var (
		conds []string
		args  []any
	)
	if !w.Since.IsZero() {
		conds = append(conds, "created_at >= ?")
		args = append(args, w.Since.UTC())
	}
	if !w.Until.IsZero() {
		conds = append(conds, "created_at < ?")
		args = append(args, w.Until.UTC())
	}
	if w.UserID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, w.UserID)
	}
	if w.Provider != "" {
		conds = append(conds, "provider = ?")
		args = append(args, w.Provider)
	}
	if w.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, w.Model)
	}
	return conds, args
}

// whereClause joins conditions into a " WHERE ..." suffix, or "" when empty.
func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// groupLimit applies the grouped-query convention: default 50, max 500.
func groupLimit(limit int) int {
	if limit <= 0 {
		return defaultGroupLimit
	}
	if limit > maxGroupLimit {
		return maxGroupLimit
	}
	return limit
}

// trendDays applies the trend convention: default 30, max 366.
func trendDays(days int) int {
	if days <= 0 {
		return defaultTrendDays
	}
	if days > maxTrendDays {
		return maxTrendDays
	}
	return days
}

// usage scanner helpers -----------------------------------------------------

// scanner is the subset of *sql.Row / *sql.Rows used to share scan logic.
type scanner interface {
	Scan(dest ...any) error
}

// scanUsageTotals scans the usageAggregateCols projection in column order.
func scanUsageTotals(sc scanner) (UsageTotals, error) {
	var t UsageTotals
	err := sc.Scan(
		&t.Calls, &t.PromptTokens, &t.CompletionTokens, &t.TotalTokens,
		&t.DurationMs, &t.Users, &t.Sessions,
	)
	return t, err
}

// Queries ------------------------------------------------------------------

// QueryUsageTotals aggregates usage_logs over the window. With no matching rows
// it returns the zero UsageTotals, never an error.
func (s *sqliteStore) QueryUsageTotals(ctx context.Context, w UsageWindow) (UsageTotals, error) {
	conds, args := usageWhereConds(w)
	q := "SELECT " + usageAggregateCols + " FROM usage_logs" + whereClause(conds)

	t, err := scanUsageTotals(s.db.QueryRowContext(ctx, q, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// An aggregate without GROUP BY always yields one row, but stay
			// defensive: no rows means no usage.
			return UsageTotals{}, nil
		}
		return UsageTotals{}, fmt.Errorf("usage totals: %w", err)
	}
	return t, nil
}

// QueryUsageByUser groups usage by user_id, biggest total_tokens first. Rows
// with an empty user_id are grouped under the empty key so that group sums
// reconcile with QueryUsageTotals; use UsageTotals.Users for the number of
// distinct attributed users.
func (s *sqliteStore) QueryUsageByUser(ctx context.Context, w UsageWindow, limit int) ([]UsageGroupRow, error) {
	return s.queryUsageGrouped(ctx, w, "user_id", limit)
}

// QueryUsageByModel groups usage by model, biggest total_tokens first.
func (s *sqliteStore) QueryUsageByModel(ctx context.Context, w UsageWindow, limit int) ([]UsageGroupRow, error) {
	return s.queryUsageGrouped(ctx, w, "model", limit)
}

// QueryUsageByProvider groups usage by provider, biggest total_tokens first.
func (s *sqliteStore) QueryUsageByProvider(ctx context.Context, w UsageWindow, limit int) ([]UsageGroupRow, error) {
	return s.queryUsageGrouped(ctx, w, "provider", limit)
}

// ProviderModelGroup is one (provider, model) bucket. The two are reported
// separately because a price is per provider *and* model: the same model name
// can cost different amounts through different providers, so summing a cost
// total requires pricing each pair.
type ProviderModelGroup struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	UsageTotals
}

// QueryUsageByProviderModel groups usage by provider and model together,
// biggest total_tokens first. Callers that need an exact cost total for a mixed
// window price each returned pair and sum the results.
func (s *sqliteStore) QueryUsageByProviderModel(ctx context.Context, w UsageWindow, limit int) ([]ProviderModelGroup, error) {
	conds, args := usageWhereConds(w)
	q := "SELECT provider, model, " + usageAggregateCols +
		" FROM usage_logs" + whereClause(conds) +
		" GROUP BY provider, model ORDER BY total_tokens DESC, provider ASC, model ASC LIMIT ?"
	args = append(args, groupLimit(limit))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage grouped by provider+model: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProviderModelGroup
	for rows.Next() {
		var g ProviderModelGroup
		if err := rows.Scan(&g.Provider, &g.Model,
			&g.Calls, &g.PromptTokens, &g.CompletionTokens, &g.TotalTokens,
			&g.DurationMs, &g.Users, &g.Sessions,
		); err != nil {
			return nil, fmt.Errorf("scan provider+model group: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider+model group: %w", err)
	}
	return out, nil
}

// queryUsageGrouped is the shared grouped-aggregate implementation. keyExpr is
// one of the NOT NULL usage_logs columns; order is total_tokens DESC with the
// key as a deterministic tie-breaker.
func (s *sqliteStore) queryUsageGrouped(ctx context.Context, w UsageWindow, keyExpr string, limit int) ([]UsageGroupRow, error) {
	conds, args := usageWhereConds(w)
	q := "SELECT " + keyExpr + " AS bucket, " + usageAggregateCols +
		" FROM usage_logs" + whereClause(conds) +
		" GROUP BY bucket ORDER BY total_tokens DESC, bucket ASC LIMIT ?"
	args = append(args, groupLimit(limit))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage grouped by %s: %w", keyExpr, err)
	}
	defer func() { _ = rows.Close() }()

	var out []UsageGroupRow
	for rows.Next() {
		var (
			g UsageGroupRow
			t UsageTotals
		)
		if err := rows.Scan(
			&g.Key,
			&t.Calls, &t.PromptTokens, &t.CompletionTokens, &t.TotalTokens,
			&t.DurationMs, &t.Users, &t.Sessions,
		); err != nil {
			return nil, fmt.Errorf("scan usage grouped by %s: %w", keyExpr, err)
		}
		g.UsageTotals = t
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage grouped by %s: %w", keyExpr, err)
	}
	return out, nil
}

// QueryUsageByDay returns one row per UTC day that has data, ordered by day
// ASC. days bounds how many of the most recent days-with-data are returned
// (default 30, max 366); the UsageWindow filters apply as everywhere else.
func (s *sqliteStore) QueryUsageByDay(ctx context.Context, w UsageWindow, days int) ([]UsageDayRow, error) {
	conds, args := usageWhereConds(w)
	// Unparseable created_at values would bucket into a NULL day; drop them
	// rather than emitting a nameless bucket.
	conds = append(conds, usageDayExpr+" IS NOT NULL")

	// Select the newest `days` buckets, then reverse into ascending order.
	q := "SELECT " + usageDayExpr + " AS day, " + usageAggregateCols +
		" FROM usage_logs" + whereClause(conds) +
		" GROUP BY day ORDER BY day DESC LIMIT ?"
	args = append(args, trendDays(days))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage by day: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UsageDayRow
	for rows.Next() {
		var (
			d UsageDayRow
			t UsageTotals
		)
		if err := rows.Scan(
			&d.Day,
			&t.Calls, &t.PromptTokens, &t.CompletionTokens, &t.TotalTokens,
			&t.DurationMs, &t.Users, &t.Sessions,
		); err != nil {
			return nil, fmt.Errorf("scan usage by day: %w", err)
		}
		d.UsageTotals = t
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage by day: %w", err)
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// QueryUsageRecent returns the newest usage rows matching the window, newest
// first. It follows the record-listing limit convention (default 100, max 1000)
// and applies exactly the same window filters as the aggregations.
func (s *sqliteStore) QueryUsageRecent(ctx context.Context, w UsageWindow, limit int) ([]UsageRecord, error) {
	conds, args := usageWhereConds(w)
	q := "SELECT " + usageSelectCols + " FROM usage_logs" + whereClause(conds) + " ORDER BY id DESC LIMIT ?"
	args = append(args, recentLimit(limit))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query recent usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanUsageRows(rows)
}
