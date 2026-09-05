package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountConcurrencyLimitsAndReleasesSlots(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "2")
	limiter := newAccountConcurrency()
	release1, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	release2, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	if limiter.Available("account-a") {
		t.Fatal("account remained available at its configured limit")
	}
	if !limiter.Available("account-b") {
		t.Fatal("one full account must not block another account")
	}
	release1()
	if !limiter.Available("account-a") {
		t.Fatal("released slot was not returned")
	}
	release1()
	release2()
}

func TestServerConcurrencyLimitRejectsWithoutQueueing(t *testing.T) {
	s := &Server{requestSlots: make(chan struct{}, 1)}
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := s.limitConcurrency(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}")))
		close(done)
	}()
	<-entered

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}")))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", recorder.Header().Get("Retry-After"))
	}
	if strings.Contains(recorder.Body.String(), "rate_limit") {
		t.Fatalf("local overload was mislabeled as upstream rate limiting: %s", recorder.Body.String())
	}

	close(release)
	<-done
}

func TestAccountConcurrencyWaitHonorsCancellation(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "1")
	limiter := newAccountConcurrency()
	release, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := limiter.Acquire(ctx, "account-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v, want deadline exceeded", err)
	}
}

func TestAccountConcurrencyUsesDocumentedDefault(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "")
	limiter := newAccountConcurrency()
	if limiter.limit != defaultAccountConcurrency {
		t.Fatalf("limit = %d, want %d", limiter.limit, defaultAccountConcurrency)
	}
}

func BenchmarkServerConcurrencyLimit(b *testing.B) {
	s := &Server{requestSlots: make(chan struct{}, 1)}
	handler := s.limitConcurrency(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}")))
		if recorder.Code != http.StatusNoContent {
			b.Fatalf("status = %d, want 204", recorder.Code)
		}
	}
}

func BenchmarkServerConcurrencyLimitHTTP(b *testing.B) {
	s := &Server{requestSlots: make(chan struct{}, 128)}
	server := httptest.NewServer(s.limitConcurrency(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	defer server.Close()

	client := server.Client()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Post(server.URL+"/v1/responses", "application/json", strings.NewReader("{}"))
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				b.Fatalf("status = %d, want 204", resp.StatusCode)
			}
		}
	})
}

func TestServerConcurrencyLimitHTTPPerformance(t *testing.T) {
	const requests = 1000
	const concurrency = 32
	s := &Server{requestSlots: make(chan struct{}, 128)}
	var active atomic.Int64
	var maximum atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(s.limitConcurrency(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
		}
		<-release
		w.WriteHeader(http.StatusNoContent)
	})))
	defer server.Close()

	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = concurrency
	transport.MaxConnsPerHost = concurrency
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	latencies := make([]time.Duration, requests)
	jobs := make(chan int)
	var workers sync.WaitGroup
	started := time.Now()
	for i := 0; i < concurrency; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				requestStarted := time.Now()
				response, err := client.Post(server.URL+"/v1/responses", "application/json", strings.NewReader("{}"))
				latencies[index] = time.Since(requestStarted)
				if err != nil {
					t.Errorf("request failed: %v", err)
					continue
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusNoContent {
					t.Errorf("status = %d, want 204", response.StatusCode)
				}
			}
		}()
	}
	for i := range latencies {
		jobs <- i
		if i == concurrency-1 {
			deadline := time.Now().Add(time.Second)
			for maximum.Load() < concurrency && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			close(release)
		}
	}
	close(jobs)
	workers.Wait()
	elapsed := time.Since(started)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("requests=%d concurrency=%d throughput=%.2f req/s p50=%s p95=%s p99=%s maximum_active=%d", requests, concurrency, float64(requests)/elapsed.Seconds(), latencies[requests*50/100], latencies[requests*95/100], latencies[requests*99/100], maximum.Load())
}
