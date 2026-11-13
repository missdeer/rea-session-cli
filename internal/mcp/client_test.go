package mcp

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestInitializeListAndCall(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client := newClient(clientConn, clientConn, nil)
	defer client.Close()
	go fakeServer(serverConn)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	protocol, info, _ := client.ServerInfo()
	if protocol != "2025-06-18" || info.Name != "rea" || info.Version != "test" {
		t.Fatalf("unexpected server info: %q %+v", protocol, info)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools across 2 pages, got %d", len(tools))
	}
	if tools[0].Name != "open_binary" || tools[1].Name != "binary_session" {
		t.Fatalf("unexpected tools: %+v", tools)
	}

	result, err := client.CallTool(ctx, "binary_session", json.RawMessage(`{"limit":1}`))
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if result.IsError {
		t.Fatal("unexpected isError")
	}
	var structured struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(result.StructuredContent, &structured); err != nil {
		t.Fatalf("structured content: %v", err)
	}
	if !structured.Active {
		t.Fatalf("unexpected structured content: %s", result.StructuredContent)
	}
}

func TestCallCancellationReachesServer(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client := newClient(clientConn, clientConn, nil)
	defer client.Close()
	go func() {
		defer serverConn.Close()
		decoder := json.NewDecoder(serverConn)
		for {
			var msg rpcMessage
			if err := decoder.Decode(&msg); err != nil {
				return
			}
			if msg.Method == "notifications/cancelled" {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := client.CallTool(ctx, "slow", nil); err == nil {
		t.Fatal("expected cancellation error")
	}
}

func fakeServer(conn net.Conn) {
	defer conn.Close()
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)
	for {
		var msg rpcMessage
		if err := decoder.Decode(&msg); err != nil {
			return
		}
		switch msg.Method {
		case "initialize":
			writeResult(encoder, msg.ID, `{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"rea","version":"test"}}`)
		case "notifications/initialized":
		case "tools/list":
			var params struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			if params.Cursor == "" {
				writeResult(encoder, msg.ID, `{"tools":[{"name":"open_binary","description":"open","inputSchema":{"type":"object"}}],"nextCursor":"page-2"}`)
			} else {
				writeResult(encoder, msg.ID, `{"tools":[{"name":"binary_session","description":"session","inputSchema":{"type":"object"}}]}`)
			}
		case "tools/call":
			writeResult(encoder, msg.ID, `{"content":[{"type":"text","text":"{\"active\":true}"}],"structuredContent":{"active":true}}`)
		}
	}
}

func writeResult(encoder *json.Encoder, id json.RawMessage, result string) {
	_ = encoder.Encode(rpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Result:  json.RawMessage(result),
	})
}
