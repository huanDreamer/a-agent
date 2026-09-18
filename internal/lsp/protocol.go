package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// The wire format is JSON-RPC 2.0 with a `Content-Length` header, the same one
// the language server protocol has used since it was called that.
//
// Three things in here are less obvious than the format suggests:
//
//   - Frame sizes are byte counts, not rune counts. A source file full of CJK
//     identifiers makes those two differ, and getting it wrong corrupts the
//     stream rather than failing a test.
//   - A read loop that exits has to wake every pending request. Otherwise the
//     caller blocks forever on a response that can no longer arrive — which is
//     exactly the shape of "the agent hung" nobody can reproduce.
//   - The server sends *requests* to the client too (progress, configuration,
//     registration). A client that does not answer them leaves the server
//     waiting, so unknown requests get an explicit "method not found" instead of
//     silence.

// headerLimit bounds a header block. It guards against a peer that never sends
// the blank line that ends one.
const headerLimit = 8 * 1024

// maxFrameSize bounds a single message. Language servers do send large payloads
// (a whole-file diagnostic report), so this is generous — it exists to turn an
// absurd length into an error rather than an allocation of that size.
const maxFrameSize = 64 << 20

// errConnClosed is returned to callers whose request was in flight when the
// connection ended.
var errConnClosed = errors.New("lsp: connection closed")

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("lsp: server error %d: %s", e.Code, e.Message)
}

// message is any JSON-RPC frame this package sends or receives. The direction is
// inferred from which fields are set, which is how the protocol works: a frame
// with both an ID and a Method is a request, one with only an ID is a response,
// one with only a Method is a notification.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *jsonID         `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// jsonID is a request identifier. LSP allows a number or a string; this client
// only ever sends numbers, but a server may echo either, so both are kept.
type jsonID struct {
	num    int64
	str    string
	isText bool
}

func (id jsonID) MarshalJSON() ([]byte, error) {
	if id.isText {
		return json.Marshal(id.str)
	}
	return json.Marshal(id.num)
}

func (id *jsonID) UnmarshalJSON(b []byte) error {
	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		id.num, id.isText = n, false
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("lsp: request id is neither a number nor a string: %s", b)
	}
	id.str, id.isText = s, true
	return nil
}

func (id *jsonID) key() string {
	if id == nil {
		return ""
	}
	if id.isText {
		return "s:" + id.str
	}
	return "n:" + strconv.FormatInt(id.num, 10)
}

// writeFrame writes one JSON message with its length header.
func writeFrame(w io.Writer, payload []byte) error {
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))
	if _, err := io.WriteString(w, header); err != nil {
		return fmt.Errorf("lsp: write header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("lsp: write body: %w", err)
	}
	return nil
}

// readFrame reads one message's payload.
//
// Headers are parsed case-insensitively and unknown ones are skipped: the
// protocol says Content-Type may be present, and real servers send other things.
func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	consumed := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		consumed += len(line)
		if consumed > headerLimit {
			return nil, fmt.Errorf("lsp: header block exceeds %d bytes without a blank line", headerLimit)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break // end of headers
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			return nil, fmt.Errorf("lsp: malformed header %q", trimmed)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q", value)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("lsp: frame has no Content-Length")
	}
	if length > maxFrameSize {
		return nil, fmt.Errorf("lsp: frame of %d bytes exceeds the %d byte cap", length, maxFrameSize)
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("lsp: read %d byte body: %w", length, err)
	}
	return buf, nil
}

// pendingRequest is one in-flight request waiting for its response.
type pendingRequest struct {
	ch chan message
}

// conn owns the frame stream: writing is serialised, reading happens on one
// goroutine that dispatches to the handlers.
type conn struct {
	rw io.ReadWriteCloser

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[string]*pendingRequest
	closed  bool
	// closeErr records why the connection ended, so a waiting caller can be told
	// rather than just "closed".
	closeErr error

	// handler is called for notifications and for server-initiated requests.
	handler func(m message)

	done chan struct{}
}

