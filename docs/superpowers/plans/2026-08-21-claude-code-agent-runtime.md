# Claude Code Agent Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a second, selectable ACP agent runtime to the migration harness — Claude Code (via `claude-agent-acp`, over stdio) alongside the existing goose runtime (over WebSocket) — with zero behavior change to goose when unselected.

**Architecture:** Extract a small transport interface (`rpcConn`) that `SessionClient` already implicitly depends on, so it works with either `WSClient` (goose) or a new `StdioClient` (claude-agent-acp, any stdio ACP agent). Add a `claude` lifecycle package mirroring `goose`'s. Branch `main.go`'s runtime construction on a new `HARNESS_AGENT_RUNTIME` config value; everything downstream of session creation (prompt building, watcher, push, exit status) is already runtime-agnostic and untouched.

**Tech Stack:** Go 1.26 (harness module), existing JSON-RPC 2.0 / ACP types in `internal/acp`.

**Spec:** `docs/superpowers/specs/2026-08-21-claude-code-agent-runtime-design.md`

## Global Constraints

- Zero behavior change to the goose runtime when `HARNESS_AGENT_RUNTIME` is unset, empty, or any value other than exactly `"claude"` (fail open to today's behavior).
- No new CRD fields, no controller changes, no new credential paths — reuse `KONVEYOR_LLM_*`/`GOOGLE_APPLICATION_CREDENTIALS_JSON` the controller already injects.
- Only `anthropic` and `gcp_vertex_ai` providers are mapped for the claude runtime; anything else logs a warning and forwards no credentials (matches the design's non-goals).
- ACP tee stays goose-only in this pass; `HARNESS_ACP_TEE=on` with the claude runtime logs a warning and the run proceeds without live viewers.
- The claude runtime runs unattended via `session/set_config_option` (`mode=bypassPermissions`), not a client-side permission auto-approve hack.
- No container image or `AgentWorkflow`/`AgentWorkflowRun` changes in this plan — Go code and unit tests only.

All file paths below are relative to `harness/` (the harness Go module root: `harness/go.mod`).

---

### Task 1: Extract the `rpcConn` transport interface

**Files:**
- Create: `internal/acp/conn.go`
- Modify: `internal/acp/session.go` (the `SessionClient` struct and `NewSessionClient`)
- Test: existing `internal/acp/*_test.go` (no new file — this task must not change behavior, only compile against an interface)

**Interfaces:**
- Produces: `rpcConn` interface (unexported, package `acp`) with methods `Call(ctx context.Context, method string, params any) (json.RawMessage, []*RPCResponse, error)`, `Send(req *Request) error`, `SendResponse(id json.RawMessage, result any, rpcErr *RPCError) error`, `SetAgentRequestHandler(fn func(*RPCResponse))`, `Done() <-chan struct{}`, `registerPending(id int64) chan *RPCResponse`, `unregisterPending(id int64)`, `addNotifSink() (int, chan *RPCResponse)`, `removeNotifSink(id int)`.
- Produces: `SessionClient.ws` is now typed `rpcConn` instead of `*WSClient`; `NewSessionClient(ws rpcConn) *SessionClient`.

- [ ] **Step 1: Create `internal/acp/conn.go`**

```go
package acp

import (
	"context"
	"encoding/json"
)

// rpcConn is the transport SessionClient needs: a JSON-RPC 2.0 connection
// to an ACP agent, request/response routing plus a notification-sink
// subscription model for in-flight calls. WSClient (goose, over
// WebSocket) and StdioClient (claude-agent-acp and other stdio-only
// agents) both satisfy it — SessionClient itself is transport-agnostic.
type rpcConn interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, []*RPCResponse, error)
	Send(req *Request) error
	SendResponse(id json.RawMessage, result any, rpcErr *RPCError) error
	SetAgentRequestHandler(fn func(*RPCResponse))
	Done() <-chan struct{}

	registerPending(id int64) chan *RPCResponse
	unregisterPending(id int64)
	addNotifSink() (int, chan *RPCResponse)
	removeNotifSink(id int)
}

var _ rpcConn = (*WSClient)(nil)
```

- [ ] **Step 2: Retype `SessionClient` in `internal/acp/session.go`**

Change:

```go
// SessionClient wraps WSClient with ACP session operations.
type SessionClient struct {
	ws          *WSClient
	initialized bool

	fwdMu     sync.Mutex
	forwarder PermissionForwarder
}

// NewSessionClient creates a session client from an existing WebSocket
// connection and takes over answering agent-initiated requests on it.
func NewSessionClient(ws *WSClient) *SessionClient {
	c := &SessionClient{ws: ws}
	ws.SetAgentRequestHandler(c.answerAgentRequest)
	return c
}
```

to:

```go
// SessionClient wraps an ACP transport (WSClient for goose, StdioClient
// for claude-agent-acp and other stdio agents) with ACP session
// operations.
type SessionClient struct {
	ws          rpcConn
	initialized bool

	fwdMu     sync.Mutex
	forwarder PermissionForwarder
}

// NewSessionClient creates a session client from an existing ACP
// connection and takes over answering agent-initiated requests on it.
func NewSessionClient(ws rpcConn) *SessionClient {
	c := &SessionClient{ws: ws}
	ws.SetAgentRequestHandler(c.answerAgentRequest)
	return c
}
```

Nothing else in `session.go` changes — every other method already calls
`c.ws.<method>` using only methods now declared on `rpcConn`.

- [ ] **Step 3: Build and run the existing test suite to confirm no behavior change**

Run: `cd harness && go build ./... && go test ./internal/acp/... ./internal/goose/... ./cmd/...`
Expected: PASS, identical to before this change (this task is a pure type-level refactor).

- [ ] **Step 4: Commit**

```bash
git add harness/internal/acp/conn.go harness/internal/acp/session.go
git commit -m "$(cat <<'EOF'
:seedling: Extract rpcConn transport interface from SessionClient

SessionClient only ever called a fixed set of WSClient methods.
Extracting that set as an interface lets a second ACP transport
(stdio, for non-goose agents) plug into the same session logic
without changing goose's behavior.
EOF
)"
```

---

### Task 2: Add `SessionClient.SetConfigOption`

**Files:**
- Modify: `internal/acp/session.go`
- Test: `internal/acp/session_config_test.go` (new)

**Interfaces:**
- Consumes: `rpcConn.Call` (Task 1).
- Produces: `SessionClient.SetConfigOption(ctx context.Context, sessionID, configID, value string) error`.

- [ ] **Step 1: Write the failing test**

Create `internal/acp/session_config_test.go`:

```go
package acp

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestSetConfigOption(t *testing.T) {
	s := newDemuxServer(t)
	c := s.dial(t)
	sc := NewSessionClient(c)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- sc.SetConfigOption(ctx, "s1", "mode", "bypassPermissions")
	}()

	req := s.next()
	if req["method"] != "session/set_config_option" {
		t.Fatalf("expected session/set_config_option, got %v", req["method"])
	}
	params, ok := req["params"].(map[string]any)
	if !ok {
		t.Fatalf("params not an object: %v", req["params"])
	}
	if params["sessionId"] != "s1" || params["configId"] != "mode" || params["value"] != "bypassPermissions" {
		t.Fatalf("unexpected params: %v", params)
	}

	id := int64(req["id"].(float64))
	s.push(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, id))

	if err := <-done; err != nil {
		t.Fatalf("SetConfigOption returned error: %v", err)
	}
}

func TestSetConfigOptionPropagatesRPCError(t *testing.T) {
	s := newDemuxServer(t)
	c := s.dial(t)
	sc := NewSessionClient(c)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- sc.SetConfigOption(ctx, "s1", "mode", "not-a-real-mode")
	}()

	req := s.next()
	id := int64(req["id"].(float64))
	s.push(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"unknown config option"}}`, id))

	err := <-done
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd harness && go test ./internal/acp/... -run TestSetConfigOption -v`
Expected: FAIL with "SetConfigOption undefined" (compile error).

- [ ] **Step 3: Add the method to `internal/acp/session.go`**

Add near `CreateSession`:

```go
// SetConfigOption sets a session-scoped config option (e.g. "mode",
// "model") via session/set_config_option. Used by the claude runtime to
// switch off tool-use confirmation prompts for unattended runs —
// answerAgentRequest's fail-closed deny remains the safety net if a
// permission request slips through anyway.
func (c *SessionClient) SetConfigOption(ctx context.Context, sessionID, configID, value string) error {
	_, _, err := c.ws.Call(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  configID,
		"value":     value,
	})
	if err != nil {
		return fmt.Errorf("session/set_config_option(%s=%s): %w", configID, value, err)
	}
	return nil
}
```

`session.go` already imports `context` and `fmt` — no new imports needed.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd harness && go test ./internal/acp/... -run TestSetConfigOption -v`
Expected: PASS (both `TestSetConfigOption` and `TestSetConfigOptionPropagatesRPCError`).

