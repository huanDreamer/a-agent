package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/mcp"
	"github.com/huan/huan-agent/internal/store"
)

// mcpSyncTimeout bounds one reconcile pass. A pass dials every changed server,
// and each dial has its own 10s handshake timeout, so this is the ceiling for
// the whole set; it exists so a save request cannot hang indefinitely.
const mcpSyncTimeout = 30 * time.Second

// registerMCPRoutes wires MCP server management: the servers the console
// manages, their live connection state, and the AI assistant that drafts a
// definition from a description.
func (s *Server) registerMCPRoutes(authed *route.RouterGroup) {
	authed.GET("/mcp/servers", s.handleListMCPServers)
	authed.POST("/mcp/servers", s.handleUpsertMCPServer)
	authed.DELETE("/mcp/servers/:id", s.handleDeleteMCPServer)
	// 测试连接 for a saved server.
	authed.POST("/mcp/servers/:id/test", s.handleTestMCPServer)
	// 测试连接 for a definition that has not been saved yet.
	authed.POST("/mcp/probe", s.handleProbeMCPServer)
	// Reconnect everything: the retry for a transport that died under a running
	// server (a crashed subprocess, a dropped SSE stream).
	authed.POST("/mcp/reload", s.handleReloadMCP)
	// The AI assistant: a description in, a draft definition out.
	authed.POST("/mcp/draft", s.handleDraftMCPServer)
}

// mcpServerView is one row of the MCP API: the stored definition plus its live
// state, so a client renders a server from one object.
//
// `env` and `headers` are returned verbatim, unlike an LLM provider's API key.
// A provider's key is a single secret the store owns and the UI only needs to
// confirm; an MCP server's env is a list the operator wrote and must be able to
// edit, and a masked list could not be edited without retyping every entry. The
// console is loopback-only in its default deployment (see server.New), which is
// the boundary this rests on.
type mcpServerView struct {
	store.MCPServer
	Runtime *mcp.ServerStatus `json:"runtime,omitempty"`
}

// handleListMCPServers returns every server with its live connection state.
func (s *Server) handleListMCPServers(ctx context.Context, c *app.RequestContext) {
	rows, err := s.store.ListMCPServers(ctx)
	if err != nil {
		s.fail(c, "list mcp servers", err)
		return
	}
	if rows == nil {
		rows = []store.MCPServer{}
	}

	live := s.mcpStatus()
	views := make([]mcpServerView, 0, len(rows))
	for _, row := range rows {
		view := mcpServerView{MCPServer: row}
		if st, ok := live[row.ID]; ok {
			status := st
			view.Runtime = &status
			// The live error is fresher than the stored one: it reflects the
			// connection this process actually made, while last_error may have
			// been written by a previous run.
			view.LastError = st.Error
		}
		views = append(views, view)
	}

	c.JSON(http.StatusOK, map[string]any{
		"servers":    views,
		"transports": store.AllMCPTransports,
		// Whether the runtime exists at all: without a tool registry (chat
		// disabled) servers can be stored but nothing connects them, and the UI
		// says so instead of showing a permanent "未连接".
		"runtime": s.mcp != nil,
	})
}

// mcpStatus indexes the runtime status by server id.
func (s *Server) mcpStatus() map[string]mcp.ServerStatus {
	out := map[string]mcp.ServerStatus{}
	if s.mcp == nil {
		return out
	}
	for _, st := range s.mcp.Status() {
		out[st.ID] = st
	}
	return out
}

// mcpServerRequest is the body of POST /api/mcp/servers.
//
// `enabled` is a pointer so an absent field means "leave as it is" rather than
// "disable it", and `overwrite` distinguishes editing a server from creating
// one: without it, saving a new server whose generated id collides with an
// existing one would silently replace that server.
type mcpServerRequest struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	Env       []string `json:"env"`
	URL       string   `json:"url"`
	Headers   []string `json:"headers"`
	Enabled   *bool    `json:"enabled"`
	Overwrite bool     `json:"overwrite"`
}

