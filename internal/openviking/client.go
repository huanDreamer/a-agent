// Package openviking is a small, dependency-free client for the OpenViking
// HTTP API (the "agent-native context database" that backs huan-agent's
// long-term memory and document store).
//
// It exists because two very different callers need the same transport: the
// memory mirror (which writes conversation turns and facts, then recalls them
// semantically) and the document syncer (which writes notes and workspace files
// into viking:// and reads back what is already there). Sharing one client
// keeps authentication, timeouts and — most importantly — error classification
// in a single place, so "OpenViking is down" is decided once instead of guessed
// at every call site.
//
// The package deliberately does not retry or circuit-break. Every method
// returns a *APIError, and IsUnavailable says whether the failure was the
// server being unreachable (degrade, keep going) or the request being wrong
// (fix the request). Deciding what to do about that belongs to the caller,
// which is the only place that knows whether a lost write matters.
package openviking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds a single request when Config.Timeout is unset. It is
// generous because write calls can be asked to wait for indexing, but not
// unbounded: a hung server must not hang a chat turn.
const DefaultTimeout = 15 * time.Second

// maxErrorBodyBytes bounds how much of a non-JSON error body is quoted back.
// The useful part is the first line; an HTML error page is not.
const maxErrorBodyBytes = 512

// Config describes how to reach one OpenViking server.
type Config struct {
	// BaseURL is the server root, e.g. "http://127.0.0.1:1933". Required.
	BaseURL string
	// APIKey is the token sent as X-API-Key. Empty is the dev auth mode, which
	// is what a locally installed `ov` uses by default.
	APIKey string
	// Account and User are the identity headers OpenViking scopes data by.
	// Empty values fall back to "default", which is what a local install uses.
	Account string
	User    string
	// Timeout bounds one request. Zero uses DefaultTimeout.
	Timeout time.Duration
	// HTTPClient overrides the underlying client. Nil builds one with Timeout.
	// It exists so tests can point at an httptest server without a real network.
	HTTPClient *http.Client
}

// Client talks to one OpenViking server. It is safe for concurrent use.
type Client struct {
	baseURL string
	apiKey  string
	account string
	user    string
	hc      *http.Client
}

// New validates the config and returns a client.
func New(cfg Config) (*Client, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, fmt.Errorf("openviking: base_url is required")
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("openviking: invalid base_url %q: %w", base, err)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		hc = &http.Client{Timeout: timeout}
	}
	account := strings.TrimSpace(cfg.Account)
	if account == "" {
		account = "default"
	}
	user := strings.TrimSpace(cfg.User)
	if user == "" {
		user = "default"
	}
	return &Client{
		// Trailing slashes are dropped so path joining cannot produce "//".
		baseURL: strings.TrimRight(base, "/"),
		apiKey:  strings.TrimSpace(cfg.APIKey),
		account: account,
		user:    user,
		hc:      hc,
	}, nil
}

// BaseURL returns the normalised server root (trailing slash removed).
func (c *Client) BaseURL() string { return c.baseURL }

// Account returns the identity the client scopes reads and writes by.
func (c *Client) Account() string { return c.account }

// User returns the user the client scopes reads and writes by.
func (c *Client) User() string { return c.user }

// envelope is OpenViking's response shape for everything but /health: a status
// string, the payload under "result", and an error object when status != "ok".
type envelope struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
	Error  *errorBody      `json:"error"`
}

// errorBody is the "error" object inside an envelope.
type errorBody struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

// do performs one JSON request and decodes the envelope's result into out.
//
// A zero timeoutMS means "no explicit wait"; a positive value is sent as the
// request's "timeout" field by the endpoint that understands it (write /
// resources), not applied here.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("openviking: encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("openviking: build %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	c.authorize(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return &APIError{Op: method + " " + path, Status: 0, Code: "UNAVAILABLE", Message: err.Error(), cause: err}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return &APIError{Op: method + " " + path, Status: resp.StatusCode, Code: "READ_BODY", Message: err.Error(), cause: err}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.decodeError(method, path, resp, raw)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return &APIError{
			Op:      method + " " + path,
			Status:  resp.StatusCode,
			Code:    "BAD_RESPONSE",
			Message: fmt.Sprintf("decode response: %v", err),
			Raw:     excerpt(raw),
			cause:   err,
		}
	}
	// Some endpoints answer {"status":"ok"} with no result (or a different
	// envelope entirely); report a server-side error status as an error.
	if env.Status != "" && env.Status != "ok" && env.Error != nil {
		return c.apiError(method, path, resp, env.Error)
	}
	if out == nil || len(env.Result) == 0 || string(env.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return &APIError{
			Op:      method + " " + path,
			Status:  resp.StatusCode,
			Code:    "BAD_RESPONSE",
			Message: fmt.Sprintf("decode result: %v", err),
			Raw:     excerpt(env.Result),
			cause:   err,
		}
	}
	return nil
}

