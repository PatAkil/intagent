// Command fakemodel is a scripted stand-in for an OpenAI-compatible chat
// completions API, so that agents which accept a custom model provider (GitHub
// Copilot CLI among them) can be driven through intagent's hooks offline,
// deterministically and for free.
//
// The script is a JSON file of turns, picked by the number of assistant
// messages already in the conversation:
//
//	{"turns": [{"tool": ["edit"], "args": {"edit": {"path": "a.go"}}}, {"text": "done"}]}
//
// A tool turn calls the first listed tool the agent offers. Every request body
// is saved under -log, so a test can read what the agent told the model.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"
)

type turn struct {
	Tool []string                  `json:"tool"`
	Args map[string]map[string]any `json:"args"`
	Text string                    `json:"text"`
}

type request struct {
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

type toolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18090", "address to listen on")
	script := flag.String("script", "script.json", "the turns to play")
	logDir := flag.String("log", ".", "directory for the request log")
	flag.Parse()
	var n atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []any{map[string]string{"id": "fake-model", "object": "model"}}})
	})
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		i := n.Add(1)
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := os.WriteFile(filepath.Join(*logDir, fmt.Sprintf("req-%03d.json", i)), raw, 0o600); err != nil {
			log.Print(err)
		}
		var req request
		_ = json.Unmarshal(raw, &req)
		t, err := pick(*script, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		answer(w, req, t, fmt.Sprintf("call_%d", i))
	})
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// pick chooses the turn for a request and resolves its tool call.
func pick(script string, req request) (turn, error) {
	data, err := os.ReadFile(script)
	if err != nil {
		return turn{}, err
	}
	var s struct{ Turns []turn }
	if err := json.Unmarshal(data, &s); err != nil {
		return turn{}, fmt.Errorf("script %s: %w", script, err)
	}
	if len(s.Turns) == 0 {
		return turn{}, fmt.Errorf("script %s has no turns", script)
	}
	n := 0
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			n++
		}
	}
	t := s.Turns[min(n, len(s.Turns)-1)]
	if len(t.Tool) == 0 {
		return t, nil
	}
	var offered []string
	for _, tool := range req.Tools {
		offered = append(offered, tool.Function.Name)
	}
	for _, name := range t.Tool {
		if slices.Contains(offered, name) {
			return turn{Tool: []string{name}, Args: map[string]map[string]any{name: t.Args[name]}}, nil
		}
	}
	return turn{Text: fmt.Sprintf("NO MATCHING TOOL; offered: %v", offered)}, nil
}

func answer(w http.ResponseWriter, req request, t turn, id string) {
	msg := map[string]any{"role": "assistant", "content": nil}
	finish := "stop"
	if len(t.Tool) > 0 {
		args, _ := json.Marshal(t.Args[t.Tool[0]])
		var c toolCall
		c.ID, c.Type, c.Function.Name, c.Function.Arguments = id, "function", t.Tool[0], string(args)
		msg["tool_calls"] = []toolCall{c}
		finish = "tool_calls"
	} else {
		msg["content"] = t.Text
	}
	usage := map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	base := map[string]any{"id": "chatcmpl-" + id, "created": time.Now().Unix(), "model": req.Model}
	if !req.Stream {
		base["object"] = "chat.completion"
		base["choices"] = []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}
		base["usage"] = usage
		writeJSON(w, base)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	event := func(choice map[string]any, extra map[string]any) {
		chunk := map[string]any{"object": "chat.completion.chunk", "choices": []any{choice}}
		for k, v := range base {
			chunk[k] = v
		}
		for k, v := range extra {
			chunk[k] = v
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	event(map[string]any{"index": 0, "delta": msg, "finish_reason": nil}, nil)
	event(map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}, map[string]any{"usage": usage})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
