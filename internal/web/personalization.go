package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"m365-copilot2api/internal/auth"
)

const personalizationUserFlagsPath = "/m365Copilot/PersonalizationUserFlags?variants=feature.EnablePersonalization"

type personalizationFlags struct {
	IsMemoryEnabled                          bool `json:"isMemoryEnabled"`
	IsCustomInstructionEnabled               bool `json:"isCustomInstructionEnabled"`
	IsPersonalizationEnabledByTenant         bool `json:"isPersonalizationEnabledByTenant"`
	IsInsightsFromConversationHistoryEnabled bool `json:"isInsightsFromConversationHistoryEnabled"`
}

func doSubstrateJSON(ctx context.Context, method, targetURL string, acc auth.AccountToken, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, targetURL, body)
	if err != nil {
		return err
	}
	req.Header = substrateHeaders(acc)
	resp, err := substrateHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("substrate endpoint returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("parse substrate response: %w", err)
		}
	}
	return nil
}

func setPersonalizationMemory(ctx context.Context, acc auth.AccountToken, enabled bool) error {
	payload, _ := json.Marshal(map[string]bool{"isMemoryEnabled": enabled})
	return doSubstrateJSON(ctx, http.MethodPost, substrateBase+personalizationUserFlagsPath, acc, bytes.NewReader(payload), nil)
}

func readPersonalizationFlags(ctx context.Context, acc auth.AccountToken) (personalizationFlags, error) {
	var flags personalizationFlags
	err := doSubstrateJSON(ctx, http.MethodGet, substrateBase+personalizationUserFlagsPath, acc, nil, &flags)
	return flags, err
}

func invalidatePersonalizationFlags(acc auth.AccountToken) {
	flagsCache.Lock()
	delete(flagsCache.m, acc.ID)
	flagsCache.Unlock()
}

func disableAndVerifyMemory(ctx context.Context, acc auth.AccountToken) (personalizationFlags, error) {
	if err := setPersonalizationMemory(ctx, acc, false); err != nil {
		return personalizationFlags{}, err
	}
	invalidatePersonalizationFlags(acc)
	flags, err := readPersonalizationFlags(ctx, acc)
	if err != nil {
		return personalizationFlags{}, err
	}
	if flags.IsMemoryEnabled || flags.IsInsightsFromConversationHistoryEnabled {
		return flags, fmt.Errorf("memory disable did not take effect")
	}
	return flags, nil
}
