package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"m365-copilot2api/internal/auth"
)

const maxGraphBatchUsers = 20

var (
	graphDomainPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	graphPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

type graphConfig struct {
	TenantID      string
	ClientID      string
	ClientSecret  string
	BaseURL       string
	TokenURL      string
	Authorization *graphAuthorizationData
}

type graphReadiness struct {
	Ready                         bool     `json:"ready"`
	Authorized                    bool     `json:"authorized"`
	MasterKeyConfigured           bool     `json:"masterKeyConfigured"`
	ClientConfigured              bool     `json:"clientConfigured"`
	ApplicationFallbackConfigured bool     `json:"applicationFallbackConfigured"`
	PermissionType                string   `json:"permissionType"`
	Message                       string   `json:"message"`
	MissingSteps                  []string `json:"missingSteps,omitempty"`
}

func graphAuthorizationHasRequiredScopes(scopes string) bool {
	required := []string{"User.ReadWrite.All", "Organization.Read.All", "LicenseAssignment.ReadWrite.All"}
	granted := strings.Fields(scopes)
	for _, requirement := range required {
		found := false
		for _, scope := range granted {
			if strings.EqualFold(scope, requirement) || strings.HasSuffix(strings.ToLower(scope), "/"+strings.ToLower(requirement)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func graphReadinessStatus() graphReadiness {
	status := graphReadiness{
		MasterKeyConfigured: auth.HasMasterKey(),
		ClientConfigured:    strings.TrimSpace(os.Getenv("M365_GRAPH_CLIENT_ID")) != "",
		PermissionType:      "none",
	}
	if authorization, err := loadGraphAuthorization(); err == nil {
		status.Authorized = true
		status.PermissionType = "delegated"
		if status.MasterKeyConfigured && graphAuthorizationHasRequiredScopes(authorization.Scopes) {
			status.Ready = true
			status.Message = "管理员连接已就绪，可以批量创建用户。"
			return status
		}
	}
	tenant := strings.TrimSpace(os.Getenv("M365_GRAPH_TENANT_ID"))
	client := strings.TrimSpace(os.Getenv("M365_GRAPH_CLIENT_ID"))
	secret := strings.TrimSpace(os.Getenv("M365_GRAPH_CLIENT_SECRET"))
	status.ApplicationFallbackConfigured = tenant != "" && client != "" && secret != ""
	if status.ApplicationFallbackConfigured {
		status.Ready = true
		status.PermissionType = "application"
		status.Message = "备用管理员连接已就绪，可以批量创建用户。"
		return status
	}
	if status.Authorized {
		if !status.MasterKeyConfigured {
			status.MissingSteps = []string{"请系统管理员完成安全存储设置，或启用备用管理员连接"}
			status.Message = "已保存的管理员连接暂时不可用。下一步：请系统管理员完成安全存储设置，或启用备用管理员连接。"
		} else {
			status.MissingSteps = []string{"重新连接管理员并同意创建用户和分配许可证所需权限"}
			status.Message = "管理员连接权限不足。下一步：点击“重新连接管理员”，并同意创建用户和分配许可证所需权限。"
		}
	} else {
		status.MissingSteps = []string{"请系统管理员完成安全存储设置并连接 Microsoft 管理员"}
		status.Message = "尚未完成管理员连接。下一步：请系统管理员完成安全存储设置，然后点击“连接 Microsoft 管理员”。"
	}
	return status
}

type graphBatchRequest struct {
	Domain                        string `json:"domain"`
	Prefix                        string `json:"prefix"`
	Start                         int    `json:"start"`
	Count                         int    `json:"count"`
	DisplayName                   string `json:"displayName"`
	UsageLocation                 string `json:"usageLocation"`
	SKUID                         string `json:"skuId"`
	InitialPassword               string `json:"initialPassword"`
	ForceChangePasswordNextSignIn bool   `json:"forceChangePasswordNextSignIn"`
}

type graphBatchResult struct {
	UserPrincipalName string `json:"userPrincipalName"`
	DisplayName       string `json:"displayName"`
	UserID            string `json:"userId,omitempty"`
	Success           bool   `json:"success"`
	LicenseAssigned   bool   `json:"licenseAssigned,omitempty"`
	Error             string `json:"error,omitempty"`
	OAuthStartURL     string `json:"oauthStartUrl,omitempty"`
}

func loadGraphConfig() (graphConfig, error) {
	cfg := graphConfig{
		TenantID:     strings.TrimSpace(os.Getenv("M365_GRAPH_TENANT_ID")),
		ClientID:     strings.TrimSpace(os.Getenv("M365_GRAPH_CLIENT_ID")),
		ClientSecret: strings.TrimSpace(os.Getenv("M365_GRAPH_CLIENT_SECRET")),
		BaseURL:      strings.TrimRight(strings.TrimSpace(os.Getenv("M365_GRAPH_BASE_URL")), "/"),
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://graph.microsoft.com"
	}
	if authorization, err := loadGraphAuthorization(); err == nil && auth.HasMasterKey() && graphAuthorizationHasRequiredScopes(authorization.Scopes) {
		cfg.ClientID = authorization.ClientID
		cfg.ClientSecret = ""
		cfg.TenantID = authorization.TenantID
		cfg.Authorization = &authorization
	} else if cfg.ClientID == "" {
		return graphConfig{}, fmt.Errorf("管理员连接尚未就绪")
	}
	cfg.TokenURL = strings.TrimSpace(os.Getenv("M365_GRAPH_TOKEN_URL"))
	if cfg.TokenURL == "" {
		tenant := cfg.TenantID
		if cfg.Authorization != nil {
			tenant = "organizations"
		} else if tenant == "" {
			return graphConfig{}, fmt.Errorf("备用管理员连接尚未配置完整")
		}
		cfg.TokenURL = "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
	}
	return cfg, nil
}

func graphAccessToken(ctx context.Context, cfg graphConfig) (string, error) {
	if cfg.Authorization != nil {
		stored := *cfg.Authorization
		refreshToken, err := auth.DecryptSecret(stored.EncryptedRefreshToken)
		if err != nil {
			return "", fmt.Errorf("could not read Microsoft Graph administrator authorization")
		}
		tokenEndpoint := cfg.TokenURL
		if tokenEndpoint == "" {
			tokenEndpoint = "https://login.microsoftonline.com/organizations/oauth2/v2.0/token"
		}
		token, err := auth.RefreshAt(refreshToken, stored.ClientID, stored.Scopes, tokenEndpoint)
		if err != nil {
			return "", err
		}
		if token.RefreshToken != "" && token.RefreshToken != refreshToken {
			encrypted, err := auth.EncryptSecret(token.RefreshToken)
			if err != nil {
				return "", err
			}
			stored.EncryptedRefreshToken = encrypted
		}
		stored.LastValidatedAt = time.Now()
		if err := saveGraphAuthorization(stored); err != nil {
			return "", err
		}
		return token.AccessToken, nil
	}
	if cfg.ClientSecret == "" {
		return "", fmt.Errorf("管理员连接尚未就绪")
	}
	form := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"grant_type":    {"client_credentials"},
		"scope":         {"https://graph.microsoft.com/.default"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 || body.AccessToken == "" {
		if body.ErrorDescription != "" {
			return "", fmt.Errorf("Graph authentication failed: %s", body.ErrorDescription)
		}
		return "", fmt.Errorf("Graph authentication failed: %s", body.Error)
	}
	return body.AccessToken, nil
}

func graphRequest(ctx context.Context, cfg graphConfig, token, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var graphErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&graphErr)
		if graphErr.Error.Message != "" {
			return fmt.Errorf("%s", graphErr.Error.Message)
		}
		return fmt.Errorf("Graph returned HTTP %d", resp.StatusCode)
	}
	if output != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output)
	}
	return nil
}

func (s *Server) graphConfigCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed")
		return
	}
	cfg, err := loadGraphConfig()
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "configuration_error", "err_error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	token, err := graphAccessToken(ctx, cfg)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "graph_error", "err_error", err.Error())
		return
	}
	var organization struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if err := graphRequest(ctx, cfg, token, http.MethodGet, "/v1.0/organization?$select=id,displayName", nil, &organization); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "graph_error", "err_error", err.Error())
		return
	}
	result := map[string]any{"configured": true}
	if len(organization.Value) > 0 {
		result["tenantId"] = organization.Value[0].ID
		result["organization"] = organization.Value[0].DisplayName
	}
	jsonOut(w, result)
}