// handleUpsertMCPServer creates or updates a server, then reconciles the
// runtime so the change is live for the next conversation.
func (s *Server) handleUpsertMCPServer(ctx context.Context, c *app.RequestContext) {
	var body mcpServerRequest
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	id := strings.TrimSpace(body.ID)
	if id == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}

	existing, err := s.store.GetMCPServer(ctx, id)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(c, "get mcp server", err)
		return
	}

	// A config-declared server is re-synced from the config file at every
	// start, so only its enabled flag can be changed here.
	if found && existing.Source == store.SourceConfig {
		s.updateConfigMCPServer(ctx, c, existing, body)
		return
	}
	// A body that carries only id + enabled is a toggle, not an edit: it does
	// not restate the definition, so it cannot destroy one, and it is allowed to
	// update an existing row without "overwrite". That is why the list's switch
	// can flip a server on and off without sending the whole definition back
	// (which the UI never had for a config-declared server anyway).
	if found && body.Enabled != nil && !bodyCarriesDefinition(body) {
		existing.Enabled = *body.Enabled
		if err := s.store.UpsertMCPServer(ctx, existing); err != nil {
			s.fail(c, "toggle mcp server", err)
			return
		}
		s.syncMCP(ctx)
		s.respondMCPServer(ctx, c, id, "mcp server enabled flag updated")
		return
	}
	if found && !body.Overwrite {
		c.JSON(http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("id %q 已被占用；换一个 id，或在编辑已有服务器时带上 overwrite", id),
		})
		return
	}
	if !found && body.Overwrite {
		// Harmless, and the message saves a support round-trip: the client
		// believes it is editing something that is not there.
		s.logger.Info("mcp server save with overwrite on a missing id", zapString("server", id))
	}

	if !providerIDPattern.MatchString(id) {
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": "id must be a slug of at most 64 characters: lowercase letters, digits, dot, dash or underscore, starting with a letter or digit",
		})
		return
	}

	row, errMsg := mcpRowFromRequest(id, body, found, existing)
	if errMsg != "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": errMsg})
		return
	}
	if err := s.store.UpsertMCPServer(ctx, row); err != nil {
		s.fail(c, "upsert mcp server", err)
		return
	}

	s.syncMCP(ctx)
	s.respondMCPServer(ctx, c, id, "mcp server saved")
}

// bodyCarriesDefinition reports whether a request restates a server's
// connection details (as opposed to only flipping `enabled`).
//
// It is what separates "toggle this server" from "create/replace this server":
// the toggle path is allowed to touch an existing row, and the definition path
// is not, because only the latter can overwrite a working configuration.
func bodyCarriesDefinition(body mcpServerRequest) bool {
	return strings.TrimSpace(body.Transport) != "" ||
		strings.TrimSpace(body.Command) != "" ||
		strings.TrimSpace(body.URL) != "" ||
		len(body.Args) > 0 || len(body.Env) > 0 || len(body.Headers) > 0
}

