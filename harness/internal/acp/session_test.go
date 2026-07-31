package acp

import (
	"encoding/json"
	"testing"
)

func TestExtractSessionIDFromNotifications(t *testing.T) {
	tests := []struct {
		name   string
		notifs []*RPCResponse
		want   string
	}{
		{
			name:   "nil notifications",
			notifs: nil,
			want:   "",
		},
		{
			name: "session/update with sessionId",
			notifs: []*RPCResponse{
				{Method: "session/update", Params: json.RawMessage(`{"sessionId":"abc-123"}`)},
			},
			want: "abc-123",
		},
		{
			name: "ignores non-session/update",
			notifs: []*RPCResponse{
				{Method: "other/method", Params: json.RawMessage(`{"sessionId":"should-skip"}`)},
			},
			want: "",
		},
		{
			name: "returns first match",
			notifs: []*RPCResponse{
				{Method: "session/update", Params: json.RawMessage(`{"sessionId":"first"}`)},
				{Method: "session/update", Params: json.RawMessage(`{"sessionId":"second"}`)},
			},
			want: "first",
		},
		{
			name: "skips empty sessionId",
			notifs: []*RPCResponse{
				{Method: "session/update", Params: json.RawMessage(`{"sessionId":""}`)},
				{Method: "session/update", Params: json.RawMessage(`{"sessionId":"real-id"}`)},
			},
			want: "real-id",
		},
		{
			name: "handles malformed JSON",
			notifs: []*RPCResponse{
				{Method: "session/update", Params: json.RawMessage(`not-json`)},
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSessionIDFromNotifications(tt.notifs)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsToolCall(t *testing.T) {
	tests := []struct {
		name string
		msg  *RPCResponse
		want bool
	}{
		{
			name: "tool_call update",
			msg: &RPCResponse{
				Method: "session/update",
				Params: json.RawMessage(`{"update":{"sessionUpdate":"tool_call"}}`),
			},
			want: true,
		},
		{
			name: "agent_message_chunk is not a tool call",
			msg: &RPCResponse{
				Method: "session/update",
				Params: json.RawMessage(`{"update":{"sessionUpdate":"agent_message_chunk"}}`),
			},
			want: false,
		},
		{
			name: "wrong method",
			msg: &RPCResponse{
				Method: "other/method",
				Params: json.RawMessage(`{"update":{"sessionUpdate":"tool_call"}}`),
			},
			want: false,
		},
		{
			name: "malformed JSON",
			msg: &RPCResponse{
				Method: "session/update",
				Params: json.RawMessage(`garbage`),
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isToolCall(tt.msg)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
