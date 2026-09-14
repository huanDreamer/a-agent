package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// DefaultAskTimeout bounds how long one of the model's questions waits for an
// answer when the deployment does not say otherwise.
//
// Ten minutes is a compromise: long enough that stepping away from the desk does
// not lose the turn, short enough that a forgotten tab does not pin a goroutine
// and a half-finished tool call for an afternoon. It is bounded again by the
// turn's own context — a stop, a closed tab or a fired deadline ends the wait —
// and by chat.turn_deadline_seconds, which the waiting time counts against.
const DefaultAskTimeout = 10 * time.Minute

// errQuestionGone is what a submission gets when the question it answers is no
// longer waiting: already answered, timed out, cancelled with its turn, or never
// existed. All four are the same 404 on purpose — a client cannot act on the
// difference, and telling a stranger which question ids exist is not worth the
// precision.
var errQuestionGone = errors.New("question is no longer pending")

// pendingQuestion is one question a running turn is waiting on.
type pendingQuestion struct {
	id      string
	session string
	// question is kept so a submission can be checked against what was actually
	// offered: an answer that names an option the card never had is a bug or a
	// forgery, and the model must not be handed either as if a person chose it.
	question tool.Question
	// answers carries the submission to the waiting tool. It is buffered (one
	// slot) so whoever delivers an answer never blocks, even in the moment the
	// waiter has given up.
	answers chan tool.Answer
	askedAt time.Time
}

// questionHub tracks the questions currently waiting for an answer.
//
// It exists because the answer arrives on a different HTTP request from the one
// the turn is streaming on: the run goroutine is parked inside the tool call, and
// this is the only object both requests can see.
type questionHub struct {
	mu      sync.Mutex
	pending map[string]*pendingQuestion
}

func newQuestionHub() *questionHub {
	return &questionHub{pending: map[string]*pendingQuestion{}}
}

// register adds a question and returns its waiting record.
func (h *questionHub) register(session string, q tool.Question) *pendingQuestion {
	p := &pendingQuestion{
		id:       q.ID,
		session:  session,
		question: q,
		answers:  make(chan tool.Answer, 1),
		askedAt:  time.Now(),
	}
	h.mu.Lock()
	h.pending[q.ID] = p
	h.mu.Unlock()
	return p
}

// peek returns the waiting question without removing it, so a submission can be
// validated before it consumes the question.
func (h *questionHub) peek(id, session string) (*pendingQuestion, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.pending[id]
	if !ok || p.session != session {
		return nil, false
	}
	return p, true
}

// resolve hands an answer to the waiting tool, consuming the question. The
// removal and the hand-off happen under one lock, so of two simultaneous
// submissions exactly one wins and the other is told the question is gone.
func (h *questionHub) resolve(id, session string, a tool.Answer) error {
	h.mu.Lock()
	p, ok := h.pending[id]
	if !ok || p.session != session {
		h.mu.Unlock()
		return errQuestionGone
	}
	delete(h.pending, id)
	h.mu.Unlock()

	// Buffered with room for exactly this message: the send cannot block.
	p.answers <- a
	return nil
}

// forget drops a question its waiter has given up on (a timeout or a cancelled
// turn), so a late submission is refused instead of being delivered to nobody.
func (h *questionHub) forget(id string) {
	h.mu.Lock()
	delete(h.pending, id)
	h.mu.Unlock()
}

// count reports how many questions are waiting. It exists for the tests and for
// a leak check: a hub that grows without bound means a waiter stopped forgetting.
func (h *questionHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.pending)
}

// turnAsker is the Asker for one turn: it publishes the question on that turn's
// SSE stream and waits for the answer to arrive through the hub.
type turnAsker struct {
	hub     *questionHub
	session string
	timeout time.Duration
	// emit writes onto the turn's event channel. It is the same channel the
	// runner reports tool calls on, which is what makes the card appear in the
	// conversation at the moment the model asks rather than after it finishes.
	emit   func(chat.Event)
	logger *zap.Logger
}

