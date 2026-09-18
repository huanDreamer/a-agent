package lsp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The manager owns every language server this process has started.
//
// Two properties are the reason it exists rather than each tool starting its own
// server:
//
//   - One server per (root, command). A language server indexes a module when it
//     starts; paying that per tool call makes the tools slower than the grep they
//     are meant to replace, so nobody would use them.
//   - A server that cannot start is remembered as broken and not retried.
//     Retrying on every call turns "gopls is not installed" into a failed process
//     spawn per tool call, and a log that repeats the same warning forever.
//
// The third property is what a caller notices: when no server covers a file,
// ClientFor returns (nil, nil) rather than an error. "This file has no language
// server" is not a failure — it is the reason the tools for it were never
// registered in the first place.

// ManagerOptions configures a Manager.
type ManagerOptions struct {
	// Servers is the configured list. Empty uses DefaultServers.
	Servers []ServerSpec
	// RequestTimeout bounds one request. Zero uses DefaultRequestTimeout.
	RequestTimeout time.Duration
	// IdleTimeout reaps a server that has not been used for this long. Zero
	// disables reaping, which is only right for a short-lived process.
	IdleTimeout time.Duration
	// HandshakeTimeout bounds the initialize exchange. Zero uses 30s, which has
	// to accommodate a first run that downloads a toolchain.
	HandshakeTimeout time.Duration
	// Version identifies this client to the server.
	Version string
	Logger  Logger
}

// Manager holds the live servers.
type Manager struct {
	opts ManagerOptions

	mu        sync.Mutex
	instances map[string]*instance
	// broken records (root|command) pairs that failed, with the reason. They are
	// not retried for the life of the process.
	broken map[string]error
	// startLocks serialises concurrent starts of the same key, so a first turn
	// that asks three questions does not spawn three servers.
	startLocks map[string]*sync.Mutex

	closed bool
	stop   chan struct{}
	done   chan struct{}
}

// instance is one running language server.
type instance struct {
	spec    ServerSpec
	root    string
	client  *Client
	cmd     *exec.Cmd
	stream  io.Closer
	tail    *stderrTail
	lastUse time.Time
}

