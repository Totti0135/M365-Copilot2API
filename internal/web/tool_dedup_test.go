package web

import "testing"

// The M365 Copilot upstream sometimes emits the same fenced command twice in a
// single reply. Without dedupe each copy becomes a separate tool round and the
// agent ledger falsely trips the stuck-loop guard, producing a 409. dedupe must
// collapse identical name+arguments while preserving order and the first id.
func TestDedupeToolCallsCollapsesIdenticalCalls(t *testing.T) {
	calls := []detectedToolCall{
		{ID: "call_1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
		{ID: "call_2", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
	}
	got := dedupeToolCalls(calls)
	if len(got) != 1 {
		t.Fatalf("expected 1 call after dedupe, got %d", len(got))
	}
	if got[0].ID != "call_1" {
		t.Fatalf("first occurrence should win, got id %q", got[0].ID)
	}
}

func TestDedupeToolCallsTreatsEquivalentJSONAsSame(t *testing.T) {
	calls := []detectedToolCall{
		{ID: "call_1", Name: "write", Arguments: []byte(`{"path":"a","content":"x"}`)},
		{ID: "call_2", Name: "write", Arguments: []byte(` { "content":"x", "path":"a" } `)},
	}
	if got := dedupeToolCalls(calls); len(got) != 1 {
		t.Fatalf("equivalent JSON args should dedupe, got %d", len(got))
	}
}

func TestDedupeToolCallsKeepsDistinctCalls(t *testing.T) {
	calls := []detectedToolCall{
		{ID: "call_1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
		{ID: "call_2", Name: "bash", Arguments: []byte(`{"command":"pwd"}`)},
		{ID: "call_3", Name: "read", Arguments: []byte(`{"path":"a"}`)},
	}
	if got := dedupeToolCalls(calls); len(got) != 3 {
		t.Fatalf("distinct calls must be preserved, got %d", len(got))
	}
}
