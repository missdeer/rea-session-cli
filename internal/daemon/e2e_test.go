package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"testing"
	"time"

	"rea-session-cli/internal/mcp"
	"rea-session-cli/internal/state"
)

// TestMockMCPServerHelper turns the test binary into a minimal MCP stdio
// server when the daemon re-executes it. It is a no-op in a normal test run.
func TestMockMCPServerHelper(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER") != "1" {
		return
	}
	serveMock(os.Stdin, os.Stdout)
}

func TestDaemonServesMCPOverLoopback(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER") == "1" {
		t.Skip("helper process")
	}
	t.Setenv("REA_SESSION_HOME", t.TempDir())
	t.Setenv("GO_WANT_HELPER", "1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			ReaCommand:  []string{os.Args[0], "-test.run=TestMockMCPServerHelper", "--"},
			IdleTimeout: time.Minute,
			Version:     "test",
			Logger:      log.New(io.Discard, "", 0),
		})
	}()

	ep := waitForEndpoint(t, 10*time.Second)

	callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer callCancel()

	toolsResp, err := Do(callCtx, ep, Request{Op: OpTools})
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if !toolsResp.OK {
		t.Fatalf("tools failed: %s %s", toolsResp.Code, toolsResp.Error)
	}
	var tools []mcp.Tool
	if err := json.Unmarshal(toolsResp.Result, &tools); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if toolsResp.Server.ProtocolVersion != "2025-06-18" {
		t.Fatalf("unexpected protocol %q", toolsResp.Server.ProtocolVersion)
	}

	callResp, err := Do(callCtx, ep, Request{Op: OpCall, Tool: "binary_session"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !callResp.OK {
		t.Fatalf("call failed: %s %s", callResp.Code, callResp.Error)
	}
	var result mcp.CallResult
	if err := json.Unmarshal(callResp.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.StructuredContent) == 0 {
		t.Fatal("expected structured content")
	}

	if _, err := Stop(callCtx, ep); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func waitForEndpoint(t *testing.T, timeout time.Duration) *state.Endpoint {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ep, err := state.Load(); err == nil {
			return ep
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("daemon did not publish an endpoint")
	return nil
}

func serveMock(in io.Reader, out io.Writer) {
	reader := bufio.NewReader(in)
	encoder := json.NewEncoder(out)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			handleMock(encoder, line)
		}
		if err != nil {
			return
		}
	}
}

func handleMock(encoder *json.Encoder, line []byte) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	reply := func(result string) {
		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"result":  json.RawMessage(result),
		})
	}
	switch msg.Method {
	case "initialize":
		reply(`{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"mock","version":"1"}}`)
	case "tools/list":
		reply(`{"tools":[{"name":"open_binary","inputSchema":{"type":"object"}},{"name":"binary_session","inputSchema":{"type":"object"}}]}`)
	case "tools/call":
		reply(`{"content":[{"type":"text","text":"ok"}],"structuredContent":{"active":true}}`)
	case "notifications/initialized", "notifications/cancelled":
	}
}
