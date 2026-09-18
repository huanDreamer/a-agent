package store

import (
	"context"
	"testing"
)

// intPtr is the smallest way to express "this field is set" in a test, which is
// the whole point of the pointer fields on TurnBudgetOverride.
func intPtr(v int) *int { return &v }

func TestGetTurnBudgetOverride_AbsentIsNil(t *testing.T) {
	s := newTestStore(t)

	ov, err := s.GetTurnBudgetOverride(context.Background())
	if err != nil {
		t.Fatalf("GetTurnBudgetOverride: %v", err)
	}
	// Nothing was ever written, so every field must stay nil: that is what makes
	// config.yaml govern, and what keeps an unset field from being pinned to a
	// stale copy of the startup default.
	if ov.MaxSteps != nil || ov.MaxTokens != nil || ov.DeadlineSecs != nil {
		t.Fatalf("override = %+v, want every field nil", ov)
	}
}

func TestSetTurnBudgetOverride_RoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.SetTurnBudgetOverride(ctx, TurnBudgetOverride{
		MaxSteps:     intPtr(60),
		MaxTokens:    intPtr(0), // explicit unlimited is a real choice, not "unset"
		DeadlineSecs: intPtr(10800),
	}); err != nil {
		t.Fatalf("SetTurnBudgetOverride: %v", err)
	}

	ov, err := s.GetTurnBudgetOverride(ctx)
	if err != nil {
		t.Fatalf("GetTurnBudgetOverride: %v", err)
	}
	if ov.MaxSteps == nil || *ov.MaxSteps != 60 {
		t.Errorf("MaxSteps = %v, want 60", ov.MaxSteps)
	}
	if ov.MaxTokens == nil || *ov.MaxTokens != 0 {
		t.Errorf("MaxTokens = %v, want an explicit 0", ov.MaxTokens)
	}
	if ov.DeadlineSecs == nil || *ov.DeadlineSecs != 10800 {
		t.Errorf("DeadlineSecs = %v, want 10800", ov.DeadlineSecs)
	}
}

func TestSetTurnBudgetOverride_NilClearsThatKey(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.SetTurnBudgetOverride(ctx, TurnBudgetOverride{
		MaxSteps:     intPtr(60),
		DeadlineSecs: intPtr(3600),
	}); err != nil {
		t.Fatalf("SetTurnBudgetOverride: %v", err)
	}
	// 恢复默认 for one dimension only: the other two must survive untouched,
	// which is what "per field" means.
	if err := s.SetTurnBudgetOverride(ctx, TurnBudgetOverride{
		MaxSteps:     intPtr(60),
		DeadlineSecs: nil,
	}); err != nil {
		t.Fatalf("SetTurnBudgetOverride: %v", err)
	}

	ov, err := s.GetTurnBudgetOverride(ctx)
	if err != nil {
		t.Fatalf("GetTurnBudgetOverride: %v", err)
	}
	if ov.MaxSteps == nil || *ov.MaxSteps != 60 {
		t.Errorf("MaxSteps = %v, want the surviving 60", ov.MaxSteps)
	}
	if ov.DeadlineSecs != nil {
		t.Errorf("DeadlineSecs = %d, want nil after the clear", *ov.DeadlineSecs)
	}
}

func TestGetTurnBudgetOverride_IgnoresNonNumericValue(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// A row that is not an integer is corruption rather than a value to act on.
	// Reading it as "no override" is the only answer that cannot make a turn run
	// a budget the console never showed.
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO app_settings (key, value) VALUES (?, ?)`, KeyChatMaxSteps, "sixty"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	ov, err := s.GetTurnBudgetOverride(ctx)
	if err != nil {
		t.Fatalf("GetTurnBudgetOverride: %v", err)
	}
	if ov.MaxSteps != nil {
		t.Errorf("MaxSteps = %d, want nil for a non-numeric row", *ov.MaxSteps)
	}
}
