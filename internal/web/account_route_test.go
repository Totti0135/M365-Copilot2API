package web

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

// newRoutingTestServer 构造带 n 个有效账号的最小 Server，专测 resolveAccount 选路。
func newRoutingTestServer(t *testing.T, n int) *Server {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := store.Upsert(auth.TokenSet{
			Email:       fmt.Sprintf("acct%d@example.com", i),
			HomeOID:     fmt.Sprintf("oid-%d", i),
			TenantID:    fmt.Sprintf("tid-%d", i),
			AccessToken: "valid-token",
			ExpiresAt:   time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("upsert account %d: %v", i, err)
		}
	}
	return &Server{
		tokens:             store,
		accountPool:        newAccountHealth(),
		accountConcurrency: newAccountConcurrency(),
	}
}

// 默认策略必须是轮询：上游 f80828f 的粘性 failover 让 16 账号池 27/27 全命中第一个账号。
func TestResolveAccountRotatesRoundRobinByDefault(t *testing.T) {
	s := newRoutingTestServer(t, 3)

	first, err := s.resolveAccount("")
	if err != nil {
		t.Fatalf("first resolveAccount: %v", err)
	}
	seen := map[string]bool{first.ID: true}
	for i := 0; i < 5; i++ {
		next, err := s.resolveAccount("")
		if err != nil {
			t.Fatalf("resolveAccount #%d: %v", i, err)
		}
		seen[next.ID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("all selections pinned to %s; want round-robin across the pool", first.ID)
	}
}

// M365_ACCOUNT_STRATEGY=sticky 恢复上游的"粘住上次健康账号"行为。
func TestResolveAccountStickyOptIn(t *testing.T) {
	t.Setenv("M365_ACCOUNT_STRATEGY", "sticky")
	s := newRoutingTestServer(t, 3)

	first, err := s.resolveAccount("")
	if err != nil {
		t.Fatalf("first resolveAccount: %v", err)
	}
	for i := 0; i < 3; i++ {
		next, err := s.resolveAccount("")
		if err != nil {
			t.Fatalf("resolveAccount #%d: %v", i, err)
		}
		if next.ID != first.ID {
			t.Fatalf("sticky strategy should pin %s, got %s on call #%d", first.ID, next.ID, i)
		}
	}
}

// 显式 round-robin 配置等价于默认。
func TestResolveAccountRoundRobinExplicit(t *testing.T) {
	t.Setenv("M365_ACCOUNT_STRATEGY", "round-robin")
	s := newRoutingTestServer(t, 2)

	first, err := s.resolveAccount("")
	if err != nil {
		t.Fatalf("first resolveAccount: %v", err)
	}
	second, err := s.resolveAccount("")
	if err != nil {
		t.Fatalf("second resolveAccount: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("round-robin strategy must rotate, both hit %s", first.ID)
	}
}
