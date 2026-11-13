package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// clientVersion is reported in initialize. It is independent of the REA
// version the server reports back.
const clientVersion = "0.1.0"

// ErrClosed is returned when the stdio connection or server process ends.
var ErrClosed = errors.New("mcp: connection closed")

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e.Data == nil {
		return fmt.Sprintf("mcp: %s (code %d)", e.Message, e.Code)
	}
	return fmt.Sprintf("mcp: %s (code %d): %s", e.Message, e.Code, e.Data)
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// NotificationHandler receives server-initiated notifications such as
// notifications/tools/list_changed.
type NotificationHandler func(method string, params json.RawMessage)

// Client is a serializable MCP client for one stdio server process.
type Client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	out     *bufio.Reader
	closeFn func()

	logf func(string, ...any)

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan rpcMessage
	nextID  int64
	closed  bool
	readErr error
	onNote  NotificationHandler

	protocolVersion string
	serverInfo      ServerInfo
	instructions    string
	stderr          *tailBuffer
}

// tailBuffer keeps the last limit bytes written to it so a failed handshake
// can report the server's own diagnostic instead of a bare EOF.
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func newTailBuffer(limit int) *tailBuffer {
	return &tailBuffer{limit: limit}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// Start launches argv as an MCP stdio server and returns a connected client.
// The child inherits the parent environment; its stderr is inherited so
// provider diagnostics remain visible.
func Start(ctx context.Context, argv []string, logf func(string, ...any)) (*Client, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("mcp: empty command")
	}
	stderr := newTailBuffer(8 << 10)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	cmd.Stderr = io.MultiWriter(os.Stderr, stderr)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: start %q: %w", argv[0], err)
	}

	c := newClient(stdin, stdout, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	c.cmd = cmd
	c.logf = logf
	c.stderr = stderr
	return c, nil
}

// StderrTail returns the most recent server stderr, bounded to 8 KiB.
func (c *Client) StderrTail() string {
	if c.stderr == nil {
		return ""
	}
	return c.stderr.String()
}

// newClient is the transport-agnostic constructor used by tests as well.
func newClient(stdin io.WriteCloser, stdout io.Reader, closeFn func()) *Client {
	c := &Client{
		stdin:   stdin,
		out:     bufio.NewReaderSize(stdout, 1<<20),
		closeFn: closeFn,
		pending: make(map[string]chan rpcMessage),
	}
	go c.readLoop()
	return c
}

// SetNotificationHandler installs the notification callback. It must be set
// before the first request if the caller cares about early notifications.
func (c *Client) SetNotificationHandler(h NotificationHandler) {
	c.mu.Lock()
	c.onNote = h
	c.mu.Unlock()
}

// Initialize performs the MCP handshake and sends notifications/initialized.
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "rea-session",
			"version": clientVersion,
		},
	}
	var res InitializeResult
	if err := c.call(ctx, "initialize", params, &res); err != nil {
		return err
	}
	c.mu.Lock()
	c.protocolVersion = res.ProtocolVersion
	c.serverInfo = res.ServerInfo
	c.instructions = res.Instructions
	c.mu.Unlock()
	return c.notify("notifications/initialized", map[string]any{})
}

// ServerInfo returns the negotiated protocol version and server identity.
func (c *Client) ServerInfo() (protocol string, info ServerInfo, instructions string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.protocolVersion, c.serverInfo, c.instructions
}

// ListTools reads every page of tools/list.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" || page.NextCursor == cursor {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// CallTool invokes one tool. An empty arguments value is sent as {}.
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (CallResult, error) {
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	params := map[string]any{
		"name":      name,
		"arguments": arguments,
	}
	var res CallResult
	err := c.call(ctx, "tools/call", params, &res)
	return res, err
}

