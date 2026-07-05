// Package channel is the local stdio adapter that bridges a Claude Code session to central
// switchboard (ADR-013). Claude Code spawns `switchboard channel` as a stdio MCP subprocess (via
// .mcp.json) with a vended credential in the environment. The adapter:
//
//   - speaks newline-delimited JSON-RPC (MCP stdio transport) with Claude Code;
//   - declares capabilities.experimental["claude/channel"] + tools, so it is BOTH a channel (push)
//     and a tool server (claim/complete) over one connection;
//   - proxies tool calls to central switchboard's agent API over HTTP with the credential; and
//   - subscribes to the agent SSE stream and, for each new todo, emits notifications/claude/channel,
//     which lands in the session as <channel source="switchboard" …>…</channel>.
//
// The durable queue is the ledger; a push is a doorbell (ADR-013). One-way for the MVP — the reply
// tool + permission relay are deferred.
package channel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	serverName    = "switchboard"
	serverVersion = "0.1.0"
	protocolVer   = "2024-11-05"
)

// Run drives the stdio adapter until stdin closes or ctx is cancelled.
func Run(ctx context.Context, baseURL, token string) error {
	if baseURL == "" || token == "" {
		return fmt.Errorf("channel: SWITCHBOARD_URL and SWITCHBOARD_TOKEN are required")
	}
	a := &adapter{
		base:   strings.TrimRight(baseURL, "/"),
		token:  token,
		out:    json.NewEncoder(os.Stdout),
		client: &http.Client{Timeout: 30 * time.Second},
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcMessage
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		a.dispatch(ctx, req)
	}
	return scanner.Err()
}