// mcpRowFromRequest validates and assembles the stored row. A non-empty return
// is a message for the client; validation is deliberately the same rule the
// runtime applies, so a saved server cannot be one that can never connect.
func mcpRowFromRequest(id string, body mcpServerRequest, found bool, existing store.MCPServer) (store.MCPServer, string) {
	transport, err := mcp.ParseTransport(body.Transport)
	if err != nil {
		return store.MCPServer{}, err.Error()
	}

	name := strings.TrimSpace(body.Name)
	if name == "" && found {
		name = existing.Name
	}
	if name == "" {
		name = id
	}

	enabled := true
	if found {
		enabled = existing.Enabled
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	row := store.MCPServer{
		ID:        id,
		Name:      name,
		Transport: store.MCPTransport(transport),
		Command:   strings.TrimSpace(body.Command),
		Args:      cleanList(body.Args),
		Env:       cleanList(body.Env),
		URL:       strings.TrimSpace(body.URL),
		Headers:   cleanList(body.Headers),
		Source:    store.SourceUser,
		Enabled:   enabled,
	}
	if err := row.Validate(); err != nil {
		return store.MCPServer{}, err.Error()
	}
	return row, ""
}

// updateConfigMCPServer is the whole of what POST may do to a config-sourced
// server: toggle enabled.
func (s *Server) updateConfigMCPServer(ctx context.Context, c *app.RequestContext, cur store.MCPServer, body mcpServerRequest) {
	if body.Enabled == nil {
		c.JSON(http.StatusConflict, map[string]string{
			"error": fmt.Sprintf(
				"服务器 %q 由配置文件声明（mcp.servers），每次启动都会按配置文件重新同步，"+
					"因此这里只能切换 enabled；请改为编辑配置文件中的对应条目", cur.ID),
		})
		return
	}
	cur.Enabled = *body.Enabled
	if err := s.store.UpsertMCPServer(ctx, cur); err != nil {
		s.fail(c, "update config mcp server", err)
		return
	}
	s.syncMCP(ctx)
	s.respondMCPServer(ctx, c, cur.ID, "mcp server enabled flag updated")
}

// handleDeleteMCPServer removes a server and unregisters its tools.
func (s *Server) handleDeleteMCPServer(ctx context.Context, c *app.RequestContext) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	row, err := s.store.GetMCPServer(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		c.JSON(http.StatusNotFound, map[string]string{"error": fmt.Sprintf("没有 id 为 %q 的 MCP 服务器", id)})
		return
	}
	if err != nil {
		s.fail(c, "get mcp server", err)
		return
	}
	if row.Source == store.SourceConfig {
		c.JSON(http.StatusConflict, map[string]string{
			"error": fmt.Sprintf(
				"服务器 %q 由配置文件声明（mcp.servers），删除会在下次启动时被重新同步回来；"+
					"请从配置文件中删除，或改为在此停用它", id),
		})
		return
	}
	if err := s.store.DeleteMCPServer(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, map[string]string{"error": fmt.Sprintf("没有 id 为 %q 的 MCP 服务器", id)})
			return
		}
		s.fail(c, "delete mcp server", err)
		return
	}
	s.syncMCP(ctx)
	s.logger.Info("mcp server deleted", zapString("server", id))
	c.JSON(http.StatusOK, map[string]any{"ok": true, "id": id})
}

// respondMCPServer answers with the saved row and its fresh runtime state.
func (s *Server) respondMCPServer(ctx context.Context, c *app.RequestContext, id, what string) {
	row, err := s.store.GetMCPServer(ctx, id)
	if err != nil {
		s.fail(c, "read back mcp server", err)
		return
	}
	view := mcpServerView{MCPServer: row}
	if st, ok := s.mcpStatus()[id]; ok {
		status := st
		view.Runtime = &status
		view.LastError = st.Error
	}
	s.logger.Info(what, zapString("server", id))
	c.JSON(http.StatusOK, map[string]any{"server": view})
}

// mcpProbeResult is what a connection test answers.
type mcpProbeResult struct {
	OK bool `json:"ok"`
	// Tools is what the server offers, so the operator sees what they are about
	// to enable rather than just "it works".
	Tools []mcpProbeTool `json:"tools"`
	// Count is len(Tools), reported separately because a client rendering a
	// summary should not have to iterate.
	Count int `json:"count"`
	// Error explains a failure. A failed test is a result, not an API error.
	Error string `json:"error,omitempty"`
}

// mcpProbeTool is one tool as a test reports it.
type mcpProbeTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// handleTestMCPServer tests a saved server by connecting and listing its tools.
func (s *Server) handleTestMCPServer(ctx context.Context, c *app.RequestContext) {
	id := strings.TrimSpace(c.Param("id"))
	row, err := s.store.GetMCPServer(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		c.JSON(http.StatusNotFound, map[string]string{"error": fmt.Sprintf("没有 id 为 %q 的 MCP 服务器", id)})
		return
	}
	if err != nil {
		s.fail(c, "get mcp server", err)
		return
	}
	s.probeMCP(ctx, c, specFromStore(row))
}

