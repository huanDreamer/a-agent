package openviking

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ---- semantic retrieval ----

// FindRequest is a semantic query. TargetURI scopes it to a viking:// subtree,
// which is how a caller keeps one project's recall from surfacing another's
// documents on a shared server; empty searches everything the user can see.
type FindRequest struct {
	Query          string
	TargetURI      string
	Limit          int
	ScoreThreshold float64
	ReadContent    bool
}

// FindHit is one retrieval hit. The same shape covers memories, resources and
// skills, because OpenViking ranks them together.
type FindHit struct {
	ContextType string   `json:"context_type"`
	URI         string   `json:"uri"`
	Score       float64  `json:"score"`
	Level       int      `json:"level"`
	Abstract    string   `json:"abstract"`
	Content     string   `json:"content"`
	Tags        []string `json:"tags"`
}

// FindResult groups hits by what they are.
type FindResult struct {
	Memories  []FindHit `json:"memories"`
	Resources []FindHit `json:"resources"`
	Skills    []FindHit `json:"skills"`
	Total     int       `json:"total"`
}

// All returns every hit, highest-scoring first within each category.
func (r FindResult) All() []FindHit {
	out := make([]FindHit, 0, len(r.Memories)+len(r.Resources)+len(r.Skills))
	out = append(out, r.Memories...)
	out = append(out, r.Resources...)
	out = append(out, r.Skills...)
	return out
}

// Find runs a semantic search.
func (c *Client) Find(ctx context.Context, req FindRequest) (*FindResult, error) {
	body := map[string]any{"query": req.Query}
	if req.TargetURI != "" {
		body["target_uri"] = req.TargetURI
	}
	if req.Limit > 0 {
		body["limit"] = req.Limit
	}
	if req.ScoreThreshold > 0 {
		body["score_threshold"] = req.ScoreThreshold
	}
	if req.ReadContent {
		body["read_content"] = true
	}
	var out FindResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/search/find", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- memory ----

// Message is one conversation turn handed to OpenViking's memory extractor.
type Message struct {
	// Role is "user" or "assistant" — the only two OpenViking accepts here.
	Role    string `json:"role"`
	Content string `json:"content"`
}

// RememberRequest appends messages to a session and optionally commits them.
//
// Committing is what turns raw turns into long-term memory on the OpenViking
// side, and it is asynchronous: the server answers with a task id that
// TaskStatus can be polled for.
type RememberRequest struct {
	// SessionID names the OpenViking session. A stable id per huan-agent
	// namespace keeps one conversation's turns in one place; a missing session
	// is created by the server on first write.
	SessionID string
	Messages  []Message
	// Commit requests memory extraction for what was just appended.
	Commit bool
	// KeepRecentCount is how many messages stay live after a commit. 0 archives
	// everything, which is what a fire-and-forget memory write wants.
	KeepRecentCount int
}

// RememberResult reports what the write and (optional) commit did.
type RememberResult struct {
	SessionID string
	// Added is how many messages the server accepted.
	Added int
	// TaskID is the extraction task, set only when Commit was requested and the
	// server accepted it.
	TaskID string
	// Archived reports whether the commit archived the session history.
	Archived bool
}

// Remember appends messages to a session and, when asked, commits them so
// OpenViking extracts long-term memory from them.
//
// A commit failure is returned together with a non-nil result: the messages are
// already stored, so a caller that only cares about the write can log and move
// on while still seeing why extraction did not happen.
func (c *Client) Remember(ctx context.Context, req RememberRequest) (*RememberResult, error) {
	if strings.TrimSpace(req.SessionID) == "" {
		return nil, fmt.Errorf("openviking: remember: session_id is required")
	}
	if len(req.Messages) == 0 {
		return &RememberResult{SessionID: req.SessionID}, nil
	}
	sid := url.PathEscape(req.SessionID)
	var res struct {
		SessionID    string `json:"session_id"`
		MessageCount int    `json:"message_count"`
		Added        int    `json:"added"`
	}
	path := "/api/v1/sessions/" + sid + "/messages/batch"
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"messages": req.Messages}, &res); err != nil {
		return nil, err
	}
	out := &RememberResult{SessionID: req.SessionID, Added: res.Added}
	if !req.Commit {
		return out, nil
	}
	keep := req.KeepRecentCount
	if keep < 0 {
		keep = 0
	}
	var commit struct {
		Status     string `json:"status"`
		TaskID     string `json:"task_id"`
		Archived   bool   `json:"archived"`
		ArchiveURI string `json:"archive_uri"`
	}
	cpath := "/api/v1/sessions/" + sid + "/commit"
	if err := c.do(ctx, http.MethodPost, cpath, map[string]any{"keep_recent_count": keep}, &commit); err != nil {
		return out, fmt.Errorf("openviking: commit session %s: %w", req.SessionID, err)
	}
	out.TaskID = commit.TaskID
	out.Archived = commit.Archived
	return out, nil
}

// ---- content ----

// WriteRequest writes text to a viking:// file.
type WriteRequest struct {
	// URI is the target file, e.g. "viking://user/default/huan-agent/x.md".
	// A new file must end in a supported text extension (.md .txt .json .yaml
	// .yml .toml .py .js .ts).
	URI     string
	Content string
	// Mode is "replace" (default, creates when missing), "create" (fails when
	// the file exists) or "append" (fails when it does not).
	Mode string
	// Tags are explicit retrieval tags stored alongside the file.
	Tags []string
	// Wait blocks until semantic/vector indexing reflects the write, which is
	// what makes "written and immediately findable" a guarantee rather than a
	// hope.
	Wait bool
	// TimeoutSeconds bounds the server-side wait. Zero uses the server default.
	TimeoutSeconds float64
}

