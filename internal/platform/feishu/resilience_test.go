package feishu

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// retryCounter is an instrumented SendMessage: it records how many times it was
// called, returns errs[n-1] for the n-th call (nil once the list is exhausted,
// or fixed when no list is given), and runs onCall after each call.
type retryCounter struct {
	mu     sync.Mutex
	calls  int
	errs   []error
	fixed  error
	onCall func(n int)
}

func (r *retryCounter) send(_ context.Context, _, _, _, _ string) error {
	r.mu.Lock()
	r.calls++
	n := r.calls
	var err error
	if len(r.errs) > 0 {
		if n <= len(r.errs) {
			err = r.errs[n-1]
		}
	} else {
		err = r.fixed
	}
	hook := r.onCall
	r.mu.Unlock()

	if hook != nil {
		hook(n)
	}
	return err
}

func (r *retryCounter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// noJitter is a fast, deterministic policy: no jitter, millisecond delays.
func noJitter(maxAttempts int, base time.Duration) RetryPolicy {
	return RetryPolicy{
		MaxAttempts: maxAttempts,
		BaseDelay:   base,
		MaxDelay:    time.Second,
		Multiplier:  2,
		Jitter:      false,
	}
}

func TestDefaultRetryPolicy(t *testing.T) {
	// The documented default constants.
	if DefaultMaxAttempts != 3 || DefaultBaseDelay != 300*time.Millisecond ||
		DefaultMaxDelay != 5*time.Second || DefaultMultiplier != 2.0 {
		t.Errorf("default constants = %d, %s, %s, %v; want 3, 300ms, 5s, 2.0",
			DefaultMaxAttempts, DefaultBaseDelay, DefaultMaxDelay, DefaultMultiplier)
	}

	p := DefaultRetryPolicy()
	if p.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", p.MaxAttempts, DefaultMaxAttempts)
	}
	if p.BaseDelay != DefaultBaseDelay {
		t.Errorf("BaseDelay = %s, want %s", p.BaseDelay, DefaultBaseDelay)
	}
	if p.MaxDelay != DefaultMaxDelay {
		t.Errorf("MaxDelay = %s, want %s", p.MaxDelay, DefaultMaxDelay)
	}
	if p.Multiplier != DefaultMultiplier {
		t.Errorf("Multiplier = %v, want %v", p.Multiplier, DefaultMultiplier)
	}
	if !p.Jitter {
		t.Error("Jitter = false, want true")
	}
}

func TestRetryBackoffDelay(t *testing.T) {
	const base = 100 * time.Millisecond
	tests := []struct {
		name    string
		attempt int
		mult    float64
		max     time.Duration
		want    time.Duration
	}{
		{"first retry uses base", 1, 2, time.Second, base},
		{"second retry doubles", 2, 2, time.Second, 2 * base},
		{"third retry quadruples", 3, 2, time.Second, 4 * base},
		{"capped at max delay", 5, 2, 300 * time.Millisecond, 300 * time.Millisecond},
		{"multiplier of 3", 3, 3, time.Second, 9 * base},
		{"zero attempt treated as first", 0, 2, time.Second, base},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := retryBackoffDelay(tc.attempt, base, tc.max, tc.mult, false)
			if got != tc.want {
				t.Errorf("retryBackoffDelay = %s, want %s", got, tc.want)
			}
		})
	}

	t.Run("jitter stays within bounds", func(t *testing.T) {
		const d = 100 * time.Millisecond
		lo, hi := time.Duration(float64(d)*0.75), time.Duration(float64(d)*1.25)
		for i := 0; i < 200; i++ {
			got := retryBackoffDelay(1, d, time.Second, 2, true)
			if got < lo || got > hi {
				t.Fatalf("jittered delay = %s, want within [%s, %s]", got, lo, hi)
			}
		}
	})
}