- [ ] **Step 5: Commit**

```bash
git add harness/internal/acp/session.go harness/internal/acp/session_config_test.go
git commit -m ":sparkles: Add SessionClient.SetConfigOption for session/set_config_option"
```

---

### Task 3: `StdioClient` — ACP over a subprocess's stdin/stdout

**Files:**
- Create: `internal/acp/stdioclient.go`
- Create: `internal/acp/stdioclient_test.go`
- Modify: `internal/acp/conn.go` (add the second interface-satisfaction assertion)

**Interfaces:**
- Consumes: `RPCResponse`, `RPCError`, `Request`, `Response`, `newRequest`, `drainNotifications` (all already defined in package `acp`, in `jsonrpc.go` / `wsclient.go`).
- Produces: `NewStdioClient(stdin io.WriteCloser, stdout io.ReadCloser) *StdioClient`, satisfying `rpcConn`.

- [ ] **Step 1: Write the failing tests**

Create `internal/acp/stdioclient_test.go`:

```go
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeAgent simulates a stdio ACP agent: it reads the client's requests
// off one pipe and lets the test push frames to the client on another —
// the stdio analog of wsclient_test.go's demuxServer.
type fakeAgent struct {
	t       *testing.T
	toAgent *io.PipeReader
	toClnt  *io.PipeWriter
	inbound chan map[string]any
}

func newFakeAgent(t *testing.T) (*fakeAgent, *StdioClient) {
	t.Helper()
	agentStdinR, agentStdinW := io.Pipe()
	agentStdoutR, agentStdoutW := io.Pipe()

	f := &fakeAgent{t: t, toAgent: agentStdinR, toClnt: agentStdoutW, inbound: make(chan map[string]any, 32)}
	go f.readLoop()

	client := NewStdioClient(agentStdinW, agentStdoutR)
	t.Cleanup(func() {
		agentStdinW.Close()
		agentStdoutW.Close()
	})
	return f, client
}

func (f *fakeAgent) readLoop() {
	scanner := bufio.NewScanner(f.toAgent)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var m map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			f.t.Errorf("agent unmarshal: %v", err)
			continue
		}
		f.inbound <- m
	}
}

func (f *fakeAgent) push(frame string) {
	if _, err := f.toClnt.Write([]byte(frame + "\n")); err != nil {
		f.t.Errorf("agent write: %v", err)
	}
}

func (f *fakeAgent) next() map[string]any {
	select {
	case m := <-f.inbound:
		return m
	case <-time.After(5 * time.Second):
		f.t.Fatal("timed out waiting for a client frame")
		return nil
	}
}

func TestStdioClientCallRoundTrip(t *testing.T) {
	f, c := newFakeAgent(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	var result json.RawMessage
	go func() {
		var err error
		result, _, err = c.Call(ctx, "initialize", map[string]any{"protocolVersion": 1})
		done <- err
	}()

	req := f.next()
	if req["method"] != "initialize" {
		t.Fatalf("expected initialize, got %v", req["method"])
	}
	id := int64(req["id"].(float64))
	f.push(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"ok":true}}`, id))

	if err := <-done; err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(result), "ok") {
		t.Fatalf("unexpected result: %s", result)
	}
}

