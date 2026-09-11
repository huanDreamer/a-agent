package feishu

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Defaults applied by RetryPolicy when a field is left at its zero value.
const (
	// DefaultMaxAttempts is the total attempt count of DefaultRetryPolicy.
	DefaultMaxAttempts = 3
	// DefaultBaseDelay is the delay before the second send attempt.
	DefaultBaseDelay = 300 * time.Millisecond
	// DefaultMaxDelay caps the computed exponential backoff.
	DefaultMaxDelay = 5 * time.Second
	// DefaultMultiplier is the exponential growth factor between attempts.
	DefaultMultiplier = 2.0
)

// jitterFraction is the share of a delay added or removed when RetryPolicy.Jitter
// is enabled (+/-25%).
const jitterFraction = 0.25

// RetryPolicy configures exponential backoff retry for outbound sends.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts including the first.
	// <= 0 disables retrying (single attempt).
	MaxAttempts int
	// BaseDelay is the delay before the second attempt. <= 0 uses DefaultBaseDelay.
	BaseDelay time.Duration
	// MaxDelay caps the computed backoff. <= 0 uses DefaultMaxDelay.
	MaxDelay time.Duration
	// Multiplier grows the delay each attempt. <= 1 uses 2.0.
	Multiplier float64
	// Jitter, when true, adds +/-25% randomisation to each delay.
	Jitter bool
}

// DefaultRetryPolicy returns a sensible policy: 3 attempts, 300ms base,
// 5s cap, 2x growth, jitter enabled.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: DefaultMaxAttempts,
		BaseDelay:   DefaultBaseDelay,
		MaxDelay:    DefaultMaxDelay,
		Multiplier:  DefaultMultiplier,
		Jitter:      true,
	}
}

// WithRetry wraps send so transient failures are retried with exponential
// backoff. Retries stop early when ctx is done. NonRetryable(err) == true
// aborts immediately. Returns the last error when all attempts fail.
//
// A nil logger is replaced by zap.NewNop. Every retry is logged at Warn level
// with the attempt number and the delay that is about to be slept.
func WithRetry(send SendMessage, p RetryPolicy, logger *zap.Logger) SendMessage {
	if send == nil {
		return send
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	// Normalise the policy once: a zero value is usable, and MaxAttempts <= 0
	// degenerates to a single attempt (no retrying).
	attempts := p.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	base := p.BaseDelay
	if base <= 0 {
		base = DefaultBaseDelay
	}
	maxDelay := p.MaxDelay
	if maxDelay <= 0 {
		maxDelay = DefaultMaxDelay
	}
	if maxDelay < base {
		maxDelay = base
	}
	multiplier := p.Multiplier
	if multiplier <= 1 {
		multiplier = DefaultMultiplier
	}
	jitter := p.Jitter

	return func(ctx context.Context, receiveID, receiveIDType, msgType, content string) error {
		var lastErr error
		// The first attempt always happens, even for an already-cancelled ctx.
		for attempt := 1; attempt <= attempts; attempt++ {
			lastErr = send(ctx, receiveID, receiveIDType, msgType, content)
			if lastErr == nil {
				return nil
			}
			// Cancellation aborts before any sleep or further attempt. Both the
			// context error and the send error stay matchable with errors.Is.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("feishu: send message aborted after %d attempt(s): %w: %w",
					attempt, ctxErr, lastErr)
			}
			// Permanent failures (bad token, no permission, invalid target...)
			// never succeed on retry, so fail fast.
			if NonRetryable(lastErr) {
				return fmt.Errorf("feishu: send message failed with non-retryable error: %w", lastErr)
			}
			if attempt == attempts {
				break
			}

			delay := retryBackoffDelay(attempt, base, maxDelay, multiplier, jitter)
			logger.Warn("feishu: send message failed, retrying",
				zap.Int("attempt", attempt),
				zap.Int("max_attempts", attempts),
				zap.Duration("delay", delay),
				zap.Error(lastErr),
			)
			if err := retrySleep(ctx, delay); err != nil {
				return fmt.Errorf("feishu: send message aborted after %d attempt(s): %w: %w",
					attempt, err, lastErr)
			}
		}
		return fmt.Errorf("feishu: send message failed after %d attempt(s): %w", attempts, lastErr)
	}
}

// retryBackoffDelay computes the delay before the given attempt (attempt 1 is
// the first retry, so it sleeps base). The result never exceeds maxDelay and,
// with jitter, is base-scaled by a random factor in [0.75, 1.25].
func retryBackoffDelay(attempt int, base, maxDelay time.Duration, multiplier float64, jitter bool) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := float64(base)
	for i := 1; i < attempt; i++ {
		d *= multiplier
		if d >= float64(maxDelay) {
			break
		}
	}
	if limit := float64(maxDelay); d > limit {
		d = limit
	}
	if jitter {
		d *= 1 + (rand.Float64()*2-1)*jitterFraction
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(d)
}

// retrySleep waits for d or until ctx is done, whichever happens first. It
// returns ctx.Err() when the context ends during the wait.
func retrySleep(ctx context.Context, d time.Duration) error {
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

// permanentFeishuCodes are well-known Feishu error codes that can never succeed
// on a retry: authentication, permission, invalid-request and invalid-target
// failures. Server-side (5xx-style) and unknown codes are treated as transient.
var permanentFeishuCodes = map[int]bool{
	99991663: true, // no permission to send to the target chat
	99991661: true, // invalid or expired access token
	99991400: true, // invalid request
	230001:   true, // invalid receive id
	230002:   true, // invalid message content
	230003:   true, // invalid message type
}

// feishuErrCodePattern matches the "code=<n>" marker that the sender embeds in
// permanent failures, e.g. fmt.Errorf("feishu: send message: code=%d msg=%s").
// A ": " separator is accepted too so HTTP-style codes are picked up.
var feishuErrCodePattern = regexp.MustCompile(`code\s*[=:]\s*(-?\d+)`)

// feishuErrCode extracts the first Feishu error code found in err (or in any
// error it wraps, since the text of the whole chain is searched).
func feishuErrCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	m := feishuErrCodePattern.FindStringSubmatch(err.Error())
	if m == nil {
		return 0, false
	}
	code, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return 0, false
	}
	return code, true
}