// NewManager builds a manager. It starts no processes: a language server is
// launched on first use, so a deployment that never asks a semantic question
// never pays for one.
func NewManager(opts ManagerOptions) *Manager {
	if len(opts.Servers) == 0 {
		opts.Servers = DefaultServers()
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = 30 * time.Second
	}
	m := &Manager{
		opts:       opts,
		instances:  map[string]*instance{},
		broken:     map[string]error{},
		startLocks: map[string]*sync.Mutex{},
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	if opts.IdleTimeout > 0 {
		go m.reapLoop()
	} else {
		close(m.done)
	}
	return m
}

// Servers returns the configured server list.
func (m *Manager) Servers() []ServerSpec { return m.opts.Servers }

// Covers reports whether any enabled server handles this path.
//
// It is what decides whether the code-intelligence tools are registered at all:
// a tool for a file type with no server can only fail, and a tool that can only
// fail is worse than one that does not exist.
func (m *Manager) Covers(path string) bool {
	return m.specFor(path) != nil
}

// AvailableCommand reports the first enabled server whose binary is actually on
// PATH.
//
// It is what decides whether the code-intelligence tools are registered. The
// distinction from HasEnabledServer matters: a configured-but-uninstalled server
// would put four tools on the menu whose every call fails with "not installed",
// and a tool that can only fail is worse than one that is absent. LookPath is a
// cheap check — no process is started just to answer this.
func (m *Manager) AvailableCommand() (ServerSpec, bool) {
	for _, s := range m.opts.Servers {
		if !s.IsEnabled() || strings.TrimSpace(s.Command) == "" {
			continue
		}
		if _, err := exec.LookPath(s.Command); err == nil {
			return s, true
		}
	}
	return ServerSpec{}, false
}

// FirstConfigured returns the first enabled server, installed or not. It is what
// the "nothing is available" log line names, so the message says which command
// to install.
func (m *Manager) FirstConfigured() (ServerSpec, bool) {
	for _, s := range m.opts.Servers {
		if s.IsEnabled() && strings.TrimSpace(s.Command) != "" {
			return s, true
		}
	}
	return ServerSpec{}, false
}

// InstallHint returns the line that installs a server binary. It is exported so
// a caller that is about to withhold the tools can say how to get them back.
func InstallHint(command string) string { return installHint(command) }

// specFor returns the first enabled server that handles a path, or nil.
func (m *Manager) specFor(path string) *ServerSpec {
	for i := range m.opts.Servers {
		s := &m.opts.Servers[i]
		if !s.IsEnabled() || strings.TrimSpace(s.Command) == "" {
			continue
		}
		if s.Covers(path) {
			return s
		}
	}
	return nil
}

// HasEnabledServer reports whether any server is configured and enabled.
//
// The workspace-level tools (`workspace_symbols`, an unfiltered `diagnostics`)
// cannot be gated on one file's extension, so they are gated on this: the
// question is "could a server here tell us anything at all". Registering them
// with no server configured would put two tools on the menu that can only fail.
func (m *Manager) HasEnabledServer() bool {
	for _, s := range m.opts.Servers {
		if s.IsEnabled() && strings.TrimSpace(s.Command) != "" {
			return true
		}
	}
	return false
}

// fileExists is a small helper for the root-marker probe.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// ClientFor returns the client that handles this file, starting its server if
// needed.
//
// (nil, nil) means "no server covers this file": not an error, but not an answer
// either, and the caller reports it as such.
//
// ceiling is the workspace root, and it is a parameter rather than manager state
// because a process serves several workspaces: RootFor must not climb above the
// one the caller is confined to, and "which workspace" is the caller's knowledge,
// not the manager's.
func (m *Manager) ClientFor(ctx context.Context, path, ceiling string) (*Client, error) {
	spec := m.specFor(path)
	if spec == nil {
		return nil, nil
	}
	root := RootFor(path, spec.RootMarkers, ceiling)
	return m.clientForSpec(ctx, *spec, root)
}

// ClientForRoot returns the client for a workspace root, choosing the server by
// the workspace's own contents rather than by one file.
//
// It is what the workspace-wide tools use. The pick is the first enabled server
// whose root markers are present at the root, falling back to the first enabled
// server at all — a repository with no marker file still deserves a server if
// the operator configured one.
func (m *Manager) ClientForRoot(ctx context.Context, root string) (*Client, error) {
	var fallback *ServerSpec
	for i := range m.opts.Servers {
		s := &m.opts.Servers[i]
		if !s.IsEnabled() || strings.TrimSpace(s.Command) == "" {
			continue
		}
		if fallback == nil {
			fallback = s
		}
		for _, marker := range s.RootMarkers {
			if marker == "" {
				continue
			}
			if fileExists(filepath.Join(root, marker)) {
				return m.clientForSpec(ctx, *s, root)
			}
		}
	}
	if fallback == nil {
		return nil, nil
	}
	return m.clientForSpec(ctx, *fallback, root)
}

// clientForSpec returns the instance for one (server, root), starting it if this
// is the first use.
func (m *Manager) clientForSpec(ctx context.Context, spec ServerSpec, root string) (*Client, error) {
	key := spec.Command + "\x00" + root

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("lsp: manager is closed")
	}
	if inst, ok := m.instances[key]; ok {
		inst.lastUse = time.Now()
		m.mu.Unlock()
		return inst.client, nil
	}
	if err, ok := m.broken[key]; ok {
		m.mu.Unlock()
		// Remembered, not retried: a missing binary must not cost a failed
		// process spawn on every call.
		return nil, err
	}
	lock := m.startLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.startLocks[key] = lock
	}
	m.mu.Unlock()

	// Serialise the first start for this key: three tools asking at once should
	// produce one server, not three.
	lock.Lock()
	defer lock.Unlock()

	m.mu.Lock()
	if inst, ok := m.instances[key]; ok {
		inst.lastUse = time.Now()
		m.mu.Unlock()
		return inst.client, nil
	}
	if err, ok := m.broken[key]; ok {
		m.mu.Unlock()
		return nil, err
	}
	m.mu.Unlock()

	client, inst, err := m.start(ctx, spec, root)
	if err != nil {
		m.mu.Lock()
		m.broken[key] = err
		m.mu.Unlock()
		if m.opts.Logger != nil {
			m.opts.Logger.Warnf("lsp: %s is unavailable and will not be retried: %v", spec.Name, err)
		}
		return nil, err
	}

	m.mu.Lock()
	m.instances[key] = inst
	m.mu.Unlock()

	if m.opts.Logger != nil {
		caps := client.Capabilities()
		m.opts.Logger.Debugf("lsp: %s started for %s (definition=%v references=%v symbols=%v)",
			spec.Name, root, caps.DefinitionProvider, caps.ReferencesProvider, caps.WorkspaceSymbol)
	}
	return client, nil
}

