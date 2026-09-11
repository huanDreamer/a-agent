package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Chat message roles. They mirror the model-facing roles but are stored as
// plain strings so the table does not depend on the LLM library.
const (
	// RoleUser is a message from the human.
	RoleUser = "user"
	// RoleAssistant is a message from the model.
	RoleAssistant = "assistant"
	// RoleTool is a tool observation.
	RoleTool = "tool"
	// RoleSystem is a system instruction.
	RoleSystem = "system"
)

// ChatSession is a persisted web-chat conversation.
type ChatSession struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	UserID    string    `json:"user_id"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// MessageCount is populated by list queries so a UI can show it without
	// fetching every message.
	MessageCount int `json:"message_count"`
}

// ChatMessage is one persisted turn part.
type ChatMessage struct {
	ID      int64  `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
	// Reasoning is the model's thinking, stored separately so the UI can keep
	// it collapsed. It is never part of what the model is asked again.
	Reasoning string `json:"reasoning,omitempty"`
	// ToolCalls is the raw JSON array of tool calls the assistant requested.
	ToolCalls string `json:"tool_calls,omitempty"`
	// ToolCallID and ToolName identify a tool-result message.
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	// UsageJSON holds the token usage for an assistant message.
	UsageJSON string `json:"usage,omitempty"`
	// Error records a failed turn so the UI can show it after a reload.
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ChatSessionFilter narrows a session listing. Zero fields are ignored.
type ChatSessionFilter struct {
	UserID string
	Limit  int
}

