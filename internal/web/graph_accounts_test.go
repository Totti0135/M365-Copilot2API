package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

func validGraphBatchRequest() graphBatchRequest {
	return graphBatchRequest{
		Domain:                        "example.com",
		Prefix:                        "user",
		Start:                         1,
		Count:                         2,
		DisplayName:                   "Test User",
		UsageLocation:                 "US",
		InitialPassword:               "Initial!Password123",
		ForceChangePasswordNextSignIn: false,
	}
}

func TestValidateGraphBatchRequestCountLimit(t *testing.T) {
	body := validGraphBatchRequest()
	body.Count = maxGraphBatchUsers + 1
	if err := validateGraphBatchRequest(&body); err == nil {
		t.Fatal("expected count above limit to fail")
	}
}

func TestValidateGraphBatchRequestRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		change func(*graphBatchRequest)
	}{
		{"domain", func(body *graphBatchRequest) { body.Domain = "invalid" }},
		{"prefix", func(body *graphBatchRequest) { body.Prefix = "bad prefix" }},
		{"start", func(body *graphBatchRequest) { body.Start = -1 }},
		{"display name", func(body *graphBatchRequest) { body.DisplayName = "" }},
		{"usage location", func(body *graphBatchRequest) { body.UsageLocation = "USA" }},
		{"password", func(body *graphBatchRequest) { body.InitialPassword = "short" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := validGraphBatchRequest()
			test.change(&body)
			if err := validateGraphBatchRequest(&body); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestGraphBatchUsersRequiresConfiguration(t *testing.T) {
	t.Setenv("M365_GRAPH_TENANT_ID", "")
	t.Setenv("M365_GRAPH_CLIENT_ID", "")
	t.Setenv("M365_GRAPH_CLIENT_SECRET", "")
	body, _ := json.Marshal(validGraphBatchRequest())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/graph/users/batch", strings.NewReader(string(body)))
	(&Server{}).graphBatchUsers(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestLoadGraphConfigUsesOrganizationsForDelegatedAuthorization(t *testing.T) {
	t.Setenv("M365_MASTER_KEY", "test-master-key")
	t.Setenv("M365_DATA_DIR", t.TempDir())
	t.Setenv("M365_GRAPH_CLIENT_ID", "client")
	t.Setenv("M365_GRAPH_CLIENT_SECRET", "")
	t.Setenv("M365_GRAPH_TENANT_ID", "")
	t.Setenv("M365_GRAPH_TOKEN_URL", "")
	encrypted, err := auth.EncryptSecret("refresh-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveGraphAuthorization(graphAuthorizationData{
		TenantID:              "tenant",
		ClientID:              "client",
		EncryptedRefreshToken: encrypted,
		Scopes:                graphDelegatedScope,
		AuthorizedAt:          time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadGraphConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TokenURL != "https://login.microsoftonline.com/organizations/oauth2/v2.0/token" {
		t.Fatalf("unexpected delegated token URL: %q", cfg.TokenURL)
	}
}

func TestLoadGraphConfigRequiresTenantForApplicationCredentials(t *testing.T) {
	t.Setenv("M365_GRAPH_CLIENT_ID", "client")
	t.Setenv("M365_GRAPH_CLIENT_SECRET", "secret")
	t.Setenv("M365_GRAPH_TENANT_ID", "")
	t.Setenv("M365_GRAPH_TOKEN_URL", "")
	if _, err := loadGraphConfig(); err == nil {
		t.Fatal("expected application credentials without tenant ID to fail")
	}
}

func TestGraphBatchUsersContinuesAfterFailure(t *testing.T) {
	created := 0
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			jsonOut(w, map[string]string{"access_token": "test-token"})
		case r.URL.Path == "/v1.0/users":
			created++
			if created == 1 {
				w.WriteHeader(http.StatusBadRequest)
				jsonOut(w, map[string]any{"error": map[string]string{"message": "duplicate user"}})
				return
			}
			jsonOut(w, map[string]string{"id": "created-user"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer graph.Close()
	t.Setenv("M365_GRAPH_TENANT_ID", "tenant")
	t.Setenv("M365_GRAPH_CLIENT_ID", "client")
	t.Setenv("M365_GRAPH_CLIENT_SECRET", "secret")
	t.Setenv("M365_GRAPH_TOKEN_URL", graph.URL+"/token")
	t.Setenv("M365_GRAPH_BASE_URL", graph.URL)
	body, _ := json.Marshal(validGraphBatchRequest())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/graph/users/batch", strings.NewReader(string(body)))
	(&Server{}).graphBatchUsers(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("got status %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Results []graphBatchResult `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 2 || response.Results[0].Success || !response.Results[1].Success {
		t.Fatalf("unexpected results: %+v", response.Results)
	}
	if response.Results[1].OAuthStartURL != "/api/auth/start" {
		t.Fatalf("unexpected OAuth URL: %q", response.Results[1].OAuthStartURL)
	}
}

func TestGraphEndpointsAreNotRegistered(t *testing.T) {
	s := &Server{
		adminPassword: "configured",
		adminSessions: map[string]time.Time{"admin-session": time.Now().Add(time.Hour)},
	}
	for _, path := range []string{
		"/api/admin/graph/config",
		"/api/admin/graph/authorization/start",
		"/api/admin/graph/authorization/status",
		"/api/admin/graph/authorization/callback",
		"/api/admin/graph/authorization/revoke",
		"/api/admin/graph/users/batch",
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(&http.Cookie{Name: "m365_admin_session", Value: "admin-session"})
		s.Routes().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d, want %d", path, recorder.Code, http.StatusNotFound)
		}
	}
}

func TestGraphAuthorizationStartStatusAndRevoke(t *testing.T) {
	t.Setenv("M365_MASTER_KEY", "test-master-key")
	t.Setenv("M365_GRAPH_CLIENT_ID", "test-client")
	t.Setenv("M365_DATA_DIR", t.TempDir())

	session := "admin-session"
	s := &Server{
		adminSessions:       map[string]time.Time{session: time.Now().Add(time.Hour)},
		graphAuthorizations: map[string]pendingGraphAuthorization{},
	}
	startRecorder := httptest.NewRecorder()
	startRequest := httptest.NewRequest(http.MethodPost, "/api/admin/graph/authorization/start", nil)
	startRequest.AddCookie(&http.Cookie{Name: "m365_admin_session", Value: session})
	s.graphAuthorizationStart(startRecorder, startRequest)
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", startRecorder.Code, startRecorder.Body.String())
	}
	var startResponse struct {
		URL            string `json:"url"`
		PermissionType string `json:"permissionType"`
	}
	if err := json.Unmarshal(startRecorder.Body.Bytes(), &startResponse); err != nil {
		t.Fatal(err)
	}
	if startResponse.URL == "" || startResponse.PermissionType != "delegated" {
		t.Fatalf("unexpected start response: %+v", startResponse)
	}

	statusRecorder := httptest.NewRecorder()
	s.graphAuthorizationStatus(statusRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/graph/authorization/status", nil))
	if statusRecorder.Code != http.StatusOK || !strings.Contains(statusRecorder.Body.String(), `"authorized":false`) {
		t.Fatalf("status before authorization = %d: %s", statusRecorder.Code, statusRecorder.Body.String())
	}

	revokeRecorder := httptest.NewRecorder()
	s.graphAuthorizationRevoke(revokeRecorder, httptest.NewRequest(http.MethodPost, "/api/admin/graph/authorization/revoke", nil))
	if revokeRecorder.Code != http.StatusOK || !strings.Contains(revokeRecorder.Body.String(), `"revoked":true`) {
		t.Fatalf("revoke status = %d: %s", revokeRecorder.Code, revokeRecorder.Body.String())
	}
}

func TestGraphAuthorizationStartRejectsWhenPendingLimitReached(t *testing.T) {
	t.Setenv("M365_MASTER_KEY", "test-master-key")
	t.Setenv("M365_GRAPH_CLIENT_ID", "test-client")

	session := "admin-session"
	pending := make(map[string]pendingGraphAuthorization, maxPendingGraphAuthorizations)
	for i := 0; i < maxPendingGraphAuthorizations; i++ {
		pending[string(rune(i))] = pendingGraphAuthorization{Created: time.Now()}
	}
	s := &Server{
		adminSessions:       map[string]time.Time{session: time.Now().Add(time.Hour)},
		graphAuthorizations: pending,
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/graph/authorization/start", nil)
	request.AddCookie(&http.Cookie{Name: "m365_admin_session", Value: session})

	s.graphAuthorizationStart(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusTooManyRequests, recorder.Body.String())
	}
	if len(s.graphAuthorizations) != maxPendingGraphAuthorizations {
		t.Fatalf("pending authorizations = %d, want %d", len(s.graphAuthorizations), maxPendingGraphAuthorizations)
	}
}