func newConn(rw io.ReadWriteCloser, handler func(message)) *conn {
	c := &conn{
		rw:      rw,
		pending: map[string]*pendingRequest{},
		handler: handler,
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// readLoop dispatches frames until the stream ends, then fails everything that
// was waiting. Waking the waiters is the point: a read loop that just returns
// leaves every caller blocked on a channel nobody will ever send to.
func (c *conn) readLoop() {
	reader := bufio.NewReader(c.rw)
	for {
		payload, err := readFrame(reader)
		if err != nil {
			c.fail(err)
			return
		}
		var m message
		if err := json.Unmarshal(payload, &m); err != nil {
			// A frame we cannot parse is not fatal: a server that logs to stdout
			// (which it should not) produces one, and abandoning the session over
			// it would be worse than skipping it.
			continue
		}
		c.dispatch(m)
	}
}

// dispatch routes one message: a response settles its request, anything else
// goes to the handler.
func (c *conn) dispatch(m message) {
	if m.ID != nil && m.Method == "" {
		c.mu.Lock()
		p := c.pending[m.ID.key()]
		delete(c.pending, m.ID.key())
		c.mu.Unlock()
		if p != nil {
			p.ch <- m
		}
		return
	}
	if c.handler != nil {
		c.handler(m)
	}
}

// fail ends the connection with a reason and wakes every waiter.
func (c *conn) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = err
	waiters := make([]*pendingRequest, 0, len(c.pending))
	for _, p := range c.pending {
		waiters = append(waiters, p)
	}
	c.pending = map[string]*pendingRequest{}
	c.mu.Unlock()

	for _, p := range waiters {
		// Buffered, so this never blocks even if the waiter has given up.
		select {
		case p.ch <- message{Error: &rpcError{Code: -32099, Message: err.Error()}}:
		default:
		}
	}
	close(c.done)
}

// wait returns the channel the connection is closed on.
func (c *conn) wait() <-chan struct{} { return c.done }

// notify sends a notification (no response expected).
func (c *conn) notify(method string, params any) error {
	return c.send(message{JSONRPC: "2.0", Method: method, Params: mustParams(params)})
}

// call sends a request and waits for its response.
func (c *conn) call(ctx doneOrDone, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		return nil, fmt.Errorf("lsp: %s: %w", method, errors.Join(errConnClosed, err))
	}
	c.nextID++
	id := &jsonID{num: c.nextID}
	p := &pendingRequest{ch: make(chan message, 1)}
	c.pending[id.key()] = p
	c.mu.Unlock()

	raw := mustParams(params)
	if err := c.send(message{JSONRPC: "2.0", ID: id, Method: method, Params: raw}); err != nil {
		c.mu.Lock()
		delete(c.pending, id.key())
		c.mu.Unlock()
		return nil, err
	}

	select {
	case m := <-p.ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id.key())
		c.mu.Unlock()
		return nil, fmt.Errorf("lsp: %s: %w", method, ctx.Err())
	case <-c.done:
		return nil, fmt.Errorf("lsp: %s: %w", method, errConnClosed)
	}
}

// send writes one frame, serialised so two goroutines cannot interleave halves
// of a message.
func (c *conn) send(m message) error {
	if m.JSONRPC == "" {
		m.JSONRPC = "2.0"
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("lsp: marshal %s: %w", m.Method, err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.rw, payload)
}

// respond answers a server-initiated request.
func (c *conn) respond(id *jsonID, result any, rpcErr *rpcError) error {
	return c.send(message{JSONRPC: "2.0", ID: id, Result: mustParams(result), Error: rpcErr})
}

// close ends the connection.
func (c *conn) close() error {
	c.fail(errConnClosed)
	return c.rw.Close()
}

// mustParams marshals params, falling back to an empty object. A marshal failure
// here would mean a bug in a fixed struct, and turning it into a protocol error
// at the call site would only obscure where it happened.
func mustParams(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// doneOrDone is the part of context.Context this file needs. Naming it keeps the
// transport testable without a context.
type doneOrDone interface {
	Done() <-chan struct{}
	Err() error
}
