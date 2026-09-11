package feishu

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// The resilience decorators in resilience.go operate on the error-only
// SendMessage primitive. The create primitive additionally returns a message
// id, so these thin adapters reuse the exact same policy logic instead of
// duplicating the backoff maths: the id is captured by the closure that is
// handed to the decorator, and read back only when the call succeeds.

// WithRetryCreate retries the id-returning create primitive according to p.
func WithRetryCreate(create CreateMessageFn, p RetryPolicy, logger *zap.Logger) CreateMessageFn {
	if create == nil {
		return nil
	}
	return func(ctx context.Context, receiveID, receiveIDType, msgType, content string) (string, error) {
		var id string
		send := func(ctx context.Context, r1, r2, r3, r4 string) error {
			var err error
			id, err = create(ctx, r1, r2, r3, r4)
			return err
		}
		if err := WithRetry(send, p, logger)(ctx, receiveID, receiveIDType, msgType, content); err != nil {
			return "", err
		}
		return id, nil
	}
}

// WithRateLimitCreate waits for a limiter token before each create call.
func WithRateLimitCreate(create CreateMessageFn, l *Limiter) CreateMessageFn {
	if create == nil {
		return nil
	}
	if l == nil {
		return create
	}
	return func(ctx context.Context, receiveID, receiveIDType, msgType, content string) (string, error) {
		var id string
		send := func(ctx context.Context, r1, r2, r3, r4 string) error {
			var err error
			id, err = create(ctx, r1, r2, r3, r4)
			return err
		}
		if err := WithRateLimit(send, l)(ctx, receiveID, receiveIDType, msgType, content); err != nil {
			return "", err
		}
		return id, nil
	}
}

// WithCreateTimeout bounds each create call by d.
func WithCreateTimeout(create CreateMessageFn, d time.Duration) CreateMessageFn {
	if create == nil || d <= 0 {
		return create
	}
	return func(ctx context.Context, receiveID, receiveIDType, msgType, content string) (string, error) {
		var id string
		send := func(ctx context.Context, r1, r2, r3, r4 string) error {
			var err error
			id, err = create(ctx, r1, r2, r3, r4)
			return err
		}
		if err := WithSendTimeout(send, d)(ctx, receiveID, receiveIDType, msgType, content); err != nil {
			return "", err
		}
		return id, nil
	}
}

// WrapCreate composes the configured resilience layers around a create
// primitive. Ordering matters: the rate limiter runs first (outermost) so
// retries of one message still count against the shared budget, then retry,
// then the per-attempt timeout.
func WrapCreate(create CreateMessageFn, p RetryPolicy, l *Limiter, attemptTimeout time.Duration, logger *zap.Logger) CreateMessageFn {
	if create == nil {
		return nil
	}
	out := WithCreateTimeout(create, attemptTimeout)
	out = WithRetryCreate(out, p, logger)
	out = WithRateLimitCreate(out, l)
	return out
}