func TestWithRetrySucceedsAfterTransientFailures(t *testing.T) {
	const base = 10 * time.Millisecond
	const margin = 3 * time.Millisecond

	tests := []struct {
		name         string
		failures     int
		wantCalls    int
		wantMinDelay time.Duration
	}{
		{"no failure", 0, 1, 0},
		{"one transient failure", 1, 2, base - margin},
		{"two transient failures", 2, 3, 3*base - margin},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := make([]error, 0, tc.failures)
			for i := 0; i < tc.failures; i++ {
				errs = append(errs, fmt.Errorf("transient %d", i+1))
			}
			rc := &retryCounter{errs: errs}
			send := WithRetry(rc.send, noJitter(4, base), nil)

			start := time.Now()
			err := send(context.Background(), "oc_1", "chat_id", "text", `{"text":"hi"}`)
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rc.count() != tc.wantCalls {
				t.Errorf("calls = %d, want %d", rc.count(), tc.wantCalls)
			}
			if elapsed < tc.wantMinDelay {
				t.Errorf("elapsed = %s, want >= %s (backoff was not applied)", elapsed, tc.wantMinDelay)
			}
		})
	}
}

func TestWithRetryGivesUpWithLastError(t *testing.T) {
	e1 := errors.New("first")
	e2 := errors.New("second")
	e3 := errors.New("third")
	rc := &retryCounter{errs: []error{e1, e2, e3}}
	send := WithRetry(rc.send, noJitter(3, time.Millisecond), nil)

	err := send(context.Background(), "oc_1", "chat_id", "text", "{}")
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if rc.count() != 3 {
		t.Errorf("calls = %d, want exactly 3", rc.count())
	}
	if !errors.Is(err, e3) {
		t.Errorf("errors.Is(err, last) = false; err = %v", err)
	}
	if errors.Is(err, e1) {
		t.Errorf("returned error should be the last one, got %v", err)
	}
}

func TestWithRetryDisabledPolicyCallsOnce(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name     string
		policy   RetryPolicy
		wantCall int
	}{
		{"zero policy", RetryPolicy{}, 1},
		{"negative max attempts", RetryPolicy{MaxAttempts: -3, BaseDelay: time.Millisecond}, 1},
		{"single attempt", RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc := &retryCounter{fixed: boom}
			send := WithRetry(rc.send, tc.policy, nil)

			start := time.Now()
			err := send(context.Background(), "oc_1", "chat_id", "text", "{}")
			elapsed := time.Since(start)

			if rc.count() != tc.wantCall {
				t.Errorf("calls = %d, want %d", rc.count(), tc.wantCall)
			}
			if !errors.Is(err, boom) {
				t.Errorf("errors.Is(err, boom) = false; err = %v", err)
			}
			if elapsed > 200*time.Millisecond {
				t.Errorf("elapsed = %s, want a single immediate attempt", elapsed)
			}
		})
	}
}

func TestWithRetryNonRetryableShortCircuits(t *testing.T) {
	orig := NonRetryable
	t.Cleanup(func() { NonRetryable = orig })

	boom := errors.New("permanent")
	rc := &retryCounter{fixed: boom}
	// Build the sender first: NonRetryable must be read per call, so a later
	// override still takes effect.
	send := WithRetry(rc.send, noJitter(5, 20*time.Millisecond), nil)

	NonRetryable = func(err error) bool { return errors.Is(err, boom) }

	start := time.Now()
	err := send(context.Background(), "oc_1", "chat_id", "text", "{}")
	elapsed := time.Since(start)

	if rc.count() != 1 {
		t.Errorf("calls = %d, want exactly 1 (no retry for non-retryable errors)", rc.count())
	}
	if !errors.Is(err, boom) {
		t.Errorf("errors.Is(err, boom) = false; err = %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("elapsed = %s, want no backoff sleep", elapsed)
	}
}

func TestWithRetryAbortsOnContextCancellation(t *testing.T) {
	tests := []struct {
		name     string
		cancelAt int
	}{
		{"cancelled after first attempt", 1},
		{"cancelled after second attempt", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			boom := errors.New("boom")
			rc := &retryCounter{fixed: boom, onCall: func(n int) {
				if n == tc.cancelAt {
					cancel()
				}
			}}
			send := WithRetry(rc.send, noJitter(10, 20*time.Millisecond), nil)

			start := time.Now()
			err := send(ctx, "oc_1", "chat_id", "text", "{}")
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("expected an error")
			}
			if got := rc.count(); got > tc.cancelAt {
				t.Errorf("calls = %d, want at most %d (retry continued after cancel)", got, tc.cancelAt)
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
			}
			if !errors.Is(err, boom) {
				t.Errorf("errors.Is(err, boom) = false; err = %v", err)
			}
			// 10 attempts at 20ms base would need hundreds of ms; aborting must
			// return promptly instead.
			if elapsed > 200*time.Millisecond {
				t.Errorf("elapsed = %s, want prompt return after cancellation", elapsed)
			}
		})
	}
}

func TestNonRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"plain error", errors.New("boom"), false},
		{"no permission 99991663", fmt.Errorf("feishu: send message: code=%d msg=no permission", 99991663), true},
		{"invalid token 99991661", fmt.Errorf("feishu: send message: code=%d msg=invalid token", 99991661), true},
		{"invalid request 99991400", fmt.Errorf("feishu: send message: code=%d msg=invalid request", 99991400), true},
		{"invalid receive id 230001", fmt.Errorf("feishu: send message: code=%d msg=bad receive id", 230001), true},
		{"invalid content 230002", fmt.Errorf("feishu: send message: code=%d msg=bad content", 230002), true},
		{"invalid msg type 230003", fmt.Errorf("feishu: send message: code=%d msg=bad type", 230003), true},
		{"client error range 40001", fmt.Errorf("feishu: send message: code=%d msg=param error", 40001), true},
		{"wrapped permanent error", fmt.Errorf("outer: %w", fmt.Errorf("feishu: send message: code=%d msg=nope", 99991663)), true},
		{"server error 500", fmt.Errorf("feishu: send message: code=%d msg=internal error", 500), false},
		{"server error 50000", fmt.Errorf("feishu: send message: code=%d msg=internal error", 50000), false},
		{"unknown high code", fmt.Errorf("feishu: send message: code=%d msg=???", 99991664), false},
		{"context canceled", context.Canceled, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NonRetryable(tc.err); got != tc.want {
				t.Errorf("NonRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}

	t.Run("deadline exceeded is retryable", func(t *testing.T) {
		if NonRetryable(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)) {
			t.Error("context.DeadlineExceeded should stay retryable")
		}
	})
}

func TestLimiterAllowRespectsBurst(t *testing.T) {
	l := NewLimiter(1, 3) // 1 token/s, burst 3: no meaningful refill during the test
	for i := 0; i < 3; i++ {
		if !l.Allow() {
			t.Fatalf("Allow() call %d = false, want true (within burst)", i+1)
		}
	}
	if l.Allow() {
		t.Error("Allow() after burst exhausted = true, want false")
	}

	t.Run("nil limiter is unlimited", func(t *testing.T) {
		var nilLimiter *Limiter
		if !nilLimiter.Allow() {
			t.Error("nil limiter Allow() = false, want true")
		}
		if err := nilLimiter.Wait(context.Background()); err != nil {
			t.Errorf("nil limiter Wait() = %v, want nil", err)
		}
	})

	t.Run("concurrent Allow never exceeds burst", func(t *testing.T) {
		cl := NewLimiter(1, 3)
		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			allow int
		)
		for i := 0; i < 25; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if cl.Allow() {
					mu.Lock()
					allow++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if allow != 3 {
			t.Errorf("concurrent Allow successes = %d, want exactly the burst of 3", allow)
		}
	})
}

func TestLimiterWaitBlocksUntilTokenFree(t *testing.T) {
	// 50 tokens/s => a refill takes ~20ms.
	l := NewLimiter(50, 1)
	send := WithRateLimit(func(_ context.Context, _, _, _, _ string) error { return nil }, l)

	start := time.Now()
	if err := send(context.Background(), "oc_1", "chat_id", "text", "{}"); err != nil {
		t.Fatalf("first send: %v", err)
	}
	first := time.Since(start)
	if first > 15*time.Millisecond {
		t.Errorf("first call took %s, want an immediate token from the burst", first)
	}

	start = time.Now()
	if err := send(context.Background(), "oc_1", "chat_id", "text", "{}"); err != nil {
		t.Fatalf("second send: %v", err)
	}
	second := time.Since(start)

	const want = 20 * time.Millisecond // 1 token / 50 tokens per second
	if second < want-5*time.Millisecond {
		t.Errorf("second call returned in %s, want >= %s (limiter did not wait)", second, want-5*time.Millisecond)
	}
	if second > time.Second {
		t.Errorf("second call took %s, want well under a second", second)
	}
}

func TestWithRateLimitPropagatesWaitError(t *testing.T) {
	l := NewLimiter(0.1, 1)
	rc := &retryCounter{}
	send := WithRateLimit(rc.send, l)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := send(ctx, "oc_1", "chat_id", "text", "{}")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}
	if rc.count() != 0 {
		t.Errorf("inner send calls = %d, want 0 when the limiter wait fails", rc.count())
	}
}

