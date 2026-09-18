package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ChatPlanRow is one conversation's task plan as it is stored.
//
// TasksJSON is the serialised task list rather than a nested type because this
// package is the persistence layer and does not own the plan's vocabulary
// (internal/tool does, next to the tools that write it). The column is text, and
// the same reasoning applies as for a message's ToolCalls: a client parses it
// defensively, and an absent plan must stay distinguishable from an empty one.
type ChatPlanRow struct {
	SessionID string
	Goal      string
	TasksJSON string
	Revision  int
	UpdatedAt time.Time
}

// GetChatPlan returns a conversation's plan. ErrNotFound means the conversation
// has never had one, which is the state every conversation starts in.
func (s *sqliteStore) GetChatPlan(ctx context.Context, sessionID string) (ChatPlanRow, error) {
	if strings.TrimSpace(sessionID) == "" {
		return ChatPlanRow{}, errors.New("store: session id is required")
	}
	var (
		row       ChatPlanRow
		updatedAt sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT session_id, goal, tasks, revision, updated_at
		  FROM chat_plans WHERE session_id = ?`, sessionID).
		Scan(&row.SessionID, &row.Goal, &row.TasksJSON, &row.Revision, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatPlanRow{}, ErrNotFound
	}
	if err != nil {
		return ChatPlanRow{}, fmt.Errorf("get chat plan: %w", err)
	}
	row.UpdatedAt = nullTime(updatedAt)
	return row, nil
}

// SetChatPlan writes a conversation's plan, replacing whatever was there.
//
// It is an upsert rather than an insert-or-update pair because the caller has no
// way to know which it is: the model's first plan_* call creates the row, and
// every later one rewrites it. Making that the storage layer's problem keeps the
// planner free of a "does this plan exist yet" branch that could race.
func (s *sqliteStore) SetChatPlan(ctx context.Context, row ChatPlanRow) error {
	if strings.TrimSpace(row.SessionID) == "" {
		return errors.New("store: session id is required")
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_plans (session_id, goal, tasks, revision, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
		  goal = excluded.goal,
		  tasks = excluded.tasks,
		  revision = excluded.revision,
		  updated_at = excluded.updated_at`,
		row.SessionID, row.Goal, row.TasksJSON, row.Revision, row.UpdatedAt)
	if err != nil {
		return fmt.Errorf("set chat plan: %w", err)
	}
	return nil
}

// DeleteChatPlan forgets a conversation's plan. A missing row is not an error:
// the caller's intent ("this conversation has no plan now") is satisfied either
// way.
func (s *sqliteStore) DeleteChatPlan(ctx context.Context, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("store: session id is required")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM chat_plans WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete chat plan: %w", err)
	}
	return nil
}
