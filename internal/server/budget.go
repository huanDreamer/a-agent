package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/store"
)

// Ceilings on what the console may set. They are deliberately not knobs: a
// number above one of these is a typo (an extra zero), and a typo that is
// accepted silently is a turn that runs for hours. config.yaml refuses a step
// count above chat.MaxStepsCeiling at startup for the same reason, and this is
// the same refusal on the console's side of the wire — a 400 the operator can
// read rather than a turn that never ends.
const (
	// MaxConsoleSteps is the largest step cap the console accepts. It matches
	// chat.MaxStepsCeiling, so the panel cannot ask for something the runner
	// would refuse to build.
	MaxConsoleSteps = chat.MaxStepsCeiling
	// MaxConsoleTurnTokens is the largest per-turn token budget the console
	// accepts. It is far above any real model window; it exists to stop 10^12.
	MaxConsoleTurnTokens = 20_000_000
	// MaxConsoleTurnDeadline is the longest per-turn wall-clock budget the
	// console accepts, and mirrors the `ask_user` ceiling in config.yaml.
	MaxConsoleTurnDeadline = 24 * time.Hour
)

// TurnBudgetStore is the slice of the store this file needs. Declaring it here
// rather than taking the whole store.Store keeps the dependency visible: the
// budget has no opinion about anything else the store holds.
type TurnBudgetStore interface {
	GetTurnBudgetOverride(ctx context.Context) (store.TurnBudgetOverride, error)
	SetTurnBudgetOverride(ctx context.Context, o store.TurnBudgetOverride) error
}

// turnBudgetStore returns the store as a budget store, or nil when the
// implementation does not provide the accessors (a test double, or a store
// built before this existed). A nil result means "config.yaml governs", which is
// exactly what the server did before the console could change anything.
func (s *Server) turnBudgetStore() TurnBudgetStore {
	bs, ok := s.store.(TurnBudgetStore)
	if !ok {
		return nil
	}
	return bs
}

// budgetSource names where one budget dimension's effective value came from.
const (
	// budgetFromConfig is the value in config.yaml (the startup default).
	budgetFromConfig = "config"
	// budgetFromConsole is an override written by 设置 → 对话预算.
	budgetFromConsole = "console"
)

// effectiveBudget resolves the per-turn budget for this moment: an override the
// console stored wins per field, and each field it never touched falls back to
// the startup config. Each field is resolved separately on purpose — setting
// only the step cap must not silently pin the other two to whatever they
// happened to be when it was saved.
//
// It is read per turn rather than captured once at startup, because that is what
// makes a change in the console apply to the very next message: no restart, and
// no second source of truth to fall out of step with what the panel shows.
func (s *Server) effectiveBudget(ctx context.Context) turnBudget {
	b := turnBudget{
		maxSteps:  s.cfg.DefaultChatMaxSteps,
		maxTokens: s.cfg.DefaultChatMaxTokens,
		deadline:  s.cfg.DefaultChatTurnDeadline,
	}
	if bs := s.turnBudgetStore(); bs != nil {
		ov, err := bs.GetTurnBudgetOverride(ctx)
		if err != nil {
			// A store failure must not stop a conversation: fall back to the
			// configured values, which is the same budget a deployment without
			// the console override would run with.
			s.logger.Warn("chat: read budget override failed, using configured values",
				zapError(err))
		} else {
			if ov.MaxSteps != nil {
				b.maxSteps = *ov.MaxSteps
			}
			if ov.MaxTokens != nil {
				b.maxTokens = *ov.MaxTokens
			}
			if ov.DeadlineSecs != nil {
				b.deadline = time.Duration(*ov.DeadlineSecs) * time.Second
			}
		}
	}
	// A zero step cap has no meaning to the runner — it would read as "use the
	// default" — so the console's 0 is normalised to the runner's default rather
	// than persisted as an unreadable value.
	if b.maxSteps <= 0 {
		b.maxSteps = chat.DefaultMaxSteps
	}
	return b
}

// turnBudget is the resolved budget for one turn, in the units each dimension is
// compared in.
type turnBudget struct {
	maxSteps  int
	maxTokens int
	deadline  time.Duration
}

