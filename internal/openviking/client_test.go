package openviking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recorder is a test server that records what it was asked and answers with a
// canned response. Every test in this file drives the client through it, which
// is the point of the injectable HTTP client: no test needs a real OpenViking.
type recorder struct {
	*httptest.Server
	requests []recorded
	handler  func(r recorded) (int, string)
	// respHeader is copied onto every response, which is how a test makes the
	// server look like it sent an x-request-id.
	respHeader http.Header
}

type recorded struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

func newRecorder(t *testing.T, handler func(r recorded) (int, string)) *recorder {
	t.Helper()
	rec := &recorder{handler: handler}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got := recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: string(body)}
		rec.requests = append(rec.requests, got)
		status, payload := 200, `{"status":"ok","result":null}`
		if rec.handler != nil {
			status, payload = rec.handler(got)
		}
		w.Header().Set("Content-Type", "application/json")
		for name, values := range rec.respHeader {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(rec.Close)
	return rec
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: baseURL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewRejectsEmptyBaseURL(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with no base_url: want error, got nil")
	}
	if _, err := New(Config{BaseURL: "   "}); err == nil {
		t.Fatal("New with blank base_url: want error, got nil")
	}
}

func TestNewNormalisesBaseURLAndIdentity(t *testing.T) {
	c, err := New(Config{BaseURL: "http://127.0.0.1:1933///", APIKey: " k ", Account: "", User: "bob"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, want := c.BaseURL(), "http://127.0.0.1:1933"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
	if got, want := c.Account(), "default"; got != want {
		t.Errorf("Account() = %q, want %q", got, want)
	}
	if got, want := c.User(), "bob"; got != want {
		t.Errorf("User() = %q, want %q", got, want)
	}
}

func TestRequestSendsAuthAndIdentityHeaders(t *testing.T) {
	rec := newRecorder(t, nil)
	c, err := New(Config{BaseURL: rec.URL, APIKey: "secret-key", Account: "acct", User: "u1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	got := rec.requests[0].Header
	if got.Get("X-API-Key") != "secret-key" {
		t.Errorf("X-API-Key = %q, want secret-key", got.Get("X-API-Key"))
	}
	if got.Get("X-OpenViking-Account") != "acct" || got.Get("X-OpenViking-User") != "u1" {
		t.Errorf("identity headers = %q / %q, want acct / u1",
			got.Get("X-OpenViking-Account"), got.Get("X-OpenViking-User"))
	}
}

func TestHealthParsesPayload(t *testing.T) {
	rec := newRecorder(t, func(recorded) (int, string) {
		return 200, `{"status":"ok","healthy":true,"version":"0.4.19","auth_mode":"dev"}`
	})
	c := newTestClient(t, rec.URL)
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.Healthy || h.Version != "0.4.19" || h.AuthMode != "dev" {
		t.Errorf("Health = %+v, want healthy 0.4.19 dev", h)
	}
}

func TestFindSendsOnlyProvidedFields(t *testing.T) {
	rec := newRecorder(t, func(recorded) (int, string) {
		return 200, `{"status":"ok","result":{"memories":[{"uri":"viking://m/1","score":0.9}],"resources":[],"skills":[],"total":1}}`
	})
	c := newTestClient(t, rec.URL)
	res, err := c.Find(context.Background(), FindRequest{Query: "部署负责人", TargetURI: "viking://user/default/huan-agent", Limit: 5})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(res.Memories) != 1 || res.Memories[0].URI != "viking://m/1" {
		t.Errorf("Find result = %+v, want one memory hit", res)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(rec.requests[0].Body), &sent); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if sent["query"] != "部署负责人" || sent["target_uri"] != "viking://user/default/huan-agent" {
		t.Errorf("body = %v", sent)
	}
	if _, ok := sent["score_threshold"]; ok {
		t.Errorf("score_threshold present though unset: %v", sent)
	}
}

func TestFindResultAllCombinesCategories(t *testing.T) {
	res := FindResult{Memories: []FindHit{{URI: "a"}}, Resources: []FindHit{{URI: "b"}}, Skills: []FindHit{{URI: "c"}}}
	if got := len(res.All()); got != 3 {
		t.Errorf("All() len = %d, want 3", got)
	}
}

func TestRememberWritesBatchThenCommits(t *testing.T) {
	rec := newRecorder(t, func(r recorded) (int, string) {
		if strings.HasSuffix(r.Path, "/commit") {
			return 200, `{"status":"ok","result":{"status":"accepted","task_id":"t-1","archived":true}}`
		}
		return 200, `{"status":"ok","result":{"session_id":"huan-agent-ns","message_count":2,"added":2}}`
	})
	c := newTestClient(t, rec.URL)
	out, err := c.Remember(context.Background(), RememberRequest{
		SessionID: "huan-agent-ns",
		Messages:  []Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}},
		Commit:    true,
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if out.Added != 2 || out.TaskID != "t-1" || !out.Archived {
		t.Errorf("Remember result = %+v, want added=2 task=t-1 archived", out)
	}
	if len(rec.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (batch + commit)", len(rec.requests))
	}
	if got, want := rec.requests[0].Path, "/api/v1/sessions/huan-agent-ns/messages/batch"; got != want {
		t.Errorf("batch path = %q, want %q", got, want)
	}
	var commit map[string]any
	if err := json.Unmarshal([]byte(rec.requests[1].Body), &commit); err != nil {
		t.Fatalf("decode commit body: %v", err)
	}
	if commit["keep_recent_count"] != float64(0) {
		t.Errorf("commit body = %v, want keep_recent_count 0", commit)
	}
}

func TestRememberWithoutCommitSkipsCommitCall(t *testing.T) {
	rec := newRecorder(t, nil)
	c := newTestClient(t, rec.URL)
	out, err := c.Remember(context.Background(), RememberRequest{SessionID: "s", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if out.TaskID != "" {
		t.Errorf("TaskID = %q, want empty", out.TaskID)
	}
	if len(rec.requests) != 1 {
		t.Errorf("requests = %d, want 1", len(rec.requests))
	}
}

func TestRememberExposesCommitFailureWithWriteResult(t *testing.T) {
	rec := newRecorder(t, func(r recorded) (int, string) {
		if strings.HasSuffix(r.Path, "/commit") {
			return 500, `{"status":"error","error":{"code":"INTERNAL","message":"boom"}}`
		}
		return 200, `{"status":"ok","result":{"added":1}}`
	})
	c := newTestClient(t, rec.URL)
	out, err := c.Remember(context.Background(), RememberRequest{SessionID: "s", Messages: []Message{{Role: "user", Content: "x"}}, Commit: true})
	if err == nil {
		t.Fatal("Remember with failing commit: want error, got nil")
	}
	if out == nil || out.Added != 1 {
		t.Fatalf("Remember result = %+v, want the write result alongside the error", out)
	}
}

func TestRememberRejectsMissingSessionAndEmptyMessages(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1")
	if _, err := c.Remember(context.Background(), RememberRequest{Messages: []Message{{Role: "user"}}}); err == nil {
		t.Error("Remember without session_id: want error, got nil")
	}
	out, err := c.Remember(context.Background(), RememberRequest{SessionID: "s"})
	if err != nil {
		t.Fatalf("Remember with no messages: %v", err)
	}
	if out.SessionID != "s" {
		t.Errorf("SessionID = %q, want s", out.SessionID)
	}
}

func TestWriteContentSendsModeAndWait(t *testing.T) {
	rec := newRecorder(t, func(recorded) (int, string) {
		return 200, `{"status":"ok","result":{"uri":"viking://user/default/x.md","written_bytes":7,"mode":"create"}}`
	})
	c := newTestClient(t, rec.URL)
	res, err := c.WriteContent(context.Background(), WriteRequest{URI: "viking://user/default/x.md", Content: "hello\n", Wait: true, Tags: []string{"t"}})
	if err != nil {
		t.Fatalf("WriteContent: %v", err)
	}
	if res.WrittenBytes != 7 {
		t.Errorf("WrittenBytes = %d, want 7", res.WrittenBytes)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(rec.requests[0].Body), &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sent["mode"] != "replace" || sent["wait"] != true {
		t.Errorf("body = %v, want mode=replace wait=true", sent)
	}
}

func TestWriteContentRejectsEmptyURI(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1")
	if _, err := c.WriteContent(context.Background(), WriteRequest{Content: "x"}); err == nil {
		t.Error("WriteContent without uri: want error, got nil")
	}
}

func TestReadContentEscapesURI(t *testing.T) {
	rec := newRecorder(t, func(recorded) (int, string) {
		return 200, `{"status":"ok","result":"# hi"}`
	})
	c := newTestClient(t, rec.URL)
	got, err := c.ReadContent(context.Background(), "viking://user/default/a b.md")
	if err != nil {
		t.Fatalf("ReadContent: %v", err)
	}
	if got != "# hi" {
		t.Errorf("ReadContent = %q, want %q", got, "# hi")
	}
	if q := rec.requests[0].Query; !strings.Contains(q, "uri=viking%3A%2F%2Fuser%2Fdefault%2Fa+b.md") {
		t.Errorf("query = %q, want the URI escaped", q)
	}
}

func TestStatReadsMetadata(t *testing.T) {
	rec := newRecorder(t, func(recorded) (int, string) {
		return 200, `{"status":"ok","result":{"name":"x.md","size":42,"uri":"viking://x.md","isDir":false}}`
	})
	c := newTestClient(t, rec.URL)
	st, err := c.Stat(context.Background(), "viking://x.md")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size != 42 || st.Name != "x.md" {
		t.Errorf("Stat = %+v", st)
	}
}

func TestUploadTempSendsMultipartAndReadsID(t *testing.T) {
	rec := newRecorder(t, func(r recorded) (int, string) {
		if !strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Errorf("Content-Type = %q, want multipart", r.Header.Get("Content-Type"))
		}
		if !strings.Contains(r.Body, "hello") || !strings.Contains(r.Body, `filename="note.md"`) {
			t.Errorf("body missing file part: %q", r.Body)
		}
		return 200, `{"status":"ok","result":{"temp_file_id":"upload_abc.md"}}`
	})
	c := newTestClient(t, rec.URL)
	id, err := c.UploadTemp(context.Background(), "note.md", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("UploadTemp: %v", err)
	}
	if id != "upload_abc.md" {
		t.Errorf("temp id = %q, want upload_abc.md", id)
	}
}

func TestUploadTempErrorsWhenNoID(t *testing.T) {
	rec := newRecorder(t, func(recorded) (int, string) {
		return 200, `{"status":"ok","result":{}}`
	})
	c := newTestClient(t, rec.URL)
	if _, err := c.UploadTemp(context.Background(), "a.md", strings.NewReader("x")); err == nil {
		t.Fatal("UploadTemp with no temp_file_id: want error, got nil")
	}
}

func TestAddResourceSendsSourceAndTarget(t *testing.T) {
	rec := newRecorder(t, func(recorded) (int, string) {
		return 200, `{"status":"ok","result":{"status":"success","root_uri":"viking://resources/a.md","warnings":["Memory linking failed"]}}`
	})
	c := newTestClient(t, rec.URL)
	res, err := c.AddResource(context.Background(), AddResourceRequest{
		TempFileID: "upload_1.md",
		To:         "viking://resources/a.md",
		Reason:     "workspace sync",
		Wait:       true,
	})
	if err != nil {
		t.Fatalf("AddResource: %v", err)
	}
	if res.RootURI != "viking://resources/a.md" || len(res.Warnings) != 1 {
		t.Errorf("AddResource = %+v", res)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(rec.requests[0].Body), &sent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sent["temp_file_id"] != "upload_1.md" {
		t.Errorf("body = %v, want the temp_file_id", sent)
	}
	if _, ok := sent["path"]; ok {
		t.Errorf("body sent path though only temp_file_id was set: %v", sent)
	}
}

func TestTaskStatus(t *testing.T) {
	rec := newRecorder(t, func(r recorded) (int, string) {
		if got, want := r.Path, "/api/v1/tasks/t-9"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		return 200, `{"status":"ok","result":{"task_id":"t-9","task_type":"session_commit","status":"failed","error":"ModelNotOpen"}}`
	})
	c := newTestClient(t, rec.URL)
	task, err := c.TaskStatus(context.Background(), "t-9")
	if err != nil {
		t.Fatalf("TaskStatus: %v", err)
	}
	if task.Status != "failed" || task.Error != "ModelNotOpen" {
		t.Errorf("TaskStatus = %+v", task)
	}
}

func TestServerErrorEnvelopeBecomesAPIError(t *testing.T) {
	rec := newRecorder(t, func(recorded) (int, string) {
		return 403, `{"status":"error","result":null,"error":{"code":"PERMISSION_DENIED","message":"nope"}}`
	})
	rec.respHeader = http.Header{"x-request-id": {"req-1"}}
	c := newTestClient(t, rec.URL)
	_, err := c.WriteContent(context.Background(), WriteRequest{URI: "viking://x.md", Content: "y"})
	if err == nil {
		t.Fatal("WriteContent against a 403: want error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Status != 403 || apiErr.Code != "PERMISSION_DENIED" || apiErr.RequestID != "req-1" {
		t.Errorf("APIError = %+v, want 403 PERMISSION_DENIED req-1", apiErr)
	}
	if !strings.Contains(err.Error(), "PERMISSION_DENIED") {
		t.Errorf("Error() = %q, want the code in it", err.Error())
	}
}

func TestNonJSONErrorBodyStillBecomesAPIError(t *testing.T) {
	rec := newRecorder(t, func(recorded) (int, string) {
		return 502, "<html>bad gateway</html>"
	})
	c := newTestClient(t, rec.URL)
	_, err := c.Health(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Status != 502 || !strings.Contains(apiErr.Message, "bad gateway") {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestUnavailableClassification(t *testing.T) {
	// A closed server: the request never completes.
	rec := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := rec.URL
	rec.Close()
	c := newTestClient(t, closedURL)
	_, err := c.Health(context.Background())
	if err == nil {
		t.Fatal("Health against a closed server: want error, got nil")
	}
	if !IsUnavailable(err) {
		t.Errorf("IsUnavailable(%v) = false, want true", err)
	}

	// A 5xx is unavailability; a 4xx is not.
	if !IsUnavailable(&APIError{Status: 503, Code: "INTERNAL"}) {
		t.Error("IsUnavailable(503) = false, want true")
	}
	if IsUnavailable(&APIError{Status: 400, Code: "INVALID_URI"}) {
		t.Error("IsUnavailable(400 INVALID_URI) = true, want false")
	}
	if IsUnavailable(nil) {
		t.Error("IsUnavailable(nil) = true, want false")
	}
}

func TestTimeoutIsUnavailable(t *testing.T) {
	rec := newRecorder(t, func(recorded) (int, string) {
		time.Sleep(200 * time.Millisecond)
		return 200, `{"status":"ok","result":null}`
	})
	hc := &http.Client{Timeout: 20 * time.Millisecond}
	c, err := New(Config{BaseURL: rec.URL, HTTPClient: hc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, ferr := c.Health(context.Background())
	if ferr == nil {
		t.Fatal("Health with a 20ms timeout: want error, got nil")
	}
	if !IsUnavailable(ferr) {
		t.Errorf("IsUnavailable(%v) = false, want true", ferr)
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(&APIError{Status: 404}) {
		t.Error("IsNotFound(404) = false, want true")
	}
	if !IsNotFound(&APIError{Code: "NotFound"}) {
		t.Error("IsNotFound(NotFound) = false, want true")
	}
	if IsNotFound(errors.New("other")) {
		t.Error("IsNotFound(other) = true, want false")
	}
}

func TestMalformedResultBecomesAPIError(t *testing.T) {
	rec := newRecorder(t, func(recorded) (int, string) {
		return 200, `{"status":"ok","result":"not-an-object"}`
	})
	c := newTestClient(t, rec.URL)
	_, err := c.Stat(context.Background(), "viking://x.md")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Code != "BAD_RESPONSE" {
		t.Errorf("Code = %q, want BAD_RESPONSE", apiErr.Code)
	}
}

func TestForgetUsesDelete(t *testing.T) {
	rec := newRecorder(t, nil)
	c := newTestClient(t, rec.URL)
	if err := c.Forget(context.Background(), "viking://x.md"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if got := rec.requests[0].Method; got != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", got)
	}
}

func TestErrorTextNeverContainsKey(t *testing.T) {
	rec := newRecorder(t, func(recorded) (int, string) {
		return 500, `{"status":"error","error":{"code":"BOOM","message":"server exploded"}}`
	})
	c, err := New(Config{BaseURL: rec.URL, APIKey: "super-secret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, ferr := c.Health(context.Background())
	if strings.Contains(fmt.Sprint(ferr), "super-secret") {
		t.Errorf("error leaked the API key: %v", ferr)
	}
}
