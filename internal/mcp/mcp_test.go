package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testServer() *Server {
	return &Server{
		Name: "intagent", Version: "test", Instructions: "Use declare_intent.",
		Tools: []Tool{
			{
				Name: "echo", Description: "Echoes text.", ReadOnly: true,
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
				Handler: func(_ context.Context, c Call) (string, error) {
					var a struct{ Text string }
					if err := Args(c, &a); err != nil {
						return "", err
					}
					if sid, _ := c.Meta["sessionId"].(string); sid != "" {
						return a.Text + " from " + sid, nil
					}
					return a.Text, nil
				},
			},
			{Name: "fail", Description: "Fails.", InputSchema: map[string]any{"type": "object"},
				Handler: func(context.Context, Call) (string, error) { return "", errors.New("server unreachable") }},
		},
	}
}

func run(t *testing.T, input string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := testServer().Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("server wrote a non-JSON line %q: %v", sc.Text(), err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

// The transcript Claude Code 2.1.288 sends, including its version probe.
const claudeSession = `{"jsonrpc":"2.0","id":"server-discover-probe-1","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}
{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"claude-code","version":"2.1.288"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"},"_meta":{"claudecode/toolUseId":"toolu_01"}}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"yo"},"_meta":{"sessionId":"01a0"}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fail","arguments":{}}}
{"jsonrpc":"2.0","id":5,"method":"ping"}
`

func TestSession(t *testing.T) {
	msgs := run(t, claudeSession)
	if len(msgs) != 7 {
		t.Fatalf("got %d replies, want 7 (the notification gets none): %v", len(msgs), msgs)
	}
	probe := msgs[0]
	if probe["id"] != "server-discover-probe-1" || probe["error"].(map[string]any)["code"].(float64) != codeMethodNotFound {
		t.Fatalf("probe reply = %v", probe)
	}
	init := msgs[1]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-11-25" || init["instructions"] != "Use declare_intent." {
		t.Fatalf("initialize = %v", init)
	}
	if _, ok := init["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatal("tools capability missing")
	}
	tools := msgs[2]["result"].(map[string]any)["tools"].([]any)
	echo := tools[0].(map[string]any)
	ann := echo["annotations"].(map[string]any)
	if echo["name"] != "echo" || ann["readOnlyHint"] != true || ann["destructiveHint"] != false || ann["openWorldHint"] != false {
		t.Fatalf("tool listing = %v", echo)
	}
	text := func(m map[string]any) (string, bool) {
		r := m["result"].(map[string]any)
		return r["content"].([]any)[0].(map[string]any)["text"].(string), r["isError"].(bool)
	}
	if got, isErr := text(msgs[3]); got != "hi" || isErr {
		t.Fatalf("echo = %q %v", got, isErr)
	}
	if got, _ := text(msgs[4]); got != "yo from 01a0" {
		t.Fatalf("meta not passed: %q", got)
	}
	if got, isErr := text(msgs[5]); got != "server unreachable" || !isErr {
		t.Fatalf("tool error = %q %v", got, isErr)
	}
	if msgs[6]["id"].(float64) != 5 {
		t.Fatalf("ping = %v", msgs[6])
	}
}

func TestVersionNegotiation(t *testing.T) {
	for req, want := range map[string]string{"2024-11-05": "2024-11-05", "2025-06-18": "2025-06-18", "2099-01-01": SupportedVersions[0], "": SupportedVersions[0]} {
		msgs := run(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+req+`"}}`+"\n")
		if got := msgs[0]["result"].(map[string]any)["protocolVersion"]; got != want {
			t.Errorf("client %q: server chose %v, want %s", req, got, want)
		}
	}
}

func TestMalformedInput(t *testing.T) {
	msgs := run(t, "{not json\n\n"+`{"jsonrpc":"1.0","id":7,"method":"x"}`+"\n"+`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"nope"}}`+"\n"+`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{"text":5}}}`+"\n")
	if len(msgs) != 4 {
		t.Fatalf("replies = %v", msgs)
	}
	codes := []float64{codeParse, codeInvalidRequest, codeInvalidParams}
	for i, c := range codes {
		if got := msgs[i]["error"].(map[string]any)["code"].(float64); got != c {
			t.Errorf("reply %d code %v, want %v", i, got, c)
		}
	}
	if r := msgs[3]["result"].(map[string]any); r["isError"] != true {
		t.Errorf("bad arguments should be a tool error: %v", r)
	}
	// A batch, and a request whose id is null, are invalid requests; a
	// notification (no id at all) still gets no answer.
	msgs = run(t, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`+"\n"+`{"jsonrpc":"2.0","id":null,"method":"ping"}`+"\n"+
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	if len(msgs) != 2 {
		t.Fatalf("replies = %v", msgs)
	}
	for i, m := range msgs {
		if got := m["error"].(map[string]any)["code"].(float64); got != codeInvalidRequest || m["id"] != nil {
			t.Errorf("reply %d = %v", i, m)
		}
	}
}
