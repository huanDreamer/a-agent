package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// The client half of one language-server session.
//
// It owns two things a caller has to understand to use it correctly:
//
//   1. Document synchronisation. A language server answers about the text it has
//      been told about, not about the file on disk. Every request therefore goes
//      through Open, which reads the file and sends didOpen/didChange first. Skip
//      that and the answers are about a stale version of the file — the kind of
//      wrong that looks like a server bug.
//
//   2. Diagnostics are asynchronous. The server pushes them; it does not return
//      them from a call. So "what is wrong with this file" is really "what has
//      the server told me lately, and has it caught up with my last write?".
//      Reported is the counter that answers the second half: it is bumped on
//      every publish, and a caller that has just changed a file waits for it to
//      move rather than trusting whatever arrived before.

// Options tunes one client.
type Options struct {
	// Name is the server's name, for logs and error messages.
	Name string
	// Root is the workspace root the server was started for.
	Root string
	// InitOptions is passed through as initializationOptions.
	InitOptions any
	// RequestTimeout bounds a single request. Zero uses DefaultRequestTimeout.
	RequestTimeout time.Duration
	// SettleWindow is how long the diagnostics for a file must stop arriving
	// before they count as settled. Zero uses DefaultSettleWindow.
	SettleWindow time.Duration
	// Logger receives diagnostics about this client. Optional.
	Logger Logger
	// Version identifies this client in the server's log.
	Version string
}

// Defaults for Options.
const (
	DefaultRequestTimeout = 30 * time.Second
	DefaultSettleWindow   = 150 * time.Millisecond
)

// Logger is the tiny slice of zap this package needs, so a test can pass a
// no-op and the package does not depend on a logging framework.
type Logger interface {
	Debugf(format string, args ...any)
	Warnf(format string, args ...any)
}

// Client is a live session with one language server.
type Client struct {
	conn *conn
	opts Options

	mu sync.Mutex
	// opened maps a filesystem path to the version last sent, so a change can
	// bump it and a repeated request does not re-send identical content.
	opened map[string]int
	// diags is the latest diagnostics per URI, with the publish counter it
	// arrived at so a waiter can tell "fresh" from "left over from before my
	// edit".
	diags map[string]*diagState
	// publishSeq counts publishes per URI; changedSeq counts local edits. A file
	// is "caught up" when its published counter has reached its changed counter.
	publishSeq map[string]int64
	changedSeq map[string]int64
	caps       ServerCapabilities

	closed bool
}

// diagState is one file's diagnostics plus when they arrived.
type diagState struct {
	items []Diagnostic
	at    time.Time
	// forChange is the changedSeq this publish answers.
	forChange int64
	// version is the document version the server published against, when it
	// said. 0 means it did not say.
	version int
}

// NewClient performs the initialize handshake and returns a ready session.
//
// The transport is any duplex stream: a subprocess's pipes in production, an
// in-memory pipe in tests. Keeping process management out of this type is what
// makes the protocol testable without installing a language server.
func NewClient(ctx context.Context, rw io.ReadWriteCloser, opts Options) (*Client, error) {
	if rw == nil {
		return nil, errors.New("lsp: transport is required")
	}
	if opts.Name == "" {
		opts.Name = "language server"
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if opts.SettleWindow <= 0 {
		opts.SettleWindow = DefaultSettleWindow
	}

	c := &Client{
		opts:       opts,
		opened:     map[string]int{},
		diags:      map[string]*diagState{},
		publishSeq: map[string]int64{},
		changedSeq: map[string]int64{},
	}
	c.conn = newConn(rw, c.handleServerMessage)

	if err := c.initialize(ctx); err != nil {
		_ = c.conn.close()
		return nil, err
	}
	return c, nil
}

// call sends a request with the client's timeout applied.
//
// The timeout lives here rather than at each call site because a language server
// that accepts a request and never answers is a real failure mode (it is
// indexing, or it has wedged), and every request has to be protected from it.
// Leaving one call site out means leaving one tool that can hang a whole turn.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.opts.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opts.RequestTimeout)
		defer cancel()
	}
	raw, err := c.conn.call(ctx, method, params)
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("lsp: %s: %s timed out after %s", c.opts.Name, method, c.opts.RequestTimeout)
	}
	return raw, err
}

// Capabilities returns what the server said it can do. Tools are registered off
// this, so a server without references support simply has no references tool.
func (c *Client) Capabilities() ServerCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps
}

