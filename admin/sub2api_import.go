package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Scopes supported by the sub2api import
const (
	sub2apiScopeAvailable = "available" // healthy and not rate limited
	sub2apiScopeHealthy   = "healthy"   // healthy (including rate limited)
	sub2apiScopeAll       = "all"       // all
)

type sub2apiCredentialRequest struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

type sub2apiImportRequest struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Scope   string `json:"scope"`
}

// sub2api server response envelope: { code, message, data }
type sub2apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// Account entry returned by the sub2api accounts/data endpoint (only the fields we care about)
type sub2apiDataAccount struct {
	Name        string                 `json:"name"`
	Platform    string                 `json:"platform"`
	Type        string                 `json:"type"`
	Credentials map[string]interface{} `json:"credentials"`
	Extra       map[string]interface{} `json:"extra,omitempty"`
}

type sub2apiDataPayload struct {
	ExportedAt string               `json:"exported_at"`
	Accounts   []sub2apiDataAccount `json:"accounts"`
}

// Account status entry returned by the paginated sub2api list endpoint
type sub2apiListAccount struct {
	ID               int64                  `json:"id"`
	Name             string                 `json:"name"`
	Platform         string                 `json:"platform"`
	Status           string                 `json:"status"`
	ErrorMessage     string                 `json:"error_message"`
	RateLimitedAt    *time.Time             `json:"rate_limited_at"`
	RateLimitResetAt *time.Time             `json:"rate_limit_reset_at"`
	Credentials      map[string]interface{} `json:"credentials"`
}

type sub2apiPaginatedItems struct {
	Items    []sub2apiListAccount `json:"items"`
	Total    int64                `json:"total"`
	Page     int                  `json:"page"`
	PageSize int                  `json:"page_size"`
}

