package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestHandleRequestToolsListIncludesTrackerTools(t *testing.T) {
	t.Parallel()

	server := &trackerServer{}
	result, rpcErr := server.handleRequest(context.Background(), jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Method:  "tools/list",
	})
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}

	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}
	tools, _ := payload["tools"].([]map[string]any)
	if len(tools) < 2 {
		t.Fatalf("unexpected tools payload: %#v", payload)
	}
	var foundTrackerGetIssue bool
	for _, tool := range tools {
		if tool["name"] == "tracker_get_issue" {
			foundTrackerGetIssue = true
		}
	}
	if !foundTrackerGetIssue {
		t.Fatalf("missing expected tracker tool; tracker_get_issue=%v payload=%#v", foundTrackerGetIssue, payload)
	}
}

func TestToMCPToolResultMapsCodexExecutorPayload(t *testing.T) {
	t.Parallel()

	mapped := toMCPToolResult(map[string]any{
		"success": true,
		"contentItems": []any{
			map[string]any{"type": "inputText", "text": `{"data":{"viewer":{"id":"abc"}}}`},
		},
	})
	content, _ := mapped["content"].([]map[string]any)
	if len(content) != 1 || content[0]["type"] != "text" {
		t.Fatalf("unexpected content mapping: %#v", mapped)
	}
	if mapped["isError"] != false {
		t.Fatalf("unexpected error flag: %#v", mapped)
	}
}

func TestServeWritesMCPFramedResponse(t *testing.T) {
	t.Parallel()

	request := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}`
	writer := &testWriter{}
	server := &trackerServer{
		in:  bufioReader(frameMessage(request)),
		out: bufio.NewWriter(writer),
	}
	err := server.serve(context.Background())
	if err != nil {
		t.Fatalf("serve failed: %v", err)
	}
	output := writer.String()
	if !strings.Contains(output, "Content-Length:") {
		t.Fatalf("missing content-length header: %q", output)
	}
	if !strings.Contains(output, `"result":{}`) {
		t.Fatalf("missing result payload: %q", output)
	}
}

func TestServeSupportsJSONLineFraming(t *testing.T) {
	t.Parallel()

	request := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\",\"params\":{}}\n"
	writer := &testWriter{}
	server := &trackerServer{
		in:  bufioReader(request),
		out: bufio.NewWriter(writer),
	}
	err := server.serve(context.Background())
	if err != nil {
		t.Fatalf("serve failed: %v", err)
	}
	output := writer.String()
	if strings.Contains(output, "Content-Length:") {
		t.Fatalf("unexpected content-length header in json-line mode: %q", output)
	}
	if !strings.Contains(output, `"result":{}`) {
		t.Fatalf("missing result payload: %q", output)
	}
	if !strings.HasSuffix(output, "\n") {
		t.Fatalf("missing newline terminator: %q", output)
	}
}

func TestInitializeEchoesClientProtocolVersion(t *testing.T) {
	t.Parallel()

	server := &trackerServer{}
	result, rpcErr := server.handleRequest(context.Background(), jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2025-11-25"}`),
	})
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}
	if got := payload["protocolVersion"]; got != "2025-11-25" {
		t.Fatalf("unexpected protocol version: %#v", got)
	}
	serverInfo, _ := payload["serverInfo"].(map[string]any)
	if got := serverInfo["name"]; got != "baton-tracker" {
		t.Fatalf("unexpected server name: %#v", got)
	}
}

type testWriter struct {
	bytes.Buffer
}

func bufioReader(input string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(input))
}

func frameMessage(payload string) string {
	return "Content-Length: " + strconv.Itoa(len(payload)) + "\r\n\r\n" + payload
}
