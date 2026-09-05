package web

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func adminTestClient(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	jar, _ := cookiejar.New(nil)
	c := ts.Client()
	c.Jar = jar
	t.Cleanup(ts.Close)
	return ts, c
}

func postJSON(t *testing.T, c *http.Client, url, body string) *http.Response {
	t.Helper()
	r, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDefaultPasswordForcesChangeAndRotatesSessions(t *testing.T) {
	strong := "Str0ng!Passw0rd#2024"
	newStrong := "N3w!Str0ng#Passw0rd2025"
	t.Setenv("M365_ADMIN_PASSWORD", strong)
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/admin-password")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, c := adminTestClient(t, s.Routes())

	r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"`+strong+`"}`)
	if r.StatusCode != 200 {
		t.Fatalf("login=%d", r.StatusCode)
	}
	var login map[string]any
	_ = json.NewDecoder(r.Body).Decode(&login)
	r.Body.Close()

	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("protected status=%d want 200 got %d", 200, r.StatusCode)
	}

	r = postJSON(t, c, ts.URL+"/api/admin/change-password", `{"current_password":"`+strong+`","new_password":"`+newStrong+`"}`)
	if r.StatusCode != 200 {
		t.Fatalf("change=%d", r.StatusCode)
	}
	r.Body.Close()

	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session status=%d", r.StatusCode)
	}

	r = postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"`+newStrong+`"}`)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("new login=%d", r.StatusCode)
	}
	r, _ = c.Get(ts.URL + "/api/accounts")
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("new session status=%d", r.StatusCode)
	}
}

func TestAdminLoginLocksAfterFiveFailures(t *testing.T) {
	pw := "L0ck!T3st#Str0ng2024"
	t.Setenv("M365_ADMIN_PASSWORD", pw)
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/admin-password-lock")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, c := adminTestClient(t, s.Routes())
	for i := 0; i < 5; i++ {
		r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"wrong"}`)
		r.Body.Close()
		if r.StatusCode != 401 {
			t.Fatalf("attempt %d=%d", i+1, r.StatusCode)
		}
	}
	r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"`+pw+`"}`)
	defer r.Body.Close()
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("locked=%d retry=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}
}

func TestPersistedPasswordOverridesBootstrapEnv(t *testing.T) {
	path := t.TempDir() + "/admin-password"
	t.Setenv("M365_ADMIN_PASSWORD_FILE", path)
	t.Setenv("M365_ADMIN_PASSWORD", "old-bootstrap-password")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	persisted := "Persisted!Str0ng#2024"
	if err := saveAdminPassword(persisted); err != nil {
		t.Fatal(err)
	}
	got, _, err := loadAdminPassword()
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(got, persisted) {
		t.Fatalf("got hash does not match persisted password")
	}
}

func TestExpiredLoginWindowResets(t *testing.T) {
	s := &Server{loginAttempts: map[string]loginAttempt{"x": {Failures: 4, WindowStart: time.Now().Add(-16 * time.Minute)}}}
	if ok, _ := s.loginAllowed("x", time.Now()); !ok {
		t.Fatal("expired window remained locked")
	}
}

func TestLoadAdminPasswordRequiresConfig(t *testing.T) {
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/no-file")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_DATA_DIR", t.TempDir()+"/empty")
	t.Setenv("M365_REQUIRE_STRONG_ADMIN_PASSWORD", "1")
	_, _, err := loadAdminPassword()
	if err == nil {
		t.Fatal("expected error when no password configured and bootstrap disabled")
	}
}

func TestLoadAdminPasswordBootstrapsDefault(t *testing.T) {
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/no-file")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_DATA_DIR", t.TempDir()+"/empty")
	t.Setenv("M365_REQUIRE_STRONG_ADMIN_PASSWORD", "")
	hash, mustChange, err := loadAdminPassword()
	if err != nil {
		t.Fatal(err)
	}
	if !mustChange {
		t.Fatal("expected bootstrap default to force password change")
	}
	if !checkPassword(hash, defaultAdminPassword) {
		t.Fatal("bootstrap hash should match default admin password")
	}
}

func TestValidNewAdminPasswordStrength(t *testing.T) {
	if err := validNewAdminPassword("short1!A", nil); err == nil {
		t.Fatal("expected short password to fail")
	}
	if err := validNewAdminPassword("alllowercasepassword", nil); err == nil {
		t.Fatal("expected single class to fail")
	}
	if err := validNewAdminPassword("Password123!", nil); err == nil {
		t.Fatal("expected blacklisted to fail")
	}
	if err := validNewAdminPassword("Str0ng!Passw0rd#2024", nil); err != nil {
		t.Fatalf("expected strong password to pass, got %v", err)
	}
	historyPw := "Hist0ry!Str0ng#2024"
	h, _ := hashPassword(historyPw)
	if err := validNewAdminPassword(historyPw, []string{h}); err == nil {
		t.Fatal("expected history reuse to fail")
	}
}

func TestResponsesAliasUsesAPIAuthentication(t *testing.T) {
	t.Setenv("M365_ADMIN_PASSWORD", "Str0ng!Passw0rd#2024")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", t.TempDir()+"/admin-password")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	s.Routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader("{}")))

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
	if strings.Contains(recorder.Body.String(), "administrator login required") {
		t.Fatalf("responses alias entered administrator middleware: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "valid API key required") {
		t.Fatalf("responses alias did not use API authentication: %s", recorder.Body.String())
	}
}

func TestAdminPasswordHistoryBcrypt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_ADMIN_PASSWORD_FILE", dir+"/admin-password")
	t.Setenv("M365_ADMIN_PASSWORD", "Init!Str0ng#2024A")
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_DATA_DIR", "")
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ts, c := adminTestClient(t, s.Routes())
	r := postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"Init!Str0ng#2024A"}`)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("login=%d", r.StatusCode)
	}
	newPw := "N3w!Str0ng#Pass2025B"
	r = postJSON(t, c, ts.URL+"/api/admin/change-password", `{"current_password":"Init!Str0ng#2024A","new_password":"`+newPw+`"}`)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("change=%d", r.StatusCode)
	}
	r = postJSON(t, c, ts.URL+"/api/admin/login", `{"password":"`+newPw+`"}`)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("re-login=%d", r.StatusCode)
	}
	r = postJSON(t, c, ts.URL+"/api/admin/change-password", `{"current_password":"`+newPw+`","new_password":"Init!Str0ng#2024A"}`)
	defer r.Body.Close()
	if r.StatusCode != 400 {
		t.Fatalf("history reuse should be 400, got %d", r.StatusCode)
	}
}
