package chathub

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestConversationReuseRequiresFreshWebSocket(t *testing.T) {
	if shouldReusePooledConnection(Request{ConversationID: "conv", SessionID: "sess"}) {
		t.Fatal("continuing an existing conversation on a pre-warmed websocket can return an immediate empty completion")
	}
	if !shouldReusePooledConnection(Request{}) {
		t.Fatal("fresh first-turn requests should remain eligible for the connection pool")
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.August, 29, 2, 0, 0, 0, time.UTC)
	if got := parseRetryAfter("90", now); got != 90 {
		t.Fatalf("integer Retry-After=%d want 90", got)
	}
	date := now.Add(2 * time.Minute).Format(http.TimeFormat)
	if got := parseRetryAfter(date, now); got != 120 {
		t.Fatalf("HTTP-date Retry-After=%d want 120", got)
	}
	if got := parseRetryAfter("invalid", now); got != 0 {
		t.Fatalf("invalid Retry-After=%d want 0", got)
	}
}

func BenchmarkParseRetryAfterSeconds(b *testing.B) {
	now := time.Date(2026, time.August, 29, 2, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if parseRetryAfter("90", now) != 90 {
			b.Fatal("unexpected Retry-After result")
		}
	}
}

func TestConnPoolRealWebSocketClosesFrameChannel(t *testing.T) {
	upgrader := websocket.Upgrader{}
	sendFrame := make(chan struct{})
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":6}`+rs))
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		select {
		case <-sendFrame:
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":1}`+rs))
		select {
		case <-readDone:
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	pool := NewConnPool(websocket.DefaultDialer, nil)
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool.Warm(ctx, Account{OID: "oid-a", TID: "tid-a"}, wsURL)

	conn, _, frames, _, reused, err := pool.Take(ctx, "oid-a", "tid-a", wsURL)
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("expected warmed websocket reuse")
	}
	close(sendFrame)
	select {
	case _, ok := <-frames:
		if !ok {
			t.Fatal("frame channel closed before queued frame")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for queued websocket frame")
	}
	_ = conn.Close()
	select {
	case _, ok := <-frames:
		if ok {
			t.Fatal("frame channel remained open after websocket close")
		}
	case <-time.After(time.Second):
		t.Fatal("frame channel was not closed")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("websocket test server did not stop")
	}
}

func TestConnPoolCancellationDoesNotLeakDial(t *testing.T) {
	dialer := *websocket.DefaultDialer
	dialer.NetDialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	pool := NewConnPool(&dialer, nil)
	defer pool.Close()
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, _, _, err := pool.Take(ctx, "oid-b", "tid-b", "ws://example.test")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Fatalf("goroutines grew from %d to %d", before, after)
	}
}

func TestConnPoolCloseUnblocksSlowConsumer(t *testing.T) {
	sendFrames := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("{}")); err != nil {
			return
		}
		select {
		case <-sendFrames:
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
			return
		}
		for i := 0; i < 128; i++ {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":1}`+rs)); err != nil {
				return
			}
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	pool := NewConnPool(websocket.DefaultDialer, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	pool.Warm(ctx, Account{OID: "oid-slow", TID: "tid-slow"}, wsURL)
	conn, _, frames, _, reused, err := pool.Take(ctx, "oid-slow", "tid-slow", wsURL)
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("expected warmed websocket reuse")
	}

	close(sendFrames)
	time.Sleep(50 * time.Millisecond)
	pool.Close()
	select {
	case _, ok := <-frames:
		for ok {
			select {
			case _, ok = <-frames:
			case <-time.After(time.Second):
				t.Fatal("slow-consumer frame channel did not close")
			}
		}
	case <-time.After(time.Second):
		t.Fatal("pool close did not unblock slow consumer")
	}
	_ = conn.Close()
}

func TestConnPoolCloseIsIdempotent(t *testing.T) {
	pool := NewConnPool(websocket.DefaultDialer, nil)
	pool.Close()
	pool.Close()
}

func TestConnPoolGoroutinesReturnToSteadyState(t *testing.T) {
	baseline := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		pool := NewConnPool(websocket.DefaultDialer, nil)
		pool.Close()
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+1 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+1 {
		t.Fatalf("goroutines did not return to steady state: baseline=%d current=%d", baseline, got)
	}
}

func TestConnPoolActiveWebSocketGoroutinesReturnToSteadyState(t *testing.T) {
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("{}")); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	baseline := runtime.NumGoroutine()
	pool := NewConnPool(websocket.DefaultDialer, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	pool.Warm(ctx, Account{OID: "oid-active", TID: "tid-active"}, wsURL)
	conn, _, _, _, reused, err := pool.Take(ctx, "oid-active", "tid-active", wsURL)
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("expected warmed websocket reuse")
	}

	pool.Close()
	_ = conn.Close()
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("active websocket server did not stop")
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+1 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+1 {
		t.Fatalf("active websocket goroutines did not return to steady state: baseline=%d current=%d", baseline, got)
	}
}

func TestConnPoolWebSocketPerformance(t *testing.T) {
	const requests = 100
	const concurrency = 2

	upgrader := websocket.Upgrader{}
	var active atomic.Int64
	var maximum atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		current := active.Add(1)
		defer active.Add(-1)
		for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("{}")); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	latencies := make([]time.Duration, requests)
	jobs := make(chan int)
	var workers sync.WaitGroup
	var reused atomic.Int64
	started := time.Now()
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			pool := NewConnPool(websocket.DefaultDialer, nil)
			defer pool.Close()
			account := Account{OID: "oid-" + string(rune('a'+worker)), TID: "tid"}
			for index := range jobs {
				requestStarted := time.Now()
				pool.Warm(context.Background(), account, wsURL)
				conn, _, _, _, hit, err := pool.Take(context.Background(), account.OID, account.TID, wsURL)
				latencies[index] = time.Since(requestStarted)
				if err != nil {
					t.Errorf("websocket request failed: %v", err)
					continue
				}
				if hit {
					reused.Add(1)
				}
				_ = conn.Close()
			}
		}(worker)
	}
	for index := range latencies {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	elapsed := time.Since(started)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("requests=%d concurrency=%d throughput=%.2f req/s p50=%s p95=%s p99=%s maximum_active=%d pool_hit_rate=%.2f%%", requests, concurrency, float64(requests)/elapsed.Seconds(), latencies[requests*50/100], latencies[requests*95/100], latencies[requests*99/100], maximum.Load(), float64(reused.Load())*100/requests)
	if reused.Load() != requests {
		t.Fatalf("reused=%d, want %d", reused.Load(), requests)
	}
}

func BenchmarkConnPoolWebSocketPoolHit(b *testing.B) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("{}")); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	pool := NewConnPool(websocket.DefaultDialer, nil)
	defer pool.Close()
	account := Account{OID: "benchmark", TID: "tenant"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.Warm(context.Background(), account, wsURL)
		conn, _, _, _, hit, err := pool.Take(context.Background(), account.OID, account.TID, wsURL)
		if err != nil || !hit {
			b.Fatalf("take failed: pool_hit=%v err=%v", hit, err)
		}
		_ = conn.Close()
	}
}
