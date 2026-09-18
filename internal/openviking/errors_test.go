package openviking

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsIndexWaitTimeoutOnServerDeadline(t *testing.T) {
	// OpenViking answers 504 DEADLINE_EXCEEDED when a waited write outruns the
	// budget. It only does so after storing the content, so this shape means
	// "saved, still indexing".
	err := &APIError{
		Op:      "POST /api/v1/content/write",
		Status:  http.StatusGatewayTimeout,
		Code:    "DEADLINE_EXCEEDED",
		Message: "Queue processing timed out after 30.0s",
	}
	if !IsIndexWaitTimeout(err) {
		t.Errorf("IsIndexWaitTimeout(504 DEADLINE_EXCEEDED) = false, want true")
	}
}

func TestIsIndexWaitTimeoutIgnoresRejections(t *testing.T) {
	cases := map[string]error{
		"invalid uri":      &APIError{Status: 400, Code: "INVALID_URI", Message: "bad extension"},
		"unauthenticated":  &APIError{Status: 401, Code: "UNAUTHENTICATED", Message: "Invalid API Key"},
		"server error":     &APIError{Status: 500, Code: "INTERNAL", Message: "boom"},
		"not found":        &APIError{Status: 404, Code: "NOT_FOUND", Message: "gone"},
		"plain error":      errors.New("boom"),
		"nil":              nil,
		"caller cancelled": context.Canceled,
	}
	for name, err := range cases {
		if IsIndexWaitTimeout(err) {
			t.Errorf("IsIndexWaitTimeout(%s) = true, want false", name)
		}
	}
}

func TestIsIndexWaitTimeoutOnCallerDeadline(t *testing.T) {
	if !IsIndexWaitTimeout(context.DeadlineExceeded) {
		t.Error("IsIndexWaitTimeout(context.DeadlineExceeded) = false, want true")
	}
	if !IsIndexWaitTimeout(errors.Join(errors.New("write failed"), context.DeadlineExceeded)) {
		t.Error("IsIndexWaitTimeout(wrapped deadline) = false, want true")
	}
}

// The client-side shape is the one production actually produced: the HTTP
// client's own timeout fires while still awaiting response headers, so no
// status ever arrives. Exercise it against a server that never answers, rather
// than hand-building the error — the point is that a real *url.Error
// classifies correctly.
func TestIsIndexWaitTimeoutOnClientTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	c, err := New(Config{BaseURL: srv.URL, Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, werr := c.WriteContent(context.Background(), WriteRequest{
		URI: "viking://user/default/x.md", Content: "hi", Wait: true,
	})
	if werr == nil {
		t.Fatal("WriteContent against a silent server: want error, got nil")
	}
	if !IsIndexWaitTimeout(werr) {
		t.Errorf("IsIndexWaitTimeout(%v) = false, want true", werr)
	}
	// It is also an availability failure: both readings are true, and callers
	// pick the one that matches the decision they are making.
	if !IsUnavailable(werr) {
		t.Errorf("IsUnavailable(%v) = false, want true", werr)
	}
	if !strings.Contains(werr.Error(), "UNAVAILABLE") {
		t.Errorf("error = %q, want the UNAVAILABLE code", werr)
	}
}

func TestIsIndexWaitTimeoutOnRequestTimeoutStatus(t *testing.T) {
	err := &APIError{Status: http.StatusRequestTimeout, Code: "TIMEOUT", Message: "slow"}
	if !IsIndexWaitTimeout(err) {
		t.Error("IsIndexWaitTimeout(408) = false, want true")
	}
}
