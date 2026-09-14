package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// OpenViking is the OpenViking context database (memory + documents) that
// huan-agent can mirror into. The whole section is off by default: a machine
// without OpenViking running must behave exactly as it did before, so nothing
// here is enabled unless an operator says so.
//
// The defaults that matter are the ones a local `ov` install already uses:
// loopback URL, dev auth (no key), the "default" account, and a
// viking://user/<user>/huan-agent subtree for everything this agent writes.
// Scoping writes under one subtree is what keeps this agent's recall from
// surfacing another project's material on a shared server.
type OpenVikingConfig struct {
	// Enable turns the integration on. When false nothing is constructed: no
	// native client, no memory mirror, no document sync, no MCP server.
	Enable bool `mapstructure:"enable" json:"enable"`
	// BaseURL is the server root, e.g. "http://127.0.0.1:1933".
	BaseURL string `mapstructure:"base_url" json:"base_url"`
	// APIKey is the token for auth_mode=api_key. Empty means dev auth, which is
	// what a locally installed `ov` uses.
	APIKey string `mapstructure:"api_key" json:"api_key"`
	// Account and User are the identity headers OpenViking scopes data by.
	// Empty falls back to "default", which a local install uses.
	Account string `mapstructure:"account" json:"account"`
	User    string `mapstructure:"user" json:"user"`
	// TimeoutSeconds bounds one API call. 0 uses the client default (15s).
	TimeoutSeconds int `mapstructure:"timeout_seconds" json:"timeout_seconds"`

	Memory    OpenVikingMemoryConfig    `mapstructure:"memory" json:"memory"`
	Documents OpenVikingDocumentsConfig `mapstructure:"documents" json:"documents"`
	MCP       OpenVikingMCPConfig       `mapstructure:"mcp" json:"mcp"`
}

// OpenVikingMemoryConfig configures the long-term memory mirror.
type OpenVikingMemoryConfig struct {
	// Enable mirrors conversation turns and facts into OpenViking. The local
	// JSONL store stays the source of truth either way.
	Enable bool `mapstructure:"enable" json:"enable"`
	// Commit asks OpenViking to extract long-term memory from the submitted
	// turns. It requires the server's VLM model to be available; when it is
	// not, extraction fails server-side and is reported through /viking status
	// rather than blocking the conversation.
	Commit bool `mapstructure:"commit" json:"commit"`
	// FlushEvery is how many messages are accumulated before a batch is
	// submitted. 0 submits every turn; a small number (4) keeps extraction cost
	// proportional to conversation length instead of turn count.
	FlushEvery int `mapstructure:"flush_every" json:"flush_every"`
	// RecallEnable recalls facts by semantic search instead of keyword
	// matching. When off (or when OpenViking is unreachable) recall falls back
	// to the local keyword index.
	RecallEnable bool `mapstructure:"recall_enable" json:"recall_enable"`
	// RecallLimit caps how many facts a recall returns.
	RecallLimit int `mapstructure:"recall_limit" json:"recall_limit"`
	// RecallTarget scopes recall to a viking:// subtree. Empty uses
	// viking://user/<user>/huan-agent, so one project's recall cannot surface
	// another's.
	RecallTarget string `mapstructure:"recall_target" json:"recall_target"`
	// JournalEnable keeps a VLM-independent append-only journal of facts
	// (viking://…/memory/journal.md). It is what makes key facts retrievable
	// even when the server's VLM model is not activated, so it defaults on.
	JournalEnable bool `mapstructure:"journal_enable" json:"journal_enable"`
	// SessionPrefix namespaces the OpenViking sessions this agent writes to.
	SessionPrefix string `mapstructure:"session_prefix" json:"session_prefix"`
}

