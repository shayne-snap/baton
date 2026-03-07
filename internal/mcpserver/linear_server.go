package mcpserver

import (
	"bufio"
	"bytes"
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
	trackerMCPServerName       = "baton-tracker"
	trackerMCPServerVersion    = "0.1.0"
	defaultMCPProtocolVersion  = "2025-11-25"
	messageFormatContentLength = "content-length"
	messageFormatJSONLine      = "json-line"
)

type trackerServer struct {
	in       *bufio.Reader
	out      *bufio.Writer
	executor *codex.DynamicToolExecutor
	format   string
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

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

func ServeLinearStdio(ctx context.Context, endpoint string, apiKey string) error {
	cfg := &config.Config{
		Tracker: config.TrackerConfig{
			Kind: "linear",
			Linear: config.TrackerLinearConfig{
				Endpoint: strings.TrimSpace(endpoint),
				APIKey:   strings.TrimSpace(apiKey),
			},
		},
	}
	return ServeTrackerStdio(ctx, cfg)
}

func ServeTrackerStdio(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("nil tracker config")
	}
	server := &trackerServer{
		in:       bufio.NewReader(os.Stdin),
		out:      bufio.NewWriter(os.Stdout),
		executor: codex.NewDynamicToolExecutor(cfg),
	}
	return server.serve(ctx)
}

func (s *trackerServer) serve(ctx context.Context) error {
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

func (s *trackerServer) handleRequest(ctx context.Context, req jsonRPCRequest) (any, *jsonRPCError) {
	switch req.Method {
	case "initialize":
		protocolVersion := requestedProtocolVersion(req.Params)
		if protocolVersion == "" {
			protocolVersion = defaultMCPProtocolVersion
		}
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    trackerMCPServerName,
				"version": trackerMCPServerVersion,
			},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "notifications/initialized":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{
			"tools": mcpToolSpecs(),
		}, nil
	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &jsonRPCError{Code: -32602, Message: "invalid params"}
		}
		toolName := strings.TrimSpace(params.Name)
		codexToolName := codexToolNameForMCPTool(toolName)
		if codexToolName == "" {
			return nil, &jsonRPCError{Code: -32601, Message: fmt.Sprintf("unknown tool %q", params.Name)}
		}
		result := s.executor.Execute(ctx, codexToolName, params.Arguments)
		return toMCPToolResult(result), nil
	case "resources/list", "prompts/list":
		return map[string]any{}, nil
	default:
		return nil, &jsonRPCError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}
	}
}

func (s *trackerServer) readMessage() ([]byte, error) {
	if err := s.detectMessageFormat(); err != nil {
		return nil, err
	}
	if s.format == messageFormatJSONLine {
		return s.readJSONLineMessage()
	}
	return s.readContentLengthMessage()
}

func (s *trackerServer) detectMessageFormat() error {
	if s.format != "" {
		return nil
	}
	for {
		peeked, err := s.in.Peek(1)
		if err != nil {
			return err
		}
		switch peeked[0] {
		case ' ', '\t', '\r', '\n':
			if _, err := s.in.ReadByte(); err != nil {
				return err
			}
		case '{', '[':
			s.format = messageFormatJSONLine
			return nil
		default:
			s.format = messageFormatContentLength
			return nil
		}
	}
}

func (s *trackerServer) readJSONLineMessage() ([]byte, error) {
	for {
		line, err := s.in.ReadBytes('\n')
		if err != nil && !(err == io.EOF && len(line) > 0) {
			return nil, err
		}
		payload := bytes.TrimSpace(line)
		if len(payload) == 0 {
			if err == io.EOF {
				return nil, io.EOF
			}
			continue
		}
		return payload, nil
	}
}

func (s *trackerServer) readContentLengthMessage() ([]byte, error) {
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

func (s *trackerServer) writeResult(id json.RawMessage, result any) error {
	return s.writeMessage(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func (s *trackerServer) writeError(id json.RawMessage, code int, message string) error {
	return s.writeMessage(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonRPCError{
			Code:    code,
			Message: message,
		},
	})
}

func (s *trackerServer) writeMessage(msg jsonRPCResponse) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if s.format == messageFormatJSONLine {
		if _, err := s.out.Write(payload); err != nil {
			return err
		}
		if err := s.out.WriteByte('\n'); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(s.out, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
			return err
		}
		if _, err := s.out.Write(payload); err != nil {
			return err
		}
	}
	return s.out.Flush()
}

func requestedProtocolVersion(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var params initializeParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return ""
	}
	return strings.TrimSpace(params.ProtocolVersion)
}

func mcpToolSpecs() []map[string]any {
	specs := codex.ToolSpecs()
	if len(specs) == 0 {
		return []map[string]any{}
	}
	tools := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
		name := strings.TrimSpace(stringValue(spec["name"]))
		if name == "" {
			continue
		}
		tools = append(tools, map[string]any{
			"name":        name,
			"description": stringValue(spec["description"]),
			"inputSchema": spec["inputSchema"],
		})
	}
	return tools
}

func codexToolNameForMCPTool(name string) string {
	for _, spec := range codex.ToolSpecs() {
		specName := strings.TrimSpace(stringValue(spec["name"]))
		if specName == name {
			return specName
		}
	}
	return ""
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