// handleProbeMCPServer tests a definition that has not been saved.
//
// It exists so 测试连接 works while a definition is still being edited: the
// alternative — saving first, then testing — would put a broken server into the
// runtime for the model to trip over.
func (s *Server) handleProbeMCPServer(ctx context.Context, c *app.RequestContext) {
	var body mcpServerRequest
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	transport, err := mcp.ParseTransport(body.Transport)
	if err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	spec := mcp.ServerSpec{
		ID:        strings.TrimSpace(body.ID),
		Name:      firstNonEmpty(strings.TrimSpace(body.Name), "probe"),
		Transport: transport,
		Command:   strings.TrimSpace(body.Command),
		Args:      cleanList(body.Args),
		Env:       cleanList(body.Env),
		URL:       strings.TrimSpace(body.URL),
		Headers:   cleanList(body.Headers),
	}
	if spec.ID == "" {
		spec.ID = mcp.ServerID(spec.Name)
	}
	s.probeMCP(ctx, c, spec)
}

// probeMCP connects, lists tools and disconnects, reporting the outcome.
func (s *Server) probeMCP(ctx context.Context, c *app.RequestContext, spec mcp.ServerSpec) {
	// A probe is a synchronous dial: bound it so a server that accepts a
	// connection and then never answers cannot hold the request forever.
	probeCtx, cancel := context.WithTimeout(ctx, mcpSyncTimeout)
	defer cancel()

	specs, err := s.mcpManager().Inspect(probeCtx, spec)
	if err != nil {
		c.JSON(http.StatusOK, mcpProbeResult{OK: false, Tools: []mcpProbeTool{}, Error: err.Error()})
		return
	}
	out := mcpProbeResult{OK: true, Tools: make([]mcpProbeTool, 0, len(specs)), Count: len(specs)}
	for _, sp := range specs {
		out.Tools = append(out.Tools, mcpProbeTool{Name: sp.Name, Description: sp.Description})
	}
	c.JSON(http.StatusOK, out)
}

// mcpManager returns the runtime manager, creating a throwaway one when the
// chat feature is off.
//
// A server with no tool registry (chat.enable = false) still needs 测试连接 to
// work: the operator is deciding what to configure, and refusing to test until
// the chat is enabled would be an arbitrary obstacle. The throwaway manager
// connects and disconnects, so nothing is registered anywhere.
func (s *Server) mcpManager() *mcp.Manager {
	if s.mcp != nil {
		return s.mcp
	}
	return mcp.NewManager(nil, mcp.ManagerOptions{Dial: s.mcpDial, Logger: s.logger})
}

// handleReloadMCP reconnects every managed server.
func (s *Server) handleReloadMCP(ctx context.Context, c *app.RequestContext) {
	if s.mcp == nil {
		c.JSON(http.StatusOK, map[string]any{"servers": []mcp.ServerStatus{}, "runtime": false})
		return
	}
	ctx, cancel := context.WithTimeout(ctx, mcpSyncTimeout)
	defer cancel()
	status := s.mcp.Refresh(ctx)
	if status == nil {
		status = []mcp.ServerStatus{}
	}
	c.JSON(http.StatusOK, map[string]any{"servers": status, "runtime": true})
}

// SyncMCP reconciles the runtime with the stored servers. It is exported so the
// process can warm the connections at startup, and is called after every edit.
func (s *Server) SyncMCP(ctx context.Context) []mcp.ServerStatus {
	if s.mcp == nil {
		return nil
	}
	syncCtx, cancel := context.WithTimeout(ctx, mcpSyncTimeout)
	defer cancel()
	return s.mcp.Apply(syncCtx, s.enabledMCPSpecs(syncCtx))
}

// syncMCP is SyncMCP for a request handler, where a failure to list is worth
// logging but must not fail the save that already succeeded.
func (s *Server) syncMCP(ctx context.Context) {
	if s.mcp == nil {
		return
	}
	if status := s.SyncMCP(ctx); status == nil {
		s.logger.Warn("mcp sync produced no status")
	}
}