// NonRetryable reports whether an error should never be retried. The default
// implementation (see WithRetry) treats permanent Feishu errors as
// non-retryable and everything else as retryable. It is a package var so
// tests and callers can override it.
//
// Rule: an error is permanent when it is context.Canceled, when it carries a
// well-known permanent code from permanentFeishuCodes (no permission, invalid
// token, invalid request, invalid receive id / content / msg type), or when it
// carries a 4xxxx-style client-error code. Codes in the 5xx-style server range,
// unknown codes and errors without any "code=" marker stay retryable.
var NonRetryable = func(err error) bool {
	if err == nil {
		return false
	}
	// Cancellation is a deliberate local decision, never a transient failure.
	if errors.Is(err, context.Canceled) {
		return true
	}
	code, ok := feishuErrCode(err)
	if !ok {
		return false
	}
	if permanentFeishuCodes[code] {
		return true
	}
	// 4xxxx-style client errors (bad arguments, auth, permission).
	return code >= 40000 && code <= 49999
}

// Limiter is a token-bucket rate limiter. Safe for concurrent use. The zero
// value is unusable; construct one with NewLimiter. A nil *Limiter is
// unlimited.
type Limiter struct {
	mu        sync.Mutex
	rate      float64 // tokens replenished per second
	burst     float64 // bucket capacity, in tokens
	tokens    float64 // tokens currently available
	last      time.Time
	unlimited bool
}

// NewLimiter returns a limiter allowing r tokens per second with a burst of b.
// r <= 0 means unlimited (Wait returns immediately); b < 1 is treated as 1.
// A non-finite rate is also treated as unlimited.
func NewLimiter(r float64, b int) *Limiter {
	if b < 1 {
		b = 1
	}
	l := &Limiter{burst: float64(b), tokens: float64(b), last: time.Now()}
	if !(r > 0) { // catches <= 0 and NaN
		l.unlimited = true
		return l
	}
	l.rate = r
	return l
}

// take tries to consume one token without blocking. It reports whether a token
// was taken and, when not, how long the caller should wait for one.
func (l *Limiter) take() (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.unlimited {
		return true, 0
	}
	l.refill(time.Now())
	if l.tokens >= 1 {
		l.tokens--
		return true, 0
	}
	needed := (1 - l.tokens) / l.rate
	return false, time.Duration(needed * float64(time.Second))
}

// refill adds the tokens accrued since the last refill, capped at burst.
// The caller must hold l.mu.
func (l *Limiter) refill(now time.Time) {
	if l.last.IsZero() {
		l.last = now
		return
	}
	elapsed := now.Sub(l.last).Seconds()
	if elapsed <= 0 {
		// Clock went backwards (or no measurable time passed): keep the
		// accumulated tokens and leave the reference point alone.
		return
	}
	l.tokens += elapsed * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = now
}

// Wait blocks until a token is available or ctx is done. Returns ctx.Err()
// when the context ends first. A nil *Limiter is unlimited.
func (l *Limiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l == nil {
		return nil
	}
	for {
		ok, wait := l.take()
		if ok {
			return nil
		}
		if wait <= 0 {
			// Guard against a zero/negative sleep turning into a busy loop.
			wait = time.Millisecond
		}
		// Never busy-spin: sleep for exactly the missing token time, and bail
		// out as soon as the context ends.
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Allow reports whether a token was available without blocking.
func (l *Limiter) Allow() bool {
	if l == nil {
		return true
	}
	ok, _ := l.take()
	return ok
}

// WithRateLimit wraps send so each call first waits for a limiter token.
// A nil limiter is unlimited, so the call proceeds immediately.
func WithRateLimit(send SendMessage, l *Limiter) SendMessage {
	if send == nil {
		return send
	}
	return func(ctx context.Context, receiveID, receiveIDType, msgType, content string) error {
		if err := l.Wait(ctx); err != nil {
			return err
		}
		return send(ctx, receiveID, receiveIDType, msgType, content)
	}
}

// WithSendTimeout wraps send so each attempt is bounded by d. d <= 0 returns
// send unchanged. When the deadline fires, the returned error wraps both
// context.DeadlineExceeded and the error reported by the inner send, so
// errors.Is works for either.
func WithSendTimeout(send SendMessage, d time.Duration) SendMessage {
	if send == nil || d <= 0 {
		return send
	}
	return func(ctx context.Context, receiveID, receiveIDType, msgType, content string) error {
		sctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()

		err := send(sctx, receiveID, receiveIDType, msgType, content)
		if err == nil {
			return nil
		}
		// Attribute the failure to the context when the deadline (or a parent
		// cancellation) won the race, keeping the inner error matchable too.
		ctxErr := sctx.Err()
		if ctxErr == nil {
			return err
		}
		full := err
		if !errors.Is(err, ctxErr) {
			full = fmt.Errorf("%w: %w", ctxErr, err)
		}
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return fmt.Errorf("feishu: send message timed out after %s: %w", d, full)
		}
		return fmt.Errorf("feishu: send message aborted: %w", full)
	}
}
