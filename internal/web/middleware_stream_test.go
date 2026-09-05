package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type flushCountingWriter struct {
	*httptest.ResponseRecorder
	flushes int
}

func (w *flushCountingWriter) Flush() {
	w.flushes++
	w.ResponseRecorder.Flush()
}

func TestMiddlewareChainPreservesSSEFlush(t *testing.T) {
	base := &flushCountingWriter{ResponseRecorder: httptest.NewRecorder()}
	h := recoverPanics(httpTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("middleware removed http.Flusher")
		}
		_, _ = w.Write([]byte("data: first\n\n"))
		f.Flush()
		_, _ = w.Write([]byte("data: second\n\n"))
		f.Flush()
	})))

	h.ServeHTTP(base, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if base.flushes != 2 {
		t.Fatalf("underlying Flush called %d times, want 2", base.flushes)
	}
}