// Close ends the connection and reaps the child process.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.readErr == nil {
		c.readErr = ErrClosed
	}
	chans := make([]chan rpcMessage, 0, len(c.pending))
	for key, ch := range c.pending {
		chans = append(chans, ch)
		delete(c.pending, key)
	}
	c.mu.Unlock()

	for _, ch := range chans {
		close(ch)
	}
	_ = c.stdin.Close()

	if c.cmd != nil {
		done := make(chan struct{})
		go func() {
			_ = c.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			<-done
		}
	} else if c.closeFn != nil {
		c.closeFn()
	}
	return nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := atomic.AddInt64(&c.nextID, 1)
	key := strconv.FormatInt(id, 10)
	req := rpcMessage{
		JSONRPC: "2.0",
		ID:      json.RawMessage(key),
		Method:  method,
		Params:  raw,
	}

	ch := make(chan rpcMessage, 1)
	c.mu.Lock()
	if c.closed {
		err := c.readErr
		c.mu.Unlock()
		if err == nil {
			err = ErrClosed
		}
		return err
	}
	c.pending[key] = ch
	c.mu.Unlock()

	if err := c.write(req); err != nil {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		_ = c.notify("notifications/cancelled", map[string]any{
			"requestId": id,
			"reason":    "client cancelled",
		})
		return ctx.Err()
	case msg, ok := <-ch:
		if !ok {
			c.mu.Lock()
			err := c.readErr
			c.mu.Unlock()
			if err == nil {
				err = ErrClosed
			}
			return err
		}
		if msg.Error != nil {
			return msg.Error
		}
		if out != nil && len(msg.Result) > 0 {
			return json.Unmarshal(msg.Result, out)
		}
		return nil
	}
}

func (c *Client) notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(rpcMessage{JSONRPC: "2.0", Method: method, Params: raw})
}

func (c *Client) write(msg rpcMessage) error {
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(line)
	return err
}

func (c *Client) readLoop() {
	for {
		line, err := c.out.ReadBytes('\n')
		if len(line) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *Client) handleLine(line []byte) {
	// Tolerate accidental non-JSON diagnostics on stdout: they must never
	// make the client believe the connection is corrupt.
	trimmed := trimJSONPrefix(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		if c.logf != nil {
			c.logf("mcp: ignoring non-JSON stdout line: %s", truncate(string(line), 200))
		}
		return
	}
	var msg rpcMessage
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		if c.logf != nil {
			c.logf("mcp: malformed JSON line: %v", err)
		}
		return
	}

	switch {
	case msg.Method != "" && len(msg.ID) > 0:
		// Server-initiated request. REA does not use sampling or elicitation,
		// but ping must still be answered.
		if msg.Method == "ping" {
			_ = c.write(rpcMessage{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Result:  json.RawMessage("{}"),
			})
			return
		}
		_ = c.write(rpcMessage{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Error:   &rpcError{Code: -32601, Message: "method not supported by client"},
		})
	case msg.Method != "":
		c.mu.Lock()
		h := c.onNote
		c.mu.Unlock()
		if h != nil {
			h(msg.Method, msg.Params)
		}
	case len(msg.ID) > 0:
		key, ok := idKey(msg.ID)
		if !ok {
			return
		}
		c.mu.Lock()
		ch := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if ch != nil {
			ch <- msg
		}
	}
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if c.readErr == nil {
		if errors.Is(err, io.EOF) {
			c.readErr = ErrClosed
		} else {
			c.readErr = fmt.Errorf("mcp: read: %w", err)
		}
	}
	chans := make([]chan rpcMessage, 0, len(c.pending))
	for key, ch := range c.pending {
		chans = append(chans, ch)
		delete(c.pending, key)
	}
	c.mu.Unlock()

	for _, ch := range chans {
		close(ch)
	}
	if c.closeFn != nil {
		c.closeFn()
	}
}

func idKey(raw json.RawMessage) (string, bool) {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		if _, err := n.Int64(); err == nil {
			return n.String(), true
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return "s:" + s, true
	}
	return "", false
}

func trimJSONPrefix(line []byte) []byte {
	start := 0
	for start < len(line) && (line[start] == ' ' || line[start] == '\t' || line[start] == '\r' || line[start] == '\n') {
		start++
	}
	return line[start:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
