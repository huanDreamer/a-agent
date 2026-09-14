package workspaces

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/workspace"
)

// Errors the callers branch on. They are distinct so the HTTP layer can answer
// 404, 409 and 400 without inspecting strings, and so the Feishu path can tell
// "no such workspace" (answer with the list) from "the store is down".
var (
	// ErrNotFound means no workspace is registered under that name.
	ErrNotFound = errors.New("工作区不存在")
	// ErrExists means a workspace with that name already exists.
	ErrExists = errors.New("工作区名字已被占用")
	// ErrNoWorkspace means nothing is registered at all and no fallback could be
	// determined, which no caller can fix by retrying.
	ErrNoWorkspace = errors.New("还没有任何工作区")
	// ErrLastWorkspace means the request would leave no workspace behind, and a
	// conversation must belong to one.
	ErrLastWorkspace = errors.New("最后一个工作区不能删除")
	// ErrNoStore means the manager was built without persistence: a wiring bug
	// rather than a user error.
	ErrNoStore = errors.New("workspaces: 未配置存储")
)

// Options configures a Manager.
type Options struct {
	// Store persists the set and the scope bindings. Required.
	Store store.Store
	// SeedRoot is the directory the first workspace points at when none is
	// registered: `tools.workspace`, or the process working directory. It is what
	// keeps a deployment that never heard of workspaces working in the directory
	// it already used.
	SeedRoot string
	// ReadOnly and Limits are the process-wide tool policy, applied to every
	// workspace's sandbox. They are NOT workspace properties: the sandbox has to
	// be built with them, and building it in one place is what keeps the answer
	// consistent.
	ReadOnly bool
	Limits   workspace.Limits
	Logger   *zap.Logger
}

// CreateInput is a new workspace: a directory the operator picked, and optionally
// the label to show for it.
type CreateInput struct {
	// Root is the directory. It must already exist (see ResolveDir).
	Root string
	// Name is the label. Empty takes the directory's base name.
	Name string
}

// DeleteResult reports what happened to a deleted workspace's conversations.
type DeleteResult struct {
	// MovedTo is the workspace the conversations were moved into.
	MovedTo string
	// MovedSessions is how many conversations are in the destination afterwards.
	MovedSessions int
}

// Manager owns the workspace set, the scope selections, and the sandboxes built
// from them.
type Manager struct {
	st       store.Store
	seedRoot string
	readOnly bool
	limits   workspace.Limits
	logger   *zap.Logger

	// cache holds one built sandbox per workspace name, each recorded with the
	// root it was built from so a renamed or re-pointed workspace cannot serve a
	// stale directory.
	mu    sync.RWMutex
	cache map[string]cachedSandbox
}

// cachedSandbox is a sandbox together with the root it was built from.
type cachedSandbox struct {
	root string
	ws   *workspace.Workspace
}

// New builds a Manager.
func New(opts Options) (*Manager, error) {
	if opts.Store == nil {
		return nil, ErrNoStore
	}
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	m := &Manager{
		st:       opts.Store,
		seedRoot: strings.TrimSpace(opts.SeedRoot),
		readOnly: opts.ReadOnly,
		limits:   opts.Limits,
		logger:   logger,
		cache:    map[string]cachedSandbox{},
	}
	if m.seedRoot == "" {
		return nil, errors.New("workspaces: 未配置初始工作区目录（tools.workspace）")
	}
	return m, nil
}

// List returns every workspace, ordered by name, with its conversation count.
func (m *Manager) List(ctx context.Context) ([]Spec, error) {
	rows, err := m.st.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Spec, 0, len(rows))
	for _, r := range rows {
		out = append(out, specFromRow(r))
	}
	return out, nil
}

// Get returns one workspace by name. The name is matched exactly and then
// case-insensitively, so a name typed as "Blog" selects the workspace called
// "blog" rather than failing.
func (m *Manager) Get(ctx context.Context, name string) (Spec, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return Spec{}, fmt.Errorf("%w: 名字为空", ErrNotFound)
	}
	if row, err := m.st.GetWorkspace(ctx, trimmed); err == nil {
		return specFromRow(row), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return Spec{}, err
	}
	rows, err := m.st.ListWorkspaces(ctx)
	if err != nil {
		return Spec{}, err
	}
	for _, r := range rows {
		if strings.EqualFold(r.Name, trimmed) {
			return specFromRow(r), nil
		}
	}
	return Spec{}, fmt.Errorf("%w: %s", ErrNotFound, trimmed)
}