// OpenVikingDocumentsConfig configures document storage and workspace sync.
type OpenVikingDocumentsConfig struct {
	// Enable turns document saving on: the save_document tool, the CLI and the
	// workspace sync.
	Enable bool `mapstructure:"enable" json:"enable"`
	// RootURI is the subtree everything is written under. Empty uses
	// viking://user/<user>/huan-agent.
	RootURI string `mapstructure:"root_uri" json:"root_uri"`
	// SyncWorkspace uploads workspace files (see WorkspaceDir).
	SyncWorkspace bool `mapstructure:"sync_workspace" json:"sync_workspace"`
	// WorkspaceDir is the directory to sync. Empty uses tools.workspace, and
	// when that is empty too, sync is skipped: uploading the process working
	// directory by accident is exactly the outcome this guard prevents.
	WorkspaceDir string `mapstructure:"workspace_dir" json:"workspace_dir"`
	// Include and Exclude are glob patterns matched against the workspace-
	// relative slash-separated path. Empty Include means the built-in text
	// allow-list.
	Include []string `mapstructure:"include" json:"include"`
	Exclude []string `mapstructure:"exclude" json:"exclude"`
	// MaxFileKB caps one synchronized file. 0 uses the default (512 KiB).
	MaxFileKB int `mapstructure:"max_file_kb" json:"max_file_kb"`
	// WaitIndex blocks each workspace write until OpenViking's semantic and
	// vector indexes reflect it. Off by default: a bulk sync should not pay an
	// index round-trip per file. Explicitly saved documents always wait.
	WaitIndex bool `mapstructure:"wait_index" json:"wait_index"`
	// BinaryMode decides what happens to non-text files: "skip" (default)
	// records them in the report, "upload" sends them through temp_upload +
	// add_resource so the server can parse them.
	BinaryMode string `mapstructure:"binary_mode" json:"binary_mode"`
	// SyncIntervalSeconds starts a background sync every N seconds. 0 (default)
	// syncs only on demand — a large workspace re-crawled on a timer is a
	// surprising amount of embedding traffic.
	SyncIntervalSeconds int `mapstructure:"sync_interval_seconds" json:"sync_interval_seconds"`
	// SyncOnExit runs one incremental workspace sync when an interactive chat
	// session ends, so the files a conversation produced are stored without
	// anyone asking. Incremental, so the cost after the first run is a hash of
	// each file.
	SyncOnExit bool `mapstructure:"sync_on_exit" json:"sync_on_exit"`
	// StatePath is the JSON file that tracks uri → content hash, which is what
	// makes a sync incremental. Empty uses ./data/openviking-docs.json.
	StatePath string `mapstructure:"state_path" json:"state_path"`
}

// OpenVikingMCPConfig configures automatic registration of the OpenViking MCP
// server, which is how the model gets OpenViking's own tools (find, read,
// write, add_resource, …) rather than only the automatic mirroring.
type OpenVikingMCPConfig struct {
	// Register adds an MCP server entry pointing at <base_url>/mcp. An entry
	// with the same name that an operator declared explicitly wins.
	Register bool `mapstructure:"register" json:"register"`
	// Name is the registered server's display name and id slug.
	Name string `mapstructure:"name" json:"name"`
}

// Defaults for the OpenViking section. They are exported because the document
// syncer and the memory mirror both need to agree on them.
const (
	// DefaultOpenVikingBaseURL is the address a local `ov` server listens on.
	DefaultOpenVikingBaseURL = "http://127.0.0.1:1933"
	// DefaultOpenVikingMCPName is the registered MCP server's name.
	DefaultOpenVikingMCPName = "openviking"
	// DefaultOpenVikingSubtree is the path segment everything is written under,
	// relative to viking://user/<user>.
	DefaultOpenVikingSubtree = "huan-agent"
	// DefaultOpenVikingRecallLimit caps a semantic recall.
	DefaultOpenVikingRecallLimit = 5
	// DefaultOpenVikingFlushEvery batches this many messages per submission.
	DefaultOpenVikingFlushEvery = 4
	// DefaultOpenVikingMaxFileKB caps one synchronized workspace file.
	DefaultOpenVikingMaxFileKB = 512
	// DefaultOpenVikingStatePath is where incremental sync state lives.
	DefaultOpenVikingStatePath = "./data/openviking-docs.json"
	// DefaultOpenVikingSessionPrefix prefixes generated OpenViking session ids.
	DefaultOpenVikingSessionPrefix = "huan-agent"
	// DefaultOpenVikingBinaryMode leaves binaries out of the workspace sync.
	DefaultOpenVikingBinaryMode = "skip"
	// DefaultOpenVikingTimeoutSeconds bounds one API call.
	DefaultOpenVikingTimeoutSeconds = 15
)