func TestStdioClientNotificationDuringCall(t *testing.T) {
	f, c := newFakeAgent(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type callOut struct {
		result json.RawMessage
		notifs []*RPCResponse
		err    error
	}
	done := make(chan callOut, 1)
	go func() {
		result, notifs, err := c.Call(ctx, "session/prompt", nil)
		done <- callOut{result, notifs, err}
	}()

	req := f.next()
	id := int64(req["id"].(float64))

	f.push(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1"}}`)
	f.push(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"stopReason":"end_turn"}}`, id))

	out := <-done
	if out.err != nil {
		t.Fatalf("Call: %v", out.err)
	}
	if len(out.notifs) != 1 || out.notifs[0].Method != "session/update" {
		t.Fatalf("expected 1 session/update notification, got %v", out.notifs)
	}
}

func TestStdioClientAgentRequestDispatchAndReply(t *testing.T) {
	f, c := newFakeAgent(t)

	got := make(chan *RPCResponse, 1)
	c.SetAgentRequestHandler(func(req *RPCResponse) {
		got <- req
		if err := c.SendResponse(req.ID, map[string]any{"outcome": "selected"}, nil); err != nil {
			t.Errorf("SendResponse: %v", err)
		}
	})

	f.push(`{"jsonrpc":"2.0","id":"ask-1","method":"session/request_permission","params":{}}`)

	select {
	case req := <-got:
		if req.Method != "session/request_permission" {
			t.Fatalf("method = %q", req.Method)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never invoked")
	}

	reply := f.next()
	if reply["id"] != "ask-1" {
		t.Fatalf("reply id = %v, want ask-1", reply["id"])
	}
}

func TestStdioClientPanickingHandlerStillReplies(t *testing.T) {
	f, c := newFakeAgent(t)
	c.SetAgentRequestHandler(func(*RPCResponse) { panic("handler boom") })

	f.push(`{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{}}`)

	reply := f.next()
	if reply["id"] != "perm-1" {
		t.Fatalf("reply id = %v, want perm-1", reply["id"])
	}
	errObj, ok := reply["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error reply, got %v", reply)
	}
	if code := errObj["code"].(float64); code != -32603 {
		t.Fatalf("error code = %v, want -32603", code)
	}
}

func TestStdioClientUnmatchedIDDropped(t *testing.T) {
	f, c := newFakeAgent(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	var result json.RawMessage
	go func() {
		var err error
		result, _, err = c.Call(ctx, "test/method", nil)
		done <- err
	}()

	req := f.next()
	id := int64(req["id"].(float64))

	// Stray response with an id nobody registered, then the real one —
	// the stray must be dropped, not misrouted.
	f.push(`{"jsonrpc":"2.0","id":999999,"result":{"stray":true}}`)
	f.push(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"ok":true}}`, id))

	if err := <-done; err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(result), "ok") {
		t.Fatalf("unexpected result: %s", result)
	}
}

// TestSessionClientOverStdioClient proves SessionClient is genuinely
// transport-generic (Task 1's point): the same ACP session operations
// that work over WSClient (see session_test.go / session_prompt_test.go)
// work unmodified over StdioClient.
func TestSessionClientOverStdioClient(t *testing.T) {
	f, c := newFakeAgent(t)
	sc := NewSessionClient(c)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct {
		id  string
		err error
	}, 1)
	go func() {
		id, err := sc.CreateSession(ctx, "/workspace/repo", nil)
		done <- struct {
			id  string
			err error
		}{id, err}
	}()

	initReq := f.next()
	if initReq["method"] != "initialize" {
		t.Fatalf("expected initialize first, got %v", initReq["method"])
	}
	initID := int64(initReq["id"].(float64))
	f.push(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":1,"agentCapabilities":{}}}`, initID))

	newReq := f.next()
	if newReq["method"] != "session/new" {
		t.Fatalf("expected session/new, got %v", newReq["method"])
	}
	newID := int64(newReq["id"].(float64))
	f.push(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"sessionId":"stdio-session-1"}}`, newID))

	out := <-done
	if out.err != nil {
		t.Fatalf("CreateSession: %v", out.err)
	}
	if out.id != "stdio-session-1" {
		t.Fatalf("sessionId = %q, want %q", out.id, "stdio-session-1")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd harness && go test ./internal/acp/... -run 'TestStdioClient|TestSessionClientOverStdioClient' -v`
Expected: FAIL (build failure — `StdioClient`/`NewStdioClient` undefined).

- [ ] **Step 3: Create `internal/acp/stdioclient.go`**

