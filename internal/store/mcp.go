package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MCPTransport is how an MCP server is reached.
//
// The three values are genuinely different clients, not a preference: stdio
// spawns a subprocess, sse speaks the legacy HTTP+SSE protocol, and http speaks
// streamable HTTP. A server that speaks one does not answer the others, which is
// why the value is stored rather than inferred.
type MCPTransport string

const (
	// MCPTransportStdio spawns a local command and talks over its pipes.
	MCPTransportStdio MCPTransport = "stdio"
	// MCPTransportSSE dials a URL and uses the HTTP+SSE transport.
	MCPTransportSSE MCPTransport = "sse"
	// MCPTransportHTTP dials a URL and uses streamable HTTP.
	MCPTransportHTTP MCPTransport = "http"
)

// AllMCPTransports is the set the console offers, in display order. It is
// served by the MCP API so the UI never hardcodes a list this build may not
// share.
var AllMCPTransports = []MCPTransport{MCPTransportStdio, MCPTransportSSE, MCPTransportHTTP}

// ValidMCPTransport reports whether t is a transport this build understands.
func ValidMCPTransport(t MCPTransport) bool {
	for _, known := range AllMCPTransports {
		if t == known {
			return true
		}
	}
	return false
}

// MCPServer is one configured MCP server.
//
// It mirrors internal/mcp.ServerSpec, which cannot be referenced here because
// the store does not depend on the MCP client. The server package converts one
// into the other.
type MCPServer struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Transport MCPTransport `json:"transport"`
	// Command, Args and Env describe a stdio server. Env entries are
	// "KEY=value" and are merged onto the process environment at spawn.
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Env     []string `json:"env"`
	// URL, Headers describe a remote server. Header entries are "Name: value".
	URL     string         `json:"url"`
	Headers []string       `json:"headers"`
	Source  ProviderSource `json:"source"`
	Enabled bool           `json:"enabled"`
	// LastError is the most recent connection failure recorded by the runtime,
	// so a broken server is visible without the user re-testing it.
	LastError string    `json:"last_error"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ResolvedTransport returns the transport, treating an empty value as stdio.
// Every row written before the transport column existed is stdio, and a blank
// field must keep meaning what it always did.
func (m MCPServer) ResolvedTransport() MCPTransport {
	if m.Transport == "" {
		return MCPTransportStdio
	}
	return m.Transport
}

// Remote reports whether this server is reached over the network rather than by
// spawning a command. It is what the validation and the UI labels branch on.
func (m MCPServer) Remote() bool { return m.ResolvedTransport() != MCPTransportStdio }

// Validate checks the fields a connection actually needs. It is the same rule
// the API applies, kept here so a row read back from the database is held to it
// too — a hand-edited file cannot put an unusable server into the runtime.
func (m MCPServer) Validate() error {
	if strings.TrimSpace(m.ID) == "" {
		return errors.New("id is required")
	}
	switch {
	case !ValidMCPTransport(m.ResolvedTransport()):
		return fmt.Errorf("unsupported transport %q", m.Transport)
	case m.Remote():
		if strings.TrimSpace(m.URL) == "" {
			return fmt.Errorf("%s server needs a url", m.ResolvedTransport())
		}
	default:
		if strings.TrimSpace(m.Command) == "" {
			return errors.New("stdio server needs a command")
		}
	}
	return nil
}

// ---------------------------------------------------------------- servers --

const mcpServerCols = `id, name, transport, command, args, env, url, headers, source, enabled, last_error, created_at, updated_at`

// UpsertMCPServer inserts or updates one server.
//
// The whole row is replaced (unlike a provider's API key, nothing here is
// write-only), because the console always edits a complete definition: a blank
// args list means "no arguments", not "leave the old ones".
func (s *sqliteStore) UpsertMCPServer(ctx context.Context, m MCPServer) error {
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("store: mcp server id is required")
	}
	if m.Transport == "" {
		m.Transport = MCPTransportStdio
	}
	if m.Source == "" {
		m.Source = SourceUser
	}
	args, err := encodeStringList(m.Args)
	if err != nil {
		return fmt.Errorf("store: encode mcp args: %w", err)
	}
	env, err := encodeStringList(m.Env)
	if err != nil {
		return fmt.Errorf("store: encode mcp env: %w", err)
	}
	headers, err := encodeStringList(m.Headers)
	if err != nil {
		return fmt.Errorf("store: encode mcp headers: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mcp_servers (`+mcpServerCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			name       = excluded.name,
			transport  = excluded.transport,
			command    = excluded.command,
			args       = excluded.args,
			env        = excluded.env,
			url        = excluded.url,
			headers    = excluded.headers,
			source     = excluded.source,
			enabled    = excluded.enabled,
			updated_at = CURRENT_TIMESTAMP`,
		m.ID, m.Name, string(m.Transport), m.Command, args, env, m.URL, headers,
		string(m.Source), boolToInt(m.Enabled), m.LastError)
	if err != nil {
		return fmt.Errorf("store: upsert mcp server: %w", err)
	}
	return nil
}

// GetMCPServer loads one server by id.
func (s *sqliteStore) GetMCPServer(ctx context.Context, id string) (MCPServer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+mcpServerCols+` FROM mcp_servers WHERE id = ?`, id)
	return scanMCPServer(row)
}

// ListMCPServers returns every server: config-declared first, then by id.
//
// Order matters to the reader, not to the runtime: the config rows are the ones
// the user cannot edit, so pinning them to the top keeps the editable set
// together.
func (s *sqliteStore) ListMCPServers(ctx context.Context) ([]MCPServer, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+mcpServerCols+` FROM mcp_servers ORDER BY source ASC, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list mcp servers: %w", err)
	}
	defer rows.Close()

	var out []MCPServer
	for rows.Next() {
		m, err := scanMCPServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMCPServer removes one server. It reports ErrNotFound for an unknown id
// so the API can answer 404 rather than a success that changed nothing.
func (s *sqliteStore) DeleteMCPServer(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM mcp_servers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete mcp server: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: mcp server %q: %w", id, ErrNotFound)
	}
	return nil
}

