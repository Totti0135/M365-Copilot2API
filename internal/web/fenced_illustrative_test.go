package web

import "testing"

// A shell code block embedded in a long explanatory answer is an illustrative
// example, not an execution request. Converting it to a tool call replaced the
// whole prose answer with content:nil, so the client saw a truncated reply
// while the upstream conversation held the full text. See the socks5 proxy
// explanation regression.
func TestFencedIllustrativeShellIsNotHijacked(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}}
	prose := "你正在使用的 URL 是一个订阅链接。\n\n" +
		"SOCKS5 代理是一种在传输层工作的通用代理协议，它不理解 HTTP 语义，" +
		"因此可以转发任意 TCP/UDP 流量。相比之下 HTTP 代理只能代理 HTTP(S) 请求。\n\n" +
		"下面是一个用 curl 走 SOCKS5 代理的例子：\n\n" +
		"```bash\ncurl --proxy \"socks5h://user:pass@1.2.3.4:1080\" https://api.ipify.org\n```\n\n" +
		"其中 socks5h 表示让代理服务器做 DNS 解析。总结：两者的核心区别在于协议层级和可代理的流量类型，" +
		"SOCKS5 更通用，HTTP 代理更专用于网页请求。"
	calls := fencedToolCalls(prose, tools, "auto")
	if len(calls) != 0 {
		t.Fatalf("illustrative shell block must not become a tool call, got %d: %+v", len(calls), calls)
	}
}

// A short "run this" answer with minimal surrounding prose is still a genuine
// execution request and must convert.
func TestFencedShortShellStillConverts(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}}
	calls := fencedToolCalls("Sure:\n```bash\nls -la\n```", tools, "auto")
	if len(calls) != 1 || calls[0].Name != "bash" {
		t.Fatalf("short execution request should convert, got %+v", calls)
	}
}

// Explicit ```toolname\n{json}``` blocks are unambiguous structured calls and
// must convert regardless of surrounding prose length.
func TestFencedExplicitToolBlockConvertsWithLongProse(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "workspace_shell"}}}
	long := ""
	for i := 0; i < 60; i++ {
		long += "这是一段很长的解释文字用来撑过阈值。"
	}
	text := long + "\n```workspace_shell\n{\"command\":\"pwd\"}\n```\n" + long
	calls := fencedToolCalls(text, tools, "auto")
	if len(calls) != 1 || calls[0].Name != "workspace_shell" {
		t.Fatalf("explicit tool block should convert despite long prose, got %+v", calls)
	}
}
