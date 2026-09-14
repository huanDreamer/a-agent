package tool

import (
	"context"
	"testing"
)

// recordingAsker satisfies the interface and remembers the question.
type recordingAsker struct {
	asked Question
}

func (r *recordingAsker) Ask(_ context.Context, q Question) (Answer, error) {
	r.asked = q
	return Answer{Status: AnswerAnswered, Selected: []string{"A"}}, nil
}

func TestWithAsker_RoundTrips(t *testing.T) {
	asker := &recordingAsker{}
	ctx := WithAsker(context.Background(), asker)

	got, ok := AskerFrom(ctx)
	if !ok {
		t.Fatal("AskerFrom did not find the installed asker")
	}
	if _, err := got.Ask(ctx, Question{Text: "选一个？"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if asker.asked.Text != "选一个？" {
		t.Errorf("question reached the asker as %+v", asker.asked)
	}
}

func TestAskerFrom_WithoutOneIsAbsenceNotAnEmptyValue(t *testing.T) {
	// A surface with no cards installs nothing, so the tool must be able to tell
	// "nobody can answer" from "an asker that answers nothing".
	for name, ctx := range map[string]context.Context{
		"plain":  context.Background(),
		"nil":    nil,
		"widely": context.WithValue(context.Background(), struct{}{}, "x"),
	} {
		t.Run(name, func(t *testing.T) {
			if a, ok := AskerFrom(ctx); ok || a != nil {
				t.Errorf("AskerFrom = (%v, %v), want (nil, false)", a, ok)
			}
		})
	}
}

func TestWithAsker_NilLeavesTheContextAlone(t *testing.T) {
	// A typed-nil Asker installed into the value slot would make AskerFrom report
	// true and then panic on the call; refusing it here is the one moment the
	// mistake is free.
	var nilAsker Asker
	ctx := WithAsker(context.Background(), nilAsker)
	if _, ok := AskerFrom(ctx); ok {
		t.Error("a nil asker was installed and reported as present")
	}
}

func TestAnswer_Answered(t *testing.T) {
	answered := Answer{Status: AnswerAnswered}
	if !answered.Answered() {
		t.Error("answered status did not report as answered")
	}
	for _, status := range []AnswerStatus{AnswerTimeout, AnswerCancelled, ""} {
		if (Answer{Status: status}).Answered() {
			t.Errorf("status %q reported as answered", status)
		}
	}
}
