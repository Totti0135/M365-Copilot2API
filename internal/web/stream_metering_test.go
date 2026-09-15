package web

import (
	"encoding/json"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func TestRemainingAllowancesFromThrottlingInfo(t *testing.T) {
	raw := json.RawMessage(`{"maxNumUserMessagesInConversation":600,"metering":{"LLMOnly":{"remainingAllowance":45},"ImageGeneration":{"remainingAllowance":12}}}`)
	got := remainingAllowances(&chathub.ThrottlingInfo{Raw: raw})
	if got["LLMOnly"] != 45 || got["ImageGeneration"] != 12 {
		t.Fatalf("expected metering extracted from ThrottlingInfo.Raw, got %v", got)
	}
}

func TestRemainingAllowancesFromRawFrame(t *testing.T) {
	got := remainingAllowances(map[string]any{
		"metering": map[string]any{"LLMOnly": map[string]any{"remainingAllowance": float64(7)}},
	})
	if got["LLMOnly"] != 7 {
		t.Fatalf("expected raw frame parsing to keep working, got %v", got)
	}
}

func TestRemainingAllowancesNilInputs(t *testing.T) {
	if got := remainingAllowances(nil); len(got) != 0 {
		t.Fatalf("nil should give empty map, got %v", got)
	}
	if got := remainingAllowances((*chathub.ThrottlingInfo)(nil)); len(got) != 0 {
		t.Fatalf("nil ThrottlingInfo should give empty map, got %v", got)
	}
}
