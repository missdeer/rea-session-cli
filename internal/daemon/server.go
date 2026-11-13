package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"rea-session-cli/internal/mcp"
	"rea-session-cli/internal/state"
)

// Config describes one daemon process.
type Config struct {
	ReaCommand  []string
	IdleTimeout time.Duration
	Version     string
	Logger      *log.Logger
}

// Server owns the long-lived MCP connection and the loopback control socket.
type Server struct {
	cfg     Config
	ctx     context.Context
	client  *mcp.Client
	ln      net.Listener
	token   string
	started time.Time

	mcpMu sync.Mutex

	toolsMu     sync.RWMutex
	toolsJSON   json.RawMessage
	toolsCount  int
	toolsLoaded bool

	lastActivity atomic.Int64
	inflight     atomic.Int64

	done         chan struct{}
	shutdownOnce sync.Once
}

// Run starts the MCP server connection, publishes the endpoint, and serves
// until the context is cancelled or a stop request arrives.
func Run(ctx context.Context, cfg Config) error {
	if len(cfg.ReaCommand) == 0 {
		return errors.New("daemon: empty REA command")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "rea-session: ", log.LstdFlags)
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 20 * time.Minute
	}

	s := &Server{
		cfg:     cfg,
		ctx:     ctx,
		started: time.Now(),
		done:    make(chan struct{}),
	}
	s.touch()

	if err := s.startMCP(); err != nil {
		_ = state.SaveError(fmt.Sprintf("cannot start the REA MCP server: %v", err))
		return err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = s.client.Close()
		_ = state.SaveError(fmt.Sprintf("cannot listen on loopback: %v", err))
		return fmt.Errorf("daemon: listen: %w", err)
	}
	token, err := state.NewToken()
	if err != nil {
		_ = ln.Close()
		_ = s.client.Close()
		_ = state.SaveError(fmt.Sprintf("cannot create a control token: %v", err))
		return err
	}
	s.ln = ln
	s.token = token

	ep := &state.Endpoint{
		Addr:      ln.Addr().String(),
		Token:     token,
		PID:       os.Getpid(),
		Version:   cfg.Version,
		StartedAt: s.started,
	}
	if err := state.Save(ep); err != nil {
		_ = ln.Close()
		_ = s.client.Close()
		_ = state.SaveError(fmt.Sprintf("cannot publish the control endpoint: %v", err))
		return fmt.Errorf("daemon: publish endpoint: %w", err)
	}
	_ = state.ClearError()
	defer func() { _ = state.Remove() }()

	cfg.Logger.Printf("ready addr=%s pid=%d rea=%v", ep.Addr, ep.PID, cfg.ReaCommand)

	go s.acceptLoop()
	go s.idleLoop()

	select {
	case <-ctx.Done():
	case <-s.done:
	}
	s.shutdown()
	return nil
}

func (s *Server) startMCP() error {
	client, err := mcp.Start(s.ctx, s.cfg.ReaCommand, func(format string, args ...any) {
		s.cfg.Logger.Printf(format, args...)
	})
	if err != nil {
		return err
	}
	s.client = client
	client.SetNotificationHandler(func(method string, params json.RawMessage) {
		switch method {
		case "notifications/tools/list_changed":
			s.invalidateTools()
		default:
			s.cfg.Logger.Printf("notification %s", method)
		}
	})

	// A first npx launch may download the package; keep this generous.
	initCtx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer cancel()
	if err := client.Initialize(initCtx); err != nil {
		tail := strings.TrimSpace(client.StderrTail())
		_ = client.Close()
		message := fmt.Sprintf("initialize MCP: %v", err)
		if tail != "" {
			message += "\n--- server stderr ---\n" + tail
		}
		return errors.New(message)
	}
	proto, info, _ := client.ServerInfo()
	s.cfg.Logger.Printf("mcp connected protocol=%s server=%s/%s", proto, info.Name, info.Version)
	return nil
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			s.cfg.Logger.Printf("accept: %v", err)
			s.shutdown()
			return
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		s.writeResponse(conn, Response{
			OK:    false,
			Code:  CodeBadRequest,
			Error: err.Error(),
		})
		return
	}
	s.touch()

	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.token)) != 1 {
		s.writeResponse(conn, Response{
			OK:    false,
			Code:  CodeUnauthorized,
			Error: "invalid token",
		})
		return
	}

	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	defer s.touch()

	switch req.Op {
	case OpPing:
		s.writeResponse(conn, Response{OK: true, Result: json.RawMessage(`{}`)})

	case OpStatus, "":
		s.writeResponse(conn, Response{OK: true, Result: json.RawMessage(`{}`)})

	case OpTools:
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
		defer cancel()
		raw, _, err := s.loadTools(ctx)
		if err != nil {
			s.writeResponse(conn, Response{OK: false, Code: CodeInternal, Error: err.Error()})
			return
		}
		s.writeResponse(conn, Response{OK: true, Result: raw})

	case OpSchema:
		ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
		defer cancel()
		schema, err := s.describe(req.Tool, ctx)
		if err != nil {
			code := CodeInternal
			if errors.Is(err, errToolNotFound) {
				code = CodeNotFound
			}
			s.writeResponse(conn, Response{OK: false, Code: code, Error: err.Error()})
			return
		}
		s.writeResponse(conn, Response{OK: true, Result: schema})

	case OpCall:
		res, failure := s.callTool(req)
		if failure != nil {
			s.writeResponse(conn, *failure)
			return
		}
		raw, err := json.Marshal(res)
		if err != nil {
			s.writeResponse(conn, Response{OK: false, Code: CodeInternal, Error: err.Error()})
			return
		}
		s.writeResponse(conn, Response{OK: true, Result: raw})

	case OpStop:
		s.writeResponse(conn, Response{OK: true, Result: json.RawMessage(`{}`)})
		go s.shutdown()

	default:
		s.writeResponse(conn, Response{
			OK:    false,
			Code:  CodeBadRequest,
			Error: fmt.Sprintf("unknown op %q", req.Op),
		})
	}
}