```go
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/konveyor/migration-harness/internal/logging"
)

// StdioClient communicates with an ACP agent over its own stdin/stdout
// using newline-delimited JSON-RPC 2.0 — the transport claude-agent-acp
// (and other non-goose ACP agents) speaks, as opposed to WSClient's
// WebSocket transport to goose serve.
//
// Demux behavior mirrors WSClient (see its doc comment): one readLoop
// goroutine owns the inbound side, routing each line by kind (response /
// notification / agent-initiated request).
type StdioClient struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser

	writeMu   sync.Mutex
	done      chan struct{}
	closeOnce sync.Once

	mu             sync.Mutex
	pending        map[int64]chan *RPCResponse
	notifSinks     map[int]chan *RPCResponse
	nextSubID      int
	onAgentRequest func(*RPCResponse)
}

// NewStdioClient wraps an already-started subprocess's stdin/stdout pipes
// (see claude.Process.Stdin/Stdout) and starts the demux goroutine.
func NewStdioClient(stdin io.WriteCloser, stdout io.ReadCloser) *StdioClient {
	c := &StdioClient{
		stdin:      stdin,
		stdout:     stdout,
		done:       make(chan struct{}),
		pending:    make(map[int64]chan *RPCResponse),
		notifSinks: make(map[int]chan *RPCResponse),
	}
	go c.readLoop()
	return c
}

// readLoop is the demux goroutine: the only reader of stdout.
func (c *StdioClient) readLoop() {
	defer close(c.done)
	scanner := bufio.NewScanner(c.stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var resp RPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			logging.Warn("stdio unmarshal: %v", err)
			continue
		}

		switch {
		case resp.IsNotification():
			c.fanOutNotification(&resp)
		case resp.IsAgentRequest():
			c.dispatchAgentRequest(&resp)
		case resp.HasID():
			c.routeResponse(&resp)
		default:
			logging.Warn("ACP frame with neither id nor method — dropping")
		}
	}
}

func (c *StdioClient) fanOutNotification(resp *RPCResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, sink := range c.notifSinks {
		select {
		case sink <- resp:
		default:
			logging.Warn("notification sink %d full, dropping %s", id, resp.Method)
		}
	}
}

func (c *StdioClient) dispatchAgentRequest(resp *RPCResponse) {
	c.mu.Lock()
	handler := c.onAgentRequest
	c.mu.Unlock()

	if handler == nil {
		logging.Warn("agent request %q with no handler — rejecting", resp.Method)
		if err := c.SendResponse(resp.ID, nil, &RPCError{Code: -32601, Message: "method not supported by harness"}); err != nil {
			logging.Warn("reply to %s: %v", resp.Method, err)
		}
		return
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logging.Warn("agent request handler panic: %v", r)
				if err := c.SendResponse(resp.ID, nil, &RPCError{Code: -32603, Message: "internal error in harness handler"}); err != nil {
					logging.Warn("reply to %s after panic: %v", resp.Method, err)
				}
			}
		}()
		handler(resp)
	}()
}

func (c *StdioClient) routeResponse(resp *RPCResponse) {
	id, numeric := resp.IntID()
	if !numeric {
		logging.Warn("ACP response with non-numeric id %s — dropping (protocol error)", resp.ID)
		return
	}

	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()

	if !ok {
		logging.Warn("ACP response with unmatched id %d — dropping (protocol error)", id)
		return
	}
	ch <- resp
}

func (c *StdioClient) registerPending(id int64) chan *RPCResponse {
	ch := make(chan *RPCResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	return ch
}

func (c *StdioClient) unregisterPending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *StdioClient) addNotifSink() (int, chan *RPCResponse) {
	ch := make(chan *RPCResponse, 256)
	c.mu.Lock()
	c.nextSubID++
	id := c.nextSubID
	c.notifSinks[id] = ch
	c.mu.Unlock()
	return id, ch
}

func (c *StdioClient) removeNotifSink(id int) {
	c.mu.Lock()
	delete(c.notifSinks, id)
	c.mu.Unlock()
}

// SetAgentRequestHandler installs the handler for agent-initiated requests
// (session/request_permission etc). The handler runs on its own goroutine
// per request and MUST reply via SendResponse — an unanswered request
// parks the agent's turn indefinitely, same as WSClient's contract.
func (c *StdioClient) SetAgentRequestHandler(fn func(*RPCResponse)) {
	c.mu.Lock()
	c.onAgentRequest = fn
	c.mu.Unlock()
}

func (c *StdioClient) writeLine(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.stdin.Write(append(data, '\n'))
	return err
}

// Send sends a JSON-RPC request.
func (c *StdioClient) Send(req *Request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	return c.writeLine(data)
}

// SendResponse sends a JSON-RPC response for an agent-initiated request.
func (c *StdioClient) SendResponse(id json.RawMessage, result any, rpcErr *RPCError) error {
	data, err := json.Marshal(&Response{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	return c.writeLine(data)
}

// Call sends a JSON-RPC request and waits for the matching response.
// Returns the response and any notifications received while waiting —
// same contract as WSClient.Call.
func (c *StdioClient) Call(ctx context.Context, method string, params any) (json.RawMessage, []*RPCResponse, error) {
	req := newRequest(method, params)

	respCh := c.registerPending(req.ID)
	defer c.unregisterPending(req.ID)
	sinkID, notifCh := c.addNotifSink()
	defer c.removeNotifSink(sinkID)

	if err := c.Send(req); err != nil {
		return nil, nil, fmt.Errorf("send %s: %w", method, err)
	}

	var notifications []*RPCResponse

	for {
		select {
		case <-ctx.Done():
			return nil, notifications, ctx.Err()
		case <-c.done:
			select {
			case msg := <-respCh:
				notifications = append(notifications, drainNotifications(notifCh)...)
				if msg.Error != nil {
					return nil, notifications, fmt.Errorf("ACP error %d: %s", msg.Error.Code, msg.Error.Message)
				}
				return msg.Result, notifications, nil
			default:
				return nil, notifications, fmt.Errorf("stdio connection closed")
			}
		case n := <-notifCh:
			notifications = append(notifications, n)
		case msg := <-respCh:
			notifications = append(notifications, drainNotifications(notifCh)...)
			if msg.Error != nil {
				return nil, notifications, fmt.Errorf("ACP error %d: %s", msg.Error.Code, msg.Error.Message)
			}
			return msg.Result, notifications, nil
		}
	}
}

// Done returns a channel that is closed when the subprocess's stdout
// reaches EOF (the process exited or closed its output).
func (c *StdioClient) Done() <-chan struct{} {
	return c.done
}

// Close closes the stdin pipe, signaling EOF to the subprocess.
func (c *StdioClient) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.stdin.Close()
	})
	return err
}
```

`drainNotifications` and `newRequest` are already defined in `wsclient.go` / `jsonrpc.go` in this same package — no duplication needed.

- [ ] **Step 4: Add the second interface assertion to `internal/acp/conn.go`**

Change:

```go
var _ rpcConn = (*WSClient)(nil)
```

to:

```go
var (
	_ rpcConn = (*WSClient)(nil)
	_ rpcConn = (*StdioClient)(nil)
)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd harness && go test ./internal/acp/... -run 'TestStdioClient|TestSessionClientOverStdioClient' -v -race`
Expected: PASS, all six tests.

- [ ] **Step 6: Run the full acp package test suite (regression check)**

Run: `cd harness && go test ./internal/acp/... -race`
Expected: PASS — goose's `WSClient`/`SessionClient` tests are unaffected.

- [ ] **Step 7: Commit**

```bash
git add harness/internal/acp/stdioclient.go harness/internal/acp/stdioclient_test.go harness/internal/acp/conn.go
git commit -m ":sparkles: Add StdioClient — ACP over a subprocess's stdin/stdout"
```

---

### Task 4: `internal/claude` — process lifecycle and provider env mapping

**Files:**
- Create: `internal/claude/lifecycle.go`
- Create: `internal/claude/lifecycle_test.go`

**Interfaces:**
- Consumes: `internal/logging` (`logging.Info`, `logging.Ok`, `logging.Warn`).
- Produces: `claude.ACPConfig{Provider, Model, APIKey, Endpoint string}`, `claude.StartACP(ctx context.Context, cfg ACPConfig) (*Process, error)`, `(*Process).Stdin() io.WriteCloser`, `(*Process).Stdout() io.ReadCloser`, `(*Process).Alive() bool`, `(*Process).Stop() error`.

- [ ] **Step 1: Write the failing tests**

Create `internal/claude/lifecycle_test.go`:

