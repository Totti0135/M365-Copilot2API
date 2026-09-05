package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesToOpenAI(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "what time", Tools: []map[string]any{{"type": "function", "name": "clock", "parameters": map[string]any{"type": "object"}}}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 1 || len(o.Tools) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestResponsesIncludeIsAcceptedForClientCompatibility(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "hello", Include: []string{"reasoning.encrypted_content"}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 1 {
		t.Fatalf("openAI()=%+v, err=%v", o, err)
	}
}

func TestResponseNamespaceIsolatesTenantAndSession(t *testing.T) {
	if responseNamespace("tenant-a", "shared") == responseNamespace("tenant-b", "shared") {
		t.Fatal("different tenants share a response namespace")
	}
	if responseNamespace("tenant", "session-a") == responseNamespace("tenant", "session-b") {
		t.Fatal("different sessions share a response namespace")
	}
}

func TestResponseSessionIDIsRequestLocal(t *testing.T) {
	a := httptest.NewRequest("POST", "/v1/responses", nil)
	b := httptest.NewRequest("POST", "/v1/responses", nil)
	a.Header.Set(sessionHeaderName, "session-a")
	b.Header.Set(sessionHeaderName, "session-b")
	if responseSessionID(a) != "session-a" || responseSessionID(b) != "session-b" {
		t.Fatalf("request session IDs crossed: a=%q b=%q", responseSessionID(a), responseSessionID(b))
	}
}

func TestResponsesToolOutputIDsAreRequestLocal(t *testing.T) {
	a := []any{map[string]any{"type": "function_call_output", "call_id": "call-a", "output": "a"}}
	b := []any{map[string]any{"type": "custom_tool_call_output", "call_id": "call-b", "output": "b"}}
	aIDs := extractResponsesToolOutputIDs(a)
	bIDs := extractResponsesToolOutputIDs(b)
	if len(aIDs) != 1 || aIDs[0] != "call-a" || len(bIDs) != 1 || bIDs[0] != "call-b" {
		t.Fatalf("tool output IDs crossed: a=%v b=%v", aIDs, bIDs)
	}
}

func TestResponseHistoryBucketsIsolateTenantAndSession(t *testing.T) {
	s := &Server{responseMessages: map[string]map[string]*RespNode{}}
	tenants := []struct {
		tenant  string
		session string
		value   string
	}{
		{tenant: "tenant-a", session: "shared", value: "tenant-a"},
		{tenant: "tenant-b", session: "shared", value: "tenant-b"},
		{tenant: "tenant-a", session: "other", value: "session-other"},
	}
	for _, item := range tenants {
		s.responseMessages[responseNamespace(item.tenant, item.session)] = map[string]*RespNode{
			"resp_shared": {Messages: []oaiMsg{{Role: "assistant", Content: item.value}}},
		}
	}
	for _, item := range tenants {
		node := s.responseMessages[responseNamespace(item.tenant, item.session)]["resp_shared"]
		if node == nil || len(node.Messages) != 1 || node.Messages[0].Content != item.value {
			t.Fatalf("response history crossed for tenant=%q session=%q: %#v", item.tenant, item.session, node)
		}
	}
}

func TestResponsesCustomExecToOpenAI(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "inspect", Tools: []map[string]any{{"type": "custom", "name": "exec", "description": "run a command", "format": map[string]any{"type": "grammar"}}}}
	_, err := r.openAI()
	if err == nil || !strings.Contains(err.Error(), "unsupported_parameter: tools") {
		t.Fatalf("err=%v, want unsupported custom tool", err)
	}
}

func TestResponsesCustomExecIsExclusiveTool(t *testing.T) {
	r := responsesRequest{Input: "edit the project", Tools: []map[string]any{
		{"type": "custom", "name": "exec", "description": "local execution"},
		{"type": "function", "name": "m365_search", "description": "native search"},
	}}
	_, err := r.openAI()
	if err == nil || !strings.Contains(err.Error(), "unsupported_parameter: tools") {
		t.Fatalf("err=%v, want unsupported custom tool", err)
	}
}

