package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/tool"
)

// Dialer opens a connection to one server. It is an interface so the runtime
// can be exercised without spawning processes or standing up an HTTP server.
type Dialer func(ctx context.Context, spec ServerSpec) (*Client, error)

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	// Dial overrides how connections are opened. Nil means Connect.
	Dial Dialer
	// Logger receives connection outcomes. Nil means no logging.
	Logger *zap.Logger
	// OnServerError is called with (id, message) whenever a server's connection
	// state changes. It is how the console persists `last_error` without the
	// manager depending on the database. An empty message means "connected".
	OnServerError func(id, msg string)
}

// Manager owns the live MCP connections and the tools they expose.
//
// It reconciles a desired set of servers against what is connected: applying a
// spec list connects what is new or changed, disconnects what is gone, and
// leaves an unchanged, healthy server alone. That is what makes an edit in
// 设置 → MCP take effect on the next conversation without restarting the
// process — and what keeps a rename from dropping a working connection.
//
// The registry it writes into is the same one a chat turn reads, so a
// connection made here is immediately callable by the model.
type Manager struct {
	reg    *tool.Registry
	logger *zap.Logger
	dial   Dialer
	onErr  func(id, msg string)

	mu      sync.Mutex
	servers map[string]*managedServer
	closed  bool
}

// managedServer is one server's live state.
type managedServer struct {
	spec        ServerSpec
	fingerprint string
	client      *Client
	tools       []string
	err         string
	connectedAt time.Time
}

// NewManager builds a manager over reg. A nil registry makes every Apply a
// no-op that still reports per-server errors, which is what a server started
// without the chat feature needs.
func NewManager(reg *tool.Registry, opts ManagerOptions) *Manager {
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	dial := opts.Dial
	if dial == nil {
		dial = Connect
	}
	return &Manager{
		reg:     reg,
		logger:  logger,
		dial:    dial,
		onErr:   opts.OnServerError,
		servers: map[string]*managedServer{},
	}
}

// ServerStatus is the runtime half of a server's state, as reported to the UI.
type ServerStatus struct {
	ID          string     `json:"id"`
	Connected   bool       `json:"connected"`
	Tools       []string   `json:"tools"`
	Error       string     `json:"error"`
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
}

// Apply reconciles the live connections with specs and returns the resulting
// status of every one of them.
//
// Every spec is validated first. An invalid one is recorded as a failure and
// skipped: one broken entry in the console must not stop the others from
// connecting.
func (m *Manager) Apply(ctx context.Context, specs []ServerSpec) []ServerStatus {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}

	desired := make(map[string]ServerSpec, len(specs))
	ordered := make([]ServerSpec, 0, len(specs))
	for _, spec := range specs {
		if strings.TrimSpace(spec.ID) == "" {
			// Fall back to the name so a caller that only has names (a config
			// file entry) still gets a stable key.
			spec.ID = ServerID(spec.Name)
		}
		if _, dup := desired[spec.ID]; dup {
			// The same id twice: the first appearance wins, which is what the map
			// assignment below would have left behind anyway.
			continue
		}
		desired[spec.ID] = spec
		ordered = append(ordered, spec)
	}

	// Disconnect servers that are no longer wanted.
	for id, cur := range m.servers {
		if _, want := desired[id]; !want {
			m.teardownLocked(id, cur)
		}
	}

	// Connect or reconnect the wanted ones, in the order the caller gave them.
	//
	// The order is the caller's and not a map's, because it decides something
	// visible: two servers that both expose a tool with the same name are
	// connected in turn, the first keeps the name, and the second is reported as a
	// collision. Ranging over a map (as this used to) made that depend on Go's
	// randomised iteration order, so which server lost its tool was decided by
	// chance — and the test that pins the behaviour flaked about one run in ten.
	for _, spec := range ordered {
		id := spec.ID
		cur := m.servers[id]
		if cur != nil && cur.err == "" && cur.client != nil && cur.fingerprint == spec.Fingerprint() {
			// Unchanged and healthy: keep the connection (and any subprocess)
			// exactly as it is.
			cur.spec = spec // the display name may have changed
			continue
		}
		if cur != nil {
			m.teardownLocked(id, cur)
		}
		m.connectLocked(ctx, id, spec)
	}

	return m.statusLocked()
}

// Refresh re-applies the current specs, forcing a reconnect. It answers the
// 重连 button: a server whose transport died (a crashed subprocess, a dropped
// SSE stream) is otherwise never retried, because its spec did not change.
func (m *Manager) Refresh(ctx context.Context) []ServerStatus {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	specs := make([]ServerSpec, 0, len(m.servers))
	for _, cur := range m.servers {
		specs = append(specs, cur.spec)
	}
	m.mu.Unlock()
	return m.Apply(ctx, specs)
}

