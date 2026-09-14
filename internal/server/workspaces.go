package server

// The console's workspace surface: the sidebar's folders and the directory
// picker that creates one.
//
// A workspace is a directory, so this API is deliberately small — list, create
// from a directory, rename the label, delete. Two things are worth knowing when
// reading it:
//
//   - Creation takes an *existing* directory. The picker navigates the server's
//     own filesystem (`GET /api/fs/dirs`) because a browser file input yields no
//     server-side path, and this console is served by the process whose
//     filesystem is being chosen.
//   - Deleting never deletes files, and cannot strand a conversation: the
//     conversations move to another workspace, and the answer says where.

import (
	"context"
	"errors"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"

	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/workspace"
	"github.com/huan/huan-agent/internal/workspaces"
)

// WorkspaceService is the workspace layer as the console uses it.
type WorkspaceService interface {
	// List returns every workspace with its conversation count.
	List(ctx context.Context) ([]workspaces.Spec, error)
	// Get returns one workspace by name.
	Get(ctx context.Context, name string) (workspaces.Spec, error)
	// Create registers an existing directory as a workspace.
	Create(ctx context.Context, in workspaces.CreateInput) (workspaces.Spec, error)
	// Rename changes a workspace's label.
	Rename(ctx context.Context, from, to string) (workspaces.Spec, error)
	// Delete removes a workspace, moving its conversations elsewhere.
	Delete(ctx context.Context, name string) (workspaces.DeleteResult, error)
	// Active returns the workspace a scope is in.
	Active(ctx context.Context, scope string) (workspaces.Spec, error)
	// Select records a scope's choice.
	Select(ctx context.Context, scope, name string) (workspaces.Spec, error)
	// Resolve returns the sandbox and spec for a scope's workspace.
	Resolve(ctx context.Context, scope string) (*workspace.Workspace, workspaces.Spec, error)
	// Open returns the sandbox for a workspace by name.
	Open(ctx context.Context, name string) (*workspace.Workspace, error)
	// Default returns the workspace a new conversation starts in.
	Default(ctx context.Context) (workspaces.Spec, error)
	// Browse lists the subdirectories of a path, for the picker.
	Browse(path string) (workspaces.DirListing, error)
}

// registerWorkspaceRoutes wires the workspace endpoints.
func (s *Server) registerWorkspaceRoutes(authed *route.RouterGroup) {
	if s.chat.Workspaces == nil {
		return
	}
	authed.GET("/workspaces", s.handleListWorkspaces)
	authed.POST("/workspaces", s.handleCreateWorkspace)
	authed.GET("/workspaces/:name", s.handleGetWorkspace)
	authed.PATCH("/workspaces/:name", s.handleRenameWorkspace)
	authed.DELETE("/workspaces/:name", s.handleDeleteWorkspace)
	// The per-conversation switch: which workspace THIS conversation works in.
	authed.GET("/chat/sessions/:id/workspace", s.handleGetSessionWorkspace)
	authed.PUT("/chat/sessions/:id/workspace", s.handleSetSessionWorkspace)
}

// listWorkspacesResponse is the whole sidebar in one answer: the workspaces, the
// directory each points at, how many conversations are in it, and which one a new
// conversation would start in.
type listWorkspacesResponse struct {
	OK         bool              `json:"ok"`
	Workspaces []workspaces.Spec `json:"workspaces"`
	Default    string            `json:"default"`
	Home       string            `json:"home,omitempty"`
	Note       string            `json:"note,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// handleListWorkspaces returns the workspace set for the sidebar.
func (s *Server) handleListWorkspaces(ctx context.Context, c *app.RequestContext) {
	specs, err := s.chat.Workspaces.List(ctx)
	if err != nil {
		s.fail(c, "list workspaces", err)
		return
	}
	out := listWorkspacesResponse{
		OK:         true,
		Workspaces: specs,
		Note:       "工作区是 agent 读写文件与执行命令的目录；每个对话属于一个工作区。删除工作区不会删除目录里的文件。",
	}
	if def, derr := s.chat.Workspaces.Default(ctx); derr == nil {
		out.Default = def.Name
	}
	if listing, berr := s.chat.Workspaces.Browse(""); berr == nil {
		out.Home = listing.Home
	}
	// A workspace whose directory has gone away is reported, not hidden: the
	// alternative is a conversation that fails on its next tool call with no
	// visible reason.
	for _, spec := range specs {
		if spec.Missing {
			out.Warnings = append(out.Warnings, "工作区 "+spec.Name+" 的目录已不存在："+spec.Root)
		}
	}
	c.JSON(http.StatusOK, out)
}

// handleGetWorkspace returns one workspace.
func (s *Server) handleGetWorkspace(ctx context.Context, c *app.RequestContext) {
	spec, err := s.chat.Workspaces.Get(ctx, c.Param("name"))
	if err != nil {
		s.workspaceError(c, "get workspace", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true, "workspace": spec})
}

// handleCreateWorkspace registers an existing directory as a workspace.
func (s *Server) handleCreateWorkspace(ctx context.Context, c *app.RequestContext) {
	var body struct {
		Root string `json:"root"`
		Name string `json:"name"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	spec, err := s.chat.Workspaces.Create(ctx, workspaces.CreateInput{Root: body.Root, Name: body.Name})
	if err != nil {
		s.workspaceError(c, "create workspace", err)
		return
	}
	s.logger.Info("workspace created from the console",
		zapString("name", spec.Name), zapString("root", spec.Root))
	c.JSON(http.StatusOK, map[string]any{"ok": true, "workspace": spec})
}

