package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type nopCloser struct{ *strings.Reader }

func (n *nopCloser) Close() error { return nil }

// TestReadSSEDropsLateResponseWithoutBlocking verifies that readSSE never
// stalls on a full per-request channel: once the requester has timed out and
// gone, the late response must be dropped immediately so the SSE reader keeps
// draining the HTTP body. This pins the non-blocking-send semantics of the
// pending map model (regression guard against reintroducing a blocking send,
// which deadlocks the whole SSE session when no waiter ever arrives).
func TestReadSSEDropsLateResponseWithoutBlocking(t *testing.T) {
	c := NewClient("http://unused")
	// Simulate a stale requester: registered channel, already full, nobody reading.
	stale := make(chan json.RawMessage, 1)
	stale <- json.RawMessage(`{"stale":true}`)
	c.mu.Lock()
	c.pending[7] = stale
	c.mu.Unlock()

	body := "data: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\n\n"
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.readSSE(&nopCloser{Reader: strings.NewReader(body)})
	}()

	select {
	case <-done:
		c.mu.Lock()
		_, still := c.pending[7]
		c.mu.Unlock()
		if !still {
			t.Fatal("readSSE must not unregister pending entries")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readSSE blocked on a full channel: unbounded blocking send regression")
	}
}

// TestReadSSEDeliversToWaiter verifies the happy path: a response whose
// requester is actively waiting receives it via its per-request channel.
func TestReadSSEDeliversToWaiter(t *testing.T) {
	c := NewClient("http://unused")
	ch := make(chan json.RawMessage, 1)
	c.mu.Lock()
	c.pending[42] = ch
	c.mu.Unlock()

	body := "data: {\"jsonrpc\":\"2.0\",\"id\":42,\"result\":{\"ok\":true}}\n\n"
	go c.readSSE(&nopCloser{Reader: strings.NewReader(body)})

	select {
	case got := <-ch:
		if !strings.Contains(string(got), `"ok":true`) {
			t.Fatalf("unexpected payload: %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for response delivery")
	}
}

// TestReadSSESkipsNotifications verifies messages without an id (server
// notifications) are consumed without touching any pending entry.
func TestReadSSESkipsNotifications(t *testing.T) {
	c := NewClient("http://unused")
	body := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\n"
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.readSSE(&nopCloser{Reader: strings.NewReader(body)})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readSSE hung on a notification without id")
	}
}

var (
	_ = bufio.NewScanner
	_ = httptest.NewRecorder
	_ = context.Background
)