// deadlineSeconds is the wall-clock dimension in the unit the API speaks.
func (b turnBudget) deadlineSeconds() int {
	return int(b.deadline / time.Second)
}

// consoleBudget is what /chat/budget answers: the effective values, where each
// came from, and what the panel will fall back to. Reporting the defaults is the
// difference between a panel that can say "恢复默认 → 60 步" and one that leaves
// the reader guessing what 默认 means.
type consoleBudget struct {
	MaxSteps          int    `json:"max_steps"`
	TurnMaxTokens     int    `json:"turn_max_tokens"`
	TurnDeadlineSecs  int    `json:"turn_deadline_seconds"`
	DefaultMaxSteps   int    `json:"default_max_steps"`
	DefaultMaxTokens  int    `json:"default_turn_max_tokens"`
	DefaultDeadline   int    `json:"default_turn_deadline_seconds"`
	SourceMaxSteps    string `json:"source_max_steps"`
	SourceMaxTokens   string `json:"source_turn_max_tokens"`
	SourceDeadline    string `json:"source_turn_deadline_seconds"`
	MaxStepsLimit     int    `json:"max_steps_limit"`
	MaxTokensLimit    int    `json:"max_turn_max_tokens_limit"`
	MaxDeadlineLimit  int    `json:"max_turn_deadline_seconds_limit"`
	HasOverride       bool   `json:"has_override"`
	RestartForContext bool   `json:"context_requires_restart"`
	ContextMaxTokens  int    `json:"context_max_tokens"`
}

// snapshotBudget renders the effective budget plus its defaults and limits.
func (s *Server) snapshotBudget(ctx context.Context) consoleBudget {
	b := s.effectiveBudget(ctx)

	out := consoleBudget{
		MaxSteps:         b.maxSteps,
		TurnMaxTokens:    b.maxTokens,
		TurnDeadlineSecs: b.deadlineSeconds(),
		DefaultMaxSteps:  s.cfg.DefaultChatMaxSteps,
		DefaultMaxTokens: s.cfg.DefaultChatMaxTokens,
		DefaultDeadline:  int(s.cfg.DefaultChatTurnDeadline / time.Second),
		SourceMaxSteps:   budgetFromConfig,
		SourceMaxTokens:  budgetFromConfig,
		SourceDeadline:   budgetFromConfig,
		MaxStepsLimit:    MaxConsoleSteps,
		MaxTokensLimit:   MaxConsoleTurnTokens,
		MaxDeadlineLimit: int(MaxConsoleTurnDeadline / time.Second),
	}
	if out.DefaultMaxSteps <= 0 {
		out.DefaultMaxSteps = chat.DefaultMaxSteps
	}
	if bs := s.turnBudgetStore(); bs != nil {
		ov, err := bs.GetTurnBudgetOverride(ctx)
		if err == nil {
			if ov.MaxSteps != nil {
				out.SourceMaxSteps = budgetFromConsole
			}
			if ov.MaxTokens != nil {
				out.SourceMaxTokens = budgetFromConsole
			}
			if ov.DeadlineSecs != nil {
				out.SourceDeadline = budgetFromConsole
			}
		}
	}
	out.HasOverride = out.SourceMaxSteps == budgetFromConsole ||
		out.SourceMaxTokens == budgetFromConsole ||
		out.SourceDeadline == budgetFromConsole
	// The in-turn condenser is built once, from context.max_tokens at startup.
	// Raising the step cap without it is the combination that turns a long task
	// into a context-limit error, so the panel says so instead of letting the
	// operator find out twenty steps in.
	out.ContextMaxTokens = s.cfg.ContextMaxTokens
	out.RestartForContext = s.cfg.ContextMaxTokens <= 0
	return out
}

// registerBudgetRoutes wires the per-turn budget endpoints.
func (s *Server) registerBudgetRoutes(authed *route.RouterGroup) {
	if s.chat.Runner == nil {
		return
	}
	authed.GET("/chat/budget", s.handleGetBudget)
	authed.PUT("/chat/budget", s.handlePutBudget)
}