func TestLimiterWaitCanceled(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		l := NewLimiter(0.5, 1)
		if !l.Allow() {
			t.Fatal("expected the burst token to be available")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		err := l.Wait(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Wait() = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("Wait() took %s, want a prompt return", elapsed)
		}
	})

	t.Run("canceled while waiting", func(t *testing.T) {
		l := NewLimiter(0.5, 1) // 2s per token
		if !l.Allow() {
			t.Fatal("expected the burst token to be available")
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(5 * time.Millisecond)
			cancel()
		}()
		defer cancel()

		start := time.Now()
		err := l.Wait(ctx)
		elapsed := time.Since(start)

		if !errors.Is(err, context.Canceled) {
			t.Errorf("Wait() = %v, want context.Canceled", err)
		}
		if elapsed > 500*time.Millisecond {
			t.Errorf("Wait() took %s, want it to stop as soon as ctx is done", elapsed)
		}
	})
}

func TestLimiterUnlimited(t *testing.T) {
	tests := []struct {
		name string
		rate float64
	}{
		{"zero rate", 0},
		{"negative rate", -3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLimiter(tc.rate, 1)
			start := time.Now()
			for i := 0; i < 100; i++ {
				if err := l.Wait(context.Background()); err != nil {
					t.Fatalf("Wait() %d = %v, want nil", i, err)
				}
				if !l.Allow() {
					t.Fatalf("Allow() %d = false, want true for an unlimited limiter", i)
				}
			}
			if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
				t.Errorf("100 unlimited waits took %s, want no blocking", elapsed)
			}
		})
	}

	t.Run("burst below one is treated as one", func(t *testing.T) {
		l := NewLimiter(1, 0)
		if !l.Allow() {
			t.Error("Allow() = false, want true (burst normalised to 1)")
		}
		if l.Allow() {
			t.Error("Allow() = true after consuming the single token, want false")
		}
	})
}

func TestLimiterConcurrentWait(t *testing.T) {
	l := NewLimiter(1000, 2) // 1ms per token
	const workers = 20

	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = l.Wait(context.Background())
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: Wait() = %v, want nil", i, err)
		}
	}
	if elapsed > time.Second {
		t.Errorf("concurrent waits took %s, want well under a second", elapsed)
	}
}

