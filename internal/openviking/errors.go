package openviking

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// APIError is any failure returned by the OpenViking client: a server-side
// error envelope, a transport failure, or an unparseable response.
//
// It carries the pieces an operator needs — the operation, the HTTP status, the
// server's own error code and message, and the request id to quote when asking
// the OpenViking maintainers — and, crucially, it is the value IsUnavailable
// classifies. There is no API key anywhere in it, by construction: the client
// only ever puts the key in a request header.
type APIError struct {
	// Op is the operation that failed, e.g. "POST /api/v1/search/find".
	Op string
	// Status is the HTTP status, or 0 when the request never completed.
	Status int
	// Code is OpenViking's error code ("PERMISSION_DENIED", "INVALID_URI", …),
	// or a client-side code ("UNAVAILABLE", "BAD_RESPONSE") for failures that
	// never reached the server's handler.
	Code string
	// Message is the server's message, or the transport error's text.
	Message string
	// RequestID is the server's x-request-id when the response carried one.
	RequestID string
	// Raw is the (bounded) error detail payload, when the server sent one.
	Raw []byte

	cause error
}

// Error renders the failure. It is deliberately one line: this string ends up in
// logs and in the console's status panel.
func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("openviking: ")
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, "HTTP %d: ", e.Status)
	}
	if e.Code != "" {
		b.WriteString(e.Code)
		b.WriteString(": ")
	}
	b.WriteString(e.Message)
	if e.RequestID != "" {
		b.WriteString(" (request_id=")
		b.WriteString(e.RequestID)
		b.WriteString(")")
	}
	return b.String()
}

// Unwrap exposes the transport error, so errors.Is/As keep working through it.
func (e *APIError) Unwrap() error { return e.cause }

// IsUnavailable reports whether err means "OpenViking cannot be reached right
// now" as opposed to "this request was wrong".
//
// The distinction is the whole point of the type: an unreachable server is a
// reason to keep working locally and warn, while a rejected request is a bug
// that must surface. A nil error is available.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		// A context timeout or a dial failure that never became an APIError
		// still means the server could not serve us.
		return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	}
	if apiErr.Status == 0 {
		// Never completed: DNS, dial, TLS, timeout, connection reset.
		return true
	}
	if apiErr.Status >= 500 {
		return true
	}
	switch strings.ToUpper(apiErr.Code) {
	case "UNAVAILABLE", "READ_BODY", "TIMEOUT", "SERVICE_UNAVAILABLE":
		return true
	}
	// A transport error wrapped with a status (rare) is still a transport error.
	var netErr net.Error
	return errors.As(apiErr.cause, &netErr)
}

// IsIndexWaitTimeout reports whether err means "the content was accepted, but
// the asynchronous index queue did not drain inside the budget we allowed".
//
// It exists because OpenViking writes the file *before* it waits for indexing,
// so a waited write that runs out of time is not a lost write: the document is
// there and searchable a moment later. Two shapes reach here, and they are not
// equally certain:
//
//   - The server's own 504 DEADLINE_EXCEEDED. Definitive — it only answers
//     that after the content is already on disk.
//   - A client-side transport timeout. Ambiguous: the request may not have
//     been seen at all, so a caller that cares must confirm with a read rather
//     than trust this.
//
// Callers must therefore treat a true result as "maybe saved", never as
// "saved". IsUnavailable also reports true for both shapes; this is the
// narrower question.
func IsIndexWaitTimeout(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return errors.Is(err, context.DeadlineExceeded)
	}
	if apiErr.Status == http.StatusGatewayTimeout || apiErr.Status == http.StatusRequestTimeout {
		return true
	}
	if strings.EqualFold(strings.ReplaceAll(apiErr.Code, "_", ""), "deadlineexceeded") {
		return true
	}
	// url.Error (and most transport errors) implement net.Error; only the
	// timeout flavour means "we stopped waiting", which is what matters here.
	var netErr net.Error
	return errors.As(apiErr.cause, &netErr) && netErr.Timeout()
}

// IsNotFound reports whether err is OpenViking saying the resource does not
// exist, which callers treat as "nothing there yet" rather than a failure.
func IsNotFound(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Status == 404 {
		return true
	}
	// Codes are compared with separators removed so both the server's
	// "NOT_FOUND" and a "NotFound" spelling classify the same way.
	return strings.EqualFold(strings.ReplaceAll(apiErr.Code, "_", ""), "notfound")
}