func validateGraphBatchRequest(body *graphBatchRequest) error {
	body.Domain = strings.TrimSpace(body.Domain)
	body.Prefix = strings.TrimSpace(body.Prefix)
	body.DisplayName = strings.TrimSpace(body.DisplayName)
	body.UsageLocation = strings.ToUpper(strings.TrimSpace(body.UsageLocation))
	body.SKUID = strings.TrimSpace(body.SKUID)
	if !graphDomainPattern.MatchString(body.Domain) || !strings.Contains(body.Domain, ".") {
		return fmt.Errorf("valid domain required")
	}
	if !graphPrefixPattern.MatchString(body.Prefix) {
		return fmt.Errorf("valid account prefix required")
	}
	if body.Start < 0 {
		return fmt.Errorf("start must be zero or greater")
	}
	if body.Count < 1 || body.Count > maxGraphBatchUsers {
		return fmt.Errorf("count must be between 1 and %d", maxGraphBatchUsers)
	}
	if body.DisplayName == "" || len(body.DisplayName) > 128 {
		return fmt.Errorf("displayName is required and must not exceed 128 characters")
	}
	if len(body.UsageLocation) != 2 {
		return fmt.Errorf("usageLocation must be a two-letter country or region code")
	}
	if len(body.InitialPassword) < 8 || len(body.InitialPassword) > 256 {
		return fmt.Errorf("initialPassword must contain 8 to 256 characters")
	}
	return nil
}