// SetMCPServerError records the outcome of the last connection attempt. It is
// written by the runtime rather than by the editor, so it has its own setter
// and does not touch updated_at — a failed connect is not an edit.
func (s *sqliteStore) SetMCPServerError(ctx context.Context, id, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE mcp_servers SET last_error = ? WHERE id = ?`, msg, id)
	if err != nil {
		return fmt.Errorf("store: set mcp server error: %w", err)
	}
	return nil
}

// scanMCPServer reads one row, however it was obtained (Query or QueryRow).
func scanMCPServer(row interface{ Scan(...any) error }) (MCPServer, error) {
	var (
		m       MCPServer
		trans   string
		source  string
		args    string
		env     string
		headers string
		enabled int
	)
	err := row.Scan(&m.ID, &m.Name, &trans, &m.Command, &args, &env, &m.URL, &headers,
		&source, &enabled, &m.LastError, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MCPServer{}, ErrNotFound
		}
		return MCPServer{}, fmt.Errorf("store: scan mcp server: %w", err)
	}
	m.Transport = MCPTransport(trans)
	if m.Transport == "" {
		m.Transport = MCPTransportStdio
	}
	m.Source = ProviderSource(source)
	m.Enabled = enabled != 0
	m.Args = decodeStringList(args)
	m.Env = decodeStringList(env)
	m.Headers = decodeStringList(headers)
	return m, nil
}

// encodeStringList renders a list as a JSON array for storage.
//
// JSON rather than a separator-joined string: an argument or a header value can
// legitimately contain any separator a hand-rolled format would pick, and an
// empty list must round-trip as an empty list.
func encodeStringList(in []string) (string, error) {
	if len(in) == 0 {
		return "", nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeStringList reads a stored JSON array, treating anything unreadable as
// an empty list: a corrupt value must not make a server unloadable, since the
// rest of its definition may still be perfectly usable.
func decodeStringList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}