// Ask publishes a question and waits for the answer.
//
// It never returns an error for "nobody answered": a timeout and a cancelled
// turn are answers with a status, because the model still has to decide what to
// do next. An error here means the machinery failed, which is a different thing
// and must not be reported to the model as if a person had ignored it.
func (a *turnAsker) Ask(ctx context.Context, q tool.Question) (tool.Answer, error) {
	// A Server built without New (tests do that) has no registry to park a
	// question in. Failing here with a sentence beats registering into a nil map
	// and taking the process down from inside a tool call.
	if a.hub == nil {
		return tool.Answer{}, errors.New("server: question registry is not initialized")
	}
	id, err := newQuestionID()
	if err != nil {
		return tool.Answer{}, fmt.Errorf("分配问题 id: %w", err)
	}
	q.ID = id

	pending := a.hub.register(a.session, q)
	// Whichever way this returns, the question stops being answerable.
	defer a.hub.forget(id)

	a.emit(chat.Event{Type: chat.EventAsk, Ask: &q, AskStatus: chat.AskPending})

	var timerC <-chan time.Time
	if a.timeout > 0 {
		timer := time.NewTimer(a.timeout)
		defer timer.Stop()
		timerC = timer.C
	}

	var answer tool.Answer
	select {
	case answer = <-pending.answers:
	case <-timerC:
		answer = tool.Answer{Status: tool.AnswerTimeout}
	case <-ctx.Done():
		// The turn is over: the browser went away, the user pressed 停止, or the
		// process is shutting down. Nobody will answer a question for a turn that
		// no longer exists.
		answer = tool.Answer{Status: tool.AnswerCancelled}
	}

	// Tell the client how it ended. A pending card that is never closed would sit
	// there offering choices for a question nobody is listening to.
	a.emit(chat.Event{
		Type:      chat.EventAsk,
		AskID:     id,
		AskStatus: string(answer.Status),
		AskAnswer: answeredOnly(answer),
	})

	waited := time.Since(pending.askedAt)
	if a.logger != nil {
		fields := []zap.Field{
			zap.String("session", a.session),
			zap.String("question", id),
			zap.String("status", string(answer.Status)),
			zap.Duration("waited", waited.Round(time.Millisecond)),
			zap.Int("options", len(q.Options)),
		}
		if answer.Answered() {
			a.logger.Info("chat: question answered", fields...)
		} else {
			// Not an error: it is exactly what the timeout exists for, and a
			// deployment that sees this often has a wait limit worth raising.
			a.logger.Info("chat: question not answered", fields...)
		}
	}
	return answer, nil
}

// answeredOnly returns the answer only when a person submitted one: a timeout has
// no answer to report, and an empty one would be indistinguishable from a person
// submitting nothing.
func answeredOnly(a tool.Answer) *tool.Answer {
	if !a.Answered() {
		return nil
	}
	return &a
}

// newQuestionID mints the handle a card's answer comes back with. It is random
// rather than sequential so a guess cannot consume somebody else's question; the
// route still checks it against the session it was asked in.
func newQuestionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// askTimeout is the configured wait limit, or the default.
func (s *Server) askTimeout() time.Duration {
	if s.chat.AskTimeout > 0 {
		return s.chat.AskTimeout
	}
	return DefaultAskTimeout
}

// handleAnswerQuestion delivers one card's answer to the turn waiting on it.
//
// It is a separate request from the streaming one by necessity: that response is
// already committed to the event stream, and a browser cannot write a second
// request body on it. The question id is the join between the two.
func (s *Server) handleAnswerQuestion(ctx context.Context, c *app.RequestContext) {
	sessionID := c.Param("id")
	questionID := c.Param("qid")

	var body struct {
		Selected []string `json:"selected"`
		Text     string   `json:"text"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	selected := trimAll(body.Selected)
	text := strings.TrimSpace(body.Text)
	if len(selected) == 0 && text == "" {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "请选择一个选项，或自己输入内容后再提交"})
		return
	}

	if s.questions == nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": errQuestionGone.Error()})
		return
	}
	pending, ok := s.questions.peek(questionID, sessionID)
	if !ok {
		c.JSON(http.StatusNotFound, map[string]string{"error": questionGoneMessage})
		return
	}

	// Validate against what the card actually offered. The checks are here rather
	// than in the tool because this is the only place that knows both what was
	// asked and what came back — and the model must never be handed an "answer"
	// that no person could have chosen from the card.
	if text != "" && !pending.question.AllowCustom {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "这个问题只能从给出的选项里选，不能自己输入"})
		return
	}
	if len(selected) > 1 && !pending.question.MultiSelect {
		c.JSON(http.StatusBadRequest, map[string]string{"error": "这个问题只能选一个选项"})
		return
	}
	if bad := unknownOption(selected, pending.question.Options); bad != "" {
		c.JSON(http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("选项 %q 不在这个问题里，请从卡片上的选项中选择", bad),
		})
		return
	}

	answer := tool.Answer{Status: tool.AnswerAnswered, Selected: selected, Text: text}
	if err := s.questions.resolve(questionID, sessionID, answer); err != nil {
		c.JSON(http.StatusNotFound, map[string]string{"error": questionGoneMessage})
		return
	}
	if s.logger != nil {
		s.logger.Info("chat: answer accepted",
			zap.String("session", sessionID),
			zap.String("question", questionID),
			zap.Int("selected", len(selected)),
			zap.Bool("custom", text != ""),
		)
	}
	c.JSON(http.StatusOK, map[string]bool{"ok": true})
}

// questionGoneMessage is what a stale submission is told. It names the likely
// causes because the common one is not a bug: the model gave up waiting, and the
// answer the user just typed arrived after the turn moved on.
const questionGoneMessage = "这个问题已经不在等待中了（可能已提交、已超时，或本轮已经结束），请让模型重新提问"

// trimAll trims every entry and drops the empty ones, so a UI that submits an
// empty selection is refused by the "nothing chosen" check rather than accepted
// as a choice named "".
func trimAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// unknownOption returns the first selected label the question never offered, or
// "" when every label is one of them.
func unknownOption(selected []string, options []tool.Option) string {
	if len(selected) == 0 {
		return ""
	}
	offered := make(map[string]struct{}, len(options))
	for _, o := range options {
		offered[o.Label] = struct{}{}
	}
	for _, s := range selected {
		if _, ok := offered[s]; !ok {
			return s
		}
	}
	return ""
}
