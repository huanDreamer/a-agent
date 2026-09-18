package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fake language server over an in-memory duplex pipe.
//
// It exists so the protocol layer can be tested exactly and fast: real language
// servers are large external binaries that may not be installed, and a protocol
// test that needs one is a test that gets skipped. Everything that can go wrong
// here — a frame split across two writes, two frames in one write, an answer
// that never comes, a server that asks the client a question — is scripted
// rather than hoped for.

type pipeTransport struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p *pipeTransport) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeTransport) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeTransport) Close() error {
	_ = p.r.Close()
	return p.w.Close()
}

type fakeServer struct {
	t          *testing.T
	fromClient *io.PipeReader
	toClient   *io.PipeWriter

	mu       sync.Mutex
	handlers map[string]func(params json.RawMessage) (any, *rpcError)
	received []message
	// autoAnswer controls whether requests with no handler get an empty result.
	// false makes them hang, which is how "a slow server" is scripted.
	autoAnswer bool
}

// newFakeClient wires a Client to a fake server and completes the handshake.
func newFakeClient(t *testing.T, opts Options) (*Client, *fakeServer) {
	t.Helper()

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()

	f := &fakeServer{
		t:          t,
		fromClient: c2sR,
		toClient:   s2cW,
		handlers:   map[string]func(json.RawMessage) (any, *rpcError){},
		autoAnswer: true,
	}
	go f.serve()

	if opts.Name == "" {
		opts.Name = "fake"
	}
	if opts.Root == "" {
		opts.Root = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewClient(ctx, &pipeTransport{r: s2cR, w: c2sW}, opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, f
}

// handle registers a response for one method.
func (f *fakeServer) handle(method string, fn func(json.RawMessage) (any, *rpcError)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = fn
}

// serve reads frames until the pipe closes.
func (f *fakeServer) serve() {
	reader := bufio.NewReader(f.fromClient)
	for {
		payload, err := readFrame(reader)
		if err != nil {
			return
		}
		var m message
		if err := json.Unmarshal(payload, &m); err != nil {
			continue
		}
		f.mu.Lock()
		f.received = append(f.received, m)
		h := f.handlers[m.Method]
		auto := f.autoAnswer
		f.mu.Unlock()

		if m.ID == nil {
			continue // a notification: nothing to answer
		}
		f.answer(m, h, auto)
	}
}

func (f *fakeServer) answer(m message, h func(json.RawMessage) (any, *rpcError), auto bool) {
	if m.Method == "initialize" && h == nil {
		f.send(message{ID: m.ID, Result: json.RawMessage(`{"capabilities":{
			"textDocumentSync":1,"definitionProvider":true,"referencesProvider":true,
			"workspaceSymbolProvider":true},"serverInfo":{"name":"fake"}}`)})
		return
	}
	if m.Method == "shutdown" && h == nil {
		f.send(message{ID: m.ID, Result: json.RawMessage(`null`)})
		return
	}
	if h == nil {
		if !auto {
			return // hang: the caller is testing a timeout
		}
		f.send(message{ID: m.ID, Result: json.RawMessage(`null`)})
		return
	}
	result, rpcErr := h(m.Params)
	f.send(message{ID: m.ID, Result: mustParams(result), Error: rpcErr})
}

// send writes one frame to the client.
func (f *fakeServer) send(m message) {
	m.JSONRPC = "2.0"
	payload, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = writeFrame(f.toClient, payload)
}

// push sends a notification to the client.
func (f *fakeServer) push(method string, params any) {
	f.send(message{Method: method, Params: mustParams(params)})
}

// publishDiagnostics pushes a diagnostics report.
func (f *fakeServer) publishDiagnostics(uri string, diags []map[string]any) {
	if diags == nil {
		diags = []map[string]any{}
	}
	f.push("textDocument/publishDiagnostics", map[string]any{"uri": uri, "diagnostics": diags})
}

// waitFor blocks until the client has sent a message with this method.
func (f *fakeServer) waitFor(method string) message {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, m := range f.received {
			if m.Method == method {
				f.mu.Unlock()
				return m
			}
		}
		f.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("the client never sent %s (received: %v)", method, f.methods())
	return message{}
}

func (f *fakeServer) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.received))
	for _, m := range f.received {
		out = append(out, m.Method)
	}
	return out
}

