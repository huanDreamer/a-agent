package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPolicy_Attempts(t *testing.T) {
	cases := []struct {
		name string
		p    Policy
		want int
	}{
		{"zero value means one attempt", Policy{}, 1},
		{"negative means one attempt", Policy{MaxAttempts: -4}, 1},
		{"one stays one", Policy{MaxAttempts: 1}, 1},
		{"configured count", Policy{MaxAttempts: 4}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.Attempts(); got != tc.want {
				t.Fatalf("Attempts() = %d, want %d", got, tc.want)
			}
			if got, want := tc.p.Retries(), tc.want-1; got != want {
				t.Fatalf("Retries() = %d, want %d", got, want)
			}
		})
	}
}

func TestPolicy_Delay_GrowsAndIsCapped(t *testing.T) {
	p := Policy{MaxAttempts: 6, BaseDelay: time.Second, MaxDelay: 10 * time.Second, Multiplier: 2}
	want := []time.Duration{
		0,                // no retry, no wait
		time.Second,      // 1st retry
		2 * time.Second,  // 2nd
		4 * time.Second,  // 3rd
		8 * time.Second,  // 4th
		10 * time.Second, // 5th: capped, and it stays capped
		10 * time.Second,
	}
	for n, w := range want {
		if got := p.Delay(n); got != w {
			t.Fatalf("Delay(%d) = %s, want %s", n, got, w)
		}
	}
}

func TestPolicy_Delay_JitterStaysUnderTheCap(t *testing.T) {
	p := Policy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 4 * time.Second, Multiplier: 2, Jitter: true}
	// The cap is a real ceiling: the runner budgets the wait it is told about,
	// so a jittered delay that overshot it would make that arithmetic a lie.
	for i := 0; i < 200; i++ {
		if got := p.Delay(3); got > 4*time.Second {
			t.Fatalf("jittered Delay(3) = %s, exceeded MaxDelay", got)
		}
		if got := p.Delay(1); got <= 0 || got > 1250*time.Millisecond {
			t.Fatalf("jittered Delay(1) = %s, want (0, 1.25s]", got)
		}
	}
}

func TestPolicy_Delay_DefaultsAndGuards(t *testing.T) {
	// A cap below the base would make the backoff shrink as attempts pile up;
	// it is raised instead.
	p := Policy{BaseDelay: 2 * time.Second, MaxDelay: time.Second}
	if got := p.Delay(3); got != 2*time.Second {
		t.Fatalf("Delay with an undersized cap = %s, want the base delay", got)
	}
	// Multiplier <= 1 would turn "exponential" into "constant" or "shrinking".
	q := Policy{BaseDelay: time.Second, Multiplier: 0.5}
	if got := q.Delay(2); got < time.Second {
		t.Fatalf("Delay with multiplier 0.5 = %s, want at least the base delay", got)
	}
	// Zero-value policy still yields the documented default base.
	if got := (Policy{}).Delay(1); got != DefaultBaseDelay {
		t.Fatalf("zero-value Delay(1) = %s, want %s", got, DefaultBaseDelay)
	}
}

func TestPolicy_Do_SucceedsAfterRetries(t *testing.T) {
	p := Policy{MaxAttempts: 3, BaseDelay: time.Millisecond}
	calls := 0
	var retries []Info
	err := p.Do(context.Background(), Op{
		Fn: func(context.Context) error {
			calls++
			if calls < 3 {
				return errors.New("boom")
			}
			return nil
		},
		OnRetry: func(i Info) { retries = append(retries, i) },
	})
	if err != nil {
		t.Fatalf("Do() = %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("Fn ran %d times, want 3", calls)
	}
	if len(retries) != 2 {
		t.Fatalf("OnRetry ran %d times, want 2", len(retries))
	}
	if retries[0].Attempt != 2 || retries[1].Attempt != 3 {
		t.Fatalf("attempt numbers = %d, %d, want 2, 3", retries[0].Attempt, retries[1].Attempt)
	}
	if retries[0].MaxAttempts != 3 {
		t.Fatalf("MaxAttempts = %d, want 3", retries[0].MaxAttempts)
	}
	if retries[1].Delay <= retries[0].Delay {
		t.Fatalf("delays did not grow: %s then %s", retries[0].Delay, retries[1].Delay)
	}
}

func TestPolicy_Do_StopsOnNonRetryable(t *testing.T) {
	p := Policy{MaxAttempts: 5, BaseDelay: time.Millisecond}
	sentinel := errors.New("permanent")
	calls := 0
	err := p.Do(context.Background(), Op{
		Fn: func(context.Context) error { calls++; return sentinel },
		Retryable: func(err error) bool {
			return !errors.Is(err, sentinel)
		},
		OnRetry: func(Info) { t.Error("OnRetry ran for a non-retryable error") },
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Do() = %v, want the sentinel", err)
	}
	if calls != 1 {
		t.Fatalf("Fn ran %d times, want 1", calls)
	}
}

func TestPolicy_Do_ReturnsLastErrorWhenAttemptsRunOut(t *testing.T) {
	p := Policy{MaxAttempts: 3, BaseDelay: time.Millisecond}
	calls := 0
	err := p.Do(context.Background(), Op{
		Fn: func(context.Context) error { calls++; return errors.New("still down") },
	})
	if err == nil || err.Error() != "still down" {
		t.Fatalf("Do() = %v, want the last failure", err)
	}
	if calls != 3 {
		t.Fatalf("Fn ran %d times, want 3", calls)
	}
}

func TestPolicy_Do_CancellationStopsRetrying(t *testing.T) {
	// A cancelled turn must not sleep out a backoff nobody is waiting for.
	p := Policy{MaxAttempts: 5, BaseDelay: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := p.Do(ctx, Op{
		Fn: func(context.Context) error {
			calls++
			cancel() // the caller stopped while the call was failing
			return errors.New("interrupted")
		},
	})
	if calls != 1 {
		t.Fatalf("Fn ran %d times, want 1", calls)
	}
	if err == nil || err.Error() != "interrupted" {
		t.Fatalf("Do() = %v, want the failure that caused the retry", err)
	}
}

func TestPolicy_Do_MaxDelayCapsTheWaitExactly(t *testing.T) {
	// The per-call cap is honoured exactly, even below the base delay: it is
	// what the runner computes from the turn's remaining wall clock.
	p := Policy{MaxAttempts: 3, BaseDelay: time.Hour}
	start := time.Now()
	var waited time.Duration
	err := p.Do(context.Background(), Op{
		Fn:       func(context.Context) error { return errors.New("down") },
		MaxDelay: 5 * time.Millisecond,
		OnRetry:  func(i Info) { waited = i.Delay },
	})
	if err == nil {
		t.Fatal("Do() = nil, want a failure")
	}
	if waited != 5*time.Millisecond {
		t.Fatalf("reported delay = %s, want the capped 5ms", waited)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Do() waited %s, want the capped delay", elapsed)
	}
}

func TestSleep_HonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep on a cancelled ctx = %v, want context.Canceled", err)
	}
	if err := Sleep(context.Background(), 0); err != nil {
		t.Fatalf("Sleep(0) = %v, want nil", err)
	}
}
