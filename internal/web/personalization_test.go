package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

func TestSetPersonalizationMemoryUsesSharedSubstrateLayer(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotAnchor, gotScenario string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		gotAnchor = r.Header.Get("X-AnchorMailbox")
		gotScenario = r.Header.Get("X-Scenario")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"value":"Success"}}`))
	}))
	defer ts.Close()

	oldBase := substrateBase
	oldClient := substrateHTTPClient
	substrateBase = ts.URL
	substrateHTTPClient = ts.Client()
	defer func() {
		substrateBase = oldBase
		substrateHTTPClient = oldClient
	}()

	acc := auth.AccountToken{AccessToken: "token", OID: "oid", TID: "tid"}
	if err := setPersonalizationMemory(context.Background(), acc, false); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method=%s", gotMethod)
	}
	if gotPath != personalizationUserFlagsPath {
		t.Fatalf("path=%s", gotPath)
	}
	if gotAuth != "Bearer token" {
		t.Fatalf("authorization=%q", gotAuth)
	}
	if gotAnchor != "Oid:oid@tid" {
		t.Fatalf("anchor=%q", gotAnchor)
	}
	if gotScenario != "OfficeWebIncludedCopilot" {
		t.Fatalf("scenario=%q", gotScenario)
	}
	if enabled, ok := gotBody["isMemoryEnabled"].(bool); !ok || enabled {
		t.Fatalf("body=%#v", gotBody)
	}
}

func TestDisableAndVerifyMemoryInvalidatesFlagsCache(t *testing.T) {
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"isMemoryEnabled":false,"isInsightsFromConversationHistoryEnabled":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":{"value":"Success"}}`))
	}))
	defer ts.Close()
	oldBase := substrateBase
	oldClient := substrateHTTPClient
	substrateBase = ts.URL
	substrateHTTPClient = ts.Client()
	defer func() { substrateBase, substrateHTTPClient = oldBase, oldClient }()

	acc := auth.AccountToken{ID: "account", AccessToken: "token", OID: "oid", TID: "tid"}
	flagsCache.Lock()
	flagsCache.m[acc.ID] = flagsCacheEntry{body: []byte(`{"isMemoryEnabled":true}`), fetchedAt: time.Now()}
	flagsCache.Unlock()
	if _, err := disableAndVerifyMemory(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	flagsCache.Lock()
	_, cached := flagsCache.m[acc.ID]
	flagsCache.Unlock()
	if cached {
		t.Fatal("flags cache retained stale pre-disable value")
	}
	if requests != 2 {
		t.Fatalf("requests=%d want POST+GET", requests)
	}
}

func TestMemoryAccountHonorsAccountID(t *testing.T) {
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		if _, err := store.Upsert(auth.TokenSet{HomeOID: id, TenantID: "tenant", AccessToken: "token-" + id, Email: id + "@example.test", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{tokens: store, accountPool: newAccountHealth(), accountConcurrency: newAccountConcurrency()}
	r := httptest.NewRequest(http.MethodGet, "/v1/memory/flags?account_id=second", nil)
	acc, ok := s.memoryAccount(r)
	if !ok || acc.ID != "second" {
		t.Fatalf("account=%#v ok=%t", acc, ok)
	}
}