```go
package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderEnvAnthropic(t *testing.T) {
	env, _ := providerEnv("anthropic", "claude-sonnet-4-5", "sk-ant-key", "https://api.anthropic.com")
	assertEnvContains(t, env, "ANTHROPIC_API_KEY", "sk-ant-key")
	assertEnvContains(t, env, "ANTHROPIC_BASE_URL", "https://api.anthropic.com")
	assertEnvContains(t, env, "ANTHROPIC_MODEL", "claude-sonnet-4-5")
	assertEnvNotPresent(t, env, "CLAUDE_CODE_USE_VERTEX")
}

func TestProviderEnvGCPVertex(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS_JSON", `{"type":"service_account","project_id":"my-gcp-project"}`)
	env, tempDirs := providerEnv("gcp-vertex-ai", "claude-sonnet-4-5", "", "global-aiplatform.googleapis.com")
	for _, d := range tempDirs {
		defer os.RemoveAll(d)
	}
	assertEnvContains(t, env, "CLAUDE_CODE_USE_VERTEX", "1")
	assertEnvContains(t, env, "CLOUD_ML_REGION", "global")
	assertEnvContains(t, env, "ANTHROPIC_VERTEX_PROJECT_ID", "my-gcp-project")
	assertEnvContains(t, env, "ANTHROPIC_MODEL", "claude-sonnet-4-5")
	assertEnvPresent(t, env, "GOOGLE_APPLICATION_CREDENTIALS")
	assertEnvNotPresent(t, env, "GOOGLE_APPLICATION_CREDENTIALS_JSON")
}

func TestProviderEnvGCPVertexRegionPrefixed(t *testing.T) {
	env, _ := providerEnv("gcp-vertex-ai", "", "", "us-east5-aiplatform.googleapis.com")
	assertEnvContains(t, env, "CLOUD_ML_REGION", "us-east5")
}

func TestProviderEnvGCPVertexNoCredentialsJSON(t *testing.T) {
	env, tempDirs := providerEnv("gcp-vertex-ai", "", "", "global-aiplatform.googleapis.com")
	if len(tempDirs) != 0 {
		t.Errorf("expected no temp dirs without credentials JSON, got %v", tempDirs)
	}
	assertEnvNotPresent(t, env, "ANTHROPIC_VERTEX_PROJECT_ID")
	assertEnvNotPresent(t, env, "GOOGLE_APPLICATION_CREDENTIALS")
}

func TestProviderEnvUnmappedProvider(t *testing.T) {
	env, _ := providerEnv("aws-bedrock", "some-model", "unused", "")
	assertEnvNotPresent(t, env, "ANTHROPIC_API_KEY")
	assertEnvNotPresent(t, env, "CLAUDE_CODE_USE_VERTEX")
	assertEnvNotPresent(t, env, "ANTHROPIC_MODEL")
}

func TestProviderEnvEmptyStringsAddNothing(t *testing.T) {
	env, _ := providerEnv("", "", "", "")
	assertEnvNotPresent(t, env, "ANTHROPIC_API_KEY")
	assertEnvNotPresent(t, env, "CLAUDE_CODE_USE_VERTEX")
}

func TestVertexRegion(t *testing.T) {
	cases := map[string]string{
		"global-aiplatform.googleapis.com":         "global",
		"us-east5-aiplatform.googleapis.com":       "us-east5",
		"https://global-aiplatform.googleapis.com": "global",
		"not-a-vertex-host.example.com":            "",
		"":                                         "",
	}
	for endpoint, want := range cases {
		if got := vertexRegion(endpoint); got != want {
			t.Errorf("vertexRegion(%q) = %q, want %q", endpoint, got, want)
		}
	}
}

func TestVertexProjectID(t *testing.T) {
	if got := vertexProjectID(`{"type":"service_account","project_id":"my-project","other":"field"}`); got != "my-project" {
		t.Errorf("vertexProjectID = %q, want %q", got, "my-project")
	}
	if got := vertexProjectID("not json"); got != "" {
		t.Errorf("vertexProjectID(invalid) = %q, want empty", got)
	}
}

func TestWriteADCFile(t *testing.T) {
	content := `{"type":"service_account","project_id":"test"}`
	path, err := writeADCFile(content)
	if err != nil {
		t.Fatalf("writeADCFile: %v", err)
	}
	defer os.RemoveAll(filepath.Dir(path))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != content {
		t.Errorf("content = %q, want %q", string(data), content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestStartACPBinaryNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir on PATH — claude-agent-acp cannot resolve
	_, err := StartACP(context.Background(), ACPConfig{})
	if err == nil {
		t.Fatal("expected error when claude-agent-acp is not on PATH")
	}
}

func assertEnvContains(t *testing.T, env []string, key, value string) {
	t.Helper()
	want := key + "=" + value
	for _, e := range env {
		if e == want {
			return
		}
	}
	t.Errorf("env missing %s=%s", key, value)
}

func assertEnvPresent(t *testing.T, env []string, key string) {
	t.Helper()
	prefix := key + "="
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			return
		}
	}
	t.Errorf("env missing key %s", key)
}

func assertEnvNotPresent(t *testing.T, env []string, key string) {
	t.Helper()
	prefix := key + "="
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			t.Errorf("env should not contain key %s, found %s", key, e)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd harness && go test ./internal/claude/... -v`
Expected: FAIL (package `internal/claude` does not exist yet).

- [ ] **Step 3: Create `internal/claude/lifecycle.go`**

