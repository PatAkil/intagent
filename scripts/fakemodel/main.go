// Command fakemodel is a scripted stand-in for three model APIs: OpenAI chat
// completions, the OpenAI Responses API and the Gemini API. Agents that accept
// a custom model endpoint (GitHub Copilot CLI, Codex, Gemini CLI) can then be
// driven through intagent's hooks offline, deterministically and for free.
//
// The script is a JSON file of turns, picked by how many model turns (or tool
// results, for the Responses API) the conversation already holds:
//
//	{"turns": [{"tool": ["edit"], "args": {"edit": {"path": "a.go"}}}, {"text": "done"}]}
//
// A tool turn calls the first listed tool the agent offers, with "args" for a
// function tool or "input" for a freeform one such as Codex's apply_patch.
// Every request body is saved under -log, so a test can read what the agent
// told the model.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

type turn struct {
	Tool  []string                  `json:"tool"`
	Args  map[string]map[string]any `json:"args"`
	Input map[string]string         `json:"input"`
	Text  string                    `json:"text"`
}

// call is the tool call a turn resolved to.
type call struct {
	name, kind string // kind is "function" or "custom"
	args       map[string]any
	input      string
}

type fake struct {
	script, logDir string
	n              atomic.Int64
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18090", "address to listen on")
	f := &fake{}
	flag.StringVar(&f.script, "script", "script.json", "the turns to play")
	flag.StringVar(&f.logDir, "log", ".", "directory for the request log")
	flag.Parse()
	srv := &http.Server{Addr: *addr, Handler: f, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, map[string]any{"object": "list", "data": []any{map[string]string{"id": "fake-model", "object": "model"}}})
		return
	}
	i := f.n.Add(1)
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(filepath.Join(f.logDir, fmt.Sprintf("req-%03d.json", i)), raw, 0o600); err != nil {
		log.Print(err)
	}
	id := fmt.Sprintf("call_%d", i)
	var err error
	switch p := r.URL.Path; {
	case strings.HasSuffix(p, ":countTokens"):
		writeJSON(w, map[string]int{"totalTokens": 10})
	case strings.Contains(p, "enerateContent"):
		err = f.gemini(w, raw, strings.HasSuffix(p, ":streamGenerateContent"))
	case strings.HasSuffix(p, "/responses"):
		err = f.responses(w, raw, id)
	default:
		err = f.chat(w, raw, id)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// pick chooses the turn after the given number of model turns and resolves
// its tool call against the tools the agent offered (name to kind).
func (f *fake) pick(turns int, offered map[string]string) (call, string, error) {
	data, err := os.ReadFile(f.script)
	if err != nil {
		return call{}, "", err
	}
	var s struct{ Turns []turn }
	if err := json.Unmarshal(data, &s); err != nil {
		return call{}, "", fmt.Errorf("script %s: %w", f.script, err)
	}
	if len(s.Turns) == 0 {
		return call{}, "", fmt.Errorf("script %s has no turns", f.script)
	}
	t := s.Turns[min(turns, len(s.Turns)-1)]
	if len(t.Tool) == 0 {
		return call{}, t.Text, nil
	}
	for _, name := range t.Tool {
		if kind, ok := offered[name]; ok {
			return call{name: name, kind: kind, args: t.Args[name], input: t.Input[name]}, "", nil
		}
	}
	return call{}, fmt.Sprintf("NO MATCHING TOOL; offered: %v", offered), nil
}

// chat answers an OpenAI chat completions request.
func (f *fake) chat(w http.ResponseWriter, raw []byte, id string) error {
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	turns, offered := 0, map[string]string{}
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			turns++
		}
	}
	for _, t := range req.Tools {
		offered[t.Function.Name] = "function"
	}
	c, text, err := f.pick(turns, offered)
	if err != nil {
		return err
	}
	msg := map[string]any{"role": "assistant", "content": text}
	finish := "stop"
	if c.name != "" {
		args, _ := json.Marshal(c.args)
		msg["content"] = nil
		msg["tool_calls"] = []any{map[string]any{"index": 0, "id": id, "type": "function",
			"function": map[string]string{"name": c.name, "arguments": string(args)}}}
		finish = "tool_calls"
	}
	usage := map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	base := map[string]any{"id": "chatcmpl-" + id, "created": time.Now().Unix(), "model": req.Model}
	if !req.Stream {
		base["object"] = "chat.completion"
		base["choices"] = []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}
		base["usage"] = usage
		writeJSON(w, base)
		return nil
	}
	chunk := func(choice, extra map[string]any) map[string]any {
		out := map[string]any{"object": "chat.completion.chunk", "choices": []any{choice}}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	writeSSE(w, "data: %s\n\n",
		chunk(map[string]any{"index": 0, "delta": msg, "finish_reason": nil}, nil),
		chunk(map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}, map[string]any{"usage": usage}))
	fmt.Fprint(w, "data: [DONE]\n\n")
	return nil
}

