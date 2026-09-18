package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/chat"
	"github.com/huan/huan-agent/internal/tool"
)

// The approval gate's server half.
//
// It is the same shape as the ask_user machinery next door — a hub registered per
// turn, an SSE event that announces the request, an answer that arrives on its own
// request, and a waiter that gives up when the turn ends — with one difference
// that the whole feature depends on:
//
//	**A timeout is a refusal.**
//
// A question that nobody answers leaves the model to decide what to do next,
// which is fine because asking is how it gathers information. An approval that
// nobody answers must not let the action through, because the moment nobody is
// watching is exactly the moment the gate has to hold. Every branch below that
// does not end in an explicit allow ends in a deny.

// DefaultApprovalTimeout bounds how long one write/exec request waits for a
// decision when the deployment does not say otherwise.
//
// It is deliberately shorter than DefaultAskTimeout: a question is the model
// gathering information, while an approval blocks an action, and leaving that
// hanging holds a turn and a half-finished tool call. It is bounded again by the
// turn's own context and by chat.turn_deadline_seconds, which the waiting time
// counts against.
//
// The value matches config.DefaultApprovalTimeout. The two are separate constants
// for the same reason DefaultAskTimeout and DefaultChatAskUserTimeoutSeconds are:
// the server must behave sensibly when built without a config, and the config must
// document a default without importing the server.
const DefaultApprovalTimeout = 5 * time.Minute

// errApprovalGone is what a submission gets when the request it decides is no
// longer waiting: already decided, timed out, cancelled with its turn, or never
// existed. All four are the same 404 for the same reason the question hub gives
// one: a client cannot act on the difference, and telling a stranger which ids
// exist is not worth the precision.
var errApprovalGone = errors.New("approval is no longer pending")

// pendingApproval is one request a running turn is waiting on.
type pendingApproval struct {
	id      string
	session string
	request tool.Request
	// decisions carries the submission to the waiting gate. It is buffered (one
	// slot) so whoever delivers a decision never blocks, even in the moment the
	// waiter has given up.
	decisions chan tool.Decision
	askedAt   time.Time
}

// approvalHub tracks the requests currently waiting for a decision.
//
// It exists for the same reason the question hub does: the decision arrives on a
// different HTTP request from the one the turn is streaming on, so this is the
// only object both can see.
type approvalHub struct {
	mu      sync.Mutex
	pending map[string]*pendingApproval
}

func newApprovalHub() *approvalHub {
	return &approvalHub{pending: map[string]*pendingApproval{}}
}

func (h *approvalHub) register(session string, req tool.Request) *pendingApproval {
	p := &pendingApproval{
		id:        req.ID,
		session:   session,
		request:   req,
		decisions: make(chan tool.Decision, 1),
		askedAt:   time.Now(),
	}
	h.mu.Lock()
	h.pending[req.ID] = p
	h.mu.Unlock()
	return p
}

// peek returns the waiting request without consuming it, so a submission can be
// validated before it decides anything.
func (h *approvalHub) peek(id, session string) (*pendingApproval, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.pending[id]
	if !ok || p.session != session {
		return nil, false
	}
	return p, true
}

// resolve hands a decision to the waiting gate, consuming the request. The
// removal and the hand-off happen under one lock, so of two simultaneous
// submissions exactly one wins and the other is told the request is gone.
func (h *approvalHub) resolve(id, session string, d tool.Decision) error {
	h.mu.Lock()
	p, ok := h.pending[id]
	if !ok || p.session != session {
		h.mu.Unlock()
		return errApprovalGone
	}
	delete(h.pending, id)
	h.mu.Unlock()

	p.decisions <- d
	return nil
}

// forget drops a request its waiter has given up on, so a late decision is
// refused instead of being delivered to nobody.
func (h *approvalHub) forget(id string) {
	h.mu.Lock()
	delete(h.pending, id)
	h.mu.Unlock()
}

// count reports how many requests are waiting. It exists for the tests and for a
// leak check: a hub that grows without bound means a waiter stopped forgetting.
func (h *approvalHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.pending)
}

// turnApprover is the Approver for one turn: it publishes the request on that
// turn's SSE stream and waits for the decision to arrive through the hub.
type turnApprover struct {
	hub     *approvalHub
	session string
	timeout time.Duration
	emit    func(chat.Event)
	logger  *zap.Logger
}

