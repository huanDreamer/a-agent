// Package viking is the OpenViking integration's facade: one value that owns
// the HTTP client, the memory mirror and the document syncer, and answers the
// questions the CLI, the Feishu bot and the console all ask — "are we
// connected?", "what has been written?", "sync now".
//
// Keeping it here (rather than in cmd) means the three front ends cannot drift
// apart in how they decide whether OpenViking is usable, and the packages that
// do the actual work (internal/openviking, internal/memory, internal/documents)
// stay independent of one another.
package viking

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/documents"
	"github.com/huan/huan-agent/internal/memory"
	"github.com/huan/huan-agent/internal/openviking"
)

// ErrDisabled is returned by New when the integration is switched off, so a
// caller that forgot to check reports a sentence instead of a nil dereference.
var ErrDisabled = errors.New("viking: openviking integration is disabled")

// healthTimeout bounds the status check. It is short because the status panel
// is on the other end of it: a hung server must not hang the console.
const healthTimeout = 5 * time.Second

// Service is the configured integration.
type Service struct {
	cfg     config.OpenVikingConfig
	client  *openviking.Client
	syncer  *documents.Syncer
	workDir string
	logger  *zap.Logger

	mu         sync.Mutex
	mirror     *memory.OpenVikingStore
	lastSync   *documents.Report
	lastSyncAt *time.Time
	lastHealth *openviking.Health
	healthErr  string
	healthAt   time.Time
}

// New builds the service. It returns ErrDisabled when the section is off, which
// every caller treats as "run exactly as before".
func New(cfg config.OpenVikingConfig, tools config.ToolsConfig, logger *zap.Logger) (*Service, error) {
	if !cfg.Enabled() {
		return nil, ErrDisabled
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	client, err := openviking.New(openviking.Config{
		BaseURL: cfg.BaseURL,
		APIKey:  cfg.APIKey,
		Account: cfg.AccountOr(),
		User:    cfg.UserOr(),
		Timeout: cfg.Timeout(),
	})
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, client: client, logger: logger}
	if cfg.Documents.Enable {
		s.workDir = cfg.Documents.WorkspaceRoot(tools)
		syncer, serr := documents.New(client, documents.Config{
			RootURI:        cfg.DocumentsRoot(),
			Include:        cfg.Documents.IncludesOr(),
			Exclude:        cfg.Documents.ExcludesOr(),
			MaxFileBytes:   cfg.Documents.MaxFileBytes(),
			UploadBinaries: cfg.Documents.UploadsBinaries(),
			WaitForIndex:   cfg.Documents.WaitIndex,
			TimeoutSeconds: float64(cfg.Timeout().Seconds()),
			StatePath:      cfg.Documents.StateFile(),
		}, logger)
		if serr != nil {
			return nil, serr
		}
		s.syncer = syncer
	}
	return s, nil
}

// Config returns the configuration the service was built from.
func (s *Service) Config() config.OpenVikingConfig { return s.cfg }

// Client returns the underlying HTTP client.
func (s *Service) Client() *openviking.Client { return s.client }

// Mirror returns the memory mirror, or nil when memory mirroring is off or
// WrapStore has not been called yet.
func (s *Service) Mirror() *memory.OpenVikingStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mirror
}

// WrapStore returns the long-term store the agent should use: the local store,
// mirrored into OpenViking.
//
// The mirror is built once and reused, because its batching and counters are
// per-process state: building one per session would split "how much have we
// submitted" across sessions and submit half-empty batches. Call it once with
// the process's local store; later calls return the same mirror.
func (s *Service) WrapStore(local memory.Store) memory.Store {
	if local == nil {
		return nil
	}
	if !s.cfg.Memory.Enable {
		return local
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mirror == nil {
		wrapped := memory.NewOpenVikingStore(local, s.client, memory.OpenVikingMirrorConfig{
			SessionPrefix: s.cfg.SessionPrefixOr(),
			Commit:        s.cfg.Memory.Commit,
			FlushEvery:    s.cfg.FlushEveryOr(),
			RecallEnable:  s.cfg.Memory.RecallEnable,
			RecallLimit:   s.cfg.RecallLimitOr(),
			RecallTarget:  s.cfg.RecallScope(),
			JournalURI:    s.cfg.DocumentsRoot() + "/memory/journal.md",
			JournalEnable: s.cfg.Memory.JournalEnable,
		}, s.logger)
		if m, ok := wrapped.(*memory.OpenVikingStore); ok {
			s.mirror = m
		}
	}
	return s.mirror
}

// Documents returns the document syncer, or nil when document storage is off.
func (s *Service) Documents() *documents.Syncer { return s.syncer }

// SyncedDocuments lists what has been stored — saved documents and synchronized
// workspace files, newest first — which is what the console shows.
func (s *Service) SyncedDocuments() []documents.Entry {
	if s.syncer == nil {
		return nil
	}
	return s.syncer.Documents()
}

// WorkspaceDir returns the directory workspace sync reads, empty when none is
// configured.
func (s *Service) WorkspaceDir() string { return s.workDir }

// SaveDocument writes one document and returns its URI.
func (s *Service) SaveDocument(ctx context.Context, doc documents.Document) (string, error) {
	if s.syncer == nil {
		return "", errors.New("viking: document storage is disabled")
	}
	return s.syncer.Save(ctx, doc)
}

// SyncWorkspace synchronizes the configured workspace and remembers the report,
// so the console can show what the last run did.
//
// A sync with no workspace directory configured is refused rather than silently
// uploading the process working directory.
func (s *Service) SyncWorkspace(ctx context.Context, full bool) (documents.Report, error) {
	if s.syncer == nil {
		return documents.Report{}, errors.New("viking: document storage is disabled")
	}
	if strings.TrimSpace(s.workDir) == "" {
		return documents.Report{}, fmt.Errorf("viking: %w (set openviking.documents.workspace_dir or tools.workspace)", documents.ErrNoWorkspace)
	}
	rep, err := s.syncer.SyncWorkspace(ctx, s.workDir, full)
	now := time.Now()
	s.mu.Lock()
	repCopy := rep
	s.lastSync = &repCopy
	s.lastSyncAt = &now
	s.mu.Unlock()
	return rep, err
}

// Search runs a semantic search scoped to the agent's subtree.
func (s *Service) Search(ctx context.Context, query string, limit int) (*openviking.FindResult, error) {
	if s.syncer != nil {
		return s.syncer.Find(ctx, query, limit)
	}
	if limit <= 0 {
		limit = 10
	}
	return s.client.Find(ctx, openviking.FindRequest{Query: query, TargetURI: s.cfg.RecallScope(), Limit: limit})
}

// Flush submits any buffered memory and is a no-op when memory is off.
func (s *Service) Flush(ctx context.Context) {
	if m := s.Mirror(); m != nil {
		m.Flush(ctx, "")
	}
}

// Close flushes buffered memory. It is safe to call more than once.
func (s *Service) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.Flush(ctx)
}

