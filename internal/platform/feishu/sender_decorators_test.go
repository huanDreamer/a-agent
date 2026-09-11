package feishu

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// countingCreate returns a CreateMessageFn that fails the first failN calls
// and then succeeds, recording the number of invocations.
func countingCreate(failN int32, err error) (CreateMessageFn, *int32) {
	var calls int32
	fn := func(_ context.Context, _, _, _, _ string) (string, error) {
		n := atomic.AddInt32(&calls, 1)
		if n <= failN {
			return "", err
		}
		return "om_ok", nil
	}
	return fn, &calls
}

// fastPolicy is a retry policy with millisecond delays to keep tests quick.
func fastPolicy(attempts int) RetryPolicy {
	return RetryPolicy{MaxAttempts: attempts, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

func TestWithRetryCreate_ReturnsIDAfterRetry(t *testing.T) {
	boom := errors.New("transient")
	create, calls := countingCreate(2, boom)

	wrapped := WithRetryCreate(create, fastPolicy(5), zap.NewNop())
	id, err := wrapped(context.Background(), "oc", "chat_id", "text", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "om_ok" {
		t.Errorf("id = %q, want om_ok", id)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("create called %d times, want 3 (2 failures + 1 success)", got)
	}
}

func TestWithRetryCreate_ExhaustsAttempts(t *testing.T) {
	boom := errors.New("always down")
	create, calls := countingCreate(100, boom)

	wrapped := WithRetryCreate(create, fastPolicy(3), zap.NewNop())
	_, err := wrapped(context.Background(), "oc", "chat_id", "text", "{}")
	if err == nil {
		t.Fatal("want an error after exhausting attempts")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap boom", err)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("create called %d times, want 3", got)
	}
}

func TestWithRetryCreate_NilPassthrough(t *testing.T) {
	if got := WithRetryCreate(nil, fastPolicy(3), nil); got != nil {
		t.Error("a nil create should stay nil")
	}
}

func TestWithRetryCreate_SingleAttemptPolicy(t *testing.T) {
	boom := errors.New("down")
	create, calls := countingCreate(100, boom)

	wrapped := WithRetryCreate(create, RetryPolicy{MaxAttempts: 1}, zap.NewNop())
	if _, err := wrapped(context.Background(), "oc", "chat_id", "text", "{}"); err == nil {
		t.Fatal("want an error")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("create called %d times, want exactly 1", got)
	}
}

func TestWithRateLimitCreate_ReturnsID(t *testing.T) {
	create, calls := countingCreate(0, nil)

	wrapped := WithRateLimitCreate(create, NewLimiter(1000, 5))
	id, err := wrapped(context.Background(), "oc", "chat_id", "text", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "om_ok" {
		t.Errorf("id = %q, want om_ok", id)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("create called %d times, want 1", got)
	}
}

func TestWithRateLimitCreate_NilLimiterPassthrough(t *testing.T) {
	create, calls := countingCreate(0, nil)
	wrapped := WithRateLimitCreate(create, nil)
	if _, err := wrapped(context.Background(), "oc", "chat_id", "text", "{}"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("create called %d times, want 1", got)
	}
}

func TestWithRateLimitCreate_NilCreate(t *testing.T) {
	if got := WithRateLimitCreate(nil, NewLimiter(1, 1)); got != nil {
		t.Error("a nil create should stay nil")
	}
}

func TestWithRateLimitCreate_PropagatesContextError(t *testing.T) {
	create, calls := countingCreate(0, nil)
	// A limiter with no tokens and a cancelled context must fail before send.
	l := NewLimiter(0.0001, 1)
	if !l.Allow() { // consume the only token
		t.Fatal("expected the first token to be available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	wrapped := WithRateLimitCreate(create, l)
	if _, err := wrapped(ctx, "oc", "chat_id", "text", "{}"); err == nil {
		t.Fatal("want an error from the cancelled context")
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("create called %d times, want 0", got)
	}
}

func TestWithCreateTimeout_Success(t *testing.T) {
	create, _ := countingCreate(0, nil)
	wrapped := WithCreateTimeout(create, time.Second)
	id, err := wrapped(context.Background(), "oc", "chat_id", "text", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "om_ok" {
		t.Errorf("id = %q, want om_ok", id)
	}
}

func TestWithCreateTimeout_Expires(t *testing.T) {
	slow := func(ctx context.Context, _, _, _, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	wrapped := WithCreateTimeout(slow, 5*time.Millisecond)
	_, err := wrapped(context.Background(), "oc", "chat_id", "text", "{}")
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to match context.DeadlineExceeded", err)
	}
}

func TestWithCreateTimeout_Disabled(t *testing.T) {
	create, calls := countingCreate(0, nil)
	wrapped := WithCreateTimeout(create, 0)
	if _, err := wrapped(context.Background(), "oc", "chat_id", "text", "{}"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("create called %d times, want 1", got)
	}
}

func TestWithCreateTimeout_NilCreate(t *testing.T) {
	if got := WithCreateTimeout(nil, time.Second); got != nil {
		t.Error("a nil create should stay nil")
	}
}

func TestWrapCreate_ComposesLayers(t *testing.T) {
	boom := errors.New("transient")
	create, calls := countingCreate(1, boom)

	wrapped := WrapCreate(create, fastPolicy(4), NewLimiter(1000, 5), time.Second, zap.NewNop())
	id, err := wrapped(context.Background(), "oc", "chat_id", "text", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "om_ok" {
		t.Errorf("id = %q, want om_ok (the id must survive the decorators)", id)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("create called %d times, want 2 (1 failure + 1 success)", got)
	}
}

func TestWrapCreate_NilCreate(t *testing.T) {
	if got := WrapCreate(nil, fastPolicy(3), nil, 0, nil); got != nil {
		t.Error("a nil create should stay nil")
	}
}

func TestWrapCreate_RetriesWithinRateLimit(t *testing.T) {
	// A fail-then-succeed create with a limiter that has exactly enough
	// burst: the retry must not deadlock waiting for tokens.
	boom := errors.New("transient")
	create, calls := countingCreate(1, boom)

	wrapped := WrapCreate(create, fastPolicy(3), NewLimiter(1000, 10), time.Second, zap.NewNop())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	id, err := wrapped(ctx, "oc", "chat_id", "text", "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "om_ok" {
		t.Errorf("id = %q, want om_ok", id)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("create called %d times, want 2", got)
	}
}