// handleGetBudget reports the budget a new turn would use right now.
func (s *Server) handleGetBudget(ctx context.Context, c *app.RequestContext) {
	c.JSON(http.StatusOK, s.snapshotBudget(ctx))
}

// budgetRequest is the panel's write. Every field is a pointer so the body can
// be sparse: a missing field is left alone, and an explicit 0 means what it says
// (unlimited) for the two dimensions where that is a real choice.
type budgetRequest struct {
	MaxSteps         *int `json:"max_steps"`
	TurnMaxTokens    *int `json:"turn_max_tokens"`
	TurnDeadlineSecs *int `json:"turn_deadline_seconds"`
	// ResetAll drops every override and hands all three dimensions back to
	// config.yaml. It exists because "restore the defaults" has to be expressible
	// even when a default is 0: without it, 0 would be ambiguous between
	// "unlimited" and "whatever the file says".
	ResetAll bool `json:"reset_all"`
}

// handlePutBudget stores the console's budget override and answers with the
// resulting state, so the panel renders what the server actually accepted rather
// than what it asked for.
func (s *Server) handlePutBudget(ctx context.Context, c *app.RequestContext) {
	var body budgetRequest
	if err := c.BindJSON(&body); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	bs := s.turnBudgetStore()
	if bs == nil {
		// The store cannot hold an override, so accepting one would be a lie.
		c.JSON(http.StatusNotImplemented, map[string]string{
			"error": "this store cannot persist settings; change chat.max_steps in config.yaml",
		})
		return
	}

	next := store.TurnBudgetOverride{}
	if !body.ResetAll {
		if body.MaxSteps != nil {
			if err := validateSteps(*body.MaxSteps); err != nil {
				c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			next.MaxSteps = body.MaxSteps
		}
		if body.TurnMaxTokens != nil {
			if err := validateTokens(*body.TurnMaxTokens); err != nil {
				c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			next.MaxTokens = body.TurnMaxTokens
		}
		if body.TurnDeadlineSecs != nil {
			if err := validateDeadline(*body.TurnDeadlineSecs); err != nil {
				c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			next.DeadlineSecs = body.TurnDeadlineSecs
		}
	}

	if err := bs.SetTurnBudgetOverride(ctx, next); err != nil {
		s.fail(c, "store the turn budget", err)
		return
	}

	// Cached runners carry no budget of their own — every dimension travels per
	// turn on chat.Request — but they are dropped anyway: a runner is the object
	// that *would* hold a stale cap if one were ever reintroduced there, and
	// invalidating on the one event that changes the budget is what keeps
	// "cached" and "effective" unable to disagree.
	s.runnerCache.reset()
	eff := s.effectiveBudget(ctx)
	s.logger.Info("chat: turn budget updated",
		zap.Int("max_steps", eff.maxSteps),
		zap.Int("turn_max_tokens", eff.maxTokens),
		zapString("turn_deadline", eff.deadline.String()),
		zapBool("reset", body.ResetAll))

	c.JSON(http.StatusOK, s.snapshotBudget(ctx))
}

// validateSteps rejects a step cap the runner could not honour. 0 is refused
// rather than silently read as "the default": a console that shows 0 while the
// server runs 60 is a worse answer than a message saying so.
func validateSteps(v int) error {
	if v < 1 {
		return errors.New("max_steps must be at least 1")
	}
	if v > MaxConsoleSteps {
		return errors.New("max_steps must be at most " + strconv.Itoa(MaxConsoleSteps))
	}
	return nil
}

func validateTokens(v int) error {
	if v < 0 {
		return errors.New("turn_max_tokens must be 0 (unlimited) or more")
	}
	if v > MaxConsoleTurnTokens {
		return errors.New("turn_max_tokens must be at most " + strconv.Itoa(MaxConsoleTurnTokens))
	}
	return nil
}

func validateDeadline(v int) error {
	if v < 0 {
		return errors.New("turn_deadline_seconds must be 0 (unlimited) or more")
	}
	if v > int(MaxConsoleTurnDeadline/time.Second) {
		return errors.New("turn_deadline_seconds must be at most " +
			strconv.Itoa(int(MaxConsoleTurnDeadline/time.Second)))
	}
	return nil
}
