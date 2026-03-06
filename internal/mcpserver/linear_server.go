package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"baton/internal/codex"
	"baton/internal/config"
)

const (
	linearMCPServerName    = "baton-linear"
	linearMCPServerVersion = "0.1.0"
	linearMCPToolName      = "graphql"
	mcpProtocolVersion     = "2024-11-05"
)

type linearServer struct {
	in       *bufio.Reader
	out      *bufio.Writer
	executor *codex.DynamicToolExecutor
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func ServeLinearStdio(ctx context.Context, endpoint string, apiKey string) error {
	cfg := &config.Config{
		Tracker: config.TrackerConfig{
			Kind:     "linear",
			Endpoint: strings.TrimSpace(endpoint),
			APIKey:   strings.TrimSpace(apiKey),
		},
	}
	server := &linearServer{
		in:       bufio.NewReader(os.Stdin),
		out:      bufio.NewWriter(os.Stdout),
		executor: codex.NewDynamicToolExecutor(cfg),
	}
	return server.serve(ctx)
}

func (s *linearServer) serve(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		payload, err := s.readMessage()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			if err := s.writeError(nil, -32700, "parse error"); err != nil {
				return err
			}
			continue
		}

		if len(req.ID) == 0 {
			continue
		}

		result, rpcErr := s.handleRequest(ctx, req)
		if rpcErr != nil {
			if err := s.writeError(req.ID, rpcErr.Code, rpcErr.Message); err != nil {
				return err
			}
			continue
		}
		if err := s.writeResult(req.ID, result); err != nil {
			return err
		}
	}
}

func (s *linearServer) handleRequest(ctx context.Context, req jsonRPCRequest) (any, *jsonRPCError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    linearMCPServerName,
				"version": linearMCPServerVersion,
			},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "notifications/initialized":
		return map[string]any{}, nil
	case "tools/list":
		spec := firstToolSpec()
		return map[string]any{
			"tools": []map[string]any{
				{
					"name":        linearMCPToolName,
					"description": stringValue(spec["description"]),
					"inputSchema": spec["inputSchema"],
				},
			},
		}, nil
	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &jsonRPCError{Code: -32602, Message: "invalid params"}
		}
		if params.Name != linearMCPToolName {
			return nil, &jsonRPCError{Code: -32601, Message: fmt.Sprintf("unknown tool %q", params.Name)}
		}
		result := s.executor.Execute(ctx, codex.LinearGraphQLTool, params.Arguments)
		return toMCPToolResult(result), nil
	case "resources/list", "prompts/list":
		return map[string]any{}, nil
	default:
		return nil, &jsonRPCError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}
	}
}

func (s *linearServer) readMessage() ([]byte, error) {
	contentLength := -1
	for {
		line, err := s.in.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			raw := strings.TrimSpace(line[len("content-length:"):])
			value, err := strconv.Atoi(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid content length %q: %w", raw, err)
			}
			contentLength = value
		}
	}
	if contentLength < 0 {
		return nil, fmt.Errorf("missing content length header")
	}
	payload := make([]byte, contentLength)
	if _, err := io.ReadFull(s.in, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (s *linearServer) writeResult(id json.RawMessage, result any) error {
	return s.writeMessage(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func (s *linearServer) writeError(id json.RawMessage, code int, message string) error {
	return s.writeMessage(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonRPCError{
			Code:    code,
			Message: message,
		},
	})
}

func (s *linearServer) writeMessage(msg jsonRPCResponse) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.out, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	if _, err := s.out.Write(payload); err != nil {
		return err
	}
	return s.out.Flush()
}

func firstToolSpec() map[string]any {
	specs := codex.ToolSpecs()
	if len(specs) == 0 {
		return map[string]any{}
	}
	return specs[0]
}

func toMCPToolResult(result map[string]any) map[string]any {
	content := []map[string]any{}
	items, _ := result["contentItems"].([]any)
	for _, item := range items {
		entry, _ := item.(map[string]any)
		if stringValue(entry["type"]) != "inputText" {
			continue
		}
		content = append(content, map[string]any{
			"type": "text",
			"text": stringValue(entry["text"]),
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{
			"type": "text",
			"text": "{}",
		})
	}
	return map[string]any{
		"content": content,
		"isError": !boolValue(result["success"]),
	}
}

func boolValue(value any) bool {
	typed, _ := value.(bool)
	return typed
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return fmt.Sprintf("%v", value)
	}
}