// count returns how many messages with this method arrived.
func (f *fakeServer) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.received {
		if m.Method == method {
			n++
		}
	}
	return n
}

// writeFile puts a file in the server root and returns its path.
func writeFile(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestClientHandshake: initialize is sent, its capabilities are decoded, and
// initialized follows. Capabilities are what decide which tools exist, so a
// mis-decode here silently removes a capability the server actually has.
func TestClientHandshake(t *testing.T) {
	c, f := newFakeClient(t, Options{Name: "fake"})

	caps := c.Capabilities()
	if !caps.DefinitionProvider || !caps.ReferencesProvider || !caps.WorkspaceSymbol {
		t.Errorf("capabilities not decoded: %+v", caps)
	}
	if caps.SyncKind != 1 {
		t.Errorf("sync kind = %d, want 1", caps.SyncKind)
	}
	if caps.RenameProvider {
		t.Error("rename was not advertised by the fake; it must not be reported as supported")
	}
	f.waitFor("initialized")

	// The initialize params must carry the root, or the server indexes nothing.
	init := f.waitFor("initialize")
	var p InitializeParams
	if err := json.Unmarshal(init.Params, &p); err != nil {
		t.Fatalf("initialize params: %v", err)
	}
	if p.RootURI == "" || p.RootPath == "" {
		t.Errorf("initialize did not name a root: %+v", p)
	}
	if len(p.WorkspaceFolders) != 1 {
		t.Errorf("workspace folders = %+v, want one", p.WorkspaceFolders)
	}
}

// TestServerCapabilitiesObjectForm: the protocol lets a capability be an object
// instead of a boolean, which means "supported, here are my options". Reading
// only booleans would drop every capability of a server that answers that way.
func TestServerCapabilitiesObjectForm(t *testing.T) {
	// The client is not involved: this is a parsing question about a payload the
	// protocol allows and servers do send.
	var caps ServerCapabilities
	raw := json.RawMessage(`{"textDocumentSync":{"openClose":true,"change":2},
		"definitionProvider":{"workDoneProgress":true},
		"referencesProvider":true,"renameProvider":{"prepareProvider":true}}`)
	if err := json.Unmarshal(raw, &caps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if caps.SyncKind != 2 {
		t.Errorf("sync kind = %d, want 2 from the object form", caps.SyncKind)
	}
	if !caps.DefinitionProvider {
		t.Error("an options object means the capability is offered")
	}
	if !caps.ReferencesProvider || !caps.RenameProvider {
		t.Errorf("capabilities wrong: %+v", caps)
	}
	if caps.WorkspaceSymbol {
		t.Error("workspaceSymbolProvider was absent and must not be reported")
	}
}

// TestRequestResponseCorrelation: answers are matched by id, so two requests in
// flight cannot swap their results. This is the failure that would show a
// definition where a reference was asked for.
func TestRequestResponseCorrelation(t *testing.T) {
	root := t.TempDir()
	c, f := newFakeClient(t, Options{Root: root})
	path := writeFile(t, root, "a.go", "package a\n\nfunc Foo() {}\n")

	f.handle("textDocument/definition", func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"uri": PathToURI(path), "range": map[string]any{
			"start": map[string]int{"line": 2, "character": 5},
			"end":   map[string]int{"line": 2, "character": 8}}}, nil
	})
	f.handle("textDocument/references", func(json.RawMessage) (any, *rpcError) {
		return []map[string]any{
			{"uri": PathToURI(path), "range": map[string]any{
				"start": map[string]int{"line": 0, "character": 0},
				"end":   map[string]int{"line": 0, "character": 1}}},
			{"uri": PathToURI(path), "range": map[string]any{
				"start": map[string]int{"line": 2, "character": 5},
				"end":   map[string]int{"line": 2, "character": 8}}},
		}, nil
	})

	ctx := context.Background()
	defs, err := c.Definition(ctx, path, LineCol{3, 6})
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(defs) != 1 || defs[0].Range.Start.Line != 2 {
		t.Errorf("definition = %+v", defs)
	}
	refs, err := c.References(ctx, path, LineCol{3, 6}, false)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("references = %+v, want 2", refs)
	}
	// The request must have opened the document first: a server answers about
	// the text it was given, not about the file on disk.
	f.waitForCount("textDocument/didOpen", 1)
}

