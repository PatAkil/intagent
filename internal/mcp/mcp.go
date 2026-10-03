// Package mcp is a minimal Model Context Protocol server over stdio: enough of
// the protocol to offer tools to Claude Code, Codex, Cursor and other clients,
// with no dependencies.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
)

// SupportedVersions lists the protocol versions this server speaks, newest first.
var SupportedVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// Tool is one callable tool.
type Tool struct {
	Name        string
	Title       string
	Description string
	InputSchema map[string]any
	// ReadOnly marks tools that change nothing, so clients can run them without asking.
	ReadOnly bool
	Handler  func(ctx context.Context, call Call) (string, error)
}

// Call is one tools/call request.
type Call struct {
	Arguments json.RawMessage
	// Meta is the request's _meta object; clients put session ids there.
	Meta map[string]any
}

// Server answers MCP requests on a stream.
type Server struct {
	Name         string
	Version      string
	Instructions string
	Tools        []Tool

	mu sync.Mutex
	w  *bufio.Writer
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

const maxMessage = 16 << 20

// Serve reads newline-delimited JSON-RPC messages from r and writes answers to
// w until r ends or ctx is cancelled.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s.w = bufio.NewWriter(w)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxMessage)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := sc.Bytes()
		if len(trimSpace(line)) == 0 {
			continue
		}
		s.handle(ctx, line)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func (s *Server) handle(ctx context.Context, line []byte) {
	if line = trimSpace(line); len(line) > 0 && line[0] == '[' && json.Valid(line) {
		// MCP has no batches since 2025-06-18.
		s.reply(json.RawMessage("null"), nil, &rpcError{Code: codeInvalidRequest, Message: "batches are not supported"})
		return
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		s.reply(json.RawMessage("null"), nil, &rpcError{Code: codeParse, Message: "parse error"})
		return
	}
	if string(req.ID) == "null" {
		// MCP requires an id that is not null; only a missing id makes a notification.
		s.reply(req.ID, nil, &rpcError{Code: codeInvalidRequest, Message: "id must not be null"})
		return
	}
	notification := len(req.ID) == 0
	if req.JSONRPC != "2.0" || req.Method == "" {
		if !notification {
			s.reply(req.ID, nil, &rpcError{Code: codeInvalidRequest, Message: "invalid request"})
		}
		return
	}
	if notification {
		return // notifications/initialized, notifications/cancelled and others need no answer
	}
	result, rerr := s.dispatch(ctx, req)
	s.reply(req.ID, result, rerr)
}

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := SupportedVersions[0]
		if slices.Contains(SupportedVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
			"instructions":    s.Instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.Tools))
		for _, t := range s.Tools {
			ann := map[string]any{"readOnlyHint": t.ReadOnly, "destructiveHint": false, "openWorldHint": false, "idempotentHint": t.ReadOnly}
			if t.Title != "" {
				ann["title"] = t.Title
			}
			tools = append(tools, map[string]any{
				"name": t.Name, "title": t.Title, "description": t.Description,
				"inputSchema": t.InputSchema, "annotations": ann,
			})
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      map[string]any  `json:"_meta"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call needs a tool name"}
		}
		i := slices.IndexFunc(s.Tools, func(t Tool) bool { return t.Name == p.Name })
		if i < 0 {
			return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		text, err := s.Tools[i].Handler(ctx, Call{Arguments: p.Arguments, Meta: p.Meta})
		if err != nil {
			return toolResult(err.Error(), true), nil
		}
		return toolResult(text, false), nil
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

func (s *Server) reply(id json.RawMessage, result any, rerr *rpcError) {
	resp := response{JSONRPC: "2.0", ID: id, Result: result, Error: rerr}
	if rerr == nil && result == nil {
		resp.Result = map[string]any{}
	}
	b, err := json.Marshal(resp)
	if err != nil {
		b, _ = json.Marshal(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: -32603, Message: fmt.Sprintf("encode: %v", err)}})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.w.Write(append(b, '\n'))
	_ = s.w.Flush()
}

// Args decodes a tool call's arguments into v and reports a friendly error.
func Args(call Call, v any) error {
	if err := json.Unmarshal(call.Arguments, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
