// Package store provides SQLite-backed persistence for huan-agent.
//
// The MVP uses pure-Go SQLite (modernc.org/sqlite) to avoid CGO. Schema
// migrations are run at Open() time and tracked in `schema_migrations`.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned by queries that yield zero rows.
var ErrNotFound = errors.New("not found")

// Store is the persistence interface.
type Store interface {
	Close() error

	// Usage log accessors (Phase 1).
	RecordUsage(ctx context.Context, e UsageEvent) error
	QueryUsage(ctx context.Context, f UsageFilter) ([]UsageRecord, error)

	// Tool invocation audit log (Phase 2).
	RecordInvocation(ctx context.Context, e InvocationEvent) error
	QueryInvocations(ctx context.Context, f InvocationFilter) ([]InvocationRecord, error)

	// Usage aggregation (Phase 5).
	QueryUsageTotals(ctx context.Context, w UsageWindow) (UsageTotals, error)
	QueryUsageByUser(ctx context.Context, w UsageWindow, limit int) ([]UsageGroupRow, error)
	QueryUsageByModel(ctx context.Context, w UsageWindow, limit int) ([]UsageGroupRow, error)
	QueryUsageByProvider(ctx context.Context, w UsageWindow, limit int) ([]UsageGroupRow, error)
	QueryUsageByProviderModel(ctx context.Context, w UsageWindow, limit int) ([]ProviderModelGroup, error)
	QueryUsageByDay(ctx context.Context, w UsageWindow, days int) ([]UsageDayRow, error)
	QueryUsageRecent(ctx context.Context, w UsageWindow, limit int) ([]UsageRecord, error)

	// Chat sessions for the web UI (Phase 5 chat).
	CreateChatSession(ctx context.Context, sess ChatSession) error
	GetChatSession(ctx context.Context, id string) (ChatSession, error)
	ListChatSessions(ctx context.Context, f ChatSessionFilter) ([]ChatSession, error)
	UpdateChatSession(ctx context.Context, id string, patch ChatSessionPatch) error
	TouchChatSession(ctx context.Context, id string) error
	DeleteChatSession(ctx context.Context, id string) error
	AppendChatMessage(ctx context.Context, sessionID string, m ChatMessage) (int64, error)
	ListChatMessages(ctx context.Context, sessionID string, limit int) ([]ChatMessage, error)
	DeleteChatMessages(ctx context.Context, sessionID string) error

	// Underlying handle, used sparingly (e.g. health checks).
	DB() *sql.DB
}

// UsageEvent is the input shape for recording an LLM call.
type UsageEvent struct {
	SessionID        string
	UserID           string // attributing end user; empty = unattributed
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	DurationMs       int64
}

// UsageRecord is the persisted view of a usage event. The JSON tags define the
// wire shape served by the admin usage API.
type UsageRecord struct {
	ID               int64  `json:"id"`
	SessionID        string `json:"session_id"`
	UserID           string `json:"user_id"` // empty = unattributed
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	DurationMs       int64  `json:"duration_ms"`
	CreatedAt        string `json:"created_at"` // RFC3339 string from SQLite
}

// UsageFilter narrows QueryUsage results. Zero-value fields are ignored.
type UsageFilter struct {
	SessionID string
	UserID    string // empty = all users
	Provider  string
	Since     time.Time // zero value = no lower bound (inclusive)
	Until     time.Time // zero value = no upper bound (exclusive)
	Limit     int       // default 100, max 1000
}

// InvocationEvent is the input shape for recording a tool call.
type InvocationEvent struct {
	SessionID  string
	UserID     string // attributing end user; empty = unattributed
	ToolName   string
	Arguments  string
	Result     string
	Err        string
	DurationMs int64
}

// InvocationRecord is the persisted view of a tool call. The JSON tags define
// the wire shape served by the admin audit API.
type InvocationRecord struct {
	ID         int64  `json:"id"`
	SessionID  string `json:"session_id"`
	UserID     string `json:"user_id"` // empty = unattributed
	ToolName   string `json:"tool_name"`
	Arguments  string `json:"arguments"`
	Result     string `json:"result"`
	Err        string `json:"err"`
	DurationMs int64  `json:"duration_ms"`
	CreatedAt  string `json:"created_at"`
}

// InvocationFilter narrows QueryInvocations results. Zero-value fields are ignored.
type InvocationFilter struct {
	SessionID string
	UserID    string // empty = all users
	ToolName  string
	Since     time.Time // zero value = no lower bound (inclusive)
	Until     time.Time // zero value = no upper bound (exclusive)
	Limit     int       // default 100, max 1000
}

// Open returns a SQLite-backed Store. The caller MUST call Close().
func Open(ctx context.Context, path string) (Store, error) {
	s := &sqliteStore{path: path}
	if err := s.dbOpen(ctx); err != nil {
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// newSQLite is the internal helper to construct a *sql.DB from a DSN.
func newSQLite(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite path is empty")
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite single-writer; queue readers
	return db, nil
}