// sub2apiAccountSummary is the merged view used for the frontend preview
type sub2apiAccountSummary struct {
	Name             string `json:"name"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	Email            string `json:"email"`
	PlanType         string `json:"plan_type"`
	Status           string `json:"status"`
	ErrorMessage     string `json:"error_message,omitempty"`
	RateLimited      bool   `json:"rate_limited"`
	Available        bool   `json:"available"`
	Healthy          bool   `json:"healthy"`
}

type sub2apiPreviewResponse struct {
	Total            int                     `json:"total"`
	OpenAITotal      int                     `json:"openai_total"`
	AvailableCount   int                     `json:"available_count"`
	HealthyCount     int                     `json:"healthy_count"`
	RateLimitedCount int                     `json:"rate_limited_count"`
	ErrorCount       int                     `json:"error_count"`
	OtherPlatform    int                     `json:"other_platform"`
	Accounts         []sub2apiAccountSummary `json:"accounts"`
}

// PreviewSub2APIAccounts fetches the sub2api account status and returns a preview.
// Credentials are used only for this request and are never written to disk.
func (h *Handler) PreviewSub2APIAccounts(c *gin.Context) {
	var req sub2apiCredentialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "failed to parse request body")
		return
	}
	baseURL, apiKey, errMsg := normalizeSub2APICreds(req.BaseURL, req.APIKey)
	if errMsg != "" {
		writeError(c, http.StatusBadRequest, errMsg)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	summaries, statErr := fetchSub2APISummaries(ctx, baseURL, apiKey)
	if statErr != nil {
		writeError(c, http.StatusBadGateway, statErr.Error())
		return
	}

	resp := summarizeSub2API(summaries)
	c.JSON(http.StatusOK, resp)
}

// ImportFromSub2API fetches accounts from sub2api and imports them into this system according to scope.
// Reuses importAccountsCommon logic such as SSE progress streaming and chatgpt_account_id deduplication.
func (h *Handler) ImportFromSub2API(c *gin.Context) {
	var req sub2apiImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "failed to parse request body")
		return
	}
	baseURL, apiKey, errMsg := normalizeSub2APICreds(req.BaseURL, req.APIKey)
	if errMsg != "" {
		writeError(c, http.StatusBadRequest, errMsg)
		return
	}
	scope := strings.ToLower(strings.TrimSpace(req.Scope))
	if scope == "" {
		scope = sub2apiScopeAvailable
	}
	if scope != sub2apiScopeAvailable && scope != sub2apiScopeHealthy && scope != sub2apiScopeAll {
		writeError(c, http.StatusBadRequest, "scope must be available / healthy / all")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	summaries, fetchErr := fetchSub2APISummaries(ctx, baseURL, apiKey)
	cancel()
	if fetchErr != nil {
		writeError(c, http.StatusBadGateway, fetchErr.Error())
		return
	}

	tokens := make([]importToken, 0, len(summaries))
	for _, s := range summaries {
		if s.Platform != "openai" {
			continue
		}
		if !sub2apiScopeMatches(scope, s.Status, s.RateLimited) {
			continue
		}
		tok, ok := sub2apiAccountToImportToken(s)
		if !ok {
			continue
		}
		tokens = append(tokens, tok)
	}

	if len(tokens) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"message":   "no importable accounts match the scope",
			"success":   0,
			"duplicate": 0,
			"failed":    0,
			"total":     0,
		})
		return
	}

	h.importAccountsCommon(c, tokens, importSettings{})
}

// sub2apiAccountInternal is the intermediate structure aggregated by the backend, containing credentials + status.
type sub2apiAccountInternal struct {
	Name             string
	Platform         string
	ChatGPTAccountID string
	Email            string
	PlanType         string
	Status           string
	ErrorMessage     string
	RateLimited      bool
	Credentials      map[string]interface{}
}

// fetchSub2APISummaries calls both the list endpoint (for status) and the data endpoint (for plaintext credentials), and merges them by chatgpt_account_id.
func fetchSub2APISummaries(ctx context.Context, baseURL, apiKey string) ([]sub2apiAccountInternal, error) {
	listAccounts, err := sub2apiFetchAllList(ctx, baseURL, apiKey)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch sub2api account list: %w", err)
	}

	dataPayload, err := sub2apiFetchData(ctx, baseURL, apiKey)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch sub2api plaintext credentials: %w", err)
	}

	// list is keyed by ID; data has no status. We match them by (name, platform),
	// because the name is unique within a platform in sub2api.
	type listKey struct {
		name     string
		platform string
	}
	listIndex := make(map[listKey]sub2apiListAccount, len(listAccounts))
	for _, a := range listAccounts {
		listIndex[listKey{a.Name, a.Platform}] = a
	}

	now := time.Now()
	out := make([]sub2apiAccountInternal, 0, len(dataPayload.Accounts))
	for _, dataAcc := range dataPayload.Accounts {
		merged := sub2apiAccountInternal{
			Name:        dataAcc.Name,
			Platform:    strings.ToLower(strings.TrimSpace(dataAcc.Platform)),
			Credentials: dataAcc.Credentials,
		}
		if dataAcc.Credentials != nil {
			merged.ChatGPTAccountID = stringFromMap(dataAcc.Credentials, "chatgpt_account_id", "account_id")
			merged.Email = stringFromMap(dataAcc.Credentials, "email")
			merged.PlanType = stringFromMap(dataAcc.Credentials, "plan_type")
		}

		if listAcc, ok := listIndex[listKey{dataAcc.Name, dataAcc.Platform}]; ok {
			merged.Status = listAcc.Status
			merged.ErrorMessage = listAcc.ErrorMessage
			if listAcc.RateLimitResetAt != nil && now.Before(*listAcc.RateLimitResetAt) {
				merged.RateLimited = true
			}
		}
		out = append(out, merged)
	}
	return out, nil
}

// summarizeSub2API builds the preview statistics and list for the frontend from the aggregated data.
func summarizeSub2API(accounts []sub2apiAccountInternal) sub2apiPreviewResponse {
	resp := sub2apiPreviewResponse{
		Accounts: make([]sub2apiAccountSummary, 0, len(accounts)),
	}
	resp.Total = len(accounts)
	for _, a := range accounts {
		if a.Platform != "openai" {
			resp.OtherPlatform++
			continue
		}
		resp.OpenAITotal++
		healthy := strings.EqualFold(a.Status, "active")
		available := healthy && !a.RateLimited
		switch {
		case healthy && a.RateLimited:
			resp.RateLimitedCount++
		case healthy:
			resp.AvailableCount++
		default:
			resp.ErrorCount++
		}
		if healthy {
			resp.HealthyCount++
		}
		resp.Accounts = append(resp.Accounts, sub2apiAccountSummary{
			Name:             a.Name,
			ChatGPTAccountID: a.ChatGPTAccountID,
			Email:            a.Email,
			PlanType:         a.PlanType,
			Status:           a.Status,
			ErrorMessage:     a.ErrorMessage,
			RateLimited:      a.RateLimited,
			Available:        available,
			Healthy:          healthy,
		})
	}
	return resp
}

func sub2apiScopeMatches(scope, status string, rateLimited bool) bool {
	healthy := strings.EqualFold(status, "active")
	switch scope {
	case sub2apiScopeAvailable:
		return healthy && !rateLimited
	case sub2apiScopeHealthy:
		return healthy
	case sub2apiScopeAll:
		return true
	}
	return false
}

func sub2apiAccountToImportToken(a sub2apiAccountInternal) (importToken, bool) {
	c := a.Credentials
	if c == nil {
		return importToken{}, false
	}
	rt := stringFromMap(c, "refresh_token")
	at := stringFromMap(c, "access_token")
	st := stringFromMap(c, "session_token", "sessionToken")
	if rt == "" && at == "" && st == "" {
		return importToken{}, false
	}
	tok := importToken{
		refreshToken:          rt,
		sessionToken:          st,
		accessToken:           at,
		name:                  a.Name,
		email:                 a.Email,
		idToken:               stringFromMap(c, "id_token"),
		accountID:             stringFromMap(c, "account_id"),
		chatgptAccountID:      a.ChatGPTAccountID,
		planType:              a.PlanType,
		expiresAt:             stringFromMap(c, "expires_at"),
		codex7DUsedPercent:    stringFromMap(c, "codex_7d_used_percent"),
		codex7DResetAt:        stringFromMap(c, "codex_7d_reset_at"),
		codex5HUsedPercent:    stringFromMap(c, "codex_5h_used_percent"),
		codex5HResetAt:        stringFromMap(c, "codex_5h_reset_at"),
		codex5HUsageUpdatedAt: stringFromMap(c, "codex_5h_usage_updated_at"),
		codexUsageUpdatedAt:   stringFromMap(c, "codex_usage_updated_at"),
	}
	if tok.name == "" {
		tok.name = tok.email
	}
	return tok, true
}

// sub2apiFetchAllList fetches the full paginated list (with the status field).
func sub2apiFetchAllList(ctx context.Context, baseURL, apiKey string) ([]sub2apiListAccount, error) {
	var all []sub2apiListAccount
	const pageSize = 200
	for page := 1; page <= 50; page++ { // hard guard for the 10,000-account limit
		u := fmt.Sprintf("%s/api/v1/admin/accounts?page=%d&page_size=%d&platform=openai", baseURL, page, pageSize)
		var env sub2apiEnvelope
		if err := sub2apiHTTPGet(ctx, u, apiKey, &env); err != nil {
			return nil, err
		}
		var paged sub2apiPaginatedItems
		if len(env.Data) == 0 || string(env.Data) == "null" {
			break
		}
		if err := json.Unmarshal(env.Data, &paged); err != nil {
			return nil, fmt.Errorf("failed to parse sub2api list response: %w", err)
		}
		all = append(all, paged.Items...)
		if int64(len(all)) >= paged.Total || len(paged.Items) == 0 {
			break
		}
	}
	return all, nil
}

// sub2apiFetchData fetches /accounts/data, including plaintext credentials.
func sub2apiFetchData(ctx context.Context, baseURL, apiKey string) (sub2apiDataPayload, error) {
	u := baseURL + "/api/v1/admin/accounts/data?platform=openai"
	var env sub2apiEnvelope
	if err := sub2apiHTTPGet(ctx, u, apiKey, &env); err != nil {
		return sub2apiDataPayload{}, err
	}
	var payload sub2apiDataPayload
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return payload, nil
	}
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		return sub2apiDataPayload{}, fmt.Errorf("failed to parse sub2api data response: %w", err)
	}
	return payload, nil
}

// sub2apiHTTPGet sends a GET request and parses the response into an envelope.
func sub2apiHTTPGet(ctx context.Context, fullURL, apiKey string, out *sub2apiEnvelope) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call sub2api: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("sub2api authentication failed (401), please check the admin api key")
	}
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("sub2api access denied (403)")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("sub2api returned HTTP %d: %s", resp.StatusCode, truncateString(string(body), 200))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("failed to parse sub2api response: %w", err)
	}
	if out.Code != 0 {
		return fmt.Errorf("sub2api error: %s", out.Message)
	}
	return nil
}

func normalizeSub2APICreds(rawBaseURL, rawAPIKey string) (string, string, string) {
	baseURL := strings.TrimSpace(rawBaseURL)
	apiKey := strings.TrimSpace(rawAPIKey)
	if baseURL == "" {
		return "", "", "please enter the sub2api base_url"
	}
	if apiKey == "" {
		return "", "", "please enter the sub2api admin api key"
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return "", "", "base_url must start with http:// or https://"
	}
	if parsed, err := url.Parse(baseURL); err != nil || parsed.Host == "" {
		return "", "", "base_url is not a valid URL"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return baseURL, apiKey, ""
}

func stringFromMap(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch s := v.(type) {
		case string:
			if t := strings.TrimSpace(s); t != "" {
				return t
			}
		case float64:
			return fmt.Sprintf("%v", s)
		case int64:
			return fmt.Sprintf("%d", s)
		case bool:
			if s {
				return "true"
			}
			return "false"
		}
	}
	return ""
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