func (s *Server) graphBatchUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed")
		return
	}
	var body graphBatchRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request_body", "invalid request body")
		return
	}
	if err := validateGraphBatchRequest(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "err_error", err.Error())
		return
	}
	readiness := graphReadinessStatus()
	if !readiness.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		jsonOut(w, map[string]any{"error": map[string]any{"type": "configuration_error", "message": readiness.Message, "readiness": readiness}})
		return
	}
	cfg, err := loadGraphConfig()
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "configuration_error", "err_error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	token, err := graphAccessToken(ctx, cfg)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "graph_error", "err_error", err.Error())
		return
	}
	results := make([]graphBatchResult, 0, body.Count)
	for i := 0; i < body.Count; i++ {
		number := body.Start + i
		mailNickname := body.Prefix + strconv.Itoa(number)
		upn := mailNickname + "@" + body.Domain
		displayName := body.DisplayName + " " + strconv.Itoa(number)
		result := graphBatchResult{UserPrincipalName: upn, DisplayName: displayName}
		create := map[string]any{
			"accountEnabled":    true,
			"displayName":       displayName,
			"mailNickname":      mailNickname,
			"userPrincipalName": upn,
			"usageLocation":     body.UsageLocation,
			"passwordProfile": map[string]any{
				"password":                      body.InitialPassword,
				"forceChangePasswordNextSignIn": body.ForceChangePasswordNextSignIn,
			},
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := graphRequest(ctx, cfg, token, http.MethodPost, "/v1.0/users", create, &created); err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		result.Success = true
		result.UserID = created.ID
		result.OAuthStartURL = "/api/auth/start"
		if body.SKUID != "" {
			assign := map[string]any{
				"addLicenses":    []map[string]any{{"skuId": body.SKUID, "disabledPlans": []string{}}},
				"removeLicenses": []string{},
			}
			if err := graphRequest(ctx, cfg, token, http.MethodPost, "/v1.0/users/"+url.PathEscape(created.ID)+"/assignLicense", assign, nil); err != nil {
				result.Success = false
				result.Error = "user created but license assignment failed: " + err.Error()
			} else {
				result.LicenseAssigned = true
			}
		}
		results = append(results, result)
	}
	jsonOut(w, map[string]any{"results": results})
}