func TestResponsesInstructionsAndCustomExecPolicyAreSystemMessages(t *testing.T) {
	r := responsesRequest{
		Instructions: "Use the repository selected by the caller.",
		Input:        "inspect the repository",
		Tools:        []map[string]any{{"type": "custom", "name": "exec", "description": "run a command"}},
	}
	_, err := r.openAI()
	if err == nil || !strings.Contains(err.Error(), "unsupported_parameter: tools") {
		t.Fatalf("err=%v, want unsupported custom tool", err)
	}
}

func TestResponsesCustomToolOutputToOpenAI(t *testing.T) {
	r := responsesRequest{Input: []any{
		map[string]any{"type": "custom_tool_call", "call_id": "call_exec", "name": "exec", "input": "uname -s"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_exec", "output": "Linux"},
	}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 2 || o.Messages[0].Role != "assistant" || o.Messages[0].ToolCalls[0]["type"] != "custom" || o.Messages[1].Role != "tool" || o.Messages[1].ToolCallID != "call_exec" {
		t.Fatalf("messages=%+v err=%v", o.Messages, err)
	}
	if err := validateToolConversation(o.Messages); err != nil {
		t.Fatalf("custom tool continuation rejected: %v", err)
	}
}

func TestValidateToolConversationAllowsReusedCallIDAfterCompletion(t *testing.T) {
	messages := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "bash:0"}}},
		{Role: "tool", ToolCallID: "bash:0"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "bash:0"}}},
		{Role: "tool", ToolCallID: "bash:0"},
	}
	if err := validateToolConversation(messages); err != nil {
		t.Fatalf("completed tool call ID reuse rejected: %v", err)
	}
}

func TestResponsesAdditionalToolsToOpenAI(t *testing.T) {
	r := responsesRequest{Model: "gpt-5.6-luna", Input: []any{
		map[string]any{
			"type": "additional_tools", "role": "developer",
			"tools": []any{
				map[string]any{"type": "custom", "name": "exec", "description": "run a command", "format": map[string]any{"type": "grammar"}},
				map[string]any{"type": "function", "name": "wait", "description": "wait", "parameters": map[string]any{"type": "object"}},
			},
		},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "run ls"}}},
	}}
	_, err := r.openAI()
	if err == nil || !strings.Contains(err.Error(), "unsupported_parameter: tools") {
		t.Fatalf("err=%v, want unsupported custom tool", err)
	}
}

func TestResponsesAdditionalToolsNoInputTools(t *testing.T) {
	r := responsesRequest{Model: "gpt-5.6-luna", Input: []any{
		map[string]any{
			"type": "additional_tools", "role": "developer",
			"tools": []any{
				map[string]any{"type": "function", "name": "wait", "description": "wait", "parameters": map[string]any{"type": "object"}},
				map[string]any{"type": "function", "name": "request_user_input", "description": "ask", "parameters": map[string]any{"type": "object"}},
			},
		},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatalf("openAI() error: %v", err)
	}
	if len(o.Tools) != 2 {
		t.Fatalf("tools=%#v, want wait + request_user_input", o.Tools)
	}
}

func TestAnthropicToOpenAI(t *testing.T) {
	r := anthropicRequest{Model: "m", System: any("be concise"), Messages: []anthropicMessage{{Role: "user", Content: any("weather")}}, Tools: []anthropicTool{{Name: "weather", InputSchema: map[string]any{"type": "object"}}}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 2 || len(o.Tools) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestAnthropicToolResult(t *testing.T) {
	r := anthropicRequest{Messages: []anthropicMessage{{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "x", "name": "f", "input": map[string]any{}}}}, {Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": "ok"}}}}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 2 || o.Messages[1].ToolCallID != "x" {
		t.Fatalf("%+v %v", o, err)
	}
}