// Create registers a directory as a workspace and returns it.
//
// The order matters: the directory is validated first, then the name, then
// uniqueness — so the error a person sees is about the thing they got wrong, and
// never a row pointing at a directory that does not exist.
func (m *Manager) Create(ctx context.Context, in CreateInput) (Spec, error) {
	root, err := ResolveDir(in.Root)
	if err != nil {
		return Spec{}, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = DefaultNameFor(root)
	}
	if err := ValidateName(name); err != nil {
		return Spec{}, err
	}
	// A case-insensitive duplicate is refused rather than merged: two workspaces
	// answering to one name would make which-one-a-scope-means depend on the query.
	if existing, gerr := m.Get(ctx, name); gerr == nil {
		return Spec{}, fmt.Errorf("%w: %s", ErrExists, existing.Name)
	} else if !errors.Is(gerr, ErrNotFound) {
		return Spec{}, gerr
	}

	if err := m.st.UpsertWorkspace(ctx, store.Workspace{Name: name, Root: root}); err != nil {
		if isUniqueViolation(err) {
			return Spec{}, fmt.Errorf("%w: %s", ErrExists, name)
		}
		return Spec{}, err
	}
	m.evict(name)
	m.logger.Info("workspace created", zap.String("name", name), zap.String("root", root))
	return m.Get(ctx, name)
}

// Rename changes a workspace's label. The directory and its files are untouched.
func (m *Manager) Rename(ctx context.Context, from, to string) (Spec, error) {
	cur, err := m.Get(ctx, from)
	if err != nil {
		return Spec{}, err
	}
	newName := strings.TrimSpace(to)
	if err := ValidateName(newName); err != nil {
		return Spec{}, err
	}
	if newName == cur.Name {
		return cur, nil
	}
	if existing, gerr := m.Get(ctx, newName); gerr == nil {
		return Spec{}, fmt.Errorf("%w: %s", ErrExists, existing.Name)
	} else if !errors.Is(gerr, ErrNotFound) {
		return Spec{}, gerr
	}

	if err := m.st.RenameWorkspace(ctx, cur.Name, newName); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Spec{}, fmt.Errorf("%w: %s", ErrNotFound, cur.Name)
		}
		if isUniqueViolation(err) {
			return Spec{}, fmt.Errorf("%w: %s", ErrExists, newName)
		}
		return Spec{}, err
	}
	// The sandbox cache is keyed by name, so the entry has to move with it: the
	// renamed workspace would otherwise rebuild its sandbox (harmless) while the
	// old name kept serving a directory that is no longer registered (not
	// harmless).
	m.evict(cur.Name)
	m.evict(newName)
	m.logger.Info("workspace renamed",
		zap.String("from", cur.Name), zap.String("to", newName), zap.String("root", cur.Root))
	return m.Get(ctx, newName)
}

// Delete removes a workspace registration and moves its conversations into
// another one.
//
// It never touches the filesystem. A conversation must belong to a workspace, so
// deleting one moves its conversations rather than stranding them, and the last
// remaining workspace is refused: there would be nowhere to put them.
func (m *Manager) Delete(ctx context.Context, name string) (DeleteResult, error) {
	cur, err := m.Get(ctx, name)
	if err != nil {
		return DeleteResult{}, err
	}
	specs, err := m.List(ctx)
	if err != nil {
		return DeleteResult{}, err
	}
	if len(specs) <= 1 {
		return DeleteResult{}, fmt.Errorf("%w：%s 是唯一的工作区，而会话必须属于某个工作区",
			ErrLastWorkspace, cur.Name)
	}

	// The destination is the first other workspace by name: deterministic, and
	// reported in the answer so the operator can move things again if they prefer
	// another one.
	dest := ""
	for _, spec := range specs {
		if !strings.EqualFold(spec.Name, cur.Name) {
			dest = spec.Name
			break
		}
	}
	moved, err := m.st.MoveWorkspaceBindings(ctx, cur.Name, dest)
	if err != nil {
		return DeleteResult{}, err
	}
	if err := m.st.DeleteWorkspace(ctx, cur.Name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DeleteResult{}, fmt.Errorf("%w: %s", ErrNotFound, cur.Name)
		}
		return DeleteResult{}, err
	}
	m.evict(cur.Name)
	m.logger.Info("workspace deleted (files untouched, conversations moved)",
		zap.String("name", cur.Name), zap.String("root", cur.Root),
		zap.String("moved_to", dest), zap.Int("in_destination", moved))
	return DeleteResult{MovedTo: dest, MovedSessions: moved}, nil
}