// enabledMCPSpecs loads the specs the runtime should have connected: every
// enabled server, by id.
func (s *Server) enabledMCPSpecs(ctx context.Context) []mcp.ServerSpec {
	rows, err := s.store.ListMCPServers(ctx)
	if err != nil {
		s.logger.Error("list mcp servers for sync failed", zapError(err))
		return nil
	}
	specs := make([]mcp.ServerSpec, 0, len(rows))
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		specs = append(specs, specFromStore(row))
	}
	return specs
}

// recordMCPError persists a connection outcome. The runtime reports through
// this so a broken server is visible in the console after a restart without
// being re-tested.
func (s *Server) recordMCPError(id, msg string) {
	if s.store == nil || id == "" {
		return
	}
	// Deliberately not the request's context: the write must land even when the
	// request that triggered the reconnect has already been answered.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.store.SetMCPServerError(ctx, id, msg); err != nil {
		s.logger.Warn("record mcp server error failed",
			zapString("server", id), zapError(err))
	}
}

// specFromStore converts a stored row into the runtime's own spec type.
func specFromStore(row store.MCPServer) mcp.ServerSpec {
	transport, err := mcp.ParseTransport(string(row.Transport))
	if err != nil {
		transport = mcp.TransportStdio
	}
	return mcp.ServerSpec{
		ID:        row.ID,
		Name:      row.Name,
		Transport: transport,
		Command:   row.Command,
		Args:      row.Args,
		Env:       row.Env,
		URL:       row.URL,
		Headers:   row.Headers,
	}
}

// SyncConfigServers mirrors the config file's mcp.servers into the store and
// returns how many entries were applied.
//
// It runs at every start, and a config-declared row is re-applied from the file
// each time, so editing one in the console (other than its enabled flag) would
// be silently undone — which is why the API refuses those edits and the UI
// shows the row as read-only.
//
// A row whose entry disappeared from the config is deleted: leaving it would
// keep connecting a server the operator removed. A row the console created
// (source = user) is untouched.
func SyncConfigServers(ctx context.Context, st store.Store, servers []config.MCPServer, logger *zap.Logger) (int, error) {
	existing, err := st.ListMCPServers(ctx)
	if err != nil {
		return 0, fmt.Errorf("list mcp servers: %w", err)
	}

	wanted := make(map[string]bool, len(servers))
	synced := 0
	for _, srv := range servers {
		name := strings.TrimSpace(srv.Name)
		if name == "" {
			continue
		}
		id := mcp.ServerID(name)
		wanted[id] = true
		if err := st.UpsertMCPServer(ctx, store.MCPServer{
			ID:        id,
			Name:      name,
			Transport: store.MCPTransport(srv.Transport),
			Command:   strings.TrimSpace(srv.Command),
			Args:      cleanList(srv.Args),
			Env:       cleanList(srv.Env),
			URL:       strings.TrimSpace(srv.URL),
			Headers:   cleanList(srv.Headers),
			Source:    store.SourceConfig,
			Enabled:   srv.IsEnabled(),
		}); err != nil {
			return synced, fmt.Errorf("sync mcp server %s: %w", id, err)
		}
		synced++
	}

	for _, row := range existing {
		if row.Source != store.SourceConfig || wanted[row.ID] {
			continue
		}
		if err := st.DeleteMCPServer(ctx, row.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return synced, fmt.Errorf("drop stale config mcp server %s: %w", row.ID, err)
		}
		if logger != nil {
			logger.Info("mcp server declared in the config file is gone; its row was dropped",
				zapString("server", row.ID))
		}
	}
	return synced, nil
}

// cleanList trims each entry and drops the blank ones, so a textarea split on
// newlines cannot produce an empty argument or an empty header.
func cleanList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// firstNonEmpty returns the first non-empty argument.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ------------------------------------------------------------- AI assistant --