func TestWithSendTimeoutExpires(t *testing.T) {
	tests := []struct {
		name string
		// inner blocks until ctx is done, then reports errFn(ctx.Err()).
		blocking func(ctx context.Context) error
	}{
		{
			name:     "returns ctx error",
			blocking: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		},
		{
			name: "returns a wrapped ctx error",
			blocking: func(ctx context.Context) error {
				<-ctx.Done()
				return fmt.Errorf("feishu: http round trip: %w", ctx.Err())
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inner := func(ctx context.Context, _, _, _, _ string) error { return tc.blocking(ctx) }
			send := WithSendTimeout(inner, 20*time.Millisecond)

			start := time.Now()
			err := send(context.Background(), "oc_1", "chat_id", "text", "{}")
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("expected a timeout error")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("errors.Is(err, context.DeadlineExceeded) = false; err = %v", err)
			}
			if !strings.Contains(err.Error(), "timed out") {
				t.Errorf("error %q should mention the timeout", err)
			}
			if elapsed > 500*time.Millisecond {
				t.Errorf("elapsed = %s, want the timeout to fire promptly", elapsed)
			}
		})
	}

	t.Run("slow inner error is attributed to the deadline", func(t *testing.T) {
		netErr := errors.New("net: connection reset")
		inner := func(_ context.Context, _, _, _, _ string) error {
			time.Sleep(30 * time.Millisecond) // ignores the deadline
			return netErr
		}
		send := WithSendTimeout(inner, 5*time.Millisecond)
		err := send(context.Background(), "oc_1", "chat_id", "text", "{}")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("errors.Is(err, context.DeadlineExceeded) = false; err = %v", err)
		}
		if !errors.Is(err, netErr) {
			t.Errorf("errors.Is(err, netErr) = false; err = %v", err)
		}
	})
}

func TestWithSendTimeoutPropagatesInnerError(t *testing.T) {
	boom := errors.New("boom")
	rc := &retryCounter{fixed: boom}
	send := WithSendTimeout(rc.send, 100*time.Millisecond)

	err := send(context.Background(), "oc_1", "chat_id", "text", "{}")
	if !errors.Is(err, boom) {
		t.Errorf("errors.Is(err, boom) = false; err = %v", err)
	}
	if rc.count() != 1 {
		t.Errorf("calls = %d, want 1", rc.count())
	}
}

func TestWithSendTimeoutDisabled(t *testing.T) {
	rc := &retryCounter{}
	inner := rc.send

	tests := []struct {
		name string
		d    time.Duration
	}{
		{"zero timeout", 0},
		{"negative timeout", -time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := WithSendTimeout(inner, tc.d)
			if reflect.ValueOf(got).Pointer() != reflect.ValueOf(inner).Pointer() {
				t.Fatal("WithSendTimeout should return send unchanged")
			}
			if err := got(context.Background(), "oc_1", "chat_id", "text", "{}"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	if rc.count() != 2 {
		t.Errorf("calls = %d, want 2", rc.count())
	}
}

func TestResilienceStackComposition(t *testing.T) {
	transient := errors.New("transient")
	rc := &retryCounter{errs: []error{transient}}

	limiter := NewLimiter(100, 1) // ~10ms per token
	send := WithRateLimit(
		WithRetry(
			WithSendTimeout(rc.send, 500*time.Millisecond),
			noJitter(3, 5*time.Millisecond),
			nil,
		),
		limiter,
	)

	if err := send(context.Background(), "oc_1", "chat_id", "text", `{"text":"hi"}`); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc.count() != 2 {
		t.Errorf("calls = %d, want 2 (one transient failure retried)", rc.count())
	}
}

func TestWithRetryNilSend(t *testing.T) {
	if got := WithRetry(nil, DefaultRetryPolicy(), nil); got != nil {
		t.Error("WithRetry(nil, ...) should return nil")
	}
	if got := WithRateLimit(nil, NewLimiter(1, 1)); got != nil {
		t.Error("WithRateLimit(nil, ...) should return nil")
	}
	if got := WithSendTimeout(nil, time.Second); got != nil {
		t.Error("WithSendTimeout(nil, ...) should return nil")
	}
}
