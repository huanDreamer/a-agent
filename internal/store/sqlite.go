package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// sqliteStore is the SQLite-backed implementation of Store.
type sqliteStore struct {
	db   *sql.DB
	path string
}

// dbOpen initializes the connection. Calling on an already-open store is
// a no-op (returns nil).
func (s *sqliteStore) dbOpen(ctx context.Context) error {
	if s.db != nil {
		return nil
	}
	db, err := newSQLite(s.path)
	if err != nil {
		return err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("ping sqlite: %w", err)
	}
	s.db = db
	return nil
}

func (s *sqliteStore) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *sqliteStore) DB() *sql.DB { return s.db }