// CreateChatSession inserts a session.
func (s *sqliteStore) CreateChatSession(ctx context.Context, sess ChatSession) error {
	if strings.TrimSpace(sess.ID) == "" {
		return errors.New("store: chat session id is required")
	}
	now := time.Now().UTC()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	if sess.UpdatedAt.IsZero() {
		sess.UpdatedAt = now
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_sessions (id, title, user_id, provider, model, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.Title, sess.UserID, sess.Provider, sess.Model, sess.CreatedAt, sess.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert chat session: %w", err)
	}
	return nil
}

// GetChatSession returns one session. It returns ErrNotFound when absent.
func (s *sqliteStore) GetChatSession(ctx context.Context, id string) (ChatSession, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, title, user_id, provider, model, created_at, updated_at,
		       (SELECT COUNT(*) FROM chat_messages m WHERE m.session_id = s.id)
		FROM chat_sessions s WHERE id = ?`, id)

	var out ChatSession
	var created, updated sql.NullTime
	if err := row.Scan(&out.ID, &out.Title, &out.UserID, &out.Provider, &out.Model,
		&created, &updated, &out.MessageCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChatSession{}, ErrNotFound
		}
		return ChatSession{}, fmt.Errorf("get chat session: %w", err)
	}
	out.CreatedAt = nullTime(created)
	out.UpdatedAt = nullTime(updated)
	return out, nil
}

// ListChatSessions returns sessions most-recently-updated first.
func (s *sqliteStore) ListChatSessions(ctx context.Context, f ChatSessionFilter) ([]ChatSession, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	q := `SELECT s.id, s.title, s.user_id, s.provider, s.model, s.created_at, s.updated_at,
	             (SELECT COUNT(*) FROM chat_messages m WHERE m.session_id = s.id)
	      FROM chat_sessions s`
	var args []any
	if f.UserID != "" {
		q += " WHERE s.user_id = ?"
		args = append(args, f.UserID)
	}
	q += " ORDER BY s.updated_at DESC, s.id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list chat sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ChatSession, 0, 8)
	for rows.Next() {
		var sess ChatSession
		var created, updated sql.NullTime
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.UserID, &sess.Provider, &sess.Model,
			&created, &updated, &sess.MessageCount); err != nil {
			return nil, fmt.Errorf("scan chat session: %w", err)
		}
		sess.CreatedAt = nullTime(created)
		sess.UpdatedAt = nullTime(updated)
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chat sessions: %w", err)
	}
	return out, nil
}

// UpdateChatSession applies a partial update. Only non-nil fields are written,
// so a caller can rename a session without accidentally clearing its model.
func (s *sqliteStore) UpdateChatSession(ctx context.Context, id string, patch ChatSessionPatch) error {
	sets := make([]string, 0, 5)
	args := make([]any, 0, 6)

	if patch.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *patch.Title)
	}
	if patch.Provider != nil {
		sets = append(sets, "provider = ?")
		args = append(args, *patch.Provider)
	}
	if patch.Model != nil {
		sets = append(sets, "model = ?")
		args = append(args, *patch.Model)
	}
	if patch.UserID != nil {
		sets = append(sets, "user_id = ?")
		args = append(args, *patch.UserID)
	}
	// Touch updated_at whenever anything is patched, so the session order
	// reflects activity rather than only creation.
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC())
	args = append(args, id)

	res, err := s.db.ExecContext(ctx,
		"UPDATE chat_sessions SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return fmt.Errorf("update chat session: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchChatSession bumps updated_at, used after appending messages.
func (s *sqliteStore) TouchChatSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE chat_sessions SET updated_at = ? WHERE id = ?", time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("touch chat session: %w", err)
	}
	return nil
}

// DeleteChatSession removes a session and, by cascade, its messages.
func (s *sqliteStore) DeleteChatSession(ctx context.Context, id string) error {
	// Delete children explicitly as well: foreign_keys is enabled per
	// connection, and relying on it alone would silently orphan rows if that
	// pragma ever changed.
	if _, err := s.db.ExecContext(ctx, "DELETE FROM chat_messages WHERE session_id = ?", id); err != nil {
		return fmt.Errorf("delete chat messages: %w", err)
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM chat_sessions WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete chat session: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendChatMessage adds a message to a session.
func (s *sqliteStore) AppendChatMessage(ctx context.Context, sessionID string, m ChatMessage) (int64, error) {
	if strings.TrimSpace(sessionID) == "" {
		return 0, errors.New("store: session id is required")
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_messages
		  (session_id, role, content, reasoning, tool_calls, tool_call_id, tool_name, usage_json, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, m.Role, m.Content, m.Reasoning, m.ToolCalls,
		m.ToolCallID, m.ToolName, m.UsageJSON, m.Error, m.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert chat message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, nil // the row is written; the id is only a convenience
	}
	return id, nil
}

// ListChatMessages returns a session's messages oldest first. limit <= 0 means
// no limit.
func (s *sqliteStore) ListChatMessages(ctx context.Context, sessionID string, limit int) ([]ChatMessage, error) {
	q := `SELECT id, role, content, reasoning, tool_calls, tool_call_id, tool_name,
	             usage_json, error, created_at
	      FROM chat_messages WHERE session_id = ? ORDER BY id ASC`
	args := []any{sessionID}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list chat messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ChatMessage, 0, 16)
	for rows.Next() {
		var m ChatMessage
		var created sql.NullTime
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.Reasoning, &m.ToolCalls,
			&m.ToolCallID, &m.ToolName, &m.UsageJSON, &m.Error, &created); err != nil {
			return nil, fmt.Errorf("scan chat message: %w", err)
		}
		m.CreatedAt = nullTime(created)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chat messages: %w", err)
	}
	return out, nil
}

// DeleteChatMessages clears a session's history but keeps the session itself,
// which is what a "clear conversation" action should do.
func (s *sqliteStore) DeleteChatMessages(ctx context.Context, sessionID string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM chat_messages WHERE session_id = ?", sessionID); err != nil {
		return fmt.Errorf("clear chat messages: %w", err)
	}
	return s.TouchChatSession(ctx, sessionID)
}

// ChatSessionPatch is a partial session update.
type ChatSessionPatch struct {
	Title    *string
	Provider *string
	Model    *string
	UserID   *string
}

// nullTime converts a nullable timestamp.
func nullTime(t sql.NullTime) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}