// EnsureSeed makes the workspace set usable at startup.
//
// Two jobs, both about invariants the rest of the product relies on:
//
//  1. if nothing is registered, register the configured `tools.workspace` (or the
//     working directory). Without it a fresh install would have no workspace at
//     all, and every conversation would have nowhere to go;
//  2. bind every conversation that has no workspace to it. A database written
//     before this feature existed holds conversations with no binding, and "every
//     conversation belongs to a workspace" has to be true for them too.
//
// It is idempotent: both steps ask the database what is actually missing.
func (m *Manager) EnsureSeed(ctx context.Context) (Spec, error) {
	specs, err := m.List(ctx)
	if err != nil {
		return Spec{}, err
	}
	if len(specs) == 0 {
		seeded, serr := m.seed(ctx)
		if serr != nil {
			return Spec{}, serr
		}
		specs = []Spec{seeded}
	}
	seed := specs[0]

	unbound, err := m.st.ListUnboundWebSessions(ctx)
	if err != nil {
		return Spec{}, err
	}
	for _, id := range unbound {
		if err := m.st.SetWorkspaceBinding(ctx, WebScope(id), seed.Name); err != nil {
			return Spec{}, err
		}
	}
	if len(unbound) > 0 {
		m.logger.Info("bound conversations that had no workspace",
			zap.Int("sessions", len(unbound)), zap.String("workspace", seed.Name))
		// Re-read: the spec above was read before the backfill, and the caller
		// shows its count (the sidebar does), so returning the stale one would
		// report a workspace with no conversations right after filing some in it.
		return m.Get(ctx, seed.Name)
	}
	return seed, nil
}

// seed registers the configured directory as the first workspace.
//
// Unlike Create it may create the directory: `tools.workspace` was always created
// on demand (that is what workspace.New does with a writable root), and a
// deployment whose configured root does not exist yet has to keep working exactly
// as it did before workspaces existed.
func (m *Manager) seed(ctx context.Context) (Spec, error) {
	root, err := filepathAbs(m.seedRoot)
	if err != nil {
		return Spec{}, err
	}
	if info, serr := os.Stat(root); serr != nil || !info.IsDir() {
		if merr := os.MkdirAll(root, 0o755); merr != nil {
			return Spec{}, fmt.Errorf("workspaces: 创建初始工作区目录 %s 失败: %w", root, merr)
		}
	}
	resolved, err := ResolveDir(root)
	if err != nil {
		return Spec{}, err
	}

	name := DefaultNameFor(resolved)
	for i := 2; ; i++ {
		if _, gerr := m.Get(ctx, name); errors.Is(gerr, ErrNotFound) {
			break
		}
		name = fmt.Sprintf("%s %d", DefaultNameFor(resolved), i)
	}
	if err := m.st.UpsertWorkspace(ctx, store.Workspace{Name: name, Root: resolved}); err != nil {
		return Spec{}, err
	}
	m.logger.Info("initial workspace registered from configuration",
		zap.String("name", name), zap.String("root", resolved),
		zap.String("hint", "可在控制台左侧重命名，或新建一个指向其他目录的工作区"))
	return m.Get(ctx, name)
}

// Default returns the workspace a new conversation should start in: the one the
// most recent conversation used, so "keep working where I was" is the default,
// and otherwise the first workspace by name.
func (m *Manager) Default(ctx context.Context) (Spec, error) {
	specs, err := m.List(ctx)
	if err != nil {
		return Spec{}, err
	}
	if len(specs) == 0 {
		return Spec{}, ErrNoWorkspace
	}
	if recent, rerr := m.st.ListChatSessions(ctx, store.ChatSessionFilter{Limit: 1}); rerr == nil &&
		len(recent) > 0 && recent[0].Workspace != "" {
		for _, spec := range specs {
			if spec.Name == recent[0].Workspace {
				return spec, nil
			}
		}
	}
	return specs[0], nil
}

