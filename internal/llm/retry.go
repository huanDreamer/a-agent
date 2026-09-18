package llm

import (
	"context"
	"errors"
	"io"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/retry"
)

// Option customises the adapter New returns.
//
// Options are separate from the Provider on purpose: a Provider describes *where*
// to send a request (endpoint, credentials, model name) and is data that travels
// from config and the database, while an Option describes *how the adapter
// behaves* — how it retries, and where it logs. Only the process that owns a
// logger can answer the second question.
type Option func(*adapterOptions)

// adapterOptions is what the Options accumulate into.
type adapterOptions struct {
	retry  retry.Policy
	logger *zap.Logger
}

// WithRetry makes every call on the returned model retry transient failures with
// exponential backoff. A policy whose Attempts() is 1 disables it, so passing the
// zero value is exactly "no retrying" — the behaviour of a deployment that never
// configured this.
//
// A nil logger is replaced by zap.NewNop: retrying is useful without logging,
// and requiring a logger would make the option unusable from a test.
func WithRetry(policy retry.Policy, logger *zap.Logger) Option {
	return func(o *adapterOptions) {
		o.retry = policy
		o.logger = logger
	}
}

// applyOptions folds the options into the adapter's behaviour.
func applyOptions(opts []Option) adapterOptions {
	var out adapterOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&out)
		}
	}
	if out.retry.Attempts() > 1 && out.logger == nil {
		out.logger = zap.NewNop()
	}
	return out
}

// retryingModel retries one model call when it fails for a reason that another
// attempt could survive.
//
// It is a wrapper rather than something the HTTP adapter does inline so that the
// retry decision is testable without a server, and so that every caller of
// llm.New — the web chat, the Feishu bot, the CLI REPL, the condenser and the
// title generator — gets it by construction instead of one call site at a time.
type retryingModel struct {
	inner  model.BaseChatModel
	policy retry.Policy
	logger *zap.Logger
}

// withRetry wraps inner, or returns it untouched when the policy allows a single
// attempt.
func withRetry(inner model.BaseChatModel, opts adapterOptions) model.BaseChatModel {
	if inner == nil || opts.retry.Attempts() <= 1 {
		return inner
	}
	logger := opts.logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &retryingModel{inner: inner, policy: opts.retry, logger: logger}
}

// Name forwards the inner model's name so tracing and logging still attribute a
// call to a specific model through the wrapper.
func (m *retryingModel) Name() string {
	if named, ok := m.inner.(interface{ Name() string }); ok {
		return named.Name()
	}
	return ""
}

// Provider forwards the inner model's provider name, for the same reason as Name.
func (m *retryingModel) Provider() string {
	if named, ok := m.inner.(interface{ Provider() string }); ok {
		return named.Provider()
	}
	return ""
}

// WithTools binds tools on the inner model and keeps the retrying around it.
//
// It is defined unconditionally — even for an inner model that cannot call tools
// — because the alternative is a wrapper that silently drops the interface: the
// chat runner type-asserts model.ToolCallingChatModel to bind tools, and a
// wrapped model that no longer satisfies it would quietly run without tools.
// A refusal here is reported as an error, which is what the runner expects.
func (m *retryingModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	tcm, ok := m.inner.(model.ToolCallingChatModel)
	if !ok {
		return nil, errors.New("llm: the underlying model does not support tool calling")
	}
	bound, err := tcm.WithTools(tools)
	if err != nil {
		return nil, err
	}
	cp := *m
	cp.inner = bound
	return &cp, nil
}

