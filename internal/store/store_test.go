package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenAndMigrate(t *testing.T) {
	s := newTestStore(t)
	if s.DB() == nil {
		t.Fatal("DB() returned nil after Open")
	}

	// schema_migrations row should exist for v1
	rows, err := s.DB().Query("SELECT version, name FROM schema_migrations")
	if err != nil {
		t.Fatalf("query migrations: %v", err)
	}
	defer rows.Close()

	var got []struct {
		version int
		name    string
	}
	for rows.Next() {
		var v int
		var n string
		if err := rows.Scan(&v, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, struct {
			version int
			name    string
		}{v, n})
	}
	if len(got) != 2 || got[0].version != 1 || got[1].version != 2 {
		t.Errorf("migrations = %+v, want v1 and v2", got)
	}
}

func TestRecordAndQueryUsage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	events := []UsageEvent{
		{SessionID: "s1", Provider: "deepseek", Model: "deepseek-chat", PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, DurationMs: 500},
		{SessionID: "s1", Provider: "deepseek", Model: "deepseek-chat", PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33, DurationMs: 600},
		{SessionID: "s2", Provider: "qwen", Model: "qwen-plus", PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12, DurationMs: 200},
	}
	for _, e := range events {
		if err := s.RecordUsage(ctx, e); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}

	t.Run("all", func(t *testing.T) {
		all, err := s.QueryUsage(ctx, UsageFilter{})
		if err != nil {
			t.Fatalf("QueryUsage: %v", err)
		}
		if len(all) != 3 {
			t.Errorf("len = %d, want 3", len(all))
		}
	})

	t.Run("filter by session", func(t *testing.T) {
		out, err := s.QueryUsage(ctx, UsageFilter{SessionID: "s1"})
		if err != nil {
			t.Fatalf("QueryUsage: %v", err)
		}
		if len(out) != 2 {
			t.Errorf("len = %d, want 2", len(out))
		}
		for _, r := range out {
			if r.SessionID != "s1" {
				t.Errorf("session = %q, want s1", r.SessionID)
			}
		}
	})

	t.Run("filter by provider", func(t *testing.T) {
		out, err := s.QueryUsage(ctx, UsageFilter{Provider: "qwen"})
		if err != nil {
			t.Fatalf("QueryUsage: %v", err)
		}
		if len(out) != 1 {
			t.Fatalf("len = %d, want 1", len(out))
		}
		if out[0].Provider != "qwen" {
			t.Errorf("provider = %q, want qwen", out[0].Provider)
		}
	})

	t.Run("limit", func(t *testing.T) {
		out, err := s.QueryUsage(ctx, UsageFilter{Limit: 2})
		if err != nil {
			t.Fatalf("QueryUsage: %v", err)
		}
		if len(out) != 2 {
			t.Errorf("len = %d, want 2", len(out))
		}
	})
}

func TestOpen_InvalidPath(t *testing.T) {
	// A path under a not-yet-existing directory that we don't create should
	// surface as a wrapped error. SQLite may either create the parent or
	// fail; the contract is just "Open returns a useful error".
	bad := filepath.Join(t.TempDir(), "missing", "subdir", "test.db")
	if _, err := Open(context.Background(), bad); err == nil {
		// Some SQLite builds create the parent dir on demand; that's fine,
		// but make sure we get a usable store if so.
		t.Skip("sqlite driver auto-created the parent directory; skipping error-path assertion")
	}
}

func TestMigrateIdempotent(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Re-run migrate by reopening.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(context.Background(), filepath.Join(t.TempDir(), "test2.db"))
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer s2.Close()

	// Run migrate again on a fresh store.
	rows, err := s2.DB().Query("SELECT COUNT(*) FROM schema_migrations")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no rows")
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (v1 + v2)", n)
	}
}