type adapter struct {
	base    string
	token   string
	client  *http.Client
	mu      sync.Mutex // serializes writes to stdout
	out     *json.Encoder
	started atomic.Bool // channel push goroutine started after "initialized"
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (a *adapter) dispatch(ctx context.Context, req rpcMessage) {
	switch req.Method {
	case "initialize":
		a.reply(req.ID, a.initializeResult(req.Params))
	case "notifications/initialized":
		// Client is ready; begin pushing todos into the session.
		if a.started.CompareAndSwap(false, true) {
			go a.streamTodos(ctx)
		}
	case "tools/list":
		a.reply(req.ID, map[string]any{"tools": toolDefs()})
	case "tools/call":
		a.reply(req.ID, a.callTool(ctx, req.Params))
	case "ping":
		a.reply(req.ID, map[string]any{})
	default:
		if len(req.ID) > 0 { // a request we don't handle → method-not-found
			a.replyErr(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (a *adapter) initializeResult(params json.RawMessage) map[string]any {
	ver := protocolVer
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		ver = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": ver,
		"capabilities": map[string]any{
			"tools":        map[string]any{},
			"experimental": map[string]any{"claude/channel": map[string]any{}},
		},
		"serverInfo":   map[string]any{"name": serverName, "version": serverVersion},
		"instructions": "Todos routed to you arrive as <channel source=\"switchboard\" …> events — a doorbell. Use list_todos to see work, claim to take one (sets a lease), then complete or fail. The durable queue is the record; a missed push is not a lost todo.",
	}
}

// --- MCP tool surface (proxied to the central agent API) ---

func toolDefs() []map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	str := map[string]any{"type": "string"}
	return []map[string]any{
		{"name": "list_todos", "description": "List todos in this endpoint's queues.",
			"inputSchema": obj(map[string]any{
				"queue": str, "state": map[string]any{"type": "string", "enum": []string{"pending", "claimed", "done", "failed"}},
				"limit": map[string]any{"type": "integer"}})},
		{"name": "claim", "description": "Claim a pending todo by id (sets a lease you must complete or fail).",
			"inputSchema": obj(map[string]any{"id": str, "lease_ttl_seconds": map[string]any{"type": "integer"}}, "id")},
		{"name": "complete", "description": "Complete a claimed todo you own.",
			"inputSchema": obj(map[string]any{"id": str, "result": map[string]any{"type": "object"}}, "id")},
		{"name": "fail", "description": "Fail a claimed todo (retries until max attempts, then dead-letters).",
			"inputSchema": obj(map[string]any{"id": str, "result": map[string]any{"type": "object"}}, "id")},
	}
}

func (a *adapter) callTool(ctx context.Context, params json.RawMessage) map[string]any {
	var p struct {
		Name string         `json:"name"`
		Args map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return toolError("bad tool params")
	}
	id, _ := p.Args["id"].(string)
	switch p.Name {
	case "list_todos":
		q := url("/agent/todos", p.Args, "queue", "state", "limit")
		return a.proxy(ctx, http.MethodGet, q, nil)
	case "claim":
		if id == "" {
			return toolError("id is required")
		}
		return a.proxy(ctx, http.MethodPost, "/agent/todos/"+id+"/claim", body(p.Args, "lease_ttl_seconds"))
	case "complete":
		if id == "" {
			return toolError("id is required")
		}
		return a.proxy(ctx, http.MethodPost, "/agent/todos/"+id+"/complete", body(p.Args, "result"))
	case "fail":
		if id == "" {
			return toolError("id is required")
		}
		return a.proxy(ctx, http.MethodPost, "/agent/todos/"+id+"/fail", body(p.Args, "result"))
	default:
		return toolError("unknown tool: " + p.Name)
	}
}

func (a *adapter) proxy(ctx context.Context, method, path string, reqBody []byte) map[string]any {
	var rdr io.Reader
	if reqBody != nil {
		rdr = bytes.NewReader(reqBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rdr)
	if err != nil {
		return toolError(err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return toolError("switchboard unreachable: " + err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	isErr := resp.StatusCode >= 400
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(b)}},
		"isError": isErr,
	}
}

// --- Channels push: subscribe to the SSE stream and notify the session ---

func (a *adapter) streamTodos(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if a.consumeStream(ctx) {
			backoff = time.Second // clean end; reset
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// consumeStream connects once and pushes todos until the stream drops. Returns true on a graceful end.
func (a *adapter) consumeStream(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/agent/stream", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{} // no timeout: long-lived stream
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(line[5:]))
		case line == "":
			if data.Len() > 0 {
				a.pushTodo(data.String())
				data.Reset()
			}
		}
	}
	return sc.Err() == nil
}

func (a *adapter) pushTodo(dataJSON string) {
	var t struct {
		ID     string `json:"id"`
		Queue  string `json:"queue"`
		Kind   string `json:"kind"`
		Source string `json:"source"`
		Title  string `json:"title"`
	}
	if json.Unmarshal([]byte(dataJSON), &t) != nil || t.ID == "" {
		return
	}
	// Breakout guard: never let payload content close the <channel> wrapper (ADR-013).
	title := strings.ReplaceAll(t.Title, "</channel>", "«/channel»")
	content := fmt.Sprintf("switchboard: todo %s on queue %q — %s", t.ID, t.Queue, title)
	meta := map[string]any{"todo_id": t.ID, "queue": t.Queue}
	if t.Kind != "" {
		meta["kind"] = t.Kind
	}
	if t.Source != "" {
		meta["source"] = t.Source
	}
	a.notify("notifications/claude/channel", map[string]any{"content": content, "meta": meta})
}

// --- JSON-RPC writers (stdout is the pipe; nothing else may write to it) ---

func (a *adapter) reply(id json.RawMessage, result any) {
	if len(id) == 0 {
		return // notification — no response
	}
	a.write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(id), "result": result})
}

func (a *adapter) replyErr(id json.RawMessage, code int, msg string) {
	a.write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(id), "error": map[string]any{"code": code, "message": msg}})
}

func (a *adapter) notify(method string, params any) {
	a.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (a *adapter) write(v any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.out.Encode(v) // json.Encoder writes a trailing newline → framing for stdio transport
}

// --- small helpers ---

func toolError(msg string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}, "isError": true}
}

func rawOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func url(path string, args map[string]any, keys ...string) string {
	var qs []string
	for _, k := range keys {
		if v, ok := args[k]; ok && v != nil {
			qs = append(qs, k+"="+fmt.Sprintf("%v", v))
		}
	}
	if len(qs) == 0 {
		return path
	}
	return path + "?" + strings.Join(qs, "&")
}

func body(args map[string]any, keys ...string) []byte {
	m := map[string]any{}
	for _, k := range keys {
		if v, ok := args[k]; ok {
			m[k] = v
		}
	}
	b, _ := json.Marshal(m)
	return b
}