// mcpDraftSystemPrompt is the instruction for the MCP configuration assistant.
//
// It is written to produce something the operator can *review and fix* rather
// than something authoritative: the model is told not to invent secrets, and to
// explain in `summary` whatever it could not determine, because a plausible but
// wrong package name is the most likely failure and the most confusing one.
const mcpDraftSystemPrompt = `你是 huan-agent 控制台的 MCP（Model Context Protocol）配置助手。用户会用一句自然语言描述他想接入的能力，或者粘贴一段文档、README、安装说明。

你的任务：把它们变成一个可以直接使用的 MCP 服务器配置。

只输出一个 JSON 对象。不要输出任何解释文字，不要用 markdown 代码块，不要输出多余字段。字段如下：
{
  "name": "简短的显示名，例如 GitHub",
  "summary": "一句话说明这个配置做什么，以及用户还需要自己补充什么（例如需要把 token 换成真实值、需要先安装某运行时）",
  "transport": "stdio 或 sse 或 http",
  "command": "仅 stdio：要执行的命令，例如 npx、uvx、node、python3",
  "args": ["仅 stdio：参数数组，例如 \"-y\"、\"@modelcontextprotocol/server-github\""],
  "env": ["仅 stdio：环境变量数组，形如 \"KEY=value\"；需要用户填写的密钥写成 \"KEY=\""],
  "url": "仅 sse / http：服务地址",
  "headers": ["仅 sse / http：请求头数组，形如 \"Authorization: Bearer \"；需要用户填写的 token 留空"]
}

规则：
1. 优先 stdio，优先官方或广泛使用的 npm 包（用 npx -y）或 Python 包（用 uvx）。
2. 不要编造包名。如果不确定，选最可能的官方包，并在 summary 里明确写"需要确认命令是否正确"。
3. 不要编造密钥、token、密钥的具体值，一律留空让用户填写。
4. 只有在对方确实提供 HTTP/SSE 地址时才使用 sse 或 http；本地可执行的能力一律 stdio。
5. 字段类型必须严格正确：args / env / headers 一定是字符串数组，没有内容时给空数组。`

