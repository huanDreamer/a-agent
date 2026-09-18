// Package retry holds the exponential-backoff policy shared by everything in a
// long agent turn that can lose a call to a transient failure: one model
// invocation (internal/llm) and one step of the ReAct loop (internal/chat).
//
// It exists as its own leaf package — no HTTP, no eino, no logger — for the same
// reason internal/chat keeps its Tracer behind an interface: the timing rule is
// the part worth testing on its own, and the callers disagree about everything
// else (what is retryable, what to tell the client, how to log it).
//
// It is deliberately close to internal/platform/feishu's RetryPolicy, which
// solves the same problem for outbound Feishu sends. They are not merged yet:
// that one is bound to a SendMessage-shaped decorator and to Feishu error codes,
// and rewriting a tested, deployed path is not part of this change.
package retry

import (
	"context"
	"math/rand"
	"time"
)

// Defaults applied to a policy field left at its zero value.
const (
	// DefaultMaxAttempts is the total attempt count (first try plus retries) a
	// Default policy allows. Three is the point where a transient failure has
	// usually cleared, and the point where a rate limit has actually had time to.
	DefaultMaxAttempts = 3
	// DefaultBaseDelay is how long the first retry waits. It is long enough for
	// a dropped connection and short enough that nobody notices on a fast
	// recovery — the delay only becomes visible from the second retry on.
	DefaultBaseDelay = 800 * time.Millisecond
	// DefaultMaxDelay caps the computed backoff. A caller waiting half an hour
	// for the fourth attempt is worse off than one told the call failed.
	DefaultMaxDelay = 30 * time.Second
	// DefaultMultiplier is the growth factor between attempts.
	DefaultMultiplier = 2.0
)

// jitterFraction is the share of a delay added or removed when Jitter is on
// (+/-25%), matching internal/platform/feishu's policy.
const jitterFraction = 0.25

// Policy describes a bounded exponential backoff.
//
// The zero value is usable and means "no retrying": every field falls back to
// its default, and a MaxAttempts of 0 is one attempt. A caller that wants
// retries says so explicitly (Default, or a config-derived policy), which is
// what keeps an unconfigured deployment behaving exactly as it did before
// retrying existed.
type Policy struct {
	// MaxAttempts is the total number of attempts *including* the first.
	// <= 0 means one attempt, i.e. no retrying.
	MaxAttempts int
	// BaseDelay is how long the first retry waits. <= 0 uses DefaultBaseDelay.
	BaseDelay time.Duration
	// MaxDelay caps the computed backoff. <= 0 uses DefaultMaxDelay; below
	// BaseDelay it is raised to it, because a cap that shrinks the first retry
	// would make the backoff get *shorter* as the call keeps failing.
	MaxDelay time.Duration
	// Multiplier grows the delay before each further attempt. <= 1 uses
	// DefaultMultiplier.
	Multiplier float64
	// Jitter, when true, randomises each delay by +/-25%. Without it, several
	// turns that failed on the same provider outage retry in lockstep and hit it
	// again together.
	Jitter bool
}

// Default returns the policy a deployment gets when it configures retrying but
// no numbers: 3 attempts, 800ms base, 30s cap, 2x growth, jitter on.
func Default() Policy {
	return Policy{
		MaxAttempts: DefaultMaxAttempts,
		BaseDelay:   DefaultBaseDelay,
		MaxDelay:    DefaultMaxDelay,
		Multiplier:  DefaultMultiplier,
		Jitter:      true,
	}
}

// normalized resolves every zero field to its default.
func (p Policy) normalized() Policy {
	out := p
	if out.BaseDelay <= 0 {
		out.BaseDelay = DefaultBaseDelay
	}
	if out.MaxDelay <= 0 {
		out.MaxDelay = DefaultMaxDelay
	}
	if out.MaxDelay < out.BaseDelay {
		out.MaxDelay = out.BaseDelay
	}
	if out.Multiplier <= 1 {
		out.Multiplier = DefaultMultiplier
	}
	return out
}

// Attempts is how many times a call governed by this policy may run in total.
func (p Policy) Attempts() int {
	if p.MaxAttempts <= 1 {
		return 1
	}
	return p.MaxAttempts
}