// Name returns the server's configured name.
func (c *Client) Name() string { return c.opts.Name }

// Root returns the workspace root this session was opened for.
func (c *Client) Root() string { return c.opts.Root }

// initialize runs the opening exchange.
func (c *Client) initialize(ctx context.Context) error {
	folders := []WorkspaceFolder{{URI: PathToURI(c.opts.Root), Name: filepathBase(c.opts.Root)}}
	params := InitializeParams{
		ProcessID:  os.Getpid(),
		ClientInfo: &ClientInfo{Name: "huan-agent", Version: c.opts.Version},
		RootURI:    PathToURI(c.opts.Root),
		RootPath:   c.opts.Root,
		Capabilities: ClientCapabilities{
			TextDocument: TextDocumentClientCapabilities{
				Synchronization: &SynchronizationCapability{},
			},
			Workspace: WorkspaceClientCapabilities{WorkspaceFolders: true, Configuration: false},
			Window:    WindowClientCapabilities{},
			General:   &GeneralClientCapabilities{PositionEncodings: []string{"utf-16"}},
		},
		InitializationOptions: c.opts.InitOptions,
		WorkspaceFolders:      folders,
	}

	raw, err := c.call(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("lsp: %s: initialize: %w", c.opts.Name, err)
	}
	var res InitializeResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &res); err != nil {
			return fmt.Errorf("lsp: %s: decode initialize result: %w", c.opts.Name, err)
		}
	}
	c.mu.Lock()
	c.caps = res.Capabilities
	c.mu.Unlock()

	// initialized is a notification: the server is entitled to ignore requests
	// that arrive before it.
	if err := c.conn.notify("initialized", struct{}{}); err != nil {
		return fmt.Errorf("lsp: %s: initialized: %w", c.opts.Name, err)
	}
	return nil
}

// Open makes sure the server has the file's current content.
//
// It is called before every request rather than being left to the caller,
// because a request against stale content is not an error the server can
// report — it is a confident answer about the wrong text.
func (c *Client) Open(ctx context.Context, path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("lsp: read %s: %w", path, err)
	}

	c.mu.Lock()
	version, already := c.opened[path]
	c.mu.Unlock()

	uri := PathToURI(path)
	if !already {
		version = 1
		c.mu.Lock()
		c.opened[path] = version
		// A fresh didOpen is also a change as far as waiting for diagnostics is
		// concerned: the server has not seen this text before.
		c.changedSeq[uri]++
		c.mu.Unlock()
		return wrapErr(c.conn.notify("textDocument/didOpen", didOpenParams{
			TextDocument: textDocumentItem{
				URI:        uri,
				LanguageID: languageIDFor(path),
				Version:    version,
				Text:       string(content),
			},
		}), c.opts.Name, "didOpen")
	}

	version++
	c.mu.Lock()
	c.opened[path] = version
	c.changedSeq[uri]++
	c.mu.Unlock()

	// Full-content sync: the server was told textDocumentSync=1 or 2, and a full
	// replacement is valid for both. Incremental diffs would save bandwidth the
	// agent does not use and introduce an off-by-one that is invisible until it
	// is not.
	return wrapErr(c.conn.notify("textDocument/didChange", didChangeParams{
		TextDocument:   versionedTextDocumentIdentifier{URI: uri, Version: version},
		ContentChanges: []textDocumentContentChange{{Text: string(content)}},
	}), c.opts.Name, "didChange")
}

// Forget drops a file from the server's view. It is what a write tool calls when
// it deletes or replaces a file wholesale, so the next Open is a fresh didOpen.
func (c *Client) Forget(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.opened, path)
}

// Definition returns the definition of the symbol at a 1-based position.
func (c *Client) Definition(ctx context.Context, path string, at LineCol) ([]Location, error) {
	_, pos, err := c.Prepare(ctx, path, at)
	if err != nil {
		return nil, err
	}
	raw, err := c.call(ctx, "textDocument/definition", TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: PathToURI(path)},
		Position:     pos,
	})
	if err != nil {
		return nil, err
	}
	return decodeLocations(raw)
}

// References returns every reference to the symbol at a 1-based position.
func (c *Client) References(ctx context.Context, path string, at LineCol, includeDeclaration bool) ([]Location, error) {
	_, pos, err := c.Prepare(ctx, path, at)
	if err != nil {
		return nil, err
	}
	raw, err := c.call(ctx, "textDocument/references", ReferenceParams{
		TextDocumentPositionParams: TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: PathToURI(path)},
			Position:     pos,
		},
		Context: ReferenceContext{IncludeDeclaration: includeDeclaration},
	})
	if err != nil {
		return nil, err
	}
	return decodeLocations(raw)
}