```go
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/konveyor/migration-harness/internal/logging"
)

// Process manages a claude-agent-acp subprocess speaking ACP over stdio.
type Process struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	done     chan struct{}
	tempDirs []string
}

// ACPConfig configures StartACP. Provider, Model, APIKey and Endpoint are
// translated to the env vars Claude Code expects — mirrors
// goose.ServeConfig's shape for the fields the two runtimes share.
type ACPConfig struct {
	Provider string
	Model    string
	APIKey   string
	Endpoint string
}

// StartACP launches claude-agent-acp, wired to stdio for ACP JSON-RPC.
func StartACP(ctx context.Context, cfg ACPConfig) (*Process, error) {
	binPath, err := exec.LookPath("claude-agent-acp")
	if err != nil {
		return nil, fmt.Errorf("claude-agent-acp not found: %w", err)
	}

	cmd := exec.CommandContext(ctx, binPath)
	env, tempDirs := providerEnv(cfg.Provider, cfg.Model, cfg.APIKey, cfg.Endpoint)
	cmd.Env = env
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		for _, d := range tempDirs {
			os.RemoveAll(d)
		}
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		for _, d := range tempDirs {
			os.RemoveAll(d)
		}
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		for _, d := range tempDirs {
			os.RemoveAll(d)
		}
		return nil, fmt.Errorf("start claude-agent-acp: %w", err)
	}

	p := &Process{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		done:     make(chan struct{}),
		tempDirs: tempDirs,
	}

	go func() {
		cmd.Wait()
		close(p.done)
	}()

	logging.Info("claude-agent-acp started (pid %d)", cmd.Process.Pid)
	return p, nil
}

// Stdin returns the subprocess's stdin, for writing ACP requests.
func (p *Process) Stdin() io.WriteCloser { return p.stdin }

// Stdout returns the subprocess's stdout, for reading ACP responses.
func (p *Process) Stdout() io.ReadCloser { return p.stdout }

// Alive returns true if the subprocess is still running.
func (p *Process) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Stop sends SIGTERM and waits up to 5 seconds, then SIGKILL. Cleans up
// any temporary credential files created during startup.
func (p *Process) Stop() error {
	defer p.cleanup()

	if !p.Alive() {
		return nil
	}

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("sigterm: %w", err)
	}

	select {
	case <-p.done:
		logging.Ok("claude-agent-acp stopped cleanly")
		return nil
	case <-time.After(5 * time.Second):
		logging.Warn("claude-agent-acp did not stop in 5s, sending SIGKILL")
		if err := p.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("sigkill: %w", err)
		}
		<-p.done
		return nil
	}
}

func (p *Process) cleanup() {
	for _, d := range p.tempDirs {
		os.RemoveAll(d)
	}
	p.tempDirs = nil
}

// providerEnv returns the current process environment with LLM provider
// credentials translated to the env vars claude-agent-acp (via the Claude
// Agent SDK) expects. Only anthropic and gcp_vertex_ai are mapped — the
// only two providers this repo currently configures; anything else is
// left unmapped with a warning, same fallback goose.providerEnv uses.
func providerEnv(provider, model, apiKey, endpoint string) (env []string, tempDirs []string) {
	env = os.Environ()
	p := strings.ReplaceAll(strings.ToLower(provider), "-", "_")

	switch p {
	case "anthropic":
		if apiKey != "" {
			env = append(env, "ANTHROPIC_API_KEY="+apiKey)
		}
		if endpoint != "" {
			env = append(env, "ANTHROPIC_BASE_URL="+endpoint)
		}
		if model != "" {
			env = append(env, "ANTHROPIC_MODEL="+model)
		}

	case "gcp_vertex_ai":
		env = append(env, "CLAUDE_CODE_USE_VERTEX=1")
		if region := vertexRegion(endpoint); region != "" {
			env = append(env, "CLOUD_ML_REGION="+region)
		}
		if content := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS_JSON"); content != "" {
			if projectID := vertexProjectID(content); projectID != "" {
				env = append(env, "ANTHROPIC_VERTEX_PROJECT_ID="+projectID)
			}
			path, err := writeADCFile(content)
			if err != nil {
				logging.Warn("write ADC file: %v", err)
			} else {
				env = append(env, "GOOGLE_APPLICATION_CREDENTIALS="+path)
				tempDirs = append(tempDirs, filepath.Dir(path))
			}
			env = filterEnvKey(env, "GOOGLE_APPLICATION_CREDENTIALS_JSON")
		}
		if model != "" {
			env = append(env, "ANTHROPIC_MODEL="+model)
		}

	default:
		if p != "" {
			logging.Warn("unmapped provider %q — credentials not forwarded to claude-agent-acp", p)
		}
	}

	return env, tempDirs
}

// vertexRegion extracts the Vertex AI region from an aiplatform endpoint
// host, e.g. "global-aiplatform.googleapis.com" -> "global",
// "us-east5-aiplatform.googleapis.com" -> "us-east5". Returns "" if the
// host doesn't match the expected "<region>-aiplatform.googleapis.com"
// shape.
func vertexRegion(endpoint string) string {
	host := endpoint
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	const suffix = "-aiplatform.googleapis.com"
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	return strings.TrimSuffix(host, suffix)
}

// vertexProjectID extracts the "project_id" field from a GCP service
// account JSON blob, so ANTHROPIC_VERTEX_PROJECT_ID doesn't need a new
// config field — the value already arrives in
// GOOGLE_APPLICATION_CREDENTIALS_JSON for the gcp_vertex_ai provider.
func vertexProjectID(credentialJSON string) string {
	var parsed struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal([]byte(credentialJSON), &parsed); err != nil {
		return ""
	}
	return parsed.ProjectID
}

func filterEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// writeADCFile writes service account JSON to a temp file for Google ADC.
// Uses a temp directory outside the repo to prevent accidental commit/push.
func writeADCFile(content string) (string, error) {
	dir, err := os.MkdirTemp("", "migration-harness-claude-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir for ADC: %w", err)
	}
	path := filepath.Join(dir, "gcp-adc.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return "", fmt.Errorf("write ADC file: %w", err)
	}
	return path, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd harness && go test ./internal/claude/... -v`
Expected: PASS, all tests including `TestStartACPBinaryNotFound`.

- [ ] **Step 5: Commit**

```bash
git add harness/internal/claude/lifecycle.go harness/internal/claude/lifecycle_test.go
git commit -m ":sparkles: Add internal/claude — claude-agent-acp process lifecycle"
```

---

### Task 5: `Config.AgentRuntime`

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.AgentRuntime string`, populated from `HARNESS_AGENT_RUNTIME` (empty when unset).

- [ ] **Step 1: Write the failing test**

In `internal/config/config_test.go`, add `"HARNESS_AGENT_RUNTIME"` to the
list inside `clearKonveyorEnv` (alongside `"HARNESS_ACP_TEE"`), and add
this test inside `TestLoadFromEnv`, after the `"reads optional param
overrides"` subtest:

```go
	t.Run("AgentRuntime defaults to empty (goose)", func(t *testing.T) {
		clearKonveyorEnv(t)
		setRequiredEnv(t)

		cfg, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.AgentRuntime != "" {
			t.Errorf("AgentRuntime = %q, want empty", cfg.AgentRuntime)
		}
	})

	t.Run("reads HARNESS_AGENT_RUNTIME", func(t *testing.T) {
		clearKonveyorEnv(t)
		setRequiredEnv(t)
		t.Setenv("HARNESS_AGENT_RUNTIME", "claude")

		cfg, err := LoadFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.AgentRuntime != "claude" {
			t.Errorf("AgentRuntime = %q, want %q", cfg.AgentRuntime, "claude")
		}
	})
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd harness && go test ./internal/config/... -run TestLoadFromEnv -v`
Expected: FAIL (compile error — `cfg.AgentRuntime` undefined).

- [ ] **Step 3: Add the field in `internal/config/config.go`**

Add to the `Config` struct, near `Provider`:

```go
	Model    string
	Provider string
	Endpoint string
	APIKey   string
	MaxTurns int

	// AgentRuntime selects the ACP agent runtime: "" or "goose" (default)
	// runs goose over WebSocket; "claude" runs Claude Code via
	// claude-agent-acp over stdio. Any other value falls back to goose.
	AgentRuntime string
```

Add to the `cfg := &Config{...}` literal in `LoadFromEnv`, near `Provider`:

```go
		Model:        model,
		Provider:     envWithFallback("KONVEYOR_LLM_PROVIDER", "KONVEYOR_MODEL_PRIMARY_PROVIDER"),
		Endpoint:     envWithFallback("KONVEYOR_LLM_ENDPOINT", "KONVEYOR_MODEL_PRIMARY_ENDPOINT"),
		APIKey:       envWithFallback("KONVEYOR_LLM_API_KEY", "KONVEYOR_MODEL_PRIMARY_API_KEY"),
		AgentRuntime: os.Getenv("HARNESS_AGENT_RUNTIME"),
		MaxTurns:     DefaultMaxTurns,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd harness && go test ./internal/config/... -v`
Expected: PASS, all `TestLoadFromEnv` subtests.

- [ ] **Step 5: Commit**

```bash
git add harness/internal/config/config.go harness/internal/config/config_test.go
git commit -m ":sparkles: Add HARNESS_AGENT_RUNTIME config option"
```

---

### Task 6: Wire the claude runtime into `main.go`

**Files:**
- Modify: `cmd/migration-harness/main.go`

**Interfaces:**
- Consumes: `claude.ACPConfig`, `claude.StartACP` (Task 4); `acp.NewStdioClient` (Task 3); `acp.NewSessionClient(rpcConn)`, `SessionClient.SetConfigOption` (Tasks 1-2); `Config.AgentRuntime` (Task 5).
- Produces: `agentProcess` interface (local to `main.go`).

This task has no new automated test (main.go has none today) — verified
by build, `go vet`, the full existing test suite, and a manual dry run
per Step 5.

- [ ] **Step 1: Add the import and the `agentProcess` interface**

In `cmd/migration-harness/main.go`, `acp` and `config` already exist in
the import list. Add the new `claude` import alphabetically before
`config` (after `acp`):

```go
	"github.com/konveyor/migration-harness/internal/acp"
	"github.com/konveyor/migration-harness/internal/claude"
	"github.com/konveyor/migration-harness/internal/config"
```

Add, right before `func runStage(...)`:

```go
// agentProcess is satisfied by both goose.ServeProcess and claude.Process
// — runStage only needs liveness and shutdown, not runtime-specific
// details.
type agentProcess interface {
	Alive() bool
	Stop() error
}
```

- [ ] **Step 2: Replace the goose-only startup block with a runtime switch**

Find this block in `runStage` (currently steps 5-6b):

```go
	// 5. Start goose serve. With the ACP tee (default) goose binds
	// loopback on :4001 and the harness owns the pod's :4000 endpoint;
	// with HARNESS_ACP_TEE=off goose takes :4000 itself as before.
	logging.Header("Goose Setup")
	goosePort := 0
	if cfg.ACPTee {
		goosePort = goose.LoopbackACPPort
	}
	srv, err := goose.StartServe(ctx, goose.ServeConfig{
		Port:         goosePort,
		BindLoopback: cfg.ACPTee,
		SecretKey:    cfg.ACPSecretKey,
		Provider:     cfg.Provider,
		Model:        cfg.Model,
		APIKey:       cfg.APIKey,
		Endpoint:     cfg.Endpoint,
	})
	if err != nil {
		return fmt.Errorf("start goose serve: %w", err)
	}
	defer srv.Stop()

	// 6. Connect ACP, create session
	wsClient, err := acp.WaitReadyDial(ctx, "127.0.0.1", srv.Port(), srv.SecretKey(), 30*time.Second)
	if err != nil {
		return fmt.Errorf("connect to goose: %w", err)
	}
	defer wsClient.Close()

	session := acp.NewSessionClient(wsClient)
	sessionID, err := session.CreateSession(ctx, cloneDir, nil)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	// 6b. Expose the run: tee listener on the pod ACP port. Viewers get
	// a verbatim pipe to goose plus the run session's live stream —
	// message/thought chunks, tool calls, usage — and may redirect the
	// run (steer/cancel) unless HARNESS_HITL_STEER=off. Permission asks
	// are offered to whoever is watching. Failure here never fails the
	// run — it only loses live viewers.
	var teeSrv *tee.Server
	if cfg.ACPTee {
		t := tee.New(tee.Config{
			SecretKey:    cfg.ACPSecretKey,
			UpstreamAddr: fmt.Sprintf("127.0.0.1:%d", srv.Port()),
			HITLTimeout:  cfg.HITLTimeout,
			SteerEnabled: cfg.HITLSteer,
		})
		if err := t.Start(goose.DefaultACPPort); err != nil {
			logging.Warn("ACP tee: %v — run continues without live viewers", err)
		} else {
			defer t.Stop()
			t.AttachRun(wsClient, sessionID)
			session.SetPermissionForwarder(t)
			teeSrv = t
			logging.Ok("ACP tee on :%d (goose loopback :%d, viewer steering %s)",
				goose.DefaultACPPort, srv.Port(), map[bool]string{true: "on", false: "off"}[cfg.HITLSteer])
		}
	}
```

Replace it with:

