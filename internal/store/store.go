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

	// --- LLM catalog: providers, their models and capability bindings ---
	UpsertProvider(ctx context.Context, p Provider) error
	GetProvider(ctx context.Context, id string) (Provider, error)
	ListProviders(ctx context.Context) ([]Provider, error)
	SetProviderKey(ctx context.Context, id, key string) error
	SetProviderError(ctx context.Context, id, msg string) error
	DeleteProvider(ctx context.Context, id string) error

	ReplaceFetchedModels(ctx context.Context, providerID string, models []Model) error
	UpsertModel(ctx context.Context, m Model) error
	ListModels(ctx context.Context, providerID string) ([]Model, error)
	GetModel(ctx context.Context, providerID, modelID string) (Model, error)
	DeleteModel(ctx context.Context, providerID, modelID string) error

	// LatestSessionModel reports the most recently used conversation's model.
	LatestSessionModel(ctx context.Context) (provider, model string, ok bool)

	SetBinding(ctx context.Context, b Binding) error
	ListBindings(ctx context.Context) ([]Binding, error)

	// MCP servers the console manages (设置 → MCP).
	UpsertMCPServer(ctx context.Context, m MCPServer) error
	GetMCPServer(ctx context.Context, id string) (MCPServer, error)
	ListMCPServers(ctx context.Context) ([]MCPServer, error)
	DeleteMCPServer(ctx context.Context, id string) error
	SetMCPServerError(ctx context.Context, id, msg string) error

	QueryUsage(ctx context.Context, f UsageFilter) ([]UsageRecord, error)

	// Tool invocation audit log (Phase 2).
	RecordInvocation(ctx context.Context, e InvocationEvent) error
	QueryInvocations(ctx context.Context, f InvocationFilter) ([]InvocationRecord, error)
	// QueryInvocationTotals is the aggregate a conversation's header shows:
	// how many tools ran in it, and how long they took together.
	QueryInvocationTotals(ctx context.Context, sessionID string) (InvocationTotals, error)

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

	// The task plan a conversation's model maintains (任务看板), and the one
	// piece of turn state that deliberately outlives its turn: it is what a turn
	// that died cannot report and the next one needs in order to continue rather
	// than start over.
	GetChatPlan(ctx context.Context, sessionID string) (ChatPlanRow, error)
	SetChatPlan(ctx context.Context, row ChatPlanRow) error
	DeleteChatPlan(ctx context.Context, sessionID string) error

	// Media attachments uploaded for a chat session (Phase 5 chat).
	// The bytes live in the workspace; these rows are what a message references.
	CreateMediaAsset(ctx context.Context, a MediaAsset) error
	GetMediaAsset(ctx context.Context, id string) (MediaAsset, error)
	ListMediaAssets(ctx context.Context, sessionID string) ([]MediaAsset, error)
	FindMediaAssetBySHA256(ctx context.Context, sessionID, digest string) (MediaAsset, error)

	// Workspaces (Phase 9): the directories the agent can be pointed at, and
	// which one each scope (a conversation, a Feishu user) is in.
	UpsertWorkspace(ctx context.Context, w Workspace) error
	ListWorkspaces(ctx context.Context) ([]Workspace, error)
	GetWorkspace(ctx context.Context, name string) (Workspace, error)
	RenameWorkspace(ctx context.Context, from, to string) error
	DeleteWorkspace(ctx context.Context, name string) error
	GetWorkspaceBinding(ctx context.Context, scope string) (string, error)
	SetWorkspaceBinding(ctx context.Context, scope, name string) error
	ClearWorkspaceBinding(ctx context.Context, scope string) error
	MoveWorkspaceBindings(ctx context.Context, from, to string) (int, error)
	ListScopesInWorkspace(ctx context.Context, name string) ([]string, error)
	ListUnboundWebSessions(ctx context.Context) ([]string, error)

	// App settings (设置 → 对话预算): the per-turn budget the console may change
	// while the process runs. Only an explicit override is stored; a nil field
	// means the key is absent and config.yaml governs.
	GetTurnBudgetOverride(ctx context.Context) (TurnBudgetOverride, error)
	SetTurnBudgetOverride(ctx context.Context, o TurnBudgetOverride) error

	// Trace store (Phase 5c). An agent turn and its observation nodes, so the
	// console can explain a turn without an external observability service.
	RecordTrace(ctx context.Context, t TraceRow) error
	RecordObservation(ctx context.Context, o ObservationRow) error
	EndTrace(ctx context.Context, id, output string, endedAt time.Time) error
	EndObservation(ctx context.Context, id string, end ObservationEnd) error
	ListTraces(ctx context.Context, f TraceFilter) ([]TraceRow, error)
	GetTrace(ctx context.Context, id string) (TraceRow, []ObservationRow, error)

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
	SessionID string
	UserID    string // attributing end user; empty = unattributed
	ToolName  string
	Arguments string
	Result    string
	Err       string
	// Workspace is the workspace the call ran in, by name. Empty means no named
	// workspace was in play (a pre-workspace row, or a deployment using only the
	// built-in default).
	//
	// It is recorded because the arguments cannot answer the question: a tool
	// path is workspace-relative, so `write_file src/main.go` looks identical in
	// every workspace, and the audit would be unable to say which project a
	// change landed in.
	Workspace  string
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
	Workspace  string `json:"workspace,omitempty"`
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
