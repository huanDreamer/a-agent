// Package agent implements the ReAct (think → tool → observe) loop
// on top of eino's flow/agent/react.Agent, plus a tool-invocation
// audit log persisted via internal/store.
package agent

import (
	"context"
	"time"

	"github.com/huan/huan-agent/internal/store"
)

// InvocationRecord is the row shape we hand to store.RecordInvocation.
type InvocationRecord struct {
	SessionID  string
	ToolName   string
	Arguments  string
	Result     string
	Err        string
	DurationMs int64
}

// AuditSink persists a tool invocation. store.Store implements it.
type AuditSink interface {
	RecordInvocation(ctx context.Context, rec store.InvocationEvent) error
}

// auditFromRecord converts our agent record to the store event.
func auditFromRecord(r InvocationRecord) store.InvocationEvent {
	return store.InvocationEvent{
		SessionID:  r.SessionID,
		ToolName:   r.ToolName,
		Arguments:  r.Arguments,
		Result:     r.Result,
		Err:        r.Err,
		DurationMs: r.DurationMs,
	}
}

// nowMillis is a tiny indirection so tests can freeze time.
var nowMillis = func() int64 { return time.Now().UnixMilli() }