```go
	// 5. Start the agent runtime and connect ACP. The claude runtime
	// (HARNESS_AGENT_RUNTIME=claude) speaks ACP over its own stdio
	// instead of a dialable port, so the tee — goose-only for now, see
	// docs/superpowers/specs/2026-08-21-claude-code-agent-runtime-design.md
	// — is skipped entirely for that runtime.
	var (
		proc      agentProcess
		session   *acp.SessionClient
		sessionID string
	)
	var teeSrv *tee.Server

	switch cfg.AgentRuntime {
	case "claude":
		logging.Header("Claude Code Setup")
		if cfg.ACPTee {
			logging.Warn("HARNESS_ACP_TEE is on, but the claude runtime does not support the ACP tee yet — continuing without live viewers")
		}

		cp, err := claude.StartACP(ctx, claude.ACPConfig{
			Provider: cfg.Provider,
			Model:    cfg.Model,
			APIKey:   cfg.APIKey,
			Endpoint: cfg.Endpoint,
		})
		if err != nil {
			return fmt.Errorf("start claude-agent-acp: %w", err)
		}
		defer cp.Stop()
		proc = cp

		stdioClient := acp.NewStdioClient(cp.Stdin(), cp.Stdout())
		defer stdioClient.Close()

		session = acp.NewSessionClient(stdioClient)
		sessionID, err = session.CreateSession(ctx, cloneDir, nil)
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}

		// No live viewer can answer tool-use prompts for this runtime
		// yet, so run unattended: bypassPermissions instead of the
		// interactive default. answerAgentRequest's fail-closed deny
		// remains the safety net if a request slips through anyway.
		if err := session.SetConfigOption(ctx, sessionID, "mode", "bypassPermissions"); err != nil {
			return fmt.Errorf("set claude session to unattended mode: %w", err)
		}

	default:
		// Start goose serve. With the ACP tee (default) goose binds
		// loopback on :4001 and the harness owns the pod's :4000
		// endpoint; with HARNESS_ACP_TEE=off goose takes :4000 itself
		// as before.
		logging.Header("Goose Setup")
		goosePort := 0
		if cfg.ACPTee {
			goosePort = goose.LoopbackACPPort
		}
		srv, err := goose.StartServe(ctx, goose.ServeConfig{
			Port:         goosePort,
			BindLoopback: cfg.ACPTee,
			SecretKey:    cfg.ACPSecretKey,
			Provider:     cfg.Provider,
			Model:        cfg.Model,
			APIKey:       cfg.APIKey,
			Endpoint:     cfg.Endpoint,
		})
		if err != nil {
			return fmt.Errorf("start goose serve: %w", err)
		}
		defer srv.Stop()
		proc = srv

		wsClient, err := acp.WaitReadyDial(ctx, "127.0.0.1", srv.Port(), srv.SecretKey(), 30*time.Second)
		if err != nil {
			return fmt.Errorf("connect to goose: %w", err)
		}
		defer wsClient.Close()

		session = acp.NewSessionClient(wsClient)
		sessionID, err = session.CreateSession(ctx, cloneDir, nil)
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}

		// Expose the run: tee listener on the pod ACP port. Viewers get
		// a verbatim pipe to goose plus the run session's live stream —
		// message/thought chunks, tool calls, usage — and may redirect
		// the run (steer/cancel) unless HARNESS_HITL_STEER=off.
		// Permission asks are offered to whoever is watching. Failure
		// here never fails the run — it only loses live viewers.
		if cfg.ACPTee {
			t := tee.New(tee.Config{
				SecretKey:    cfg.ACPSecretKey,
				UpstreamAddr: fmt.Sprintf("127.0.0.1:%d", srv.Port()),
				HITLTimeout:  cfg.HITLTimeout,
				SteerEnabled: cfg.HITLSteer,
			})
			if err := t.Start(goose.DefaultACPPort); err != nil {
				logging.Warn("ACP tee: %v — run continues without live viewers", err)
			} else {
				defer t.Stop()
				t.AttachRun(wsClient, sessionID)
				session.SetPermissionForwarder(t)
				teeSrv = t
				logging.Ok("ACP tee on :%d (goose loopback :%d, viewer steering %s)",
					goose.DefaultACPPort, srv.Port(), map[bool]string{true: "on", false: "off"}[cfg.HITLSteer])
			}
		}
	}
```

- [ ] **Step 3: Replace the two `srv.Alive()` references**

Find (step 10):

```go
	// 10. Check goose health
	if !srv.Alive() {
		logging.Err("goose serve crashed")
	}
```

Replace with:

```go
	// 10. Check the agent process health
	if !proc.Alive() {
		logging.Err("agent process crashed")
	}
```

Find (step 13):

```go
	// 13. Determine exit status from ACP/goose signals
	stageFailed := err != nil || !srv.Alive() || cancelled
```

Replace with:

```go
	// 13. Determine exit status from ACP/agent signals
	stageFailed := err != nil || !proc.Alive() || cancelled
```

- [ ] **Step 4: Build and run the full test suite**

Run: `cd harness && go build ./... && go vet ./... && go test ./... -race`
Expected: PASS across every package, no vet warnings.

- [ ] **Step 5: Manual dry run against the "not found" error path**

The `agent-base` image ships `goose` but not `claude-agent-acp` yet
(that's the follow-up image work called out as out of scope). Confirm
the new runtime fails loudly rather than silently falling back, by
running the binary locally with the claude runtime selected and no
`claude-agent-acp` on `PATH`:

Run:
```bash
cd harness && go build -o /tmp/migration-harness ./cmd/migration-harness
HARNESS_AGENT_RUNTIME=claude \
KONVEYOR_LLM_MODEL=claude-sonnet-4-5 \
HUB_BASE_URL=https://example.invalid \
APP_ID=1 \
KONVEYOR_ACP_SECRET_KEY=test \
TARGET_BRANCH=test-branch \
/tmp/migration-harness run 2>&1 | tail -20
```
Expected: fails at Hub resolution (no real Hub at that URL) before ever
reaching the claude runtime branch — this only confirms the binary
still builds and starts; it is not a substitute for an integration test
against a real Hub + `claude-agent-acp` binary, which is out of scope
for this plan (see the spec's non-goals).

- [ ] **Step 6: Commit**

```bash
git add harness/cmd/migration-harness/main.go
git commit -m "$(cat <<'EOF'
:sparkles: Wire the claude agent runtime into migration-harness run

HARNESS_AGENT_RUNTIME=claude drives Claude Code (via claude-agent-acp,
over stdio) instead of goose. Runs unattended via
session/set_config_option(mode=bypassPermissions) since there is no
live viewer to answer tool-use prompts yet — the ACP tee stays
goose-only for this pass. Default (unset/anything else) is byte-for-
byte the existing goose behavior.
EOF
)"
```

---

### Task 7: Full verification pass

**Files:** none (verification only).

- [ ] **Step 1: Full build, vet, and test with race detector**

Run: `cd harness && go build ./... && go vet ./... && go test ./... -race -count=1`
Expected: PASS, zero vet warnings, zero test failures.

- [ ] **Step 2: Confirm goose's own tests still pass unmodified**

Run: `cd harness && go test ./internal/goose/... ./internal/acp/acptest/... -race -v`
Expected: PASS — these packages were not touched by this plan; this
step exists to make the "zero behavior change to goose" claim
verifiable, not assumed.

- [ ] **Step 3: Update `docs/superpowers/specs/2026-08-21-claude-code-agent-runtime-design.md`'s non-goals note if scope changed**

If any task above deviated from the spec (e.g. a provider mapping
needed adjusting once real testing surfaced a gap), add a short "Status"
section at the top of the spec file noting what shipped vs. what was
deferred, so the spec stays an accurate record. If nothing deviated,
skip this step.

- [ ] **Step 4: Final commit (only if Step 3 produced a change)**

```bash
git add docs/superpowers/specs/2026-08-21-claude-code-agent-runtime-design.md
git commit -m ":book: Note implementation status on the claude runtime design doc"
```