var errToolNotFound = errors.New("tool not found")

func (s *Server) callTool(req Request) (*mcp.CallResult, *Response) {
	if req.Tool == "" {
		return nil, &Response{OK: false, Code: CodeBadRequest, Error: "tool is required"}
	}
	timeout := 10 * time.Minute
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()

	s.mcpMu.Lock()
	res, err := s.client.CallTool(ctx, req.Tool, req.Arguments)
	s.mcpMu.Unlock()
	if err != nil {
		code := CodeToolError
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = CodeTimeout
		}
		return nil, &Response{OK: false, Code: code, Error: err.Error()}
	}
	return &res, nil
}

func (s *Server) loadTools(ctx context.Context) (json.RawMessage, int, error) {
	s.toolsMu.RLock()
	if s.toolsLoaded {
		raw, n := s.toolsJSON, s.toolsCount
		s.toolsMu.RUnlock()
		return raw, n, nil
	}
	s.toolsMu.RUnlock()

	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	if s.toolsLoaded {
		return s.toolsJSON, s.toolsCount, nil
	}

	s.mcpMu.Lock()
	tools, err := s.client.ListTools(ctx)
	s.mcpMu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return nil, 0, err
	}
	s.toolsJSON = raw
	s.toolsCount = len(tools)
	s.toolsLoaded = true
	return raw, s.toolsCount, nil
}

func (s *Server) describe(name string, ctx context.Context) (json.RawMessage, error) {
	raw, _, err := s.loadTools(ctx)
	if err != nil {
		return nil, err
	}
	var tools []mcp.Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, err
	}
	for _, tool := range tools {
		if tool.Name == name {
			if len(tool.InputSchema) == 0 {
				return json.RawMessage(`{}`), nil
			}
			return tool.InputSchema, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", errToolNotFound, name)
}

func (s *Server) invalidateTools() {
	s.toolsMu.Lock()
	s.toolsJSON = nil
	s.toolsCount = 0
	s.toolsLoaded = false
	s.toolsMu.Unlock()
}

func (s *Server) status() ServerStatus {
	s.toolsMu.RLock()
	count, loaded := s.toolsCount, s.toolsLoaded
	s.toolsMu.RUnlock()

	proto, info, _ := s.client.ServerInfo()
	return ServerStatus{
		PID:             os.Getpid(),
		Addr:            s.ln.Addr().String(),
		Running:         true,
		Version:         s.cfg.Version,
		ProtocolVersion: proto,
		ServerName:      info.Name,
		ServerVersion:   info.Version,
		ReaCommand:      s.cfg.ReaCommand,
		Tools:           count,
		ToolsLoaded:     loaded,
		UptimeSeconds:   int64(time.Since(s.started).Seconds()),
		StartedAt:       s.started,
	}
}

func (s *Server) writeResponse(conn net.Conn, resp Response) {
	resp.Server = s.status()
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	_, _ = conn.Write(append(raw, '\n'))
}

func (s *Server) touch() {
	s.lastActivity.Store(time.Now().UnixNano())
}

func (s *Server) idleLoop() {
	interval := max(s.cfg.IdleTimeout/4, time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if s.inflight.Load() > 0 {
				continue
			}
			last := time.Unix(0, s.lastActivity.Load())
			if time.Since(last) >= s.cfg.IdleTimeout {
				s.cfg.Logger.Printf("idle for %s; shutting down", s.cfg.IdleTimeout)
				s.shutdown()
				return
			}
		}
	}
}

func (s *Server) shutdown() {
	s.shutdownOnce.Do(func() {
		close(s.done)
		if s.ln != nil {
			_ = s.ln.Close()
		}
		if s.client != nil {
			_ = s.client.Close()
		}
		_ = state.Remove()
		s.cfg.Logger.Printf("stopped")
	})
}