// Status is the integration's state, as reported to an operator.
type Status struct {
	Enable  bool   `json:"enable"`
	BaseURL string `json:"base_url"`
	Account string `json:"account"`
	User    string `json:"user"`
	// Subtree is the viking:// subtree everything is written under.
	Subtree string `json:"subtree"`
	// Connected reports whether the last health check answered.
	Connected bool   `json:"connected"`
	Version   string `json:"version,omitempty"`
	AuthMode  string `json:"auth_mode,omitempty"`
	// HealthError is why the check failed, empty when it succeeded.
	HealthError string    `json:"health_error,omitempty"`
	CheckedAt   time.Time `json:"checked_at,omitempty"`

	Memory    MemoryStatus    `json:"memory"`
	Documents DocumentsStatus `json:"documents"`
	MCP       MCPStatus       `json:"mcp"`
}

// MemoryStatus reports the memory mirror.
type MemoryStatus struct {
	Enable bool                   `json:"enable"`
	Commit bool                   `json:"commit"`
	Stats  memory.OpenVikingStats `json:"stats"`
}

// DocumentsStatus reports document storage.
type DocumentsStatus struct {
	Enable        bool   `json:"enable"`
	RootURI       string `json:"root_uri,omitempty"`
	WorkspaceDir  string `json:"workspace_dir,omitempty"`
	SyncWorkspace bool   `json:"sync_workspace"`
	Tracked       int    `json:"tracked"`
	// LastSync is the most recent sync in this process, nil when none ran yet.
	LastSync *documents.Report `json:"last_sync,omitempty"`
	// LastSyncAt is when it ran, nil when nothing has synced yet.
	LastSyncAt *time.Time `json:"last_sync_at,omitempty"`
}

// MCPStatus reports whether OpenViking's own MCP tools are registered.
type MCPStatus struct {
	Registered bool   `json:"registered"`
	Name       string `json:"name,omitempty"`
}

// Status gathers the current state and performs a health check.
//
// The health check is the only network call, and its failure is reported as
// data (Connected false plus HealthError) rather than as an error: "the server
// is down" is exactly what the caller wants to display.
func (s *Service) Status(ctx context.Context) Status {
	st := Status{
		Enable:  true,
		BaseURL: s.client.BaseURL(),
		Account: s.client.Account(),
		User:    s.client.User(),
		Subtree: s.cfg.Subtree(),
		Memory: MemoryStatus{
			Enable: s.cfg.Memory.Enable,
			Commit: s.cfg.Memory.Commit,
		},
		Documents: DocumentsStatus{
			Enable:        s.syncer != nil,
			SyncWorkspace: s.cfg.Documents.SyncWorkspace,
			WorkspaceDir:  s.workDir,
		},
		MCP: MCPStatus{Registered: s.cfg.MCP.Register, Name: s.cfg.MCP.MCPName()},
	}
	if s.syncer != nil {
		st.Documents.RootURI = s.syncer.RootURI()
		st.Documents.Tracked = len(s.syncer.Documents())
	}

	s.mu.Lock()
	if m := s.mirror; m != nil {
		st.Memory.Stats = m.Stats()
	}
	st.Documents.LastSync = s.lastSync
	st.Documents.LastSyncAt = s.lastSyncAt
	s.mu.Unlock()

	hctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	health, err := s.client.Health(hctx)
	s.mu.Lock()
	s.healthAt = time.Now()
	if err != nil {
		s.lastHealth = nil
		s.healthErr = err.Error()
	} else {
		s.lastHealth = health
		s.healthErr = ""
	}
	st.CheckedAt = s.healthAt
	health, healthErr := s.lastHealth, s.healthErr
	s.mu.Unlock()

	if healthErr != "" {
		st.HealthError = healthErr
		return st
	}
	st.Connected = true
	if health != nil {
		st.Version = health.Version
		st.AuthMode = health.AuthMode
	}
	return st
}

// DataDir returns the directory this integration keeps local state in, derived
// from the sync state path. It is used by the console to say where the record
// of synced documents lives.
func (s *Service) DataDir() string {
	statePath := s.cfg.Documents.StateFile()
	if statePath == "" {
		return ""
	}
	return filepath.Dir(statePath)
}