// decodeError builds an APIError from a non-2xx response, preferring the
// OpenViking error envelope over the HTTP status text.
func (c *Client) decodeError(method, path string, resp *http.Response, raw []byte) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Error != nil {
		return c.apiError(method, path, resp, env.Error)
	}
	return &APIError{
		Op:      method + " " + path,
		Status:  resp.StatusCode,
		Code:    "HTTP_" + http.StatusText(resp.StatusCode),
		Message: strings.TrimSpace(string(excerpt(raw))),
		Raw:     excerpt(raw),
	}
}

func (c *Client) apiError(method, path string, resp *http.Response, body *errorBody) error {
	return &APIError{
		Op:        method + " " + path,
		Status:    resp.StatusCode,
		Code:      body.Code,
		Message:   body.Message,
		RequestID: resp.Header.Get("x-request-id"),
		Raw:       body.Details,
	}
}

// authorize sets the authentication and identity headers. The key goes in a
// header and nowhere else: it is never logged, never quoted in an error, and
// never returned to a caller.
func (c *Client) authorize(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	if c.account != "" {
		req.Header.Set("X-OpenViking-Account", c.account)
	}
	if c.user != "" {
		req.Header.Set("X-OpenViking-User", c.user)
	}
}

// excerpt bounds a body quoted back in an error message.
func excerpt(b []byte) []byte {
	if len(b) <= maxErrorBodyBytes {
		return b
	}
	return b[:maxErrorBodyBytes]
}

// ---- health ----

// Health is the payload of GET /health (which does not use the envelope).
type Health struct {
	Status   string `json:"status"`
	Healthy  bool   `json:"healthy"`
	Version  string `json:"version"`
	AuthMode string `json:"auth_mode"`
}

// Health checks that the server is up. It is the cheapest way to decide whether
// the rest of the client is worth calling.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return nil, fmt.Errorf("openviking: build health request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.authorize(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &APIError{Op: "GET /health", Code: "UNAVAILABLE", Message: err.Error(), cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, &APIError{Op: "GET /health", Status: resp.StatusCode, Code: "READ_BODY", Message: rerr.Error(), cause: rerr}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.decodeError(http.MethodGet, "/health", resp, raw)
	}
	var h Health
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, &APIError{
			Op:      "GET /health",
			Status:  resp.StatusCode,
			Code:    "BAD_RESPONSE",
			Message: fmt.Sprintf("decode health: %v", err),
			Raw:     excerpt(raw),
			cause:   err,
		}
	}
	return &h, nil
}

// ---- multipart (temp upload) ----

// UploadTemp uploads a local file to OpenViking's temp area and returns the
// temp_file_id that AddResource consumes.
//
// It exists because the HTTP server refuses host filesystem paths outright
// ("direct host filesystem paths are not allowed") — that is a deliberate
// security boundary of the server, and uploading is the supported way for a
// process on the same machine to hand it a file.
func (c *Client) UploadTemp(ctx context.Context, filename string, content io.Reader) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreatePart(filePartHeader(filename))
	if err != nil {
		return "", fmt.Errorf("openviking: build multipart: %w", err)
	}
	if _, err := io.Copy(part, content); err != nil {
		return "", fmt.Errorf("openviking: write multipart: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("openviking: close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/resources/temp_upload", &buf)
	if err != nil {
		return "", fmt.Errorf("openviking: build upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	c.authorize(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", &APIError{Op: "POST /api/v1/resources/temp_upload", Code: "UNAVAILABLE", Message: err.Error(), cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return "", &APIError{Op: "POST /api/v1/resources/temp_upload", Status: resp.StatusCode, Code: "READ_BODY", Message: rerr.Error(), cause: rerr}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", c.decodeError(http.MethodPost, "/api/v1/resources/temp_upload", resp, raw)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", &APIError{
			Op:      "POST /api/v1/resources/temp_upload",
			Status:  resp.StatusCode,
			Code:    "BAD_RESPONSE",
			Message: fmt.Sprintf("decode upload: %v", err),
			Raw:     excerpt(raw),
			cause:   err,
		}
	}
	if env.Error != nil {
		return "", c.apiError(http.MethodPost, "/api/v1/resources/temp_upload", resp, env.Error)
	}
	var out struct {
		TempFileID string `json:"temp_file_id"`
	}
	if err := json.Unmarshal(env.Result, &out); err != nil {
		return "", &APIError{
			Op:      "POST /api/v1/resources/temp_upload",
			Status:  resp.StatusCode,
			Code:    "BAD_RESPONSE",
			Message: fmt.Sprintf("decode upload result: %v", err),
			Raw:     excerpt(env.Result),
			cause:   err,
		}
	}
	if out.TempFileID == "" {
		return "", &APIError{
			Op:      "POST /api/v1/resources/temp_upload",
			Status:  resp.StatusCode,
			Code:    "BAD_RESPONSE",
			Message: "upload returned no temp_file_id",
			Raw:     excerpt(raw),
		}
	}
	return out.TempFileID, nil
}

// filePartHeader builds the multipart part header for a file upload. The
// content type is declared as octet-stream: OpenViking sniffs the real type
// from the bytes, and guessing here would only risk a mismatch.
func filePartHeader(filename string) textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", "application/octet-stream")
	return h
}