// handleDraftMCPServer asks the model for a server definition.
//
// The draft is returned for review and is NOT saved: a model that misunderstands
// the request must not be able to change a working configuration, and the
// console pre-fills its form with the result so the operator confirms it.
func (s *Server) handleDraftMCPServer(ctx context.Context, c *app.RequestContext) {
	var body struct {
		Description string `json:"description"`
		// Document is optional pasted context (a README, an installation
		// section). It is sent as-is, bounded, because it is the difference
		// between a guessed package name and the real one.
		Document string `json:"document"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	desc := strings.TrimSpace(body.Description)
	if desc == "" && strings.TrimSpace(body.Document) == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "请描述你想接入的 MCP 服务（description），或粘贴一段文档（document）"})
		return
	}

	user := "用户描述：\n" + desc
	if doc := strings.TrimSpace(body.Document); doc != "" {
		user += "\n\n用户粘贴的文档（可能包含真实的安装命令）：\n" + truncateForMessage(doc, 6000)
	}
	user += "\n\n已知的服务器 id（不要重复使用）：" + s.existingMCPIDs(ctx)

	result, err := s.draftJSON(ctx, mcpDraftSystemPrompt, user)
	if err != nil {
		c.JSON(http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	draftRow, warnings := mcpDraftToRequest(result.Fields, desc, s.existingMCPIDsSet(ctx))
	out := map[string]any{
		"draft":    draftRow,
		"warnings": warnings,
		"notes":    result.Notes,
		"raw":      result.Raw,
		"model":    map[string]string{"provider": result.Provider, "model": result.Model},
	}
	c.JSON(http.StatusOK, out)
}

// mcpDraftToRequest turns the model's JSON into a definition the form can save,
// plus the list of things the operator has to fix before it will work.
//
// A partially usable draft is more useful than a rejection: the console shows
// the form pre-filled, marks the missing fields, and the operator supplies the
// one thing the model could not know (usually a token).
func mcpDraftToRequest(fields map[string]any, description string, taken map[string]bool) (mcpServerRequest, []string) {
	var warnings []string

	name := stringField(fields, "name")
	if name == "" {
		name = firstNonEmpty(firstLine(description, 40), "MCP 服务器")
		warnings = append(warnings, "模型没有给出名称，已根据描述填充，请确认")
	}

	transport, err := mcp.ParseTransport(stringField(fields, "transport"))
	if err != nil {
		warnings = append(warnings, "模型给出的 transport 无法识别，已按 stdio 处理")
		transport = mcp.TransportStdio
	}

	out := mcpServerRequest{
		ID:        mcp.ServerID(name),
		Name:      name,
		Transport: string(transport),
		Command:   stringField(fields, "command"),
		Args:      cleanList(stringListField(fields, "args")),
		URL:       stringField(fields, "url"),
	}
	// Placeholders are detected on the model's raw lists, before cleaning trims
	// the trailing space that marks "paste your token here".
	rawEnv := stringListField(fields, "env")
	rawHeaders := stringListField(fields, "headers")
	out.Env = cleanList(rawEnv)
	out.Headers = cleanList(rawHeaders)
	warnings = append(warnings, placeholderWarnings(rawEnv, rawHeaders)...)
	enabled := true
	out.Enabled = &enabled

	// Normalise the list fields so a client never has to handle null: an empty
	// args list means "no arguments", which is a list.
	out.Args = orEmptyStrings(out.Args)
	out.Env = orEmptyStrings(out.Env)
	out.Headers = orEmptyStrings(out.Headers)

	if taken[out.ID] {
		out.ID = uniqueMCPID(out.ID, taken)
		warnings = append(warnings, fmt.Sprintf("id %q 已存在，建议改为 %q", mcp.ServerID(name), out.ID))
	}

	switch transport {
	case mcp.TransportStdio:
		if out.Command == "" {
			warnings = append(warnings, "缺少 command：请填写要执行的命令（例如 npx）")
		}
		if out.URL != "" {
			warnings = append(warnings, "stdio 不需要 url，已忽略")
			out.URL = ""
		}
	default:
		if out.URL == "" {
			warnings = append(warnings, "缺少 url：远程 MCP 服务器必须给出地址")
		}
		if out.Command != "" {
			warnings = append(warnings, "远程传输不需要 command，已忽略")
			out.Command, out.Args, out.Env = "", nil, nil
		}
	}
	return out, warnings
}

// placeholderWarnings reports the entries the model deliberately left blank for
// the operator to fill in. It runs on the raw lists, before cleanList trims the
// trailing space that marks a "paste the token here" header value.
func placeholderWarnings(env, headers []string) []string {
	var warnings []string
	for _, e := range env {
		key, value, ok := strings.Cut(e, "=")
		if ok && strings.TrimSpace(value) == "" && strings.TrimSpace(key) != "" {
			warnings = append(warnings, fmt.Sprintf("环境变量 %s 还没有值，请填写后再保存", strings.TrimSpace(key)))
		}
	}
	for _, h := range headers {
		idx := strings.Index(h, ":")
		if idx < 0 {
			continue
		}
		value := h[idx+1:]
		if strings.TrimSpace(value) == "" || strings.HasSuffix(value, " ") {
			warnings = append(warnings, fmt.Sprintf("请求头 %s 还没有填写完整（例如 token），请补充后再保存", strings.TrimSpace(h[:idx])))
		}
	}
	return warnings
}

// existingMCPIDsSet returns the ids already in use.
func (s *Server) existingMCPIDsSet(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	rows, err := s.store.ListMCPServers(ctx)
	if err != nil {
		return out
	}
	for _, row := range rows {
		out[row.ID] = true
	}
	return out
}

// existingMCPIDs renders the ids already in use, for the prompt.
func (s *Server) existingMCPIDs(ctx context.Context) string {
	ids := s.existingMCPIDsSet(ctx)
	if len(ids) == 0 {
		return "（暂无）"
	}
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	return strings.Join(list, "、")
}

// uniqueMCPID appends -2, -3 … until the id is free.
func uniqueMCPID(id string, taken map[string]bool) string {
	for i := 2; i < 100; i++ {
		candidate := fmt.Sprintf("%s-%d", id, i)
		if !taken[candidate] {
			return candidate
		}
	}
	return id
}