// TestDefinitionSingleObjectAndNull: the protocol allows one location, an array,
// or null, and servers use all three.
func TestDefinitionShapes(t *testing.T) {
	root := t.TempDir()
	c, f := newFakeClient(t, Options{Root: root})
	path := writeFile(t, root, "a.go", "package a\n")

	loc := map[string]any{"uri": PathToURI(path), "range": map[string]any{
		"start": map[string]int{"line": 0, "character": 0},
		"end":   map[string]int{"line": 0, "character": 3}}}

	// A single object rather than an array.
	f.handle("textDocument/definition", func(json.RawMessage) (any, *rpcError) { return loc, nil })
	got, err := c.Definition(context.Background(), path, LineCol{1, 1})
	if err != nil || len(got) != 1 {
		t.Errorf("single-object result: %+v, %v", got, err)
	}

	// null.
	f.handle("textDocument/definition", func(json.RawMessage) (any, *rpcError) { return nil, nil })
	got, err = c.Definition(context.Background(), path, LineCol{1, 1})
	if err != nil || len(got) != 0 {
		t.Errorf("null result should be an empty answer, got %+v, %v", got, err)
	}

	// LocationLink, which some servers always answer with.
	f.handle("textDocument/definition", func(json.RawMessage) (any, *rpcError) {
		return []map[string]any{{"targetUri": PathToURI(path), "targetRange": map[string]any{
			"start": map[string]int{"line": 1, "character": 2},
			"end":   map[string]int{"line": 1, "character": 5}}}}, nil
	})
	got, err = c.Definition(context.Background(), path, LineCol{1, 1})
	if err != nil {
		t.Fatalf("LocationLink form: %v", err)
	}
	if len(got) != 1 || got[0].Range.Start.Line != 1 {
		t.Errorf("LocationLink not decoded: %+v", got)
	}
}

// TestServerRequestGetsAnAnswer is the "do not hang the server" rule: a language
// server can send requests of its own, and one that never gets an answer waits
// for it — which shows up as every later request being slow.
func TestServerRequestGetsAnAnswer(t *testing.T) {
	c, f := newFakeClient(t, Options{Name: "fake"})
	_ = c

	// The server asks the client to create a progress token. The client must
	// answer, with an error saying it does not support it.
	f.send(message{ID: &jsonID{num: 9001}, Method: "window/workDoneProgress/create",
		Params: json.RawMessage(`{"token":"t1"}`)})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range f.receivedResponses() {
			if m.ID != nil && m.ID.num == 9001 {
				if m.Error == nil || m.Error.Code != -32601 {
					t.Errorf("expected a method-not-found error, got %+v", m)
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the client never answered the server's request; the server would wait for it")
}

// TestConnectionCloseWakesWaiters: the property that keeps a crashed language
// server from turning into a hung agent. Every pending request has to be failed,
// not abandoned.
func TestConnectionCloseWakesWaiters(t *testing.T) {
	root := t.TempDir()
	c, f := newFakeClient(t, Options{Root: root})
	path := writeFile(t, root, "a.go", "package a\n")

	f.mu.Lock()
	f.autoAnswer = false // the server will not answer
	f.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, err := c.Definition(context.Background(), path, LineCol{1, 1})
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	// The server dies.
	_ = f.toClient.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a request against a dead server must fail")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the request hung after the connection closed; this is the shape of an unreproducible agent hang")
	}
}

// TestRequestTimeout: a server that accepts a request and never answers must not
// hold the turn forever.
func TestRequestTimeout(t *testing.T) {
	root := t.TempDir()
	c, f := newFakeClient(t, Options{Root: root, RequestTimeout: 80 * time.Millisecond})
	path := writeFile(t, root, "a.go", "package a\n")

	f.mu.Lock()
	f.autoAnswer = false
	f.mu.Unlock()

	start := time.Now()
	_, err := c.Definition(context.Background(), path, LineCol{1, 1})
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the timeout took %v; it should have honoured the 80ms budget", elapsed)
	}
}

