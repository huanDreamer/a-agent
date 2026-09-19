package server

// HTTP-level tests for the artifact routes.
//
// These cover what internal/artifact's unit tests cannot: the serving path is
// where a stored file becomes bytes on a socket, and that is where the security
// decisions actually live.
//
// Three of them matter beyond "does the route answer":
//
//  1. a traversal attempt must not escape the root. internal/artifact refuses it;
//     this test proves the refusal is reached through the handler rather than
//     bypassed by it — the handler strips a leading slash and hands the rest to
//     the store as data, so the order is easy to get wrong.
//  2. an HTML artifact must carry the sandbox CSP. Without it a page the model
//     wrote runs as the logged-in console and can call the API as the operator.
//  3. with public_urls off, the file route must require a session even though it
//     is registered on the open group — that registration is deliberate (the
//     route checks auth itself so the same path need not be registered twice),
//     and this is the test that keeps it honest.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/huan/huan-agent/internal/artifact"
	"github.com/huan/huan-agent/internal/config"
	"github.com/huan/huan-agent/internal/pricing"
	"github.com/huan/huan-agent/internal/store"
	"github.com/huan/huan-agent/internal/tool"
)

// buildArtifactServer is buildServer with the artifact store turned on: a root
// in the test's temp dir, whose path is returned so a test can put a file there.
func buildArtifactServer(t *testing.T, seed func(store.Store)) (*Server, store.Store, string) {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if seed != nil {
		seed(st)
	}

	root := filepath.Join(t.TempDir(), "artifacts")

	hash, err := HashPassword(adminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	srv, err := New(Config{
		Host:          "127.0.0.1",
		Port:          0,
		MetricsEnable: true,
		Version:       "test-version",
		Provider:      "deepseek",
		Model:         "deepseek-chat",
		Logger:        zap.NewNop(),
		Artifacts: ArtifactSettings{
			Enable: true,
			Root:   root,
		},
	}, st, pricing.NewTable(map[string]pricing.Rate{
		"deepseek/deepseek-chat": {PromptPer1K: 0.001, CompletionPer1K: 0.002},
	}, pricing.Rate{}), config.AdminConfig{
		Username:     "admin",
		PasswordHash: hash,
		// TrustLoopback is left false on purpose: the 401 test below is only
		// meaningful if a request from this machine still has to log in.
		RequireLogin: true,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv, st, root
}

// artifactLogin logs in and returns a cookie-carrying client.
func artifactLogin(t *testing.T, srv *Server) *http.Client {
	t.Helper()
	client := newJar(t)
	body, err := json.Marshal(map[string]string{"password": adminPassword})
	if err != nil {
		t.Fatalf("marshal login: %v", err)
	}
	resp, err := client.Post("http://"+srv.Addr()+"/api/login", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	return client
}

// seedArtifact writes a file under root and its row into the store, which is what
// the serving route needs: the row is the index, the file is the bytes.
func seedArtifact(t *testing.T, st store.Store, root string, a store.Artifact) {
	t.Helper()
	const content = "<h1>hello</h1>"
	abs := filepath.Join(root, filepath.FromSlash(a.Path))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	if a.ID == "" {
		a.ID = "art-1"
	}
	if a.Kind == "" {
		a.Kind = store.ArtifactKindHTML
	}
	if a.MIME == "" {
		a.MIME = "text/html; charset=utf-8"
	}
	if a.Bytes == 0 {
		a.Bytes = int64(len(content))
	}
	if a.Source == "" {
		a.Source = "save_artifact"
	}
	a.CreatedAt = time.Now().UTC()
	if err := st.CreateArtifact(context.Background(), a); err != nil {
		t.Fatalf("create artifact: %v", err)
	}
}

// decodeStatus asserts the status, decodes the body into v and closes it.
//
// requireStatus closes the body as part of failing, so it cannot be paired with
// a decode the way the other tests do: the pair here is one call for exactly
// that reason.
func decodeStatus(t *testing.T, resp *http.Response, want int, v any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, want, buf.String())
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestArtifactListAndGet(t *testing.T) {
	const sess = "sess-1"
	srv, st, root := buildArtifactServer(t, nil)
	seedArtifact(t, st, root, store.Artifact{
		SessionID: sess, Title: "报告", Path: sess + "/report.html",
	})
	startHarness(t, srv)
	client := artifactLogin(t, srv)
	base := "http://" + srv.Addr()

	var list struct {
		OK      bool   `json:"ok"`
		Enabled bool   `json:"enabled"`
		Count   int    `json:"count"`
		Session string `json:"session"`
		// Distinct field names: Session above is the echo of the query, this is
		// the artifact's owner. Declaring both keeps the test from passing on a
		// response that answered the wrong key.
		Artifacts []struct {
			ID           string `json:"id"`
			Title        string `json:"title"`
			Path         string `json:"path"`
			URL          string `json:"url"`
			SessionID    string `json:"session_id"`
			Kind         string `json:"kind"`
			SessionTitle string `json:"session_title"`
		} `json:"artifacts"`
	}
	resp, err := client.Get(base + "/api/artifacts?session=" + sess)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	decodeStatus(t, resp, http.StatusOK, &list)

	if !list.OK || !list.Enabled || list.Count != 1 || len(list.Artifacts) != 1 {
		t.Fatalf("list = %+v", list)
	}

	got := list.Artifacts[0]
	// The URL must be the serving route, built from the same constant the model
	// is handed — a link the console renders and a route that exists cannot drift.
	if got.URL != "/api/artifacts/files/sess-1/report.html" {
		t.Errorf("url = %q", got.URL)
	}
	if got.SessionID != sess {
		t.Errorf("session_id = %q", got.SessionID)
	}

	// One session's view must not leak another's.
	var other struct {
		Count     int `json:"count"`
		Artifacts []struct {
			ID string `json:"id"`
		} `json:"artifacts"`
	}
	resp2, err := client.Get(base + "/api/artifacts?session=sess-2")
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	decodeStatus(t, resp2, http.StatusOK, &other)
	if other.Count != 0 || len(other.Artifacts) != 0 {
		t.Errorf("session sess-2 saw %d artifacts", other.Count)
	}

	// Omitting the filter is 产物中心: every session's artifacts.
	var all struct {
		Count int `json:"count"`
	}
	resp3, err := client.Get(base + "/api/artifacts")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	decodeStatus(t, resp3, http.StatusOK, &all)
	if all.Count != 1 {
		t.Errorf("unfiltered count = %d, want 1", all.Count)
	}

	var one struct {
		Artifact struct {
			Title string `json:"title"`
		} `json:"artifact"`
	}
	resp4, err := client.Get(base + "/api/artifacts/art-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	decodeStatus(t, resp4, http.StatusOK, &one)
	if one.Artifact.Title != "报告" {
		t.Errorf("title = %q", one.Artifact.Title)
	}
}

func TestArtifactFileServingCarriesSandboxHeaders(t *testing.T) {
	srv, st, root := buildArtifactServer(t, nil)
	seedArtifact(t, st, root, store.Artifact{SessionID: "s1", Path: "s1/page.html"})
	startHarness(t, srv)
	client := artifactLogin(t, srv)

	resp, err := client.Get("http://" + srv.Addr() + "/api/artifacts/files/s1/page.html")
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	// Artifact bytes are chosen by the model, so the browser must not re-sniff.
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing nosniff")
	}
	// The whole point: a page the model wrote must not run as this origin.
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") || strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("csp = %q", csp)
	}
}

func TestArtifactFileRefusesTraversal(t *testing.T) {
	srv, st, root := buildArtifactServer(t, nil)
	seedArtifact(t, st, root, store.Artifact{SessionID: "s1", Path: "s1/page.html"})

	// A file outside the root, which traversal would reach if the guard failed.
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(secret, []byte("do not serve"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	startHarness(t, srv)
	client := artifactLogin(t, srv)
	base := "http://" + srv.Addr()

	for _, p := range []string{
		"../secret.txt",
		"s1/../../secret.txt",
		"%2e%2e/secret.txt",
		"..%2fsecret.txt",
	} {
		resp, err := client.Get(base + "/api/artifacts/files/" + p)
		if err != nil {
			t.Fatalf("get %s: %v", p, err)
		}
		body := make([]byte, 256)
		n, _ := resp.Body.Read(body)
		_ = resp.Body.Close()
		// One-directional on purpose: whether the refusal is a 404 from the store
		// or a 404 from path normalization is not the contract — that the bytes
		// never leave the root is.
		if strings.Contains(string(body[:n]), "do not serve") {
			t.Errorf("traversal %q served a file outside the root (status %d)", p, resp.StatusCode)
		}
		if resp.StatusCode == http.StatusOK {
			t.Errorf("traversal %q answered 200", p)
		}
	}
}

func TestArtifactFileRequiresSessionUnlessPublic(t *testing.T) {
	srv, st, root := buildArtifactServer(t, nil)
	seedArtifact(t, st, root, store.Artifact{SessionID: "s1", Path: "s1/page.html"})
	startHarness(t, srv)

	// No login: the file route is registered on the open group but must ask.
	resp, err := http.Get("http://" + srv.Addr() + "/api/artifacts/files/s1/page.html")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated file status = %d, want 401", resp.StatusCode)
	}

	// The listing must ask too, and with the same answer.
	resp2, err := http.Get("http://" + srv.Addr() + "/api/artifacts")
	if err != nil {
		t.Fatalf("get list: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status = %d, want 401", resp2.StatusCode)
	}
}

func TestArtifactFileRefusesUnknownPath(t *testing.T) {
	srv, st, root := buildArtifactServer(t, nil)
	seedArtifact(t, st, root, store.Artifact{SessionID: "s1", Path: "s1/page.html"})
	startHarness(t, srv)
	client := artifactLogin(t, srv)

	resp, err := client.Get("http://" + srv.Addr() + "/api/artifacts/files/s1/nope.html")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestArtifactsDisabledAnswersEnabledFalse(t *testing.T) {
	// A feature that is off is a state the console renders, not a 404 it cannot
	// tell apart from a broken deployment.
	srv, _ := buildServer(t, "", "", nil)
	startHarness(t, srv)
	client := artifactLogin(t, srv)

	resp, err := client.Get("http://" + srv.Addr() + "/api/artifacts")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var body struct {
		OK      bool   `json:"ok"`
		Enabled bool   `json:"enabled"`
		Message string `json:"message"`
	}
	decodeStatus(t, resp, http.StatusOK, &body)
	if body.Enabled {
		t.Fatal("enabled = true on a server with no artifact store")
	}
	if body.Message == "" {
		t.Error("disabled response carries no explanation")
	}

	// The file route has no such state to render, so it 404s rather than
	// pretending it could have served something.
	resp2, err := client.Get("http://" + srv.Addr() + "/api/artifacts/files/anything.html")
	if err != nil {
		t.Fatalf("get file: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("disabled file route status = %d, want 404", resp2.StatusCode)
	}
}

func TestArtifactDeleteRemovesRowAndFile(t *testing.T) {
	srv, st, root := buildArtifactServer(t, nil)
	seedArtifact(t, st, root, store.Artifact{SessionID: "s1", Path: "s1/page.html"})
	startHarness(t, srv)
	client := artifactLogin(t, srv)
	base := "http://" + srv.Addr()

	req, err := http.NewRequest(http.MethodDelete, base+"/api/artifacts/art-1", nil)
	if err != nil {
		t.Fatalf("new delete: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	var out struct {
		OK          bool   `json:"ok"`
		Deleted     string `json:"deleted"`
		FileRemoved bool   `json:"file_removed"`
	}
	decodeStatus(t, resp, http.StatusOK, &out)

	if !out.OK || out.Deleted != "art-1" {
		t.Errorf("delete response = %+v", out)
	}
	if !out.FileRemoved {
		t.Error("file_removed = false")
	}
	if _, err := os.Stat(filepath.Join(root, "s1", "page.html")); !os.IsNotExist(err) {
		t.Errorf("file still present after delete: %v", err)
	}
	if _, err := st.GetArtifact(context.Background(), "art-1"); err == nil {
		t.Error("row still present after delete")
	}

	// Deleting twice is a 404, not a second success.
	req2, err := http.NewRequest(http.MethodDelete, base+"/api/artifacts/art-1", nil)
	if err != nil {
		t.Fatalf("new delete 2: %v", err)
	}
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("delete 2: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", resp2.StatusCode)
	}
}

func TestArtifactGetUnknownIsNotFound(t *testing.T) {
	srv, _, _ := buildArtifactServer(t, nil)
	startHarness(t, srv)
	client := artifactLogin(t, srv)

	resp, err := client.Get("http://" + srv.Addr() + "/api/artifacts/missing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestArtifactSavedThroughSaverIsServable closes the loop the other tests leave
// open: they seed a row and a file by hand, so a bug in how a saved artifact is
// filed — the path prefix, the recorded MIME, the URL the model is handed — would
// pass every one of them. Here the bytes go in through the same Saver the turn
// publishes, and come back out through the serving route.
func TestArtifactSavedThroughSaverIsServable(t *testing.T) {
	srv, st, root := buildArtifactServer(t, nil)
	startHarness(t, srv)
	client := artifactLogin(t, srv)
	base := "http://" + srv.Addr()

	// The store the server built, reached the way a turn reaches it.
	files, err := artifact.New(root, artifact.Options{})
	if err != nil {
		t.Fatalf("artifact.New: %v", err)
	}
	const sess = "sess-saver"
	saver, err := artifact.NewSaver(files, st, artifact.SaverOptions{
		Owner:     sess,
		URLPrefix: artifactsURLPrefix,
	})
	if err != nil {
		t.Fatalf("NewSaver: %v", err)
	}

	const html = "<!doctype html><html><body><h1>巡检报告</h1></body></html>"
	res, err := saver.SaveArtifact(context.Background(), tool.ArtifactInput{
		Title:   "巡检报告",
		Name:    "report.html",
		Kind:    store.ArtifactKindHTML,
		Source:  "save_artifact",
		Content: strings.NewReader(html),
	})
	if err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}
	if res.URL == "" {
		t.Fatal("saver returned no URL")
	}
	if !strings.HasPrefix(res.Path, sess+"/") {
		t.Errorf("path = %q, want it under %s/", res.Path, sess)
	}

	// What the saver handed the model must be exactly what the route serves.
	resp, err := client.Get(base + res.URL)
	if err != nil {
		t.Fatalf("get saved artifact: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status for %s = %d, want 200", res.URL, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != html {
		t.Errorf("served body = %q", string(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}

	// And it must be listed under its session, with the same URL.
	var list struct {
		Count     int `json:"count"`
		Artifacts []struct {
			Title  string `json:"title"`
			URL    string `json:"url"`
			MIME   string `json:"mime"`
			Bytes  int64  `json:"bytes"`
			Kind   string `json:"kind"`
			Source string `json:"source"`
		} `json:"artifacts"`
	}
	listed, err := client.Get(base + "/api/artifacts?session=" + sess)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	decodeStatus(t, listed, http.StatusOK, &list)
	if list.Count != 1 {
		t.Fatalf("count = %d, want 1", list.Count)
	}
	got := list.Artifacts[0]
	if got.URL != res.URL {
		t.Errorf("listed url = %q, saver said %q", got.URL, res.URL)
	}
	if got.Kind != store.ArtifactKindHTML {
		t.Errorf("kind = %q", got.Kind)
	}
	if got.Bytes != int64(len(html)) {
		t.Errorf("bytes = %d, want %d", got.Bytes, len(html))
	}
	if got.Source != "save_artifact" {
		t.Errorf("source = %q", got.Source)
	}

	// Deleting it through the API must take the file with it, end to end.
	req, err := http.NewRequest(http.MethodDelete, base+"/api/artifacts/"+idOfFirst(t, client, base, sess), nil)
	if err != nil {
		t.Fatalf("new delete: %v", err)
	}
	del, err := client.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	var out struct {
		FileRemoved bool `json:"file_removed"`
	}
	decodeStatus(t, del, http.StatusOK, &out)
	if !out.FileRemoved {
		t.Error("file_removed = false")
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(res.Path))); !os.IsNotExist(err) {
		t.Errorf("file still present after delete: %v", err)
	}
}

// idOfFirst returns the id of the only artifact listed for a session.
func idOfFirst(t *testing.T, client *http.Client, base, session string) string {
	t.Helper()
	resp, err := client.Get(base + "/api/artifacts?session=" + session)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list struct {
		Artifacts []struct {
			ID string `json:"id"`
		} `json:"artifacts"`
	}
	decodeStatus(t, resp, http.StatusOK, &list)
	if len(list.Artifacts) != 1 {
		t.Fatalf("want exactly 1 artifact, got %d", len(list.Artifacts))
	}
	return list.Artifacts[0].ID
}
