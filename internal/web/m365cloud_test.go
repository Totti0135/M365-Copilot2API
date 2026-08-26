package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestCloudClient 构造指向 httptest 服务器的云客户端，绕过真实 token 刷新。
func newTestCloudClient(handler http.Handler) (*M365CloudClient, *httptest.Server) {
	srv := httptest.NewServer(handler)
	c := NewM365CloudClient("client-id", "tenant-id", "refresh-token")
	c.chatEndpoint = srv.URL
	c.accessToken = "test-token"
	c.expiresAt = time.Now().Add(time.Hour)
	return c, srv
}

// recordRequest 捕获请求体的 action 与 state，供断言。
type recordRequest struct {
	action string
	state  map[string]any
}

func decodeRequest(t *testing.T, r *http.Request) recordRequest {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	rec := recordRequest{}
	rec.action, _ = body["action"].(string)
	rec.state, _ = body["state"].(map[string]any)
	return rec
}

// writeJSON 按生产响应的 Content-Type 回写 JSON。
func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestListConversationsUsesGetConversationPageHistoryList(t *testing.T) {
	var got recordRequest
	c, srv := newTestCloudClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = decodeRequest(t, r)
		writeJSON(w, `{"store":{"conversationPageHistoryList":{"chats":[
			{"conversationId":"map-conv","createTimeUtc":1.7e12},
			"{\"conversationId\":\"str-conv\",\"createTimeUtc\":1.8e12}"
		]}}}`)
	}))
	defer srv.Close()

	chats, err := c.ListConversations()
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if got.action != "GetConversationPageHistoryList" {
		t.Fatalf("action = %q, want GetConversationPageHistoryList", got.action)
	}
	// dispatcher 要求 state 带 conversationPageHistoryList（否则报 'agentList' 未定义）
	hl, ok := got.state["conversationPageHistoryList"].(map[string]any)
	if !ok {
		t.Fatalf("state.conversationPageHistoryList missing: %v", got.state)
	}
	if _, ok := hl["chats"].([]any); !ok {
		t.Fatalf("state.conversationPageHistoryList.chats missing: %v", hl)
	}
	if len(chats) != 2 {
		t.Fatalf("chats = %d, want 2 (map 与 string 两种条目形态)", len(chats))
	}
	if chats[0]["conversationId"] != "map-conv" || chats[1]["conversationId"] != "str-conv" {
		t.Fatalf("conversationIds = %v, %v", chats[0]["conversationId"], chats[1]["conversationId"])
	}
}

func TestListConversationsEmpty(t *testing.T) {
	c, srv := newTestCloudClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"store":{"conversationPageHistoryList":{"chats":[],"metrics":{"chatCount":0}}}}`)
	}))
	defer srv.Close()

	chats, err := c.ListConversations()
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(chats) != 0 {
		t.Fatalf("chats = %d, want 0", len(chats))
	}
}

func TestListConversationsFallsBackToRefreshNavPane(t *testing.T) {
	actions := []string{}
	c, srv := newTestCloudClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := decodeRequest(t, r)
		actions = append(actions, rec.action)
		if rec.action == "GetConversationPageHistoryList" {
			// 新 action 的 store 里没有历史列表（协议又变）→ 走 RefreshNavPane 兜底
			writeJSON(w, `{"store":{"isEURegion":true,"notebooks":[]}}`)
			return
		}
		writeJSON(w, `{"store":{"conversationPageHistoryList":{"chats":[
			{"conversationId":"legacy-conv"}]}}}`)
	}))
	defer srv.Close()

	chats, err := c.ListConversations()
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(actions) != 2 || actions[0] != "GetConversationPageHistoryList" || actions[1] != "RefreshNavPane" {
		t.Fatalf("actions = %v, want [GetConversationPageHistoryList RefreshNavPane]", actions)
	}
	if len(chats) != 1 || chats[0]["conversationId"] != "legacy-conv" {
		t.Fatalf("chats = %v, want legacy-conv", chats)
	}
}

func TestListConversationsBothShapesMissingReturnsEmpty(t *testing.T) {
	c, srv := newTestCloudClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"store":{"isEURegion":true}}`)
	}))
	defer srv.Close()

	chats, err := c.ListConversations()
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(chats) != 0 {
		t.Fatalf("chats = %d, want graceful empty", len(chats))
	}
}

func TestListConversationsNonJSONResponse(t *testing.T) {
	c, srv := newTestCloudClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>login redirect</html>"))
	}))
	defer srv.Close()

	_, err := c.ListConversations()
	if err == nil || !strings.Contains(err.Error(), "content type") {
		t.Fatalf("err = %v, want content-type error", err)
	}
}