// WorkspaceSymbols searches symbols by name across the workspace.
func (c *Client) WorkspaceSymbols(ctx context.Context, query string) ([]Symbol, error) {
	raw, err := c.call(ctx, "workspace/symbol", SymbolParams{Query: query})
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var syms []Symbol
	if err := json.Unmarshal(raw, &syms); err != nil {
		return nil, fmt.Errorf("lsp: decode symbols: %w", err)
	}
	return syms, nil
}

// Prepare opens the document and converts a 1-based position into the
// protocol's, returning the file's content as well.
//
// It is the single entry point for every position-based request, because the two
// steps are not optional: a server answers about the text it has been told
// about, so a request that skips the open gets a confident answer about a
// document the server has never seen.
func (c *Client) Prepare(ctx context.Context, path string, at LineCol) ([]byte, Position, error) {
	if err := c.Open(ctx, path); err != nil {
		return nil, Position{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, Position{}, fmt.Errorf("lsp: read %s: %w", path, err)
	}
	pos, err := ToLSP(content, at)
	if err != nil {
		return nil, Position{}, err
	}
	return content, pos, nil
}

// Diagnostics returns the diagnostics last published for a path, and whether the
// server has caught up with the last change this client made to it.
//
// caughtUp false is not an error: it means the server has not published since the
// last write, so what is returned may be about the previous content. Callers
// report that rather than presenting stale diagnostics as current.
func (c *Client) Diagnostics(path string) (items []Diagnostic, caughtUp bool) {
	uri := PathToURI(path)
	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.diags[uri]
	if st == nil {
		// Nothing has ever been published for this file, so there is no answer
		// to give and none to claim: caughtUp false says "unknown", which is
		// what a caller must not mistake for "clean".
		return nil, false
	}
	return append([]Diagnostic{}, st.items...), st.forChange >= c.changedSeq[uri]
}

// WaitDiagnostics waits until the diagnostics for a path have settled, or until
// maxWait passes.
//
// "Settled" means two things together, and both are needed:
//
//   - the server has published since this client's last change to the file, so
//     the answer is about the current text; and
//   - nothing new has arrived for SettleWindow, so the server is not still
//     emitting.
//
// Waiting only for the first would return the first of several reports (servers
// often send an empty set, then the real one). Waiting only for the second would
// return the previous file's diagnostics on a fast edit.
func (c *Client) WaitDiagnostics(ctx context.Context, path string, maxWait time.Duration) (items []Diagnostic, settled bool) {
	if maxWait <= 0 {
		maxWait = time.Second
	}
	deadline := time.Now().Add(maxWait)
	uri := PathToURI(path)
	tick := c.opts.SettleWindow / 5
	if tick < 5*time.Millisecond {
		tick = 5 * time.Millisecond
	}
	timer := time.NewTicker(tick)
	defer timer.Stop()

	for {
		c.mu.Lock()
		changed := c.changedSeq[uri]
		st := c.diags[uri]
		c.mu.Unlock()

		if st != nil && st.forChange >= changed && time.Since(st.at) >= c.opts.SettleWindow {
			// Non-nil even when empty: an empty report is a real report ("this
			// file is clean"), and a nil slice has to keep meaning "nothing has
			// been published".
			return append([]Diagnostic{}, st.items...), true
		}
		if time.Now().After(deadline) {
			if st == nil {
				return nil, false
			}
			return append([]Diagnostic{}, st.items...), false
		}
		select {
		case <-ctx.Done():
			if st == nil {
				return nil, false
			}
			return append([]Diagnostic(nil), st.items...), false
		case <-c.conn.wait():
			if st == nil {
				return nil, false
			}
			return append([]Diagnostic(nil), st.items...), false
		case <-timer.C:
		}
	}
}

// handleServerMessage deals with notifications and server-initiated requests.
func (c *Client) handleServerMessage(m message) {
	switch m.Method {
	case "textDocument/publishDiagnostics":
		c.onPublishDiagnostics(m.Params)
	case "":
		// A response with no method that did not match a pending request: an
		// answer to a request this client already gave up on. Dropping it is
		// correct.
	default:
		if m.ID != nil {
			// A request from the server. Answering "method not found" is what
			// keeps it from waiting on a client that will never answer.
			_ = c.conn.respond(m.ID, nil, &rpcError{Code: -32601, Message: "method not supported by huan-agent: " + m.Method})
			return
		}
		if c.opts.Logger != nil {
			c.opts.Logger.Debugf("lsp: %s: ignoring notification %s", c.opts.Name, m.Method)
		}
	}
}

// onPublishDiagnostics records one push.
func (c *Client) onPublishDiagnostics(raw json.RawMessage) {
	var p DiagnosticParams
	if err := json.Unmarshal(raw, &p); err != nil {
		if c.opts.Logger != nil {
			c.opts.Logger.Warnf("lsp: %s: undecodable publishDiagnostics: %v", c.opts.Name, err)
		}
		return
	}
	items := make([]Diagnostic, 0, len(p.Diagnostics))
	for _, d := range p.Diagnostics {
		items = append(items, Diagnostic{
			Range:    d.Range,
			Severity: d.Severity,
			Code:     d.Code.s,
			Source:   d.Source,
			Message:  d.Message,
			Related:  len(d.RelatedInformation),
		})
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// An empty array is a real report: it means "this file is clean now", and
	// treating it as "nothing arrived" is how a fixed error keeps being shown.
	c.publishSeq[p.URI]++
	c.diags[p.URI] = &diagState{
		items:     items,
		at:        time.Now(),
		forChange: c.changedSeq[p.URI],
		version:   p.Version,
	}
}

// Close ends the session.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	// Best-effort shutdown, then the transport. A server that does not answer
	// shutdown within a moment is killed by the owner of the process, so this
	// does not wait long.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = c.conn.call(ctx, "shutdown", nil)
	_ = c.conn.notify("exit", nil)
	return c.conn.close()
}

// decodeLocations handles the three shapes the protocol allows for a location
// result: one Location, an array of them, or null.
func decodeLocations(raw json.RawMessage) ([]Location, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var one Location
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("lsp: decode location: %w", err)
		}
		return []Location{one}, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("lsp: decode locations: %w", err)
		}
		out := make([]Location, 0, len(items))
		for _, item := range items {
			// The entry's shape is decided by its keys, not by trying one decode
			// and falling back on error: a LocationLink decodes into a Location
			// without complaint and yields an empty uri, so "did the unmarshal
			// fail" cannot tell the two apart.
			var probe map[string]json.RawMessage
			if err := json.Unmarshal(item, &probe); err != nil {
				continue
			}
			if _, isLink := probe["targetUri"]; isLink {
				var link struct {
					TargetURI            string `json:"targetUri"`
					TargetRange          Range  `json:"targetRange"`
					TargetSelectionRange Range  `json:"targetSelectionRange"`
				}
				if err := json.Unmarshal(item, &link); err != nil {
					continue
				}
				r := link.TargetRange
				if r.Start == r.End && link.TargetSelectionRange.Start != link.TargetSelectionRange.End {
					r = link.TargetSelectionRange
				}
				out = append(out, Location{URI: link.TargetURI, Range: r})
				continue
			}
			var loc Location
			if err := json.Unmarshal(item, &loc); err != nil {
				continue
			}
			out = append(out, loc)
		}
		return out, nil
	}
	return nil, fmt.Errorf("lsp: unexpected location result: %s", trimmed)
}

// SortDiagnostics orders diagnostics the way a reader wants them: errors before
// warnings, then by position. Sorting here rather than at each call site keeps
// tool output stable, which is what makes it testable and diffable.
func SortDiagnostics(items []Diagnostic) {
	sort.SliceStable(items, func(i, j int) bool {
		si, sj := items[i].Severity, items[j].Severity
		// A missing severity means "unknown", which sorts with errors: a problem
		// of unknown weight is not something to bury under the warnings.
		if si == 0 {
			si = SeverityError
		}
		if sj == 0 {
			sj = SeverityError
		}
		if si != sj {
			return si < sj
		}
		if items[i].Range.Start.Line != items[j].Range.Start.Line {
			return items[i].Range.Start.Line < items[j].Range.Start.Line
		}
		return items[i].Range.Start.Character < items[j].Range.Start.Character
	})
}

// wrapErr adds the server's name to a transport failure.
func wrapErr(err error, name, what string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("lsp: %s: %s: %w", name, what, err)
}

func filepathBase(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 && i+1 < len(p) {
		return p[i+1:]
	}
	return p
}