// Active returns the workspace a scope is in.
//
// A scope that has chosen nothing — and a scope whose choice no longer exists —
// gets the default, with a warning in the second case: that fallback should be
// findable in the log rather than deduced from behaviour.
func (m *Manager) Active(ctx context.Context, scope string) (Spec, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return m.Default(ctx)
	}
	name, err := m.st.GetWorkspaceBinding(ctx, scope)
	if errors.Is(err, store.ErrNotFound) {
		return m.Default(ctx)
	}
	if err != nil {
		return Spec{}, err
	}
	spec, gerr := m.Get(ctx, name)
	if errors.Is(gerr, ErrNotFound) {
		fallback, ferr := m.Default(ctx)
		if ferr != nil {
			return Spec{}, ferr
		}
		m.logger.Warn("workspace binding points at a workspace that no longer exists; using the default",
			zap.String("scope", scope), zap.String("workspace", name), zap.String("fallback", fallback.Name))
		return fallback, nil
	}
	if gerr != nil {
		return Spec{}, gerr
	}
	return spec, nil
}

// Select records which workspace a scope works in, and returns it.
//
// The choice is per scope and never global: two conversations working in two
// projects at the same time is the point of the feature, and a shared pointer
// would have them fight over it.
func (m *Manager) Select(ctx context.Context, scope, name string) (Spec, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return Spec{}, invalidf("缺少会话标识，无法切换工作区")
	}
	spec, err := m.Get(ctx, name)
	if err != nil {
		return Spec{}, err
	}
	// Open it before recording the choice, so a scope is never bound to a
	// workspace whose directory has gone away.
	if _, err := m.Open(ctx, spec.Name); err != nil {
		return Spec{}, err
	}
	if err := m.st.SetWorkspaceBinding(ctx, scope, spec.Name); err != nil {
		return Spec{}, err
	}
	m.logger.Info("workspace selected",
		zap.String("scope", scope), zap.String("workspace", spec.Name), zap.String("root", spec.Root))
	return spec, nil
}

// Open returns the sandbox for a workspace by name, reusing a cached one when it
// was built from the same root.
func (m *Manager) Open(ctx context.Context, name string) (*workspace.Workspace, error) {
	spec, err := m.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return m.sandboxFor(spec)
}

// Resolve is the hot path: the sandbox and spec for a scope's active workspace,
// which is what a turn needs before it runs any tool.
func (m *Manager) Resolve(ctx context.Context, scope string) (*workspace.Workspace, Spec, error) {
	spec, err := m.Active(ctx, scope)
	if err != nil {
		return nil, Spec{}, err
	}
	ws, err := m.sandboxFor(spec)
	if err != nil {
		return nil, Spec{}, err
	}
	return ws, spec, nil
}

// sandboxFor returns a sandbox for spec, from the cache when it was built from
// the same root.
func (m *Manager) sandboxFor(spec Spec) (*workspace.Workspace, error) {
	m.mu.RLock()
	cached, ok := m.cache[spec.Name]
	m.mu.RUnlock()
	if ok && cached.root == spec.Root {
		return cached.ws, nil
	}

	ws, err := sandbox(spec, m.readOnly, m.limits)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.cache[spec.Name] = cachedSandbox{root: spec.Root, ws: ws}
	m.mu.Unlock()
	return ws, nil
}

// evict drops a cached sandbox after a write.
func (m *Manager) evict(name string) {
	m.mu.Lock()
	delete(m.cache, name)
	m.mu.Unlock()
}

// Names returns the workspace names, for a caller that only needs to offer
// choices (the Feishu picker, an error message listing what exists).
func (m *Manager) Names(ctx context.Context) ([]string, error) {
	specs, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.Name)
	}
	sort.Strings(out)
	return out, nil
}

// isUniqueViolation reports whether an error is SQLite's UNIQUE constraint
// failure.
//
// The store layer wraps errors, so the check is on the message: it is the one
// signal available without depending on the driver's error type, and reporting a
// duplicate name as a server fault would be worse than a string match on a
// message SQLite has kept stable for years.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "UNIQUE CONSTRAINT FAILED") ||
		strings.Contains(msg, "CONSTRAINT FAILED: UNIQUE")
}

// filepathAbs is filepath.Abs with the error wrapped.
func filepathAbs(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("workspaces: 解析路径 %q 失败: %w", path, err)
	}
	return abs, nil
}