// rtool is a Responses API tool; a namespace holds more tools.
type rtool struct {
	Type  string  `json:"type"`
	Name  string  `json:"name"`
	Tools []rtool `json:"tools"`
}

func collect(offered map[string]string, tools []rtool) {
	for _, t := range tools {
		if t.Type == "namespace" {
			collect(offered, t.Tools)
		} else if t.Name != "" {
			offered[t.Name] = t.Type
		}
	}
}

// responses answers an OpenAI Responses API request, as Codex streams it.
// Codex offers its tools in the request or in an "additional_tools" input item.
func (f *fake) responses(w http.ResponseWriter, raw []byte, id string) error {
	var req struct {
		Input []struct {
			Type  string  `json:"type"`
			Tools []rtool `json:"tools"`
		} `json:"input"`
		Tools []rtool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	turns, offered := 0, map[string]string{}
	collect(offered, req.Tools)
	for _, it := range req.Input {
		if strings.HasSuffix(it.Type, "_call_output") {
			turns++
		}
		collect(offered, it.Tools)
	}
	c, text, err := f.pick(turns, offered)
	if err != nil {
		return err
	}
	item := map[string]any{"type": "message", "role": "assistant", "id": "msg_" + id,
		"content": []any{map[string]string{"type": "output_text", "text": text}}}
	switch {
	case c.kind == "custom":
		item = map[string]any{"type": "custom_tool_call", "call_id": id, "name": c.name, "input": c.input}
	case c.name != "":
		args, _ := json.Marshal(c.args)
		item = map[string]any{"type": "function_call", "call_id": id, "name": c.name, "arguments": string(args)}
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, ev := range []map[string]any{
		{"type": "response.created", "response": map[string]string{"id": "resp_" + id}},
		{"type": "response.output_item.done", "item": item},
		{"type": "response.completed", "response": map[string]any{"id": "resp_" + id, "usage": usage}},
	} {
		writeSSE(w, "event: "+ev["type"].(string)+"\ndata: %s\n\n", ev)
	}
	return nil
}

// gemini answers a Gemini generateContent request, streamed as server-sent
// events when the agent asked for streamGenerateContent.
func (f *fake) gemini(w http.ResponseWriter, raw []byte, stream bool) error {
	var req struct {
		Contents []struct {
			Role string `json:"role"`
		} `json:"contents"`
		Tools []struct {
			FunctionDeclarations []struct {
				Name string `json:"name"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	turns, offered := 0, map[string]string{}
	for _, c := range req.Contents {
		if c.Role == "model" {
			turns++
		}
	}
	for _, t := range req.Tools {
		for _, d := range t.FunctionDeclarations {
			offered[d.Name] = "function"
		}
	}
	c, text, err := f.pick(turns, offered)
	if err != nil {
		return err
	}
	part := map[string]any{"text": text}
	if c.name != "" {
		part = map[string]any{"functionCall": map[string]any{"name": c.name, "args": c.args}}
	}
	resp := map[string]any{
		"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{part}},
			"finishReason": "STOP", "index": 0}},
		"usageMetadata": map[string]int{"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15},
		"modelVersion":  "fake",
	}
	if !stream {
		writeJSON(w, resp)
		return nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	writeSSE(w, "data: %s\r\n\r\n", resp)
	return nil
}

func writeSSE(w http.ResponseWriter, format string, events ...map[string]any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.Header().Set("Cache-Control", "no-cache")
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, format, b)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
