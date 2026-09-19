package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Settings keys owned by this file. They are namespaced so a later tenant of
// app_settings cannot collide with the budget's three.
const (
	// KeyChatMaxSteps is the tool-calling step cap for one web chat turn.
	KeyChatMaxSteps = "chat.max_steps"
	// KeyChatTurnMaxTokens is the per-turn token budget; 0 means unlimited.
	KeyChatTurnMaxTokens = "chat.turn_max_tokens"
	// KeyChatTurnDeadlineSeconds is the per-turn wall-clock cap; 0 means
	// unlimited.
	KeyChatTurnDeadlineSeconds = "chat.turn_deadline_seconds"
	// KeyClaudeCodeMode is which mode the console switched the agent to:
	// "claudecode" (run on ~/.claude/settings.json) or "native" (run on this
	// deployment's own model config). An absent key means "whatever
	// claudecode.enable in config.yaml says", which is what keeps the file
	// authoritative until someone actually flips the switch.
	KeyClaudeCodeMode = "claudecode.mode"
)

// The two values KeyClaudeCodeMode may hold.
const (
	ClaudeCodeModeNative = "native"
	ClaudeCodeModeCompat = "claudecode"
)

// TurnBudgetOverride is the set of per-turn budget values the console has
// changed, as stored. Every field is a pointer so "not set" stays distinct from
// an explicit 0: 0 is a meaningful choice (unlimited) for the token and deadline
// dimensions, and conflating the two would turn 恢复默认 on one field into an
// override of all three.
type TurnBudgetOverride struct {
	MaxSteps     *int
	MaxTokens    *int
	DeadlineSecs *int
}

// GetTurnBudgetOverride returns the stored budget override. A key that was never
// written comes back nil, which the caller reads as "use config.yaml's value".
func (s *sqliteStore) GetTurnBudgetOverride(ctx context.Context) (TurnBudgetOverride, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM app_settings WHERE key IN (?, ?, ?)`,
		KeyChatMaxSteps, KeyChatTurnMaxTokens, KeyChatTurnDeadlineSeconds)
	if err != nil {
		return TurnBudgetOverride{}, fmtErr("read app settings", err)
	}
	defer func() { _ = rows.Close() }()

	var out TurnBudgetOverride
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return TurnBudgetOverride{}, fmtErr("scan app setting", err)
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			// A row that is not an integer is corruption rather than a value to
			// act on: falling back to the config default is the only reading that
			// cannot make a turn behave differently from what the console shows.
			continue
		}
		switch key {
		case KeyChatMaxSteps:
			v := n
			out.MaxSteps = &v
		case KeyChatTurnMaxTokens:
			v := n
			out.MaxTokens = &v
		case KeyChatTurnDeadlineSeconds:
			v := n
			out.DeadlineSecs = &v
		}
	}
	if err := rows.Err(); err != nil {
		return TurnBudgetOverride{}, fmtErr("iterate app settings", err)
	}
	return out, nil
}

// SetTurnBudgetOverride writes the three budget values. A nil field deletes that
// key, which is how 恢复默认 is expressed: the field goes back to being governed
// by config.yaml.
func (s *sqliteStore) SetTurnBudgetOverride(ctx context.Context, o TurnBudgetOverride) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmtErr("begin app settings tx", err)
	}
	pairs := []struct {
		key   string
		value *int
	}{
		{KeyChatMaxSteps, o.MaxSteps},
		{KeyChatTurnMaxTokens, o.MaxTokens},
		{KeyChatTurnDeadlineSeconds, o.DeadlineSecs},
	}
	for _, p := range pairs {
		if p.value == nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM app_settings WHERE key = ?`, p.key); err != nil {
				_ = tx.Rollback()
				return fmtErr("clear app setting %s", err, p.key)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO app_settings (key, value, updated_at)
			VALUES (?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
			p.key, strconv.Itoa(*p.value)); err != nil {
			_ = tx.Rollback()
			return fmtErr("write app setting %s", err, p.key)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmtErr("commit app settings", err)
	}
	return nil
}

// GetClaudeCodeMode returns the mode the console stored.
//
// The second result says whether the key exists at all. The distinction is the
// whole reason this is not a plain string: an operator who never touched the
// switch must get config.yaml's value, not an override this process invented on
// first read — otherwise editing the config file would stop having any effect
// the moment the console was opened once.
func (s *sqliteStore) GetClaudeCodeMode(ctx context.Context) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key = ?`, KeyClaudeCodeMode).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmtErr("read app setting %s", err, KeyClaudeCodeMode)
	}
	switch value {
	case ClaudeCodeModeNative, ClaudeCodeModeCompat:
		return value, true, nil
	default:
		// A row that is not one of the two modes is corruption rather than a
		// value to act on: reporting it as absent falls back to the config file,
		// which is the only reading that cannot silently pick a mode.
		return "", false, nil
	}
}

// SetClaudeCodeMode stores the mode. An empty mode deletes the key, which is how
// the config file becomes authoritative again.
func (s *sqliteStore) SetClaudeCodeMode(ctx context.Context, mode string) error {
	if mode == "" {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM app_settings WHERE key = ?`, KeyClaudeCodeMode); err != nil {
			return fmtErr("clear app setting %s", err, KeyClaudeCodeMode)
		}
		return nil
	}
	if mode != ClaudeCodeModeNative && mode != ClaudeCodeModeCompat {
		return fmt.Errorf("store: %q is not a Claude Code mode (%q or %q)",
			mode, ClaudeCodeModeNative, ClaudeCodeModeCompat)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO app_settings (key, value, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		KeyClaudeCodeMode, mode); err != nil {
		return fmtErr("write app setting %s", err, KeyClaudeCodeMode)
	}
	return nil
}