// WriteResult reports what the server wrote and how far indexing got.
type WriteResult struct {
	URI            string `json:"uri"`
	RootURI        string `json:"root_uri"`
	Mode           string `json:"mode"`
	WrittenBytes   int    `json:"written_bytes"`
	SemanticStatus string `json:"semantic_status"`
	VectorStatus   string `json:"vector_status"`
}

// WriteContent writes (or appends) text content.
func (c *Client) WriteContent(ctx context.Context, req WriteRequest) (*WriteResult, error) {
	if strings.TrimSpace(req.URI) == "" {
		return nil, fmt.Errorf("openviking: write: uri is required")
	}
	mode := req.Mode
	if mode == "" {
		mode = "replace"
	}
	body := map[string]any{"uri": req.URI, "content": req.Content, "mode": mode}
	if len(req.Tags) > 0 {
		body["tags"] = req.Tags
	}
	if req.Wait {
		body["wait"] = true
	}
	if req.TimeoutSeconds > 0 {
		body["timeout"] = req.TimeoutSeconds
	}
	var out WriteResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/content/write", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReadContent reads one viking:// file as text.
func (c *Client) ReadContent(ctx context.Context, uri string) (string, error) {
	var out string
	path := "/api/v1/content/read?uri=" + url.QueryEscape(uri)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return "", err
	}
	return out, nil
}

// StatInfo is the metadata of one viking:// entry.
type StatInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"isDir"`
	URI     string `json:"uri"`
	ID      string `json:"id"`
	ModTime string `json:"modTime"`
}

// Stat returns metadata for one viking:// path.
func (c *Client) Stat(ctx context.Context, uri string) (*StatInfo, error) {
	var out StatInfo
	path := "/api/v1/fs/stat?uri=" + url.QueryEscape(uri)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Forget deletes one viking:// entry. It is irreversible on the server side.
func (c *Client) Forget(ctx context.Context, uri string) error {
	path := "/api/v1/fs?uri=" + url.QueryEscape(uri)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// ---- resources ----

// AddResourceRequest ingests a file, URL or repository.
//
// Exactly one of TempFileID or Path identifies the source: the HTTP server
// rejects host filesystem paths outright, so a local file must go through
// UploadTemp first and arrive here as a TempFileID.
type AddResourceRequest struct {
	Path       string
	TempFileID string
	// To is the exact target file URI. It must be resource content (under
	// viking://resources/ or a user resource path); the server rejects a
	// directory or a memory path with INVALID_URI.
	To string
	// Parent is a target directory, used instead of To when the server should
	// name the file.
	Parent       string
	CreateParent bool
	Reason       string
	Instruction  string
	Tags         []string
	// Wait blocks until ingestion finishes. Parsing a document can take a
	// while, so this is worth setting only for interactive paths.
	Wait           bool
	TimeoutSeconds float64
	// ProcessingMode is "semantic_and_vectors" (default) or "vectors_only".
	ProcessingMode string
}

// AddResourceResult is the ingestion outcome. Warnings are non-fatal notes from
// the server (typically "memory linking failed" when the VLM model is not
// available on the account).
type AddResourceResult struct {
	Status       string   `json:"status"`
	Errors       []string `json:"errors"`
	Warnings     []string `json:"warnings"`
	RootURI      string   `json:"root_uri"`
	ContextCount int      `json:"context_count"`
}

// AddResource ingests a resource into viking://.
func (c *Client) AddResource(ctx context.Context, req AddResourceRequest) (*AddResourceResult, error) {
	body := map[string]any{}
	if req.TempFileID != "" {
		body["temp_file_id"] = req.TempFileID
	}
	if req.Path != "" {
		body["path"] = req.Path
	}
	if req.To != "" {
		body["to"] = req.To
	}
	if req.Parent != "" {
		body["parent"] = req.Parent
	}
	if req.CreateParent {
		body["create_parent"] = true
	}
	if req.Reason != "" {
		body["reason"] = req.Reason
	}
	if req.Instruction != "" {
		body["instruction"] = req.Instruction
	}
	if len(req.Tags) > 0 {
		body["tags"] = req.Tags
	}
	if req.Wait {
		body["wait"] = true
	}
	if req.TimeoutSeconds > 0 {
		body["timeout"] = req.TimeoutSeconds
	}
	if req.ProcessingMode != "" {
		body["processing_mode"] = req.ProcessingMode
	}
	var out AddResourceResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/resources", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- tasks ----

// TaskInfo is one asynchronous server task. Memory extraction is the one this
// package cares about: a failed task is how "the VLM model is not activated"
// reaches an operator.
type TaskInfo struct {
	TaskID     string `json:"task_id"`
	TaskType   string `json:"task_type"`
	Status     string `json:"status"`
	ResourceID string `json:"resource_id"`
	Stage      string `json:"stage"`
	Error      string `json:"error"`
}

// TaskStatus reads one task.
func (c *Client) TaskStatus(ctx context.Context, taskID string) (*TaskInfo, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, fmt.Errorf("openviking: task: id is required")
	}
	var out TaskInfo
	if err := c.do(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
