package acp

import (
	"encoding/json"
	"sync/atomic"
)

// JSON-RPC 2.0 types for ACP communication with goose serve.

type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r *RPCResponse) IsNotification() bool {
	return r.ID == nil && r.Method != ""
}

// IsAgentRequest reports whether the message is a request initiated by the
// agent that the client must answer (e.g. session/request_permission,
// elicitation/create). Unlike a notification it carries an ID, and unlike
// a response to one of our own calls it carries a method.
func (r *RPCResponse) IsAgentRequest() bool {
	return r.ID != nil && r.Method != ""
}

// Response is an outgoing JSON-RPC response to an agent-initiated request.
type Response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int64     `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return e.Message
}

var requestID atomic.Int64

func newRequest(method string, params any) *Request {
	return &Request{
		JSONRPC: "2.0",
		ID:      requestID.Add(1),
		Method:  method,
		Params:  params,
	}
}
