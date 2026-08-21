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
