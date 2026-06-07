package store

import (
	"context"
	"fmt"
)

// migration represents a single ordered schema migration.
type migration struct {
	version int
	name    string
	up      string
}

// migrations is the ordered list applied at Open() time. Append new entries;
// never mutate or remove an existing one.
var migrations = []migration{
	{
		version: 1,
		name:    "create_usage_logs",
		up: `CREATE TABLE IF NOT EXISTS usage_logs (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id       TEXT    NOT NULL,
			provider         TEXT    NOT NULL,
			model            TEXT    NOT NULL,
			prompt_tokens    INTEGER NOT NULL,
			completion_tokens INTEGER NOT NULL,
			total_tokens     INTEGER NOT NULL,
			duration_ms      INTEGER NOT NULL,
			created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_usage_session  ON usage_logs(session_id);
		CREATE INDEX IF NOT EXISTS idx_usage_provider ON usage_logs(provider);
		CREATE INDEX IF NOT EXISTS idx_usage_created  ON usage_logs(created_at);`,
	},
}

// Migrate applies any pending migrations idempotently.
func (s *sqliteStore) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT    NOT NULL,
			applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`); err != nil {
		return fmtErr("create migrations table", err)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmtErr("read migrations", err)
	}
	applied := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return fmtErr("scan migration", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmtErr("iterate migrations", err)
	}
	_ = rows.Close()

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmtErr("begin tx", err)
		}
		if _, err := tx.ExecContext(ctx, m.up); err != nil {
			_ = tx.Rollback()
			return fmtErr("apply migration %d (%s)", err, m.version, m.name)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
			m.version, m.name); err != nil {
			_ = tx.Rollback()
			return fmtErr("record migration %d", err, m.version)
		}
		if err := tx.Commit(); err != nil {
			return fmtErr("commit migration %d", err, m.version)
		}
	}
	return nil
}

func fmtErr(msg string, err error, args ...any) error {
	format := msg
	if len(args) > 0 {
		format = fmt.Sprintf(msg, args...)
	}
	return fmt.Errorf("%s: %w", format, err)
}
