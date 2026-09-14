package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Workspace is one working directory the agent can be pointed at.
//
// It is a registration, not a sandbox and not a policy: the bytes live on disk
// at Root, the confinement is applied by internal/workspace when a turn runs, and
// what the agent may *do* there (write files, run commands) is process-wide
// configuration (`tools.*`) rather than a property of the workspace. A workspace
// answers one question — which directory — and the console organises
// conversations by it, like a folder.
type Workspace struct {
	// Name is the label the sidebar shows and a scope refers to. It is NOT a
	// path component: it is chosen by the operator (defaulting to the directory's
	// base name) and may be renamed at any time.
	Name string `json:"name"`
	// Root is the absolute directory this workspace points at.
	Root string `json:"root"`
	// SessionCount is how many conversations are in it. It is computed by the
	// list query rather than stored, so it cannot drift from the bindings.
	SessionCount int `json:"session_count"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UpsertWorkspace inserts or updates a workspace registration.
//
// The name is the identity: re-registering an existing name re-points it at a new
// root in place. Renaming is a separate operation (RenameWorkspace) because it has
// to move the bindings that refer to the old name along with it.
func (s *sqliteStore) UpsertWorkspace(ctx context.Context, w Workspace) error {
	name := strings.TrimSpace(w.Name)
	if name == "" {
		return errors.New("store: workspace name is required")
	}
	if strings.TrimSpace(w.Root) == "" {
		return fmt.Errorf("store: workspace %q: root is required", name)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workspaces (name, root) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET root = excluded.root, updated_at = CURRENT_TIMESTAMP`,
		name, w.Root,
	)
	if err != nil {
		return fmt.Errorf("store: upsert workspace %q: %w", name, err)
	}
	return nil
}

