package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Test helpers --------------------------------------------------------------

// testContext returns a context cancelled by t.Cleanup.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// at builds an explicit UTC instant for fixtures, so day bucketing and window
// filtering are deterministic regardless of when the test runs.
func at(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

// utcString renders ts exactly as the SQLite driver binds a time.Time
// parameter (time.Time.String() of the UTC instant), so rows inserted directly
// by tests are identical in shape to rows written by RecordUsage. This matters
// because created_at window comparisons are textual.
func utcString(ts time.Time) string { return ts.UTC().String() }

// usageRow is one directly-inserted usage_logs row. RecordUsage always stamps
// time.Now(), so trend/window fixtures insert explicit created_at values.
type usageRow struct {
	at                             time.Time
	user, provider, model, session string
	prompt, completion, total      int
	duration                       int64
}

// seedUsageRows inserts fixture rows in a single transaction.
func seedUsageRows(t *testing.T, s Store, rows []usageRow) {
	t.Helper()
	ctx := testContext(t)

	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO usage_logs
		(session_id, user_id, provider, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare insert: %v", err)
	}
	defer func() { _ = stmt.Close() }()

	for i, r := range rows {
		if _, err := stmt.ExecContext(ctx,
			r.session, r.user, r.provider, r.model,
			r.prompt, r.completion, r.total, r.duration, utcString(r.at),
		); err != nil {
			t.Fatalf("insert fixture row %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
}

// usageFixture spans 3 calendar days, 3 users (one unattributed), 2 providers,
// 2 models and 3 sessions, with rows landing exactly on day boundaries.
func usageFixture() []usageRow {
	return []usageRow{
		{at: at(2024, time.May, 1, 8, 0, 0), user: "alice", provider: "deepseek", model: "deepseek-chat", session: "s1",
			prompt: 10, completion: 20, total: 30, duration: 100},
		{at: at(2024, time.May, 1, 9, 30, 0), user: "alice", provider: "deepseek", model: "deepseek-chat", session: "s1",
			prompt: 5, completion: 5, total: 10, duration: 50},
		{at: at(2024, time.May, 2, 0, 0, 0), user: "bob", provider: "qwen", model: "qwen-plus", session: "s2",
			prompt: 100, completion: 200, total: 300, duration: 1000},
		{at: at(2024, time.May, 3, 0, 0, 0), user: "", provider: "deepseek", model: "deepseek-chat", session: "s1",
			prompt: 1, completion: 2, total: 3, duration: 7},
		{at: at(2024, time.May, 3, 23, 59, 59), user: "alice", provider: "qwen", model: "qwen-plus", session: "s3",
			prompt: 40, completion: 60, total: 100, duration: 500},
	}
}

// fixtureDir returns a fresh temp-dir path, isolating each test's database.
func fixtureDir(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

// newLegacyStore builds a database with only the migrations declared before
// beforeVersion, mimicking a database created by an older binary.
func newLegacyStore(t *testing.T, ctx context.Context, path string, beforeVersion int) *sql.DB {
	t.Helper()

	db, err := newSQLite(path)
	if err != nil {
		t.Fatalf("newSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT    NOT NULL,
			applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, m := range migrations {
		if m.version >= beforeVersion {
			break
		}
		if _, err := db.ExecContext(ctx, m.up); err != nil {
			t.Fatalf("apply migration %d (%s): %v", m.version, m.name, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
			m.version, m.name); err != nil {
			t.Fatalf("record migration %d: %v", m.version, err)
		}
	}
	return db
}

// hasColumn reports whether table exposes column, via pragma_table_info.
// table is a literal from the test, never user input.
func hasColumn(t *testing.T, s Store, table, column string) bool {
	t.Helper()

	rows, err := s.DB().Query(fmt.Sprintf("SELECT name FROM pragma_table_info('%s')", table))
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	return false
}

func hasIndex(t *testing.T, s Store, name string) bool {
	t.Helper()

	var n int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("query index %s: %v", name, err)
	}
	return n > 0
}

func assertTotals(t *testing.T, label string, got, want UsageTotals) {
	t.Helper()
	if got != want {
		t.Errorf("%s: totals = %+v, want %+v", label, got, want)
	}
}

func groupKeys(rows []UsageGroupRow) []string {
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, r.Key)
	}
	return keys
}

func dayStrings(rows []UsageDayRow) []string {
	days := make([]string, 0, len(rows))
	for _, r := range rows {
		days = append(days, r.Day)
	}
	return days
}

func assertEqualStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: len = %d (%v), want %d (%v)", label, len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", label, i, got[i], want[i])
		}
	}
}

// Migration -----------------------------------------------------------------

func TestMigrationV3AddsUserID(t *testing.T) {
	ctx := testContext(t)
	path := fixtureDir(t, "legacy.db")

	// A database from before v3: usage_logs/tool_invocations without user_id.
	legacy := newLegacyStore(t, ctx, path, 3)
	if _, err := legacy.ExecContext(ctx, `INSERT INTO usage_logs
		(session_id, provider, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, created_at)
		VALUES ('s-legacy', 'deepseek', 'deepseek-chat', 7, 8, 15, 42, ?)`,
		utcString(at(2024, time.January, 2, 3, 4, 5))); err != nil {
		t.Fatalf("insert legacy usage: %v", err)
	}
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO tool_invocations (session_id, tool_name, arguments) VALUES ('s-legacy', 'echo', '{}')`); err != nil {
		t.Fatalf("insert legacy invocation: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy store: %v", err)
	}

	// Open runs Migrate, which applies v3 on top of the legacy schema.
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	assertAppliedMigrations(t, s)

	for _, table := range []string{"usage_logs", "tool_invocations"} {
		if !hasColumn(t, s, table, "user_id") {
			t.Errorf("%s.user_id missing after v3", table)
		}
	}
	for _, idx := range []string{"idx_usage_user", "idx_inv_user"} {
		if !hasIndex(t, s, idx) {
			t.Errorf("index %s missing after v3", idx)
		}
	}

	// The column is NOT NULL DEFAULT ''.
	var (
		notNull int
		dflt    sql.NullString
	)
	if err := s.DB().QueryRow(
		`SELECT "notnull", dflt_value FROM pragma_table_info('usage_logs') WHERE name = 'user_id'`,
	).Scan(&notNull, &dflt); err != nil {
		t.Fatalf("pragma_table_info(user_id): %v", err)
	}
	if notNull != 1 {
		t.Errorf(`usage_logs.user_id "notnull" = %d, want 1`, notNull)
	}
	if !dflt.Valid || strings.Trim(dflt.String, "'") != "" {
		t.Errorf("usage_logs.user_id default = %v, want ''", dflt)
	}

	// Pre-existing rows default to the empty (unattributed) user.
	out, err := s.QueryUsage(ctx, UsageFilter{SessionID: "s-legacy"})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("legacy usage rows = %d, want 1", len(out))
	}
	if out[0].UserID != "" {
		t.Errorf("legacy usage user_id = %q, want %q", out[0].UserID, "")
	}

	inv, err := s.QueryInvocations(ctx, InvocationFilter{SessionID: "s-legacy"})
	if err != nil {
		t.Fatalf("QueryInvocations: %v", err)
	}
	if len(inv) != 1 {
		t.Fatalf("legacy invocation rows = %d, want 1", len(inv))
	}
	if inv[0].UserID != "" {
		t.Errorf("legacy invocation user_id = %q, want %q", inv[0].UserID, "")
	}

	// Legacy rows stay visible to the aggregations, as unattributed usage.
	totals, err := s.QueryUsageTotals(ctx, UsageWindow{})
	if err != nil {
		t.Fatalf("QueryUsageTotals: %v", err)
	}
	assertTotals(t, "legacy", totals, UsageTotals{
		Calls: 1, PromptTokens: 7, CompletionTokens: 8, TotalTokens: 15,
		DurationMs: 42, Users: 0, Sessions: 1,
	})

	// Reopening the same file re-runs Migrate: the ALTER must not run again.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen after v3: %v", err)
	}
	defer func() { _ = s2.Close() }()

	assertAppliedMigrations(t, s2)
	if !hasColumn(t, s2, "usage_logs", "user_id") || !hasColumn(t, s2, "tool_invocations", "user_id") {
		t.Error("user_id column lost after reopening the migrated database")
	}
}

// Recording and listing -----------------------------------------------------

func TestRecordUsagePersistsUserID(t *testing.T) {
	s := newTestStore(t)
	ctx := testContext(t)

	before := time.Now().UTC()
	events := []UsageEvent{
		{SessionID: "s1", UserID: "alice", Provider: "deepseek", Model: "deepseek-chat", PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, DurationMs: 4},
		{SessionID: "s1", UserID: "bob", Provider: "qwen", Model: "qwen-plus", PromptTokens: 5, CompletionTokens: 6, TotalTokens: 11, DurationMs: 12},
		{SessionID: "s2", Provider: "deepseek", Model: "deepseek-chat", PromptTokens: 7, CompletionTokens: 8, TotalTokens: 15, DurationMs: 16},
	}
	for _, e := range events {
		if err := s.RecordUsage(ctx, e); err != nil {
			t.Fatalf("RecordUsage(%s): %v", e.SessionID, err)
		}
	}
	after := time.Now().UTC()

	// user_id round-trips through insert and select.
	all, err := s.QueryUsage(ctx, UsageFilter{})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
	want := map[string]int{"alice": 1, "bob": 1, "": 1}
	seen := map[string]int{}
	for _, r := range all {
		seen[r.UserID]++
		if r.CreatedAt == "" {
			t.Error("CreatedAt is empty")
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("user_ids = %v, want %v", seen, want)
	}
	for user, n := range want {
		if seen[user] != n {
			t.Errorf("user %q recorded %d times, want %d", user, seen[user], n)
		}
	}
	if all[0].SessionID != "s2" || all[0].UserID != "" {
		t.Errorf("newest row = %+v, want the unattributed s2 row", all[0])
	}

	t.Run("filter by user", func(t *testing.T) {
		out, err := s.QueryUsage(ctx, UsageFilter{UserID: "alice"})
		if err != nil {
			t.Fatalf("QueryUsage: %v", err)
		}
		if len(out) != 1 || out[0].UserID != "alice" {
			t.Fatalf("out = %+v, want exactly the alice row", out)
		}
	})

	t.Run("filter by user and session", func(t *testing.T) {
		out, err := s.QueryUsage(ctx, UsageFilter{SessionID: "s1", UserID: "alice"})
		if err != nil {
			t.Fatalf("QueryUsage: %v", err)
		}
		if len(out) != 1 || out[0].UserID != "alice" || out[0].SessionID != "s1" {
			t.Fatalf("out = %+v, want the alice/s1 row", out)
		}
	})

	t.Run("unknown user", func(t *testing.T) {
		out, err := s.QueryUsage(ctx, UsageFilter{UserID: "nobody"})
		if err != nil {
			t.Fatalf("QueryUsage: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("len = %d, want 0", len(out))
		}
	})

	t.Run("time window", func(t *testing.T) {
		cases := []struct {
			name string
			f    UsageFilter
			want int
		}{
			{"since before, until after", UsageFilter{Since: before.Add(-time.Minute), Until: after.Add(time.Minute)}, 3},
			{"since in the future", UsageFilter{Since: after.Add(time.Minute)}, 0},
			{"until in the past", UsageFilter{Until: before.Add(-time.Minute)}, 0},
			{"since inclusive at now", UsageFilter{Since: before.Add(-time.Second)}, 3},
		}
		for _, tc := range cases {
			out, err := s.QueryUsage(ctx, tc.f)
			if err != nil {
				t.Fatalf("%s: QueryUsage: %v", tc.name, err)
			}
			if len(out) != tc.want {
				t.Errorf("%s: len = %d, want %d", tc.name, len(out), tc.want)
			}
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

func TestInvocationUserAndTimeFilters(t *testing.T) {
	s := newTestStore(t)
	ctx := testContext(t)

	events := []InvocationEvent{
		{SessionID: "s1", UserID: "alice", ToolName: "echo", Arguments: "{}"},
		{SessionID: "s1", UserID: "bob", ToolName: "echo", Arguments: "{}"},
		{SessionID: "s2", UserID: "alice", ToolName: "time", Arguments: "{}"},
		{SessionID: "s2", ToolName: "calc", Arguments: "{}"}, // unattributed
	}
	for _, e := range events {
		if err := s.RecordInvocation(ctx, e); err != nil {
			t.Fatalf("RecordInvocation: %v", err)
		}
	}
	now := time.Now().UTC()

	byUser, err := s.QueryInvocations(ctx, InvocationFilter{UserID: "alice"})
	if err != nil {
		t.Fatalf("QueryInvocations: %v", err)
	}
	if len(byUser) != 2 {
		t.Fatalf("alice invocations = %d, want 2", len(byUser))
	}
	for _, r := range byUser {
		if r.UserID != "alice" {
			t.Errorf("user_id = %q, want alice", r.UserID)
		}
	}

	both, err := s.QueryInvocations(ctx, InvocationFilter{UserID: "alice", ToolName: "time"})
	if err != nil {
		t.Fatalf("QueryInvocations: %v", err)
	}
	if len(both) != 1 {
		t.Errorf("alice/time invocations = %d, want 1", len(both))
	}

	unattributed, err := s.QueryInvocations(ctx, InvocationFilter{SessionID: "s2"})
	if err != nil {
		t.Fatalf("QueryInvocations: %v", err)
	}
	if len(unattributed) != 2 {
		t.Fatalf("s2 invocations = %d, want 2", len(unattributed))
	}
	if unattributed[0].UserID != "" || unattributed[0].ToolName != "calc" {
		t.Errorf("newest s2 row = %+v, want unattributed calc", unattributed[0])
	}

	windowCases := []struct {
		name string
		f    InvocationFilter
		want int
	}{
		{"since in the past", InvocationFilter{Since: now.Add(-time.Hour)}, 4},
		{"until in the past", InvocationFilter{Until: now.Add(-time.Hour)}, 0},
		{"since in the future", InvocationFilter{Since: now.Add(time.Hour)}, 0},
	}
	for _, tc := range windowCases {
		out, err := s.QueryInvocations(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: QueryInvocations: %v", tc.name, err)
		}
		if len(out) != tc.want {
			t.Errorf("%s: len = %d, want %d", tc.name, len(out), tc.want)
		}
	}
}

// Aggregations --------------------------------------------------------------

func TestQueryUsageTotals(t *testing.T) {
	s := newTestStore(t)
	ctx := testContext(t)
	seedUsageRows(t, s, usageFixture())

	// Whole window: 5 calls, 3 users (one unattributed), 3 sessions,
	// 2 providers, 2 models, 3 calendar days.
	totals, err := s.QueryUsageTotals(ctx, UsageWindow{})
	if err != nil {
		t.Fatalf("QueryUsageTotals: %v", err)
	}
	assertTotals(t, "all", totals, UsageTotals{
		Calls:            5,
		PromptTokens:     156, // 10+5+100+1+40
		CompletionTokens: 287, // 20+5+200+2+60
		TotalTokens:      443, // 30+10+300+3+100
		DurationMs:       1657,
		Users:            2, // alice, bob ("" excluded)
		Sessions:         3, // s1, s2, s3
	})

	cases := []struct {
		name string
		w    UsageWindow
		want UsageTotals
	}{
		{
			name: "by user",
			w:    UsageWindow{UserID: "alice"},
			want: UsageTotals{Calls: 3, PromptTokens: 55, CompletionTokens: 85, TotalTokens: 140, DurationMs: 650, Users: 1, Sessions: 2},
		},
		{
			name: "by provider",
			w:    UsageWindow{Provider: "qwen"},
			want: UsageTotals{Calls: 2, PromptTokens: 140, CompletionTokens: 260, TotalTokens: 400, DurationMs: 1500, Users: 2, Sessions: 2},
		},
		{
			name: "by model",
			w:    UsageWindow{Model: "deepseek-chat"},
			want: UsageTotals{Calls: 3, PromptTokens: 16, CompletionTokens: 27, TotalTokens: 43, DurationMs: 157, Users: 1, Sessions: 1},
		},
		{
			// Row D sits exactly on the lower bound: inclusive.
			name: "since inclusive",
			w:    UsageWindow{Since: at(2024, time.May, 3, 0, 0, 0)},
			want: UsageTotals{Calls: 2, PromptTokens: 41, CompletionTokens: 62, TotalTokens: 103, DurationMs: 507, Users: 1, Sessions: 2},
		},
		{
			// Row D sits exactly on the upper bound: exclusive.
			name: "until exclusive",
			w:    UsageWindow{Until: at(2024, time.May, 3, 0, 0, 0)},
			want: UsageTotals{Calls: 3, PromptTokens: 115, CompletionTokens: 225, TotalTokens: 340, DurationMs: 1150, Users: 2, Sessions: 2},
		},
		{
			name: "single day",
			w:    UsageWindow{Since: at(2024, time.May, 2, 0, 0, 0), Until: at(2024, time.May, 3, 0, 0, 0)},
			want: UsageTotals{Calls: 1, PromptTokens: 100, CompletionTokens: 200, TotalTokens: 300, DurationMs: 1000, Users: 1, Sessions: 1},
		},
		{
			name: "empty window",
			w:    UsageWindow{Since: at(2024, time.May, 2, 0, 0, 0), Until: at(2024, time.May, 2, 0, 0, 0)},
			want: UsageTotals{},
		},
		{
			name: "unknown user",
			w:    UsageWindow{UserID: "nobody"},
			want: UsageTotals{},
		},
		{
			name: "user and model",
			w:    UsageWindow{UserID: "alice", Model: "qwen-plus"},
			want: UsageTotals{Calls: 1, PromptTokens: 40, CompletionTokens: 60, TotalTokens: 100, DurationMs: 500, Users: 1, Sessions: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.QueryUsageTotals(ctx, tc.w)
			if err != nil {
				t.Fatalf("QueryUsageTotals: %v", err)
			}
			assertTotals(t, tc.name, got, tc.want)
		})
	}
}

func TestQueryUsageTotalsUsersExcludesEmptyUserID(t *testing.T) {
	s := newTestStore(t)
	ctx := testContext(t)
	seedUsageRows(t, s, []usageRow{
		{at: at(2024, time.May, 1, 10, 0, 0), user: "", provider: "p", model: "m", session: "s1", prompt: 1, completion: 1, total: 2, duration: 1},
		{at: at(2024, time.May, 1, 11, 0, 0), user: "", provider: "p", model: "m", session: "s2", prompt: 1, completion: 1, total: 2, duration: 1},
		{at: at(2024, time.May, 1, 12, 0, 0), user: "alice", provider: "p", model: "m", session: "s3", prompt: 1, completion: 1, total: 2, duration: 1},
	})

	totals, err := s.QueryUsageTotals(ctx, UsageWindow{})
	if err != nil {
		t.Fatalf("QueryUsageTotals: %v", err)
	}
	if totals.Calls != 3 {
		t.Errorf("Calls = %d, want 3", totals.Calls)
	}
	if totals.Users != 1 {
		t.Errorf("Users = %d, want 1 (unattributed rows must not be counted)", totals.Users)
	}
	if totals.Sessions != 3 {
		t.Errorf("Sessions = %d, want 3", totals.Sessions)
	}

	// The unattributed rows still appear as the empty-key group in ByUser.
	byUser, err := s.QueryUsageByUser(ctx, UsageWindow{}, 0)
	if err != nil {
		t.Fatalf("QueryUsageByUser: %v", err)
	}
	assertEqualStrings(t, "byUser keys", groupKeys(byUser), []string{"", "alice"})
}

func TestQueryUsageByUserModelProviderOrdering(t *testing.T) {
	s := newTestStore(t)
	ctx := testContext(t)
	seedUsageRows(t, s, usageFixture())

	t.Run("by user", func(t *testing.T) {
		rows, err := s.QueryUsageByUser(ctx, UsageWindow{}, 0)
		if err != nil {
			t.Fatalf("QueryUsageByUser: %v", err)
		}
		// bob 300 > alice 140 > "" 3, descending by total tokens.
		assertEqualStrings(t, "keys", groupKeys(rows), []string{"bob", "alice", ""})
		assertTotals(t, "bob", rows[0].UsageTotals, UsageTotals{
			Calls: 1, PromptTokens: 100, CompletionTokens: 200, TotalTokens: 300, DurationMs: 1000, Users: 1, Sessions: 1,
		})
		assertTotals(t, "alice", rows[1].UsageTotals, UsageTotals{
			Calls: 3, PromptTokens: 55, CompletionTokens: 85, TotalTokens: 140, DurationMs: 650, Users: 1, Sessions: 2,
		})
		assertTotals(t, "unattributed", rows[2].UsageTotals, UsageTotals{
			Calls: 1, PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, DurationMs: 7, Users: 0, Sessions: 1,
		})
	})

	t.Run("by model", func(t *testing.T) {
		rows, err := s.QueryUsageByModel(ctx, UsageWindow{}, 0)
		if err != nil {
			t.Fatalf("QueryUsageByModel: %v", err)
		}
		assertEqualStrings(t, "keys", groupKeys(rows), []string{"qwen-plus", "deepseek-chat"})
		if rows[0].TotalTokens != 400 || rows[1].TotalTokens != 43 {
			t.Errorf("totals = %d/%d, want 400/43", rows[0].TotalTokens, rows[1].TotalTokens)
		}
	})

	t.Run("by provider", func(t *testing.T) {
		rows, err := s.QueryUsageByProvider(ctx, UsageWindow{}, 0)
		if err != nil {
			t.Fatalf("QueryUsageByProvider: %v", err)
		}
		assertEqualStrings(t, "keys", groupKeys(rows), []string{"qwen", "deepseek"})
		if rows[0].TotalTokens != 400 || rows[1].TotalTokens != 43 {
			t.Errorf("totals = %d/%d, want 400/43", rows[0].TotalTokens, rows[1].TotalTokens)
		}
	})

	t.Run("window applies", func(t *testing.T) {
		w := UsageWindow{Since: at(2024, time.May, 3, 0, 0, 0)}
		rows, err := s.QueryUsageByUser(ctx, w, 0)
		if err != nil {
			t.Fatalf("QueryUsageByUser: %v", err)
		}
		assertEqualStrings(t, "keys", groupKeys(rows), []string{"alice", ""})

		byModel, err := s.QueryUsageByModel(ctx, w, 0)
		if err != nil {
			t.Fatalf("QueryUsageByModel: %v", err)
		}
		assertEqualStrings(t, "model keys", groupKeys(byModel), []string{"qwen-plus", "deepseek-chat"})

		byProvider, err := s.QueryUsageByProvider(ctx, UsageWindow{Until: at(2024, time.May, 3, 0, 0, 0)}, 0)
		if err != nil {
			t.Fatalf("QueryUsageByProvider: %v", err)
		}
		assertEqualStrings(t, "provider keys", groupKeys(byProvider), []string{"qwen", "deepseek"})
	})

	t.Run("group sums match totals", func(t *testing.T) {
		totals, err := s.QueryUsageTotals(ctx, UsageWindow{})
		if err != nil {
			t.Fatalf("QueryUsageTotals: %v", err)
		}
		rows, err := s.QueryUsageByUser(ctx, UsageWindow{}, 0)
		if err != nil {
			t.Fatalf("QueryUsageByUser: %v", err)
		}
		var calls, tokens int64
		for _, r := range rows {
			calls += r.Calls
			tokens += r.TotalTokens
		}
		if calls != totals.Calls || tokens != totals.TotalTokens {
			t.Errorf("grouped sums = %d calls/%d tokens, totals = %d/%d", calls, tokens, totals.Calls, totals.TotalTokens)
		}
	})

	t.Run("limit", func(t *testing.T) {
		rows, err := s.QueryUsageByUser(ctx, UsageWindow{}, 1)
		if err != nil {
			t.Fatalf("QueryUsageByUser: %v", err)
		}
		assertEqualStrings(t, "keys", groupKeys(rows), []string{"bob"})
	})

	t.Run("no matching rows", func(t *testing.T) {
		rows, err := s.QueryUsageByUser(ctx, UsageWindow{UserID: "nobody"}, 0)
		if err != nil {
			t.Fatalf("QueryUsageByUser: %v", err)
		}
		if rows != nil {
			t.Errorf("rows = %#v, want nil", rows)
		}
	})
}

func TestQueryUsageByDay(t *testing.T) {
	s := newTestStore(t)
	ctx := testContext(t)
	seedUsageRows(t, s, usageFixture())

	rows, err := s.QueryUsageByDay(ctx, UsageWindow{}, 0)
	if err != nil {
		t.Fatalf("QueryUsageByDay: %v", err)
	}
	// One row per day with data, ascending.
	assertEqualStrings(t, "days", dayStrings(rows), []string{"2024-05-01", "2024-05-02", "2024-05-03"})
	assertTotals(t, "2024-05-01", rows[0].UsageTotals, UsageTotals{
		Calls: 2, PromptTokens: 15, CompletionTokens: 25, TotalTokens: 40, DurationMs: 150, Users: 1, Sessions: 1,
	})
	assertTotals(t, "2024-05-02", rows[1].UsageTotals, UsageTotals{
		Calls: 1, PromptTokens: 100, CompletionTokens: 200, TotalTokens: 300, DurationMs: 1000, Users: 1, Sessions: 1,
	})
	// 00:00:00Z and 23:59:59Z on 2024-05-03 bucket into the same UTC day;
	// the unattributed row is not counted in Users.
	assertTotals(t, "2024-05-03", rows[2].UsageTotals, UsageTotals{
		Calls: 2, PromptTokens: 41, CompletionTokens: 62, TotalTokens: 103, DurationMs: 507, Users: 1, Sessions: 2,
	})

	t.Run("day boundary is UTC", func(t *testing.T) {
		// A row at exactly 2024-05-02T00:00:00Z belongs to 05-02, not 05-01.
		midnight, err := s.QueryUsageByDay(ctx, UsageWindow{
			Since: at(2024, time.May, 2, 0, 0, 0),
			Until: at(2024, time.May, 2, 0, 0, 1),
		}, 0)
		if err != nil {
			t.Fatalf("QueryUsageByDay: %v", err)
		}
		assertEqualStrings(t, "midnight days", dayStrings(midnight), []string{"2024-05-02"})

		// A row at 2024-05-03T23:59:59Z stays on 05-03, not the next day.
		late, err := s.QueryUsageByDay(ctx, UsageWindow{
			Since: at(2024, time.May, 3, 23, 59, 59),
			Until: at(2024, time.May, 4, 0, 0, 0),
		}, 0)
		if err != nil {
			t.Fatalf("QueryUsageByDay: %v", err)
		}
		assertEqualStrings(t, "late days", dayStrings(late), []string{"2024-05-03"})
		if len(late) == 1 && late[0].TotalTokens != 100 {
			t.Errorf("2024-05-03 late bucket tokens = %d, want 100", late[0].TotalTokens)
		}
	})

	t.Run("window applies", func(t *testing.T) {
		windowed, err := s.QueryUsageByDay(ctx, UsageWindow{Since: at(2024, time.May, 2, 0, 0, 0)}, 0)
		if err != nil {
			t.Fatalf("QueryUsageByDay: %v", err)
		}
		assertEqualStrings(t, "days", dayStrings(windowed), []string{"2024-05-02", "2024-05-03"})

		byUser, err := s.QueryUsageByDay(ctx, UsageWindow{UserID: "alice"}, 0)
		if err != nil {
			t.Fatalf("QueryUsageByDay: %v", err)
		}
		assertEqualStrings(t, "alice days", dayStrings(byUser), []string{"2024-05-01", "2024-05-03"})
		if byUser[1].TotalTokens != 100 {
			t.Errorf("2024-05-03 alice tokens = %d, want 100", byUser[1].TotalTokens)
		}
	})

	t.Run("days bounds the number of buckets", func(t *testing.T) {
		two, err := s.QueryUsageByDay(ctx, UsageWindow{}, 2)
		if err != nil {
			t.Fatalf("QueryUsageByDay: %v", err)
		}
		assertEqualStrings(t, "days", dayStrings(two), []string{"2024-05-02", "2024-05-03"})
	})

	t.Run("no matching rows", func(t *testing.T) {
		none, err := s.QueryUsageByDay(ctx, UsageWindow{UserID: "nobody"}, 5)
		if err != nil {
			t.Fatalf("QueryUsageByDay: %v", err)
		}
		if none != nil {
			t.Errorf("days = %#v, want nil", none)
		}
	})
}

func TestQueryUsageByDayDefaultsAndClamp(t *testing.T) {
	s := newTestStore(t)
	ctx := testContext(t)

	// 400 consecutive days with one row each, total_tokens = day index + 1.
	const days = 400
	start := at(2024, time.January, 1, 12, 0, 0)
	rows := make([]usageRow, 0, days)
	for i := 0; i < days; i++ {
		rows = append(rows, usageRow{
			at: start.AddDate(0, 0, i), user: "d", provider: "p", model: "m", session: "s",
			prompt: i + 1, completion: 0, total: i + 1, duration: int64(i + 1),
		})
	}
	seedUsageRows(t, s, rows)

	dayOf := func(i int) string { return start.AddDate(0, 0, i).Format("2006-01-02") }

	t.Run("days <= 0 defaults to 30", func(t *testing.T) {
		for _, d := range []int{0, -7} {
			got, err := s.QueryUsageByDay(ctx, UsageWindow{}, d)
			if err != nil {
				t.Fatalf("QueryUsageByDay(%d): %v", d, err)
			}
			if len(got) != 30 {
				t.Fatalf("days = %v: len = %d, want 30", d, len(got))
			}
			if got[0].Day != dayOf(days-30) || got[29].Day != dayOf(days-1) {
				t.Errorf("days = %v: range = %s..%s, want %s..%s",
					d, got[0].Day, got[29].Day, dayOf(days-30), dayOf(days-1))
			}
			if got[0].TotalTokens != int64(days-30+1) || got[29].TotalTokens != days {
				t.Errorf("days = %v: totals = %d..%d, want %d..%d",
					d, got[0].TotalTokens, got[29].TotalTokens, days-30+1, days)
			}
		}
	})

	t.Run("days above the cap clamps to 366", func(t *testing.T) {
		got, err := s.QueryUsageByDay(ctx, UsageWindow{}, 100000)
		if err != nil {
			t.Fatalf("QueryUsageByDay: %v", err)
		}
		if len(got) != maxTrendDays {
			t.Fatalf("len = %d, want %d", len(got), maxTrendDays)
		}
		if got[0].Day != dayOf(days-maxTrendDays) || got[maxTrendDays-1].Day != dayOf(days-1) {
			t.Errorf("range = %s..%s, want %s..%s",
				got[0].Day, got[maxTrendDays-1].Day, dayOf(days-maxTrendDays), dayOf(days-1))
		}
	})

	t.Run("ascending order", func(t *testing.T) {
		got, err := s.QueryUsageByDay(ctx, UsageWindow{}, 50)
		if err != nil {
			t.Fatalf("QueryUsageByDay: %v", err)
		}
		if len(got) != 50 {
			t.Fatalf("len = %d, want 50", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].Day >= got[i].Day {
				t.Fatalf("days not ascending: %s then %s", got[i-1].Day, got[i].Day)
			}
		}
	})
}

func TestQueryUsageByDayBucketsGoWrittenRows(t *testing.T) {
	s := newTestStore(t)
	ctx := testContext(t)

	// The accept boundary: a row written by RecordUsage is stamped with the
	// current UTC instant, so it must land in today's UTC day.
	startDay := time.Now().UTC().Format("2006-01-02")
	if err := s.RecordUsage(ctx, UsageEvent{
		SessionID: "s1", UserID: "alice", Provider: "deepseek", Model: "deepseek-chat",
		PromptTokens: 1, CompletionTokens: 2, TotalTokens: 5, DurationMs: 9,
	}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	endDay := time.Now().UTC().Format("2006-01-02")

	rows, err := s.QueryUsageByDay(ctx, UsageWindow{}, 1)
	if err != nil {
		t.Fatalf("QueryUsageByDay: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("days = %v, want exactly one bucket", dayStrings(rows))
	}
	if rows[0].Day != startDay && rows[0].Day != endDay {
		t.Errorf("day = %q, want the UTC day of the write (%q or %q)", rows[0].Day, startDay, endDay)
	}
	if rows[0].TotalTokens != 5 || rows[0].Calls != 1 || rows[0].Users != 1 {
		t.Errorf("bucket = %+v, want 1 call / 5 tokens / 1 user", rows[0].UsageTotals)
	}

	// The stored value is a UTC instant whose text starts with that day.
	var stored string
	if err := s.DB().QueryRow(`SELECT CAST(created_at AS TEXT) FROM usage_logs`).Scan(&stored); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if !strings.HasPrefix(stored, rows[0].Day) {
		t.Errorf("stored created_at = %q, want the %s UTC day prefix", stored, rows[0].Day)
	}
}

func TestQueryUsageRecent(t *testing.T) {
	s := newTestStore(t)
	ctx := testContext(t)
	seedUsageRows(t, s, usageFixture())

	rows, err := s.QueryUsageRecent(ctx, UsageWindow{}, 0)
	if err != nil {
		t.Fatalf("QueryUsageRecent: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("len = %d, want 5", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].ID <= rows[i].ID {
			t.Fatalf("not newest first: id %d before %d", rows[i-1].ID, rows[i].ID)
		}
	}
	if rows[0].SessionID != "s3" || rows[0].UserID != "alice" || rows[0].TotalTokens != 100 {
		t.Errorf("newest = %+v, want the 2024-05-03T23:59:59 alice/s3 row", rows[0])
	}

	t.Run("window applies", func(t *testing.T) {
		byUser, err := s.QueryUsageRecent(ctx, UsageWindow{UserID: "alice"}, 0)
		if err != nil {
			t.Fatalf("QueryUsageRecent: %v", err)
		}
		if len(byUser) != 3 {
			t.Fatalf("alice rows = %d, want 3", len(byUser))
		}
		for _, r := range byUser {
			if r.UserID != "alice" {
				t.Errorf("user_id = %q, want alice", r.UserID)
			}
		}

		early, err := s.QueryUsageRecent(ctx, UsageWindow{Until: at(2024, time.May, 3, 0, 0, 0)}, 0)
		if err != nil {
			t.Fatalf("QueryUsageRecent: %v", err)
		}
		if len(early) != 3 {
			t.Fatalf("rows before 2024-05-03 = %d, want 3", len(early))
		}
		if early[0].SessionID != "s2" {
			t.Errorf("newest early row = %+v, want s2", early[0])
		}

		windowed, err := s.QueryUsageRecent(ctx, UsageWindow{
			Since:    at(2024, time.May, 2, 0, 0, 0),
			Until:    at(2024, time.May, 3, 0, 0, 0),
			Provider: "qwen",
			Model:    "qwen-plus",
		}, 0)
		if err != nil {
			t.Fatalf("QueryUsageRecent: %v", err)
		}
		if len(windowed) != 1 || windowed[0].TotalTokens != 300 {
			t.Fatalf("windowed rows = %+v, want the single 300-token qwen row", windowed)
		}
	})

	t.Run("limit", func(t *testing.T) {
		two, err := s.QueryUsageRecent(ctx, UsageWindow{}, 2)
		if err != nil {
			t.Fatalf("QueryUsageRecent: %v", err)
		}
		if len(two) != 2 {
			t.Errorf("len = %d, want 2", len(two))
		}
	})

	t.Run("no matching rows", func(t *testing.T) {
		none, err := s.QueryUsageRecent(ctx, UsageWindow{UserID: "nobody"}, 0)
		if err != nil {
			t.Fatalf("QueryUsageRecent: %v", err)
		}
		if none != nil {
			t.Errorf("rows = %#v, want nil", none)
		}
	})
}

func TestQueryUsageGroupedAndRecentLimits(t *testing.T) {
	s := newTestStore(t)
	ctx := testContext(t)

	// 1100 rows with distinct users and increasing token totals, so ordering,
	// the default limits and the hard caps are all observable.
	const rows = 1100
	seed := make([]usageRow, 0, rows)
	base := at(2024, time.June, 1, 0, 0, 0)
	for i := 0; i < rows; i++ {
		seed = append(seed, usageRow{
			at: base.Add(time.Duration(i) * time.Second), user: fmt.Sprintf("u%04d", i),
			provider: "p", model: "m", session: "s",
			prompt: i + 1, completion: 0, total: i + 1, duration: 1,
		})
	}
	seedUsageRows(t, s, seed)

	t.Run("grouped default limit is 50", func(t *testing.T) {
		got, err := s.QueryUsageByUser(ctx, UsageWindow{}, 0)
		if err != nil {
			t.Fatalf("QueryUsageByUser: %v", err)
		}
		if len(got) != defaultGroupLimit {
			t.Fatalf("len = %d, want %d", len(got), defaultGroupLimit)
		}
		if got[0].Key != "u1099" || got[0].TotalTokens != rows {
			t.Errorf("top group = %+v, want u1099 with %d tokens", got[0], rows)
		}
	})

	t.Run("grouped limit is capped at 500", func(t *testing.T) {
		got, err := s.QueryUsageByUser(ctx, UsageWindow{}, 100000)
		if err != nil {
			t.Fatalf("QueryUsageByUser: %v", err)
		}
		if len(got) != maxGroupLimit {
			t.Fatalf("len = %d, want %d", len(got), maxGroupLimit)
		}
		if got[0].Key != "u1099" || got[maxGroupLimit-1].Key != "u0600" {
			t.Errorf("keys = %s..%s, want u1099..u0600", got[0].Key, got[maxGroupLimit-1].Key)
		}
	})

	t.Run("recent default limit is 100", func(t *testing.T) {
		got, err := s.QueryUsageRecent(ctx, UsageWindow{}, 0)
		if err != nil {
			t.Fatalf("QueryUsageRecent: %v", err)
		}
		if len(got) != 100 {
			t.Fatalf("len = %d, want 100", len(got))
		}
		if got[0].ID != rows || got[99].ID != rows-99 {
			t.Errorf("ids = %d..%d, want %d..%d", got[0].ID, got[99].ID, rows, rows-99)
		}
	})

	t.Run("recent limit is capped at 1000", func(t *testing.T) {
		got, err := s.QueryUsageRecent(ctx, UsageWindow{}, 100000)
		if err != nil {
			t.Fatalf("QueryUsageRecent: %v", err)
		}
		if len(got) != 1000 {
			t.Fatalf("len = %d, want 1000", len(got))
		}
		if got[0].ID != rows || got[999].ID != rows-999 {
			t.Errorf("ids = %d..%d, want %d..%d", got[0].ID, got[999].ID, rows, rows-999)
		}
	})

	t.Run("recent honours the window", func(t *testing.T) {
		got, err := s.QueryUsageRecent(ctx, UsageWindow{Since: base.Add(time.Duration(rows-3) * time.Second)}, 0)
		if err != nil {
			t.Fatalf("QueryUsageRecent: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		if got[2].UserID != "u1097" || got[0].UserID != "u1099" {
			t.Errorf("users = %s..%s, want u1097..u1099", got[2].UserID, got[0].UserID)
		}
	})
}

func TestAnalyticsErrorsAreWrapped(t *testing.T) {
	s := newTestStore(t)
	ctx := testContext(t)

	// Dropping the table makes every query fail; each method must surface a
	// wrapped error instead of swallowing it or panicking.
	if _, err := s.DB().ExecContext(ctx, `DROP TABLE usage_logs`); err != nil {
		t.Fatalf("drop usage_logs: %v", err)
	}

	calls := []struct {
		name string
		fn   func() error
	}{
		{"QueryUsageTotals", func() error { _, err := s.QueryUsageTotals(ctx, UsageWindow{}); return err }},
		{"QueryUsageByUser", func() error { _, err := s.QueryUsageByUser(ctx, UsageWindow{}, 0); return err }},
		{"QueryUsageByModel", func() error { _, err := s.QueryUsageByModel(ctx, UsageWindow{}, 0); return err }},
		{"QueryUsageByProvider", func() error { _, err := s.QueryUsageByProvider(ctx, UsageWindow{}, 0); return err }},
		{"QueryUsageByDay", func() error { _, err := s.QueryUsageByDay(ctx, UsageWindow{}, 0); return err }},
		{"QueryUsageRecent", func() error { _, err := s.QueryUsageRecent(ctx, UsageWindow{}, 0); return err }},
		{"QueryUsage", func() error { _, err := s.QueryUsage(ctx, UsageFilter{}); return err }},
	}
	for _, c := range calls {
		err := c.fn()
		if err == nil {
			t.Errorf("%s: err = nil, want a wrapped error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "usage") {
			t.Errorf("%s: err = %q, want a usage-context message", c.name, err)
		}
	}
}

func TestAnalyticsEmptyResults(t *testing.T) {
	s := newTestStore(t)
	ctx := testContext(t)

	totals, err := s.QueryUsageTotals(ctx, UsageWindow{})
	if err != nil {
		t.Fatalf("QueryUsageTotals: %v", err)
	}
	assertTotals(t, "empty", totals, UsageTotals{})

	groups := map[string][]UsageGroupRow{}
	byUser, err := s.QueryUsageByUser(ctx, UsageWindow{}, 0)
	if err != nil {
		t.Fatalf("QueryUsageByUser: %v", err)
	}
	groups["user"] = byUser
	byModel, err := s.QueryUsageByModel(ctx, UsageWindow{}, 0)
	if err != nil {
		t.Fatalf("QueryUsageByModel: %v", err)
	}
	groups["model"] = byModel
	byProvider, err := s.QueryUsageByProvider(ctx, UsageWindow{}, 0)
	if err != nil {
		t.Fatalf("QueryUsageByProvider: %v", err)
	}
	groups["provider"] = byProvider
	for name, rows := range groups {
		if rows != nil {
			t.Errorf("by %s = %#v, want nil (empty, not an error)", name, rows)
		}
	}

	days, err := s.QueryUsageByDay(ctx, UsageWindow{}, 0)
	if err != nil {
		t.Fatalf("QueryUsageByDay: %v", err)
	}
	if days != nil {
		t.Errorf("by day = %#v, want nil", days)
	}

	recent, err := s.QueryUsageRecent(ctx, UsageWindow{}, 0)
	if err != nil {
		t.Fatalf("QueryUsageRecent: %v", err)
	}
	if recent != nil {
		t.Errorf("recent = %#v, want nil", recent)
	}

	list, err := s.QueryUsage(ctx, UsageFilter{})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("QueryUsage len = %d, want 0", len(list))
	}

	// A window on an empty table (including time bounds) stays empty.
	windowed, err := s.QueryUsageTotals(ctx, UsageWindow{
		Since: at(2024, time.January, 1, 0, 0, 0),
		Until: at(2024, time.December, 31, 0, 0, 0),
	})
	if err != nil {
		t.Fatalf("QueryUsageTotals: %v", err)
	}
	assertTotals(t, "empty window", windowed, UsageTotals{})
}

func TestQueryUsageByProviderModel(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Two providers, and the SAME model name through both, to prove the
	// grouping key is the pair rather than the model alone.
	seed := []UsageEvent{
		{UserID: "u1", SessionID: "s1", Provider: "deepseek", Model: "shared-model",
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		{UserID: "u2", SessionID: "s2", Provider: "qwen", Model: "shared-model",
			PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300},
		{UserID: "u1", SessionID: "s1", Provider: "deepseek", Model: "shared-model",
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		{UserID: "u3", SessionID: "s3", Provider: "qwen", Model: "other-model",
			PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	for _, e := range seed {
		if err := st.RecordUsage(ctx, e); err != nil {
			t.Fatalf("record usage: %v", err)
		}
	}

	groups, err := st.QueryUsageByProviderModel(ctx, UsageWindow{}, 50)
	if err != nil {
		t.Fatalf("QueryUsageByProviderModel: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3 (provider,model pairs): %+v", len(groups), groups)
	}

	// Ordered by total tokens descending, ties broken by provider then model:
	// both shared-model pairs total 300, and "deepseek" sorts before "qwen".
	got := []string{groups[0].Provider + "/" + groups[1].Provider, groups[2].Provider}
	if got[0] != "deepseek/qwen" {
		t.Errorf("first two groups = %v, want the two 300-token pairs in provider order", got[0])
	}
	if groups[0].TotalTokens != 300 || groups[1].TotalTokens != 300 || groups[2].TotalTokens != 15 {
		t.Errorf("order = %d/%d/%d, want 300/300/15",
			groups[0].TotalTokens, groups[1].TotalTokens, groups[2].TotalTokens)
	}

	// The same model name must appear under both providers, keeping its own totals.
	byPair := map[string]int64{}
	for _, g := range groups {
		byPair[g.Provider+"/"+g.Model] = g.TotalTokens
	}
	if byPair["deepseek/shared-model"] != 300 {
		t.Errorf("deepseek/shared-model = %d, want 300 (two rows combined)",
			byPair["deepseek/shared-model"])
	}
	if byPair["qwen/shared-model"] != 300 {
		t.Errorf("qwen/shared-model = %d, want 300", byPair["qwen/shared-model"])
	}
	if byPair["qwen/other-model"] != 15 {
		t.Errorf("qwen/other-model = %d, want 15", byPair["qwen/other-model"])
	}

	// A window filter must apply here too.
	filtered, err := st.QueryUsageByProviderModel(ctx, UsageWindow{Provider: "qwen"}, 50)
	if err != nil {
		t.Fatalf("filtered query: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered groups = %d, want 2", len(filtered))
	}
	for _, g := range filtered {
		if g.Provider != "qwen" {
			t.Errorf("filter leaked a %q row", g.Provider)
		}
	}
}

func TestQueryUsageByProviderModel_Empty(t *testing.T) {
	st := newTestStore(t)
	groups, err := st.QueryUsageByProviderModel(context.Background(), UsageWindow{}, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("groups = %d, want 0", len(groups))
	}
}