// Generate runs one completion, retrying it while the failure looks transient.
func (m *retryingModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	var out *schema.Message
	err := m.policy.Do(ctx, retry.Op{
		Fn: func(ctx context.Context) error {
			msg, err := m.inner.Generate(ctx, msgs, opts...)
			if err != nil {
				return err
			}
			out = msg
			return nil
		},
		Retryable: IsRetryable,
		OnRetry:   m.retryLogger("generate"),
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Stream runs one streamed completion, retrying while the failure happens before
// anything has been handed to the caller.
//
// "Before anything has been handed over" is the whole rule: the first chunk is
// pulled here, inside the retry, so a provider that drops the connection while
// opening the stream is retried, while a stream that dies *after* the model
// started answering is passed through untouched. Replaying the latter would make
// the reader see the beginning of the answer twice — that case belongs to the
// caller, which knows how to tell its client to discard a partial step (see
// internal/chat's step retry).
//
// The cost of pulling the first chunk here is that Stream blocks until the model
// produces it instead of returning immediately. Every caller in this repository
// reads the stream at once, and a stream nobody has read from yet is not a state
// worth preserving.
func (m *retryingModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	var (
		reader *schema.StreamReader[*schema.Message]
		first  *schema.Message
		done   bool
	)
	err := m.policy.Do(ctx, retry.Op{
		Fn: func(ctx context.Context) error {
			sr, err := m.inner.Stream(ctx, msgs, opts...)
			if err != nil {
				return err
			}
			msg, rerr := sr.Recv()
			switch {
			case rerr == nil:
				reader, first, done = sr, msg, false
				return nil
			case errors.Is(rerr, io.EOF):
				// An empty stream is not a failure: the model answered with
				// nothing, and the loop treats an empty message as an empty
				// answer. Retrying would buy the same nothing a second time.
				reader, first, done = sr, nil, true
				return nil
			default:
				// Closing before the retry matters: the failed attempt holds an
				// HTTP response body and a goroutine of its own.
				sr.Close()
				return rerr
			}
		},
		Retryable: IsRetryable,
		OnRetry:   m.retryLogger("stream"),
	})
	if err != nil {
		return nil, err
	}
	return replayFirst(reader, first, done), nil
}

// replayFirst returns a stream that yields the already-read first chunk and then
// the rest of the inner stream.
func replayFirst(inner *schema.StreamReader[*schema.Message], first *schema.Message, empty bool) *schema.StreamReader[*schema.Message] {
	if empty {
		return inner
	}
	out, w := newMessagePipe(16)
	go func() {
		defer w.Close()
		defer inner.Close()
		if first != nil {
			if closed := w.Send(first, nil); closed {
				return
			}
		}
		for {
			msg, err := inner.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					_ = w.Send(nil, err)
				}
				return
			}
			if closed := w.Send(msg, nil); closed {
				return
			}
		}
	}()
	return out
}

// retryLogger renders the one warn line a retry writes.
//
// It is a warn and not an info because the reader of it is someone explaining a
// turn that took three times as long as it should have, and the retried call is
// invisible everywhere else: an attempt that succeeded on retry leaves no other
// trace.
func (m *retryingModel) retryLogger(op string) func(retry.Info) {
	return func(info retry.Info) {
		m.logger.Warn("llm: call failed, retrying",
			zap.String("op", op),
			zap.String("provider", m.Provider()),
			zap.String("model", m.Name()),
			zap.Int("attempt", info.Attempt),
			zap.Int("max_attempts", info.MaxAttempts),
			zap.Duration("delay", info.Delay),
			zap.Error(info.Err),
		)
	}
}

// IsRetryable reports whether a failed call is worth another attempt.
//
// The rule is about what the failure *means*, not about whether it is an error:
// a 429 or a 502 is the provider asking for another try in a moment, while a 400
// or a 401 will fail identically no matter how many times it is paid for. Status
// codes are read off *LLMError, which every failure this package returns is
// wrapped in; anything that is not (a plain transport error from the SDK, a
// malformed response) is treated as transient, because that is what it looks
// like from here.
//
// It is exported because the chat runner asks the same question one level up,
// through the Retryable() method *LLMError carries. Sharing the predicate keeps
// the two answers from drifting apart.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	// A deliberate local decision, never a transient failure: the user pressed
	// stop, or the process is shutting down.
	if errors.Is(err, context.Canceled) {
		return false
	}
	var llmErr *LLMError
	if !errors.As(err, &llmErr) {
		return true
	}
	switch llmErr.StatusCode {
	case 0:
		// No HTTP status: a connection that never completed, or a response this
		// adapter could not read. Both are the shape of a blip.
		return true
	case 408, 409, 425, 429:
		return true
	}
	return llmErr.StatusCode >= 500
}

// Retryable reports whether another attempt could survive this failure. It exists
// so a caller one layer up — internal/chat's step retry — can make the same
// decision without importing this package: it asks through the small interface
// `interface{ Retryable() bool }` instead.
func (e *LLMError) Retryable() bool { return IsRetryable(e) }
