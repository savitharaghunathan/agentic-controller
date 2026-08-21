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