// handleRenameWorkspace changes a workspace's label.
//
// The label is all that changes: the directory is not touched, which is why this
// is a PATCH carrying one field rather than a general edit.
func (s *Server) handleRenameWorkspace(ctx context.Context, c *app.RequestContext) {
	var body struct {
		Name string `json:"name"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	from := c.Param("name")
	spec, err := s.chat.Workspaces.Rename(ctx, from, body.Name)
	if err != nil {
		s.workspaceError(c, "rename workspace", err)
		return
	}
	s.logger.Info("workspace renamed from the console", zapString("from", from), zapString("to", spec.Name))
	c.JSON(http.StatusOK, map[string]any{"ok": true, "from": from, "workspace": spec})
}

// handleDeleteWorkspace removes a registration and says where its conversations
// went.
func (s *Server) handleDeleteWorkspace(ctx context.Context, c *app.RequestContext) {
	name := c.Param("name")
	res, err := s.chat.Workspaces.Delete(ctx, name)
	if err != nil {
		s.workspaceError(c, "delete workspace", err)
		return
	}
	s.logger.Info("workspace unregistered from the console (files untouched)",
		zapString("name", name), zapString("moved_to", res.MovedTo))
	c.JSON(http.StatusOK, map[string]any{
		"ok":       true,
		"moved_to": res.MovedTo,
		"note": "工作区已删除；目录与其中的文件保持原样，未被删除。它里面的对话已移动到「" +
			res.MovedTo + "」。",
	})
}

// handleGetSessionWorkspace reports which workspace a conversation is in.
func (s *Server) handleGetSessionWorkspace(ctx context.Context, c *app.RequestContext) {
	sess, ok := s.chatSession(ctx, c)
	if !ok {
		return
	}
	spec, err := s.chat.Workspaces.Active(ctx, workspaces.WebScope(sess.ID))
	if err != nil {
		s.workspaceError(c, "get session workspace", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true, "workspace": spec})
}

// handleSetSessionWorkspace moves one conversation into another workspace.
func (s *Server) handleSetSessionWorkspace(ctx context.Context, c *app.RequestContext) {
	sess, ok := s.chatSession(ctx, c)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}

	scope := workspaces.WebScope(sess.ID)
	before, err := s.chat.Workspaces.Active(ctx, scope)
	if err != nil {
		s.workspaceError(c, "read previous workspace", err)
		return
	}
	after, err := s.chat.Workspaces.Select(ctx, scope, body.Name)
	if err != nil {
		s.workspaceError(c, "switch session workspace", err)
		return
	}
	s.logger.Info("session workspace switched",
		zapString("session", sess.ID), zapString("from", before.Name), zapString("to", after.Name),
		zapString("root", after.Root))
	c.JSON(http.StatusOK, map[string]any{"ok": true, "from": before.Name, "workspace": after})
}

// registerFSRoutes wires the directory browser the picker is built on.
//
// It is separate from the workspace routes because it is about the machine
// rather than about the workspace set — and because it is the one endpoint whose
// job is to answer questions about the server's filesystem, which is worth being
// able to find by name. It reads directories only: nothing here creates, moves
// or deletes anything.
func (s *Server) registerFSRoutes(authed *route.RouterGroup) {
	if s.chat.Workspaces == nil {
		return
	}
	authed.GET("/fs/dirs", s.handleBrowseDirs)
}

// handleBrowseDirs lists the subdirectories of a path, for the picker.
func (s *Server) handleBrowseDirs(_ context.Context, c *app.RequestContext) {
	listing, err := s.chat.Workspaces.Browse(c.Query("path"))
	if err != nil {
		// A path that cannot be read is the caller's input (or the filesystem's
		// answer to it), not a server fault: the picker shows the reason and stays
		// where it is.
		var invalid *workspaces.ValidationError
		if errors.As(err, &invalid) {
			c.JSON(http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		s.fail(c, "browse directories", err)
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"ok":            true,
		"path":          listing.Path,
		"parent":        listing.Parent,
		"home":          listing.Home,
		"entries":       listing.Entries,
		"truncated":     listing.Truncated,
		"root_too_high": listing.RootTooHigh,
		"note":          "只列出目录；工作区必须是一个已存在的目录。",
	})
}

// chatSession loads the session named in the path, answering 404 when it is
// unknown. The bool is false when a response has already been written.
func (s *Server) chatSession(ctx context.Context, c *app.RequestContext) (store.ChatSession, bool) {
	sess, err := s.store.GetChatSession(ctx, c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]any{"ok": false, "error": "会话不存在"})
			return store.ChatSession{}, false
		}
		s.fail(c, "get chat session", err)
		return store.ChatSession{}, false
	}
	return sess, true
}

// workspaceError maps the workspace layer's errors onto status codes, so the
// console can distinguish "you picked a bad directory" from "the server broke".
func (s *Server) workspaceError(c *app.RequestContext, what string, err error) {
	var invalid *workspaces.ValidationError
	switch {
	case errors.As(err, &invalid):
		c.JSON(http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
	case errors.Is(err, workspaces.ErrNotFound):
		c.JSON(http.StatusNotFound, map[string]any{"ok": false, "error": err.Error()})
	case errors.Is(err, workspaces.ErrExists):
		c.JSON(http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
	case errors.Is(err, workspaces.ErrLastWorkspace):
		// A conflict rather than a refusal: the request is well formed, the state
		// it would produce is not allowed.
		c.JSON(http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
	case errors.Is(err, workspaces.ErrNoWorkspace):
		c.JSON(http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
	default:
		s.logger.Warn("workspace request failed", zapString("op", what), zapError(err))
		c.JSON(http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
	}
}