// start launches one server and completes its handshake.
func (m *Manager) start(ctx context.Context, spec ServerSpec, root string) (*Client, *instance, error) {
	stream, cmd, tail, err := startProcess(ctx, spec, root)
	if err != nil {
		return nil, nil, err
	}

	hsCtx, cancel := context.WithTimeout(ctx, m.opts.HandshakeTimeout)
	defer cancel()

	client, err := NewClient(hsCtx, stream, Options{
		Name:           spec.Name,
		Root:           root,
		InitOptions:    spec.InitOptions,
		RequestTimeout: m.opts.RequestTimeout,
		Version:        m.opts.Version,
		Logger:         m.opts.Logger,
	})
	if err != nil {
		// A server that dies during the handshake almost always says why on
		// stderr, and that message is the whole difference between a fixable
		// problem and a mystery.
		_ = stream.Close()
		terminateProcess(cmd, time.Second)
		if said := tail.String(); said != "" {
			return nil, nil, fmt.Errorf("lsp: %s: handshake failed: %w (server said: %s)", spec.Name, err, firstLines(said, 5))
		}
		return nil, nil, fmt.Errorf("lsp: %s: handshake failed: %w", spec.Name, err)
	}

	return client, &instance{
		spec:    spec,
		root:    root,
		client:  client,
		cmd:     cmd,
		stream:  stream,
		tail:    tail,
		lastUse: time.Now(),
	}, nil
}

// FileDiagnostics is the Diagnoser implementation used by the edit-feedback
// decorator: the diagnostics for one file, waiting for the server to catch up.
//
// The three outcomes are distinct on purpose — see Diagnosed. A clean file and a
// file no server covers both produce an empty list, and reporting the second as
// the first would tell the model "no problems" about a file nothing checked.
func (m *Manager) FileDiagnostics(ctx context.Context, path, ceiling string, wait time.Duration) ([]Diagnostic, Diagnosed, error) {
	client, err := m.ClientFor(ctx, path, ceiling)
	if err != nil {
		return nil, NoServer, err
	}
	if client == nil {
		return nil, NoServer, nil
	}
	// Open before waiting: the server has to be told about the new content, or it
	// will publish diagnostics for the previous version and the wait would return
	// a confident answer about the file as it was.
	if err := client.Open(ctx, path); err != nil {
		return nil, NotReady, err
	}
	items, settled := client.WaitDiagnostics(ctx, path, wait)
	if !settled {
		return items, NotReady, nil
	}
	return items, Ready, nil
}

// Reap stops servers that have been idle for longer than the configured timeout.
//
// It is exported so a test can drive it without waiting on a timer.
func (m *Manager) Reap() int {
	if m.opts.IdleTimeout <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-m.opts.IdleTimeout)

	m.mu.Lock()
	var doomed []*instance
	var keys []string
	for key, inst := range m.instances {
		if inst.lastUse.Before(cutoff) {
			doomed = append(doomed, inst)
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		delete(m.instances, key)
	}
	m.mu.Unlock()

	for _, inst := range doomed {
		m.stopInstance(inst)
		if m.opts.Logger != nil {
			m.opts.Logger.Debugf("lsp: reaped idle %s for %s", inst.spec.Name, inst.root)
		}
	}
	return len(doomed)
}

// reapLoop reaps on a timer.
func (m *Manager) reapLoop() {
	defer close(m.done)
	interval := m.opts.IdleTimeout / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.Reap()
		}
	}
}

// stopInstance shuts one server down and kills whatever it left behind.
func (m *Manager) stopInstance(inst *instance) {
	if inst == nil {
		return
	}
	// Close the protocol first: a server asked to shut down flushes its caches,
	// and one killed outright leaves a lock file that slows the next start.
	_ = inst.client.Close()
	if inst.stream != nil {
		_ = inst.stream.Close()
	}
	terminateProcess(inst.cmd, 2*time.Second)
}

// Close stops every server. It is idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.stop)
	instances := make([]*instance, 0, len(m.instances))
	for _, inst := range m.instances {
		instances = append(instances, inst)
	}
	m.instances = map[string]*instance{}
	m.mu.Unlock()

	for _, inst := range instances {
		m.stopInstance(inst)
	}
	<-m.done
	return nil
}

// BrokenServers reports the servers that failed to start, with the reason.
//
// It is what the console and the logs use to answer "why are there no code
// intelligence tools", which is otherwise unanswerable from the outside.
func (m *Manager) BrokenServers() map[string]error {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]error, len(m.broken))
	for k, v := range m.broken {
		out[k] = v
	}
	return out
}

// IsServerMissing reports whether an error is "the configured command does not
// exist", which is the one failure worth telling an operator about every time.
func IsServerMissing(err error) bool { return errors.Is(err, errServerMissing) }

// firstLines keeps an error message short but informative: a server's stderr can
// be hundreds of lines of its own JSON logs.
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}