// DefaultOpenVikingInclude is the text allow-list used when none is configured.
// It is deliberately narrow: a workspace sync is meant to publish the notes and
// source files a person would hand over, not every artefact a build produced.
var DefaultOpenVikingInclude = []string{
	"**/*.md", "**/*.markdown", "**/*.txt", "**/*.json", "**/*.yaml", "**/*.yml",
	"**/*.toml", "**/*.go", "**/*.py", "**/*.js", "**/*.ts", "**/*.sh", "**/*.sql",
}

// DefaultOpenVikingExclude keeps the obvious noise out of a sync.
var DefaultOpenVikingExclude = []string{
	".git/**", "node_modules/**", "vendor/**", "dist/**", "build/**",
	"data/**", "bin/**", "**/*.db", "**/*.sqlite", "**/*.lock", "**/*.log",
}

// Enabled reports whether the integration is usable: a switch and an address.
func (c OpenVikingConfig) Enabled() bool {
	return c.Enable && strings.TrimSpace(c.BaseURL) != ""
}

// Timeout returns the configured per-call timeout.
func (c OpenVikingConfig) Timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return DefaultOpenVikingTimeoutSeconds * time.Second
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// AccountOr returns the configured account, defaulting to "default".
func (c OpenVikingConfig) AccountOr() string {
	if s := strings.TrimSpace(c.Account); s != "" {
		return s
	}
	return "default"
}

// UserOr returns the configured user, defaulting to "default".
func (c OpenVikingConfig) UserOr() string {
	if s := strings.TrimSpace(c.User); s != "" {
		return s
	}
	return "default"
}

// Subtree returns the viking:// subtree this agent owns
// (viking://user/<user>/huan-agent), which is the default scope for both
// document writes and memory recall.
func (c OpenVikingConfig) Subtree() string {
	return fmt.Sprintf("viking://user/%s/%s", c.UserOr(), DefaultOpenVikingSubtree)
}

// DocumentsRoot returns where documents are written: the configured root_uri,
// or the agent's subtree.
func (c OpenVikingConfig) DocumentsRoot() string {
	if s := strings.TrimSpace(c.Documents.RootURI); s != "" {
		return strings.TrimRight(s, "/")
	}
	return c.Subtree()
}

// RecallScope returns the subtree semantic recall is limited to.
func (c OpenVikingConfig) RecallScope() string {
	if s := strings.TrimSpace(c.Memory.RecallTarget); s != "" {
		return strings.TrimRight(s, "/")
	}
	return c.Subtree()
}

// SessionPrefixOr returns the OpenViking session id prefix.
func (c OpenVikingConfig) SessionPrefixOr() string {
	if s := strings.TrimSpace(c.Memory.SessionPrefix); s != "" {
		return s
	}
	return DefaultOpenVikingSessionPrefix
}

// RecallLimitOr returns the recall cap.
func (c OpenVikingConfig) RecallLimitOr() int {
	if c.Memory.RecallLimit > 0 {
		return c.Memory.RecallLimit
	}
	return DefaultOpenVikingRecallLimit
}

// FlushEveryOr returns the batch size for memory submission. A negative value
// means "every turn".
func (c OpenVikingConfig) FlushEveryOr() int {
	if c.Memory.FlushEvery < 0 {
		return 0
	}
	return c.Memory.FlushEvery
}

