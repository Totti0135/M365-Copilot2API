package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordingEmit captures emitted Anthropic SSE frames as "name:json" pairs.
type recordingEmit struct {
	frames []string
}

func (re *recordingEmit) emit(name string, v any) bool {
	b, _ := json.Marshal(v)
	re.frames = append(re.frames, name+":"+string(b))
	return true
}

func (re *recordingEmit) names() []string {
	out := make([]string, 0, len(re.frames))
	for _, f := range re.frames {
		out = append(out, strings.SplitN(f, ":", 2)[0])
	}
	return out
}

func translateCanned(t *testing.T, inner string) (*anthropicStreamTranslator, *recordingEmit) {
	t.Helper()
	re := &recordingEmit{}
	tr := newAnthropicStreamTranslator(re.emit)
	scanner := bufio.NewScanner(strings.NewReader(inner))
	scanner.Buffer(make([]byte, 4096), 10<<20)
	tr.scan(scanner, context.Background())
	return tr, re
}

func TestAnthropicStreamTranslatorThinkingThenTextDeltas(t *testing.T) {
	inner := strings.Join([]string{
		`: connected`,
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":"pondering"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	tr, re := translateCanned(t, inner)
	// The trailing content_block_stop for the text block is emitted by
	// closeAllBlocks during adapter finalize, not during scan.
	want := []string{
		"content_block_start", "content_block_delta", // thinking
		"content_block_stop",
		"content_block_start", "content_block_delta", "content_block_delta", // text
	}
	if got := re.names(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}
	if !strings.Contains(re.frames[1], `"thinking_delta"`) || !strings.Contains(re.frames[1], "pondering") {
		t.Fatalf("thinking delta missing: %s", re.frames[1])
	}
	if !strings.Contains(re.frames[4], `"text_delta"`) || !strings.Contains(re.frames[4], "Hel") {
		t.Fatalf("first text delta missing: %s", re.frames[4])
	}
	if tr.text.String() != "Hello" || tr.stopReason() != "end_turn" || tr.outputText() != "Hello" {
		t.Fatalf("translator state: text=%q stop=%q", tr.text.String(), tr.stopReason())
	}
}

func TestAnthropicStreamTranslatorToolCallFragments(t *testing.T) {
	inner := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	tr, re := translateCanned(t, inner)
	joined := strings.Join(re.frames, "\n")
	for _, want := range []string{
		`"type":"tool_use"`, `"id":"call_1"`, `"name":"get_weather"`,
		`"input_json_delta"`, `"{\"city\":"`, `"\"Paris\"}"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in frames:\n%s", want, joined)
		}
	}
	if len(tr.callOrder) != 1 || tr.callOrder[0].args.String() != `{"city":"Paris"}` {
		t.Fatalf("tool args = %v", tr.callOrder)
	}
	if tr.stopReason() != "tool_use" || tr.outputText() != `get_weather{"city":"Paris"}` {
		t.Fatalf("stop=%q output=%q", tr.stopReason(), tr.outputText())
	}
	tr.closeAllBlocks()
	ends := re.frames[len(re.frames)-1]
	if !strings.Contains(ends, `"content_block_stop"`) {
		t.Fatalf("finalize did not close tool block: %s", ends)
	}
}

func TestAnthropicStreamTranslatorMapsLengthToMaxTokens(t *testing.T) {
	inner := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		``,
	}, "\n")
	tr, _ := translateCanned(t, inner)
	if tr.stopReason() != "max_tokens" {
		t.Fatalf("stop = %q, want max_tokens", tr.stopReason())
	}
}

func TestAnthropicStreamTranslatorStopsOnInnerErrorChunk(t *testing.T) {
	inner := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"so far"}}]}`,
		`data: {"error":{"message":"upstream is rate limiting; try again shortly","code":"rate_limit"}}`,
		`data: {"choices":[{"index":0,"delta":{"content":"after failure"}}]}`,
		``,
	}, "\n")
	tr, re := translateCanned(t, inner)
	if !tr.failed {
		t.Fatal("inner error chunk not flagged")
	}
	if tr.text.String() != "so far" {
		t.Fatalf("text after error leaked: %q", tr.text.String())
	}
	joined := strings.Join(re.frames, "\n")
	if strings.Contains(joined, "after failure") {
		t.Fatalf("frames continued after error:\n%s", joined)
	}
}

func TestAnthropicStreamTranslatorSurvivesJunkLines(t *testing.T) {
	inner := strings.Join([]string{
		`: keepalive`,
		`data: not-json`,
		`data: {"choices":[]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}`,
		``,
	}, "\n")
	tr, re := translateCanned(t, inner)
	if tr.failed || tr.clientGone || tr.text.String() != "ok" {
		t.Fatalf("junk lines broke translation: failed=%t gone=%t text=%q", tr.failed, tr.clientGone, tr.text.String())
	}
	// Scan-time frames are block start + delta only; the trailing
	// content_block_stop comes from closeAllBlocks in the adapter finalize.
	if len(re.frames) != 2 {
		t.Fatalf("frames = %v", re.frames)
	}
}

// The full /v1/messages streaming path with a Server that has no accounts:
// message_start is already out, so the inner failure must surface as an
// Anthropic error event instead of a hang or a panic (nil usage log).
func TestAnthropicMessagesStreamEmitsErrorEventForInnerFailure(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	s.anthropicMessages(w, r)
	body := w.Body.String()
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	for _, want := range []string{"event: message_start", "event: error"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "event: message_stop") {
		t.Fatalf("unexpected message_stop after inner failure: %s", body)
	}
}
