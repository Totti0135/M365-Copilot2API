package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type failingRoundTripper struct {
	calls int
}

func (f *failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls++
	return nil, errors.New("connection reset")
}

func TestRemoveProxyNormalizesAndRejectsMissing(t *testing.T) {
	if err := ConfigurePool([]string{"http://example.com/"}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveProxy("http://example.com"); err != nil {
		t.Fatal(err)
	}
	if len(ProxyPoolStatus()) != 0 {
		t.Fatalf("pool not empty: %#v", ProxyPoolStatus())
	}
	if err := RemoveProxy("http://missing.example"); err == nil {
		t.Fatal("expected missing proxy error")
	}
}

func TestPoolDoesNotReplayNonIdempotentPost(t *testing.T) {
	first := &failingRoundTripper{}
	p := &Pool{entries: []*poolEntry{{raw: "first"}, {raw: "second"}}}
	rt := &poolRoundTripper{pool: p, entry: p.entries[0], base: first}
	req, err := http.NewRequest(http.MethodPost, "http://example.test", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("expected transport error")
	}
	if first.calls != 1 {
		t.Fatalf("POST transport calls=%d want 1", first.calls)
	}
}

func TestDirectTransportConfiguration(t *testing.T) {
	transport, ok := directClients().HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatal("HTTP transport has unexpected type")
	}
	if transport.DialContext == nil {
		t.Fatal("DialContext is nil")
	}
	if transport.MaxIdleConns != 100 || transport.IdleConnTimeout != 90*time.Second {
		t.Fatalf("idle pool configuration = %d, %v", transport.MaxIdleConns, transport.IdleConnTimeout)
	}
	if transport.TLSHandshakeTimeout != 10*time.Second || transport.ExpectContinueTimeout != time.Second {
		t.Fatalf("timeout configuration = %v, %v", transport.TLSHandshakeTimeout, transport.ExpectContinueTimeout)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("HTTP/2 is not enabled")
	}
}

func TestTransportPropagatesDialFailure(t *testing.T) {
	want := errors.New("dial failure")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, want
	}
	client := &http.Client{Transport: transport}
	_, err := client.Get("http://example.test")
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want dial failure", err)
	}
}

func TestTransportReportsTCPReset(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		tcp := conn.(*net.TCPConn)
		if err := tcp.SetLinger(0); err != nil {
			_ = tcp.Close()
			done <- err
			return
		}
		done <- tcp.Close()
	}()

	client := &http.Client{Timeout: time.Second}
	_, err = client.Get("http://" + listener.Addr().String())
	if err == nil {
		t.Fatal("expected TCP reset error")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reset server")
	}
}

func TestTransportSlowReadHonorsTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "a")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, "b")
	}))
	defer server.Close()

	client := &http.Client{Timeout: 25 * time.Millisecond}
	response, err := client.Get(server.URL)
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want deadline exceeded", err)
		}
		return
	}
	defer response.Body.Close()
	_, err = io.ReadAll(response.Body)
	if err == nil {
		t.Fatal("expected slow read timeout")
	}
}

func TestTransportCancellationInterruptsHalfOpenConnection(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}
	client := &http.Client{Transport: transport}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
}

func TestTransportReportsEOFBeforeResponse(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}
	go func() {
		buf := make([]byte, 4096)
		_, _ = serverConn.Read(buf)
		_ = serverConn.Close()
	}()
	_, err := (&http.Client{Transport: transport}).Get("http://example.test")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v, want EOF", err)
	}
}

func TestTransportReusesHTTP1Connection(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	client := directClients().HTTP
	for i := 0; i < 3; i++ {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("connections = %d, want 1", got)
	}
}

func TestTransportNegotiatesHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := directClients().HTTP.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- local test server
	response, err := (&http.Client{Transport: transport}).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "HTTP/2.0" {
		t.Fatalf("protocol = %q, want HTTP/2.0", body)
	}
}

func TestTransportRejectsTLSHandshakeFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_, _ = conn.Write([]byte("not tls"))
			_ = conn.Close()
		}
	}()
	transport := directClients().HTTP.Transport.(*http.Transport).Clone()
	transport.TLSHandshakeTimeout = 100 * time.Millisecond
	_, err = (&http.Client{Transport: transport}).Get("https://" + listener.Addr().String())
	if err == nil {
		t.Fatal("expected TLS handshake failure")
	}
}

func TestTransportConnectionPoolExhaustionHonorsTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-release:
			_, _ = io.WriteString(w, "ok")
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()

	transport := directClients().HTTP.Transport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 1
	client := &http.Client{Transport: transport}
	firstDone := make(chan error, 1)
	go func() {
		response, err := client.Get(server.URL)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			err = response.Body.Close()
		}
		firstDone <- err
	}()

	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Do(request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pool wait error = %v, want deadline exceeded", err)
	}
	close(release)
	if err = <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestTransportRecoversAfterIdleConnectionsClose(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	transport := directClients().HTTP.Transport.(*http.Transport).Clone()
	client := &http.Client{Transport: transport}
	request := func() {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	request()
	transport.CloseIdleConnections()
	request()
	if got := connections.Load(); got != 2 {
		t.Fatalf("connections = %d, want 2 after idle close recovery", got)
	}
}

func TestTransportMultiplexesHTTP2Requests(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("protocol = %s, want HTTP/2", r.Proto)
		}
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			_, _ = io.WriteString(w, "ok")
		case <-time.After(time.Second):
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	transport := directClients().HTTP.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- local test server
	client := &http.Client{Transport: transport}
	var wait sync.WaitGroup
	errorsCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response, err := client.Get(server.URL)
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				err = response.Body.Close()
			}
			errorsCh <- err
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for concurrent HTTP/2 requests")
		}
	}
	close(release)
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrent requests = %d, want 2", maximum.Load())
	}
}