// ListWorkspaces returns every workspace with its conversation count, by name.
//
// The count comes from the bindings, which is where "which conversation is in
// which workspace" actually lives, so the sidebar can group without a second
// request and without the two being able to disagree.
func (s *sqliteStore) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT w.name, w.root, w.created_at, w.updated_at,
		        (SELECT COUNT(*) FROM workspace_bindings b
		          WHERE b.workspace = w.name AND b.scope LIKE 'web:%') AS sessions
		   FROM workspaces w ORDER BY w.name`)
	if err != nil {
		return nil, fmt.Errorf("store: list workspaces: %w", err)
	}
	defer rows.Close()

	var out []Workspace
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.Name, &w.Root, &w.CreatedAt, &w.UpdatedAt, &w.SessionCount); err != nil {
			return nil, fmt.Errorf("store: scan workspace: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate workspaces: %w", err)
	}
	return out, nil
}

// GetWorkspace returns one workspace by name, or ErrNotFound.
func (s *sqliteStore) GetWorkspace(ctx context.Context, name string) (Workspace, error) {
	var w Workspace
	err := s.db.QueryRowContext(ctx,
		`SELECT w.name, w.root, w.created_at, w.updated_at,
		        (SELECT COUNT(*) FROM workspace_bindings b
		          WHERE b.workspace = w.name AND b.scope LIKE 'web:%') AS sessions
		   FROM workspaces w WHERE w.name = ?`, strings.TrimSpace(name),
	).Scan(&w.Name, &w.Root, &w.CreatedAt, &w.UpdatedAt, &w.SessionCount)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, ErrNotFound
	}
	if err != nil {
		return Workspace{}, fmt.Errorf("store: get workspace %q: %w", name, err)
	}
	return w, nil
}

// RenameWorkspace changes a workspace's label and moves every binding that
// referred to the old name, in one transaction.
//
// Both halves are one operation on purpose: a binding left pointing at a name
// that no longer exists would strand the conversations that used it — they would
// fall back to another workspace while claiming to be somewhere else.
func (s *sqliteStore) RenameWorkspace(ctx context.Context, from, to string) error {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" {
		return errors.New("store: workspace names are required")
	}
	if from == to {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: rename workspace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspaces WHERE name = ?`, from).Scan(&exists); err != nil {
		return fmt.Errorf("store: rename workspace %q: %w", from, err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE workspaces SET name = ?, updated_at = CURRENT_TIMESTAMP WHERE name = ?`, to, from); err != nil {
		// A duplicate name trips the unique index on lower(name); the caller
		// turns that into "already exists" rather than a 500.
		return fmt.Errorf("store: rename workspace %q to %q: %w", from, to, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE workspace_bindings SET workspace = ? WHERE workspace = ?`, to, from); err != nil {
		return fmt.Errorf("store: move bindings %q to %q: %w", from, to, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: rename workspace: commit: %w", err)
	}
	return nil
}

// DeleteWorkspace removes a registration and every binding that pointed at it.
//
// It never touches the filesystem: the directory a workspace names is left
// exactly as it is, because removing a registration is a decision about where the
// agent may work, not about what the operator's disk holds. A caller that needs
// the conversations to survive moves them first with MoveWorkspaceBindings —
// which is what internal/workspaces does before calling this.
//
// A missing name reports ErrNotFound.
func (s *sqliteStore) DeleteWorkspace(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("store: workspace name is required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete workspace %q: begin: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("store: delete workspace %q: %w", name, err)
	}
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM workspace_bindings WHERE workspace = ?`, name); err != nil {
		return fmt.Errorf("store: unbind workspace %q: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete workspace %q: commit: %w", name, err)
	}
	return nil
}

// MoveWorkspaceBindings re-points every scope in one workspace at another and
// returns how many now belong to the destination.
//
// It is what makes deleting a workspace non-destructive to conversations: they
// keep their messages and their ids and simply belong somewhere else.
func (s *sqliteStore) MoveWorkspaceBindings(ctx context.Context, from, to string) (int, error) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE workspace_bindings SET workspace = ?, updated_at = CURRENT_TIMESTAMP
		  WHERE workspace = ?`, to, from); err != nil {
		return 0, fmt.Errorf("store: move bindings %q to %q: %w", from, to, err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspace_bindings WHERE workspace = ?`, to).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count bindings for %q: %w", to, err)
	}
	return n, nil
}

// GetWorkspaceBinding returns the workspace a scope is in, or ErrNotFound.
//
// ErrNotFound is distinct from an empty name on purpose: "this conversation has
// no workspace" is a state the caller must resolve (by falling back), not a
// valid answer to hide behind an empty string.
func (s *sqliteStore) GetWorkspaceBinding(ctx context.Context, scope string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT workspace FROM workspace_bindings WHERE scope = ?`, strings.TrimSpace(scope)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: get workspace binding %q: %w", scope, err)
	}
	return name, nil
}

// SetWorkspaceBinding records which workspace a scope works in.
func (s *sqliteStore) SetWorkspaceBinding(ctx context.Context, scope, name string) error {
	scope = strings.TrimSpace(scope)
	name = strings.TrimSpace(name)
	if scope == "" {
		return errors.New("store: workspace binding scope is required")
	}
	if name == "" {
		return errors.New("store: workspace binding name is required")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO workspace_bindings (scope, workspace) VALUES (?, ?)
		 ON CONFLICT(scope) DO UPDATE SET workspace = excluded.workspace,
		                                 updated_at = CURRENT_TIMESTAMP`,
		scope, name,
	); err != nil {
		return fmt.Errorf("store: set workspace binding %q: %w", scope, err)
	}
	return nil
}

// ClearWorkspaceBinding removes a scope's choice. A scope with no binding is not
// an error: the desired state is reached either way.
func (s *sqliteStore) ClearWorkspaceBinding(ctx context.Context, scope string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM workspace_bindings WHERE scope = ?`, strings.TrimSpace(scope)); err != nil {
		return fmt.Errorf("store: clear workspace binding %q: %w", scope, err)
	}
	return nil
}

// ListScopesInWorkspace returns the scopes bound to a workspace, newest first.
func (s *sqliteStore) ListScopesInWorkspace(ctx context.Context, name string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT scope FROM workspace_bindings WHERE workspace = ? ORDER BY updated_at DESC`, name)
	if err != nil {
		return nil, fmt.Errorf("store: list scopes in %q: %w", name, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, fmt.Errorf("store: scan scope: %w", err)
		}
		out = append(out, scope)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate scopes: %w", err)
	}
	return out, nil
}

// ListUnboundWebSessions returns the ids of conversations with no workspace
// binding.
//
// It exists for the one-time backfill that keeps "every conversation belongs to a
// workspace" true for a database that predates the rule: rather than trusting a
// migration to have covered every case, the startup path asks the database which
// conversations are actually homeless and binds them.
func (s *sqliteStore) ListUnboundWebSessions(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id FROM chat_sessions s
		  WHERE NOT EXISTS (
		        SELECT 1 FROM workspace_bindings b WHERE b.scope = 'web:' || s.id)
		  ORDER BY s.updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list unbound sessions: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan unbound session: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate unbound sessions: %w", err)
	}
	return out, nil
}