// Approve publishes a request and waits for a decision.
//
// It returns an error only when the machinery failed. "Nobody answered" is a
// DecisionDeny with Source=timeout, not an error, because the gate has to treat
// it as a refusal either way and a person's silence should not be reported to the
// model as a broken tool.
func (a *turnApprover) Approve(ctx context.Context, req tool.Request) (tool.Decision, error) {
	if a.hub == nil {
		// A Server built without New (tests do that) has no registry to park a
		// request in. Failing here with a sentence beats registering into a nil
		// map and taking the process down from inside a tool call.
		return tool.Decision{}, errors.New("server: approval registry is not initialized")
	}

	id, err := newQuestionID()
	if err != nil {
		return tool.Decision{}, fmt.Errorf("分配审批 id: %w", err)
	}
	req.ID = id
	if req.Timeout <= 0 && a.timeout > 0 {
		req.Timeout = a.timeout.Milliseconds()
	}

	pending := a.hub.register(a.session, req)
	// Whichever way this returns, the request stops being decidable. A late
	// submission then gets a 404 rather than flipping a decision already made.
	defer a.hub.forget(id)

	a.emit(chat.Event{Type: chat.EventApproval, Approval: &req})

	timeout := a.timeout
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Millisecond
	}
	var timerC <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timerC = timer.C
	}

	var decision tool.Decision
	select {
	case decision = <-pending.decisions:
		if decision.Source == "" {
			decision.Source = tool.SourceHuman
		}
	case <-timerC:
		decision = tool.Decision{Kind: tool.DecisionDeny, Source: tool.SourceTimeout}
	case <-ctx.Done():
		// The turn is over: the browser went away, 停止 was pressed, or the
		// process is shutting down. Nobody will decide for a turn that no longer
		// exists, and an undecided request must not become an allowed one.
		decision = tool.Decision{Kind: tool.DecisionDeny, Source: tool.SourceTimeout,
			Reason: "本轮已经结束，没有人做出决定"}
	}

	// Tell the client how it ended. A pending card that is never closed would sit
	// there offering buttons for a request nobody is listening to.
	a.emit(chat.Event{
		Type:             chat.EventApproval,
		ApprovalID:       id,
		ApprovalDecision: string(decision.Kind),
		ApprovalReason:   decision.Reason,
		ApprovalSource:   decision.Source,
	})

	if a.logger != nil {
		fields := []zap.Field{
			zap.String("session", a.session),
			zap.String("approval", id),
			zap.String("tool", req.Tool),
			zap.String("capability", string(req.Capability)),
			zap.String("decision", string(decision.Kind)),
			zap.String("source", decision.Source),
			zap.Duration("waited", time.Since(pending.askedAt).Round(time.Millisecond)),
		}
		if decision.Allowed() {
			a.logger.Info("chat: action approved", fields...)
		} else {
			// A refusal is the gate working, not a failure. It is logged at info
			// for a person's decision and at warn for a timeout, because a
			// deployment that sees many timeouts has a wait limit worth raising
			// (or nobody watching, which is what the gate is for).
			if decision.Source == tool.SourceTimeout {
				a.logger.Warn("chat: action refused (no decision)", fields...)
			} else {
				a.logger.Info("chat: action refused", fields...)
			}
		}
	}
	return decision, nil
}

// approvalTimeout is the configured wait limit, or the default.
func (s *Server) approvalTimeout() time.Duration {
	if s.chat.ApprovalTimeout > 0 {
		return s.chat.ApprovalTimeout
	}
	return DefaultApprovalTimeout
}

// handleDecideApproval delivers one card's decision to the turn waiting on it.
//
// Separate from the streaming request for the same reason the question endpoint
// is: that response is already committed to the event stream, and a browser
// cannot write a second body on it. The request id is the join between the two.
func (s *Server) handleDecideApproval(ctx context.Context, c *app.RequestContext) {
	sessionID := c.Param("id")
	approvalID := c.Param("aid")

	var body struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	kind := tool.DecisionKind(strings.TrimSpace(body.Decision))
	if !kind.Valid() {
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("未知的决定 %q（want allow_once, allow_turn or deny）", body.Decision),
		})
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if len([]rune(reason)) > 500 {
		reason = string([]rune(reason)[:500])
	}

	if s.approvals == nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": errApprovalGone.Error()})
		return
	}
	pending, ok := s.approvals.peek(approvalID, sessionID)
	if !ok {
		c.JSON(http.StatusNotFound, map[string]string{"error": approvalGoneMessage})
		return
	}

	// A reason is only meaningful on a refusal. Dropping it otherwise keeps the
	// audit honest: "rejected because X" and "allowed, and here is X" are
	// different facts, and the model reads the reason as an instruction.
	if kind != tool.DecisionDeny {
		reason = ""
	}

	decision := tool.Decision{Kind: kind, Reason: reason, Source: tool.SourceHuman}
	if err := s.approvals.resolve(approvalID, sessionID, decision); err != nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": approvalGoneMessage})
		return
	}
	if s.logger != nil {
		s.logger.Info("chat: approval recorded",
			zap.String("session", sessionID),
			zap.String("approval", approvalID),
			zap.String("tool", pending.request.Tool),
			zap.String("decision", string(kind)),
			zap.Bool("with_reason", reason != ""),
		)
	}
	c.JSON(http.StatusOK, map[string]bool{"ok": true})
}

// approvalGoneMessage is what a stale submission is told. It names the likely
// causes because the common one is not a bug: the request timed out, and the
// decision the user just clicked arrived after the turn moved on.
const approvalGoneMessage = "这个审批已经不在等待中了（可能已提交、已超时，或本轮已经结束）。超时按拒绝处理，请让模型重新发起。"