// connectLocked dials one server and registers its tools. Caller holds mu.
func (m *Manager) connectLocked(ctx context.Context, id string, spec ServerSpec) {
	state := &managedServer{
		spec:        spec,
		fingerprint: spec.Fingerprint(),
	}

	if err := spec.Validate(); err != nil {
		state.err = err.Error()
		m.servers[id] = state
		m.reportError(id, state.err)
		return
	}

	client, err := m.dial(ctx, spec)
	if err != nil {
		state.err = err.Error()
		m.servers[id] = state
		m.reportError(id, state.err)
		m.logger.Warn("mcp server connect failed",
			zap.String("server", spec.Name), zap.Error(err))
		return
	}

	names, errs := m.registerTools(ctx, client)
	state.client = client
	state.tools = names
	state.connectedAt = time.Now()
	if len(errs) > 0 {
		// A partially registered server stays connected and usable: the tools
		// that did register work, and the error says which ones did not.
		state.err = strings.Join(errs, "; ")
	}
	m.servers[id] = state
	m.reportError(id, state.err)

	m.logger.Info("mcp server connected",
		zap.String("server", spec.Name),
		zap.String("transport", string(spec.ResolvedTransport())),
		zap.Int("tools", len(names)),
		zap.Int("failed", len(errs)))
}

// registerTools registers every tool the client exposes, returning the names it
// added and one message per tool that could not be added.
//
// The per-tool rule (a name already taken is reported and skipped, not fatal)
// lives in registerToolSpecs, which the one-shot bridge in bridge.go shares.
func (m *Manager) registerTools(ctx context.Context, c *Client) ([]string, []string) {
	if m.reg == nil {
		return nil, nil
	}
	specs, err := c.ListTools(ctx)
	if err != nil {
		return nil, []string{fmt.Sprintf("list tools: %v", err)}
	}
	return registerToolSpecs(m.reg, c, specs, m.logger)
}

// teardownLocked unregisters a server's tools and closes its connection.
// Caller holds mu.
func (m *Manager) teardownLocked(id string, cur *managedServer) {
	if m.reg != nil {
		for _, name := range cur.tools {
			m.reg.Unregister(name)
		}
	}
	if cur.client != nil {
		if err := cur.client.Close(); err != nil {
			m.logger.Warn("mcp server close failed",
				zap.String("server", cur.spec.Name), zap.Error(err))
		}
	}
	delete(m.servers, id)
}

// reportError tells the observer about a state change, tolerating a nil one.
func (m *Manager) reportError(id, msg string) {
	if m.onErr != nil {
		m.onErr(id, msg)
	}
}

// Status returns the live state of every managed server, by id.
func (m *Manager) Status() []ServerStatus {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

// CallTool invokes one tool on a connected server, addressed by the server's id
// or by its configured name.
//
// It exists for the mcp_tool hook of Claude Code compatibility mode, which
// names a server and a tool rather than the registry name an MCP tool is
// bridged under — and which must not be able to *start* anything: a hook runs
// on every tool call, so triggering an OAuth flow or a connection from one
// would be a side effect nobody asked for. Only an already-connected server is
// used, and a server that is not connected is an error the hook's failure list
// carries.
func (m *Manager) CallTool(ctx context.Context, server, tool, argsJSON string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("mcp: no manager")
	}
	server = strings.TrimSpace(server)
	tool = strings.TrimSpace(tool)
	if server == "" || tool == "" {
		return "", fmt.Errorf("mcp: server and tool are required")
	}

	m.mu.Lock()
	var found *managedServer
	for id, cur := range m.servers {
		if id == server || cur.spec.Name == server {
			found = cur
			break
		}
	}
	m.mu.Unlock()

	switch {
	case found == nil:
		return "", fmt.Errorf("mcp: server %q is not configured", server)
	case found.client == nil:
		// The recorded error is the useful half: "not connected" alone sends the
		// operator looking for a problem the server already reported.
		if found.err != "" {
			return "", fmt.Errorf("mcp: server %q is not connected: %s", server, found.err)
		}
		return "", fmt.Errorf("mcp: server %q is not connected", server)
	}
	return found.client.CallTool(ctx, tool, argsJSON)
}

func (m *Manager) statusLocked() []ServerStatus {
	out := make([]ServerStatus, 0, len(m.servers))
	for id, cur := range m.servers {
		st := ServerStatus{
			ID:        id,
			Connected: cur.client != nil,
			Tools:     append([]string(nil), cur.tools...),
			Error:     cur.err,
		}
		if st.Tools == nil {
			st.Tools = []string{}
		}
		if !cur.connectedAt.IsZero() {
			at := cur.connectedAt
			st.ConnectedAt = &at
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Close disconnects every server. The manager is unusable afterwards.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	for id, cur := range m.servers {
		if m.reg != nil {
			for _, name := range cur.tools {
				m.reg.Unregister(name)
			}
		}
		if cur.client != nil {
			_ = cur.client.Close()
		}
		delete(m.servers, id)
	}
	m.closed = true
}

// Inspect connects to spec, lists its tools and disconnects immediately,
// without touching the runtime.
//
// It is what 测试连接 answers with: the operator wants to know whether a
// definition works (and what it offers) before it is saved, and a definition
// being edited must not disturb the connection a live server already has.
func (m *Manager) Inspect(ctx context.Context, spec ServerSpec) ([]*tool.Spec, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	dial := Connect
	if m != nil {
		dial = m.dial
	}
	client, err := dial(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	return client.ListTools(ctx)
}

// ServerID derives a stable id from a display name, for a server that has no
// database row (an entry declared in the config file).
//
// It keeps [a-z0-9._-], collapses everything else to "-", and prefixes a
// leading digit so the result is usable as a URL segment and as a slug.
func ServerID(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	id := strings.Trim(b.String(), "-.")
	if id == "" {
		return "mcp-server"
	}
	if id[0] >= '0' && id[0] <= '9' {
		return "mcp-" + id
	}
	return id
}
