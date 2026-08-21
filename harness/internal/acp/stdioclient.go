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
	if err := scanner.Err(); err != nil {
		logging.Warn("stdio read: %v", err)
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