// Retries is how many times the call may be retried (Attempts minus the first).
func (p Policy) Retries() int { return p.Attempts() - 1 }

// Delay is how long to wait before the retry numbered n (n = 1 is the first
// retry, so it waits BaseDelay). It never exceeds MaxDelay.
//
// Jitter is applied before the cap, so MaxDelay is a real ceiling: a caller that
// caps a wait to make room for it inside a turn budget can rely on it. (The
// Feishu policy jitters after capping and can overshoot by 25%; here the
// remaining-budget arithmetic in the runner would be a guess if it did.)
func (p Policy) Delay(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	q := p.normalized()
	d := float64(q.BaseDelay)
	for i := 1; i < n; i++ {
		d *= q.Multiplier
		if d >= float64(q.MaxDelay) {
			break
		}
	}
	if q.Jitter {
		d *= 1 + (rand.Float64()*2-1)*jitterFraction
	}
	if d > float64(q.MaxDelay) {
		d = float64(q.MaxDelay)
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(d)
}

// Info describes one retry that is about to happen. It is handed to Op.OnRetry,
// which is where a caller logs it and where the runner tells the browser it is
// about to re-run a step.
type Info struct {
	// Attempt is the attempt number about to run: 2 for the first retry.
	Attempt int
	// MaxAttempts is the policy's total, so a reader can say "2/3".
	MaxAttempts int
	// Delay is how long the caller is about to wait.
	Delay time.Duration
	// Err is the failure that caused this retry.
	Err error
}

// Op is one retried operation.
type Op struct {
	// Fn is a single attempt. It runs again after each wait, with the same ctx.
	Fn func(ctx context.Context) error
	// Retryable decides whether err deserves another attempt. Nil means every
	// error does, which is only right for a caller whose errors are all
	// transport-shaped; an HTTP caller must supply one, or a 401 is bought
	// three times.
	Retryable func(error) bool
	// OnRetry runs before each wait. It must not block for long: it is on the
	// path between two attempts.
	OnRetry func(Info)
	// MaxDelay caps the wait for this call only (0 = the policy's cap). The
	// runner uses it to keep a retry inside the turn's remaining wall clock.
	MaxDelay time.Duration
}

// Do runs op.Fn until it succeeds, until the error is not retryable, until the
// policy's attempts run out, or until ctx is done — whichever comes first. It
// returns the last error.
//
// Cancellation is checked before every attempt and during every wait, so a
// stopped turn stops retrying immediately instead of sleeping out a backoff
// nobody is waiting for. The caller's own cancellation is never retried: the
// first check happens before the first attempt's retry decision, so an attempt
// that failed *because* the context ended returns that failure instead of
// starting another one.
func (p Policy) Do(ctx context.Context, op Op) error {
	if op.Fn == nil {
		return nil
	}
	in := op
	if in.Retryable == nil {
		in.Retryable = func(error) bool { return true }
	}

	total := p.Attempts()
	var last error
	for attempt := 1; attempt <= total; attempt++ {
		last = in.Fn(ctx)
		if last == nil {
			return nil
		}
		if attempt == total {
			break
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return last
		}
		if !in.Retryable(last) {
			return last
		}
		delay := p.Delay(attempt)
		if in.MaxDelay > 0 && delay > in.MaxDelay {
			// This call's own ceiling, applied after the policy's: unlike the
			// policy's MaxDelay (which is raised to BaseDelay so a misconfigured
			// cap cannot make the backoff shrink), this one is honoured exactly,
			// because it is what a caller computed from a real remaining budget.
			delay = in.MaxDelay
		}
		if in.OnRetry != nil {
			in.OnRetry(Info{
				Attempt:     attempt + 1,
				MaxAttempts: total,
				Delay:       delay,
				Err:         last,
			})
		}
		if err := Sleep(ctx, delay); err != nil {
			// The wait was cut short by the caller. The useful error is the one
			// that caused the retry, not the cancellation of a sleep.
			return last
		}
	}
	return last
}

// Sleep waits for d or until ctx is done, whichever is first. A non-positive d
// returns immediately (after still honouring an already-cancelled ctx).
func Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