// TestDiagnosticsCaughtUpLogic is the heart of the edit-feedback design: the
// server pushes diagnostics asynchronously, so "are these about the file as it
// is now?" is a counter comparison, not a guess.
func TestDiagnosticsCaughtUpLogic(t *testing.T) {
	root := t.TempDir()
	c, f := newFakeClient(t, Options{Root: root})
	path := writeFile(t, root, "a.go", "package a\n\nvar X = 1\n")
	uri := PathToURI(path)

	// Nothing has been published and nothing changed: unknown, not "clean".
	if _, caughtUp := c.Diagnostics(path); caughtUp {
		t.Error("with no publish at all, caughtUp must be false: claiming clean is a lie")
	}

	// Open the file (a change), then publish.
	if err := c.Open(context.Background(), path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.publishDiagnostics(uri, []map[string]any{{
		"range":    map[string]any{"start": map[string]int{"line": 2, "character": 4}, "end": map[string]int{"line": 2, "character": 5}},
		"severity": SeverityError, "message": "undefined: X", "source": "compiler",
		"code": "U1000",
	}})
	waitForCondition(t, func() bool {
		_, ok := c.Diagnostics(path)
		return ok
	}, "diagnostics to be published")

	items, caughtUp := c.Diagnostics(path)
	if !caughtUp {
		t.Error("after a publish that answers our open, the diagnostics are current")
	}
	if len(items) != 1 || items[0].Message != "undefined: X" || items[0].Source != "compiler" {
		t.Errorf("diagnostics = %+v", items)
	}
	if items[0].Code != "U1000" {
		t.Errorf("a string code was not decoded: %q", items[0].Code)
	}

	// Now change the file: the previous publish no longer describes it, and
	// claiming otherwise is exactly how a fixed error keeps being reported.
	if err := c.Open(context.Background(), path); err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if _, caughtUp := c.Diagnostics(path); caughtUp {
		t.Error("after a change with no new publish, caughtUp must be false")
	}

	// An empty array is a real report: the file is clean now.
	f.publishDiagnostics(uri, nil)
	waitForCondition(t, func() bool {
		_, ok := c.Diagnostics(path)
		return ok
	}, "the clean publish to arrive")
	items, caughtUp = c.Diagnostics(path)
	if !caughtUp || len(items) != 0 {
		t.Errorf("an empty publish means clean and current, got %d items caughtUp=%v", len(items), caughtUp)
	}
}

// TestWaitDiagnosticsSettlesAfterTheChange: the sequence a real server produces
// is an empty publish first and the real one a moment later. Returning on the
// first would report "no problems" for a file that has one.
func TestWaitDiagnosticsSettlesAfterTheChange(t *testing.T) {
	root := t.TempDir()
	c, f := newFakeClient(t, Options{Root: root, SettleWindow: 40 * time.Millisecond})
	path := writeFile(t, root, "a.go", "package a\nvar X = 1\n")
	uri := PathToURI(path)

	if err := c.Open(context.Background(), path); err != nil {
		t.Fatalf("Open: %v", err)
	}

	go func() {
		f.publishDiagnostics(uri, nil) // the "still thinking" empty report
		time.Sleep(20 * time.Millisecond)
		f.publishDiagnostics(uri, []map[string]any{{
			"range":    map[string]any{"start": map[string]int{"line": 1, "character": 4}, "end": map[string]int{"line": 1, "character": 5}},
			"severity": SeverityError, "message": "undefined: X",
		}})
	}()

	items, settled := c.WaitDiagnostics(context.Background(), path, time.Second)
	if !settled {
		t.Fatal("expected the diagnostics to settle")
	}
	if len(items) != 1 || items[0].Message != "undefined: X" {
		t.Errorf("settled on the wrong report: %+v (the empty first report must not win)", items)
	}
}

// TestWaitDiagnosticsTimesOut: a server that never publishes must not hang the
// tool. "Not settled" is an honest answer the caller can report; waiting forever
// is not.
func TestWaitDiagnosticsTimesOut(t *testing.T) {
	root := t.TempDir()
	c, _ := newFakeClient(t, Options{Root: root, SettleWindow: 30 * time.Millisecond})
	path := writeFile(t, root, "a.go", "package a\n")
	if err := c.Open(context.Background(), path); err != nil {
		t.Fatalf("Open: %v", err)
	}

	start := time.Now()
	items, settled := c.WaitDiagnostics(context.Background(), path, 120*time.Millisecond)
	if settled {
		t.Error("nothing was published, so nothing can be settled")
	}
	if items != nil {
		t.Errorf("items = %+v, want none", items)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("WaitDiagnostics took %v; it must honour its maxWait", elapsed)
	}
}

// TestWaitDiagnosticsHonoursContext: a cancelled turn must not be held open by a
// language server.
func TestWaitDiagnosticsHonoursContext(t *testing.T) {
	root := t.TempDir()
	c, _ := newFakeClient(t, Options{Root: root, SettleWindow: 30 * time.Millisecond})
	path := writeFile(t, root, "a.go", "package a\n")
	if err := c.Open(context.Background(), path); err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		c.WaitDiagnostics(ctx, path, 5*time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitDiagnostics ignored a cancelled context")
	}
}

// TestOpenSendsFullContentOnEveryChange: the server must never be looking at a
// version of the file the agent has already rewritten.
func TestOpenSendsFullContentOnEveryChange(t *testing.T) {
	root := t.TempDir()
	c, f := newFakeClient(t, Options{Root: root})
	path := writeFile(t, root, "a.go", "package a\n")

	if err := c.Open(context.Background(), path); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	f.waitForCount("textDocument/didOpen", 1)
	if f.count("textDocument/didOpen") != 1 || f.count("textDocument/didChange") != 0 {
		t.Fatalf("first open should be a didOpen: %v", f.methods())
	}

	writeFile(t, root, "a.go", "package a\n\nfunc Added() {}\n")
	if err := c.Open(context.Background(), path); err != nil {
		t.Fatalf("second Open: %v", err)
	}
	f.waitForCount("textDocument/didChange", 1)
	if f.count("textDocument/didChange") != 1 {
		t.Fatalf("a re-open must send didChange: %v", f.methods())
	}

	// The change must carry the new content, not a diff.
	f.mu.Lock()
	var last message
	for _, m := range f.received {
		if m.Method == "textDocument/didChange" {
			last = m
		}
	}
	f.mu.Unlock()
	if !strings.Contains(string(last.Params), "func Added") {
		t.Errorf("didChange did not carry the current content: %s", last.Params)
	}

	// Forget makes the next Open a fresh didOpen, which is what a caller does
	// after replacing a file wholesale.
	c.Forget(path)
	if err := c.Open(context.Background(), path); err != nil {
		t.Fatalf("Open after Forget: %v", err)
	}
	f.waitForCount("textDocument/didOpen", 2)
	if f.count("textDocument/didOpen") != 2 {
		t.Errorf("Forget should force a new didOpen: %v", f.methods())
	}
}

// TestCloseIsIdempotent: a crashed server and a normal shutdown both end here.
func TestCloseIsIdempotent(t *testing.T) {
	c, _ := newFakeClient(t, Options{Name: "fake"})
	if err := c.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// receivedResponses returns the frames the fake got that are responses (an id
// and no method).
func (f *fakeServer) receivedResponses() []message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]message, 0, len(f.received))
	for _, m := range f.received {
		if m.ID != nil && m.Method == "" {
			out = append(out, m)
		}
	}
	return out
}

// waitForCount blocks until the fake has recorded this many frames of a method.
//
// Notifications have no response to wait on, so a test that asserts on one
// immediately after sending it is racing the fake server's read loop — which is
// exactly the kind of flake that only shows up under -race, when everything is
// slower.
func (f *fakeServer) waitForCount(method string, want int) {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.count(method) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	f.t.Fatalf("the client sent %s %d times, want %d (received: %v)",
		method, f.count(method), want, f.methods())
}

func waitForCondition(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
