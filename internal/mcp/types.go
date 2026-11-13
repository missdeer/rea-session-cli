// Package mcp implements the small subset of the Model Context Protocol that
// rea-session needs: a newline-delimited JSON-RPC 2.0 client over stdio.
package mcp

import "encoding/json"

// ProtocolVersion is the newest MCP revision this client speaks. The server
// answers with the version it negotiated, which is what callers should report.
const ProtocolVersion = "2025-06-18"

// Tool is one entry from tools/list. InputSchema is kept raw so the CLI can
// print it on demand without knowing any domain type.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	Annotations map[string]any  `json:"annotations,omitempty"`
}

// Content is one item of a tools/call content array. REA currently returns
// text only, but data/mimeType are retained for forward compatibility.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// CallResult is the result object of tools/call.
type CallResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
	Meta              json.RawMessage `json:"_meta,omitempty"`
}

// ServerInfo identifies the MCP server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult is the result object of initialize.
type InitializeResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities,omitempty"`
	ServerInfo      ServerInfo      `json:"serverInfo"`
	Instructions    string          `json:"instructions,omitempty"`
}