// IncludesOr returns the sync allow-list.
func (c OpenVikingDocumentsConfig) IncludesOr() []string {
	if len(c.Include) > 0 {
		return c.Include
	}
	return DefaultOpenVikingInclude
}

// ExcludesOr returns the sync deny-list.
func (c OpenVikingDocumentsConfig) ExcludesOr() []string {
	if len(c.Exclude) > 0 {
		return c.Exclude
	}
	return DefaultOpenVikingExclude
}

// MaxFileBytes returns the per-file cap in bytes.
func (c OpenVikingDocumentsConfig) MaxFileBytes() int64 {
	kb := c.MaxFileKB
	if kb <= 0 {
		kb = DefaultOpenVikingMaxFileKB
	}
	return int64(kb) << 10
}

// UploadsBinaries reports whether non-text files are ingested instead of
// skipped.
func (c OpenVikingDocumentsConfig) UploadsBinaries() bool {
	return strings.EqualFold(strings.TrimSpace(c.BinaryMode), "upload")
}

// StateFile returns the sync state path.
func (c OpenVikingDocumentsConfig) StateFile() string {
	if s := strings.TrimSpace(c.StatePath); s != "" {
		return s
	}
	return DefaultOpenVikingStatePath
}

// SyncInterval returns the background sync period; 0 disables it.
func (c OpenVikingDocumentsConfig) SyncInterval() time.Duration {
	if c.SyncIntervalSeconds <= 0 {
		return 0
	}
	return time.Duration(c.SyncIntervalSeconds) * time.Second
}

// WorkspaceRoot resolves the directory the workspace sync publishes: the
// documents-specific override, then tools.workspace, then the process working
// directory. It returns "" when there is nothing safe to sync, which the sync
// treats as "disabled" rather than "sync everything".
func (c OpenVikingDocumentsConfig) WorkspaceRoot(tools ToolsConfig) string {
	if s := strings.TrimSpace(c.WorkspaceDir); s != "" {
		return filepath.Clean(s)
	}
	if s := strings.TrimSpace(tools.Workspace); s != "" {
		return filepath.Clean(s)
	}
	return ""
}

// MCPName returns the MCP server name to register under.
func (c OpenVikingMCPConfig) MCPName() string {
	if s := strings.TrimSpace(c.Name); s != "" {
		return s
	}
	return DefaultOpenVikingMCPName
}

// ApplyOpenVikingMCP merges the OpenViking MCP server into mcp.servers when the
// integration is enabled and configured to register.
//
// It runs after config load so that every consumer of mcp.servers — the CLI
// chat loop, the Feishu bot and the console's runtime manager — picks the
// server up without knowing OpenViking exists. An entry the operator declared
// explicitly under the same name is left alone: a hand-written definition is a
// deliberate override, not a duplicate.
func (c *Config) ApplyOpenVikingMCP() {
	if c == nil || !c.OpenViking.Enabled() || !c.OpenViking.MCP.Register {
		return
	}
	name := c.OpenViking.MCP.MCPName()
	for _, s := range c.MCP.Servers {
		if strings.EqualFold(strings.TrimSpace(s.Name), name) {
			return
		}
	}
	headers := make([]string, 0, 3)
	if key := strings.TrimSpace(c.OpenViking.APIKey); key != "" {
		headers = append(headers, "X-API-Key: "+key)
	}
	if account := strings.TrimSpace(c.OpenViking.Account); account != "" {
		headers = append(headers, "X-OpenViking-Account: "+account)
	}
	if user := strings.TrimSpace(c.OpenViking.User); user != "" {
		headers = append(headers, "X-OpenViking-User: "+user)
	}
	c.MCP.Servers = append(c.MCP.Servers, MCPServer{
		Name:      name,
		Transport: "http",
		URL:       strings.TrimRight(strings.TrimSpace(c.OpenViking.BaseURL), "/") + "/mcp",
		Headers:   headers,
	})
}
