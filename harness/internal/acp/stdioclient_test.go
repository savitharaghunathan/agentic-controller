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
	if err := scanner.Err(); err != nil {
		f.t.Errorf("fakeAgent read: %v", err)
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
