package daemon

import (
	"encoding/json"
	"time"
)

// Request is one CLI-to-daemon operation. Every connection carries exactly one
// request and one response; the daemon keeps the MCP session, not the socket.
type Request struct {
	Token     string          `json:"token"`
	Op        string          `json:"op"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	TimeoutMS int64           `json:"timeout_ms,omitempty"`
}

// Response is the daemon's reply.
type Response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
	Server ServerStatus    `json:"server"`
}

// ServerStatus is embedded in every response so the CLI can render daemon
// health without a second round trip.
type ServerStatus struct {
	PID             int       `json:"pid"`
	Addr            string    `json:"addr"`
	Running         bool      `json:"running"`
	Version         string    `json:"version"`
	Phase           string    `json:"phase,omitempty"`
	InitError       string    `json:"init_error,omitempty"`
	ProtocolVersion string    `json:"protocol_version,omitempty"`
	ServerName      string    `json:"server_name,omitempty"`
	ServerVersion   string    `json:"server_version,omitempty"`
	ReaCommand      []string  `json:"rea_command,omitempty"`
	Tools           int       `json:"tools"`
	ToolsLoaded     bool      `json:"tools_loaded"`
	UptimeSeconds   int64     `json:"uptime_seconds"`
	StartedAt       time.Time `json:"started_at,omitempty"`
}

// Operation names.
const (
	OpPing   = "ping"
	OpStatus = "status"
	OpTools  = "tools"
	OpSchema = "schema"
	OpCall   = "call"
	OpStop   = "stop"
)

// Error codes.
const (
	CodeBadRequest   = "bad_request"
	CodeUnauthorized = "unauthorized"
	CodeNotFound     = "not_found"
	CodeToolError    = "tool_error"
	CodeTimeout      = "timeout"
	CodeInternal     = "internal"
)
