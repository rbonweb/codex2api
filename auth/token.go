package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OpenAI OAuth constants (same as CLIProxyAPI / sub2api)
const (
	TokenURL   = "https://auth.openai.com/oauth/token"
	SessionURL = "https://chatgpt.com/api/auth/session"
	ClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
	MaxRetries = 3
)

// ResinRequestDecorator is injected from outside (main.go) to rewrite the request URL and add headers when Resin is enabled.
// Avoids a circular dependency (auth → proxy). Parameters: (originalURL, accountIdentifier) → (newURL)
// The caller must set the X-Resin-Account header on the returned req.
var ResinRequestDecorator func(targetURL, accountID string) string

// TokenData holds the token information obtained from one RT refresh
type TokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    time.Time
}

// AccountInfo is the account information parsed from id_token
type AccountInfo struct {
	Email                 string    `json:"email"`
	ChatGPTAccountID      string    `json:"chatgpt_account_id"`
	UserID                string    `json:"user_id"`
	PlanType              string    `json:"chatgpt_plan_type"`
	SubscriptionExpiresAt time.Time `json:"-"`
}

// RefreshAccessToken exchanges an RT for an AT
// resinAccountID is optional; when Resin is enabled, pass the account identifier for sticky proxying
func RefreshAccessToken(ctx context.Context, refreshToken string, proxyURL string, resinAccountID ...string) (*TokenData, *AccountInfo, error) {
	data, err := json.Marshal(map[string]string{
		"grant_type":     "refresh_token",
		"client_id":      ClientID,
		"refresh_token":  refreshToken,
		"refresh_reason": codexOAuthRefreshReason(ctx),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encode refresh request: %w", err)
	}

	// Resin reverse-proxy mode: rewrite the URL
	targetURL := TokenURL
	accountID := ""
	if len(resinAccountID) > 0 {
		accountID = resinAccountID[0]
	}
	if ResinRequestDecorator != nil && accountID != "" {
		targetURL = ResinRequestDecorator(TokenURL, accountID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	applyCodexOAuthIdentity(req)

	// Resin reverse proxy: inject the account identity header
	if ResinRequestDecorator != nil && accountID != "" {
		req.Header.Set("X-Resin-Account", accountID)
	}

	// In Resin reverse-proxy mode, use a standard HTTP client (no proxy; Resin handles routing)
	var client *http.Client
	if ResinRequestDecorator != nil && accountID != "" {
		client = &http.Client{Timeout: 30 * time.Second}
	} else {
		client = buildUTLSHTTPClient(proxyURL)
	}
	resp, err := client.Do(req)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return nil, nil, fmt.Errorf("refresh connection failed: %w", err)
		}
		return nil, nil, &oauthRefreshUncertainError{fmt.Errorf("refresh request result is unknown: %w", err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, &oauthRefreshUncertainError{fmt.Errorf("failed to read refresh response: %w", err)}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, nil, newOAuthRefreshHTTPError(resp.StatusCode, body, refreshToken)
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, nil, &oauthRefreshUncertainError{fmt.Errorf("failed to parse refresh response: %w", err)}
	}
	tokenResp.AccessToken = strings.TrimSpace(tokenResp.AccessToken)
	tokenResp.RefreshToken = strings.TrimSpace(tokenResp.RefreshToken)
	tokenResp.IDToken = strings.TrimSpace(tokenResp.IDToken)
	if tokenResp.AccessToken == "" {
		return nil, nil, &oauthRefreshUncertainError{fmt.Errorf("refresh response is missing access_token")}
	}
	if tokenResp.ExpiresIn <= 0 {
		tokenResp.ExpiresIn = 3600
	}

	td := &TokenData{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		IDToken:      tokenResp.IDToken,
		ExpiresIn:    tokenResp.ExpiresIn,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}

	// Keep the new RT; if it comes back empty, keep the old one
	if strings.TrimSpace(td.RefreshToken) == "" {
		td.RefreshToken = refreshToken
	}

	// Parse id_token to get the account information
	info := parseIDToken(tokenResp.IDToken)

	// Fallback: if fields are missing from id_token, try to extract them from access_token
	if tokenResp.AccessToken != "" && (info.PlanType == "" || info.SubscriptionExpiresAt.IsZero()) {
		if atInfo := ParseAccessToken(tokenResp.AccessToken); atInfo != nil {
			if info.PlanType == "" && atInfo.PlanType != "" {
				log.Printf("[token] id_token is missing plan_type, falling back to fetch it from access_token: %s", atInfo.PlanType)
				info.PlanType = atInfo.PlanType
			}
			// Also fill in the other empty fields from the fallback
			if info.Email == "" && atInfo.Email != "" {
				info.Email = atInfo.Email
			}
			if info.ChatGPTAccountID == "" && atInfo.ChatGPTAccountID != "" {
				info.ChatGPTAccountID = atInfo.ChatGPTAccountID
			}
			if info.SubscriptionExpiresAt.IsZero() && !atInfo.SubscriptionExpiresAt.IsZero() {
				info.SubscriptionExpiresAt = atInfo.SubscriptionExpiresAt
			}
		}
	}

	return td, info, nil
}

// RefreshWithRetry refreshes the RT with retries
// resinAccountID is optional; when Resin is enabled, pass the account identifier
func RefreshWithRetry(ctx context.Context, refreshToken string, proxyURL string, resinAccountID ...string) (*TokenData, *AccountInfo, error) {
	var lastErr error
	for attempt := 0; attempt < MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		td, info, err := RefreshAccessToken(ctx, refreshToken, proxyURL, resinAccountID...)
		if err == nil {
			return td, info, nil
		}

		// Non-retryable errors are returned immediately
		if isNonRetryable(err) || isOAuthRefreshUncertain(err) {
			return nil, nil, err
		}
		lastErr = err
	}
	return nil, nil, fmt.Errorf("refresh failed after %d retries: %w", MaxRetries, lastErr)
}

// RefreshWithSessionToken exchanges the ChatGPT Web session_token for a new AT as a fallback.
func RefreshWithSessionToken(ctx context.Context, sessionToken string, proxyURL string, resinAccountID ...string) (*TokenData, *AccountInfo, error) {
	sessionToken = strings.TrimSpace(sessionToken)
	if sessionToken == "" {
		return nil, nil, fmt.Errorf("session_token is empty")
	}

	targetURL := SessionURL
	accountID := ""
	if len(resinAccountID) > 0 {
		accountID = resinAccountID[0]
	}
	if ResinRequestDecorator != nil && accountID != "" {
		targetURL = ResinRequestDecorator(SessionURL, accountID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create session request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(&http.Cookie{Name: "__Secure-next-auth.session-token", Value: sessionToken})
	if ResinRequestDecorator != nil && accountID != "" {
		req.Header.Set("X-Resin-Account", accountID)
	}

	var client *http.Client
	if ResinRequestDecorator != nil && accountID != "" {
		client = &http.Client{Timeout: 30 * time.Second}
	} else {
		client = buildUTLSHTTPClient(proxyURL)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("session refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read session response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("session refresh failed (status %d): %s", resp.StatusCode, string(body))
	}

	var sessionResp struct {
		AccessToken string `json:"accessToken"`
		Expires     string `json:"expires"`
		User        struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &sessionResp); err != nil {
		return nil, nil, fmt.Errorf("failed to parse session response: %w", err)
	}
	accessToken := strings.TrimSpace(sessionResp.AccessToken)
	if accessToken == "" {
		return nil, nil, fmt.Errorf("session response is missing accessToken")
	}

	expiresAt := parseSessionExpiresAt(sessionResp.Expires)
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(time.Hour)
	}
	td := &TokenData{
		AccessToken: accessToken,
		ExpiresIn:   int64(time.Until(expiresAt).Seconds()),
		ExpiresAt:   expiresAt,
	}
	if td.ExpiresIn <= 0 {
		td.ExpiresIn = 3600
		td.ExpiresAt = time.Now().Add(time.Hour)
	}

	info := &AccountInfo{}
	if atInfo := ParseAccessToken(accessToken); atInfo != nil {
		info.Email = atInfo.Email
		info.ChatGPTAccountID = atInfo.ChatGPTAccountID
		info.PlanType = atInfo.PlanType
		info.SubscriptionExpiresAt = atInfo.SubscriptionExpiresAt
	}
	if info.Email == "" {
		info.Email = strings.TrimSpace(sessionResp.User.Email)
	}
	return td, info, nil
}

func RefreshWithSessionTokenRetry(ctx context.Context, sessionToken string, proxyURL string, resinAccountID ...string) (*TokenData, *AccountInfo, error) {
	var lastErr error
	for attempt := 0; attempt < MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
		td, info, err := RefreshWithSessionToken(ctx, sessionToken, proxyURL, resinAccountID...)
		if err == nil {
			return td, info, nil
		}
		lastErr = err
	}
	return nil, nil, fmt.Errorf("session refresh failed after %d retries: %w", MaxRetries, lastErr)
}

func parseSessionExpiresAt(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// IsPermanentRefreshFailure reports whether an RT or session refresh can no longer be recovered.
// Retrying this kind of error still cannot yield an AT, so the account should be marked unauthorized and must not stay in "refreshing".
func IsPermanentRefreshFailure(err error) bool {
	return isNonRetryable(err)
}

type permanentRefreshFailureClassifier interface {
	PermanentRefreshFailure() bool
}

type oauthRefreshUncertainError struct{ err error }

func (e *oauthRefreshUncertainError) Error() string { return e.err.Error() }
func (e *oauthRefreshUncertainError) Unwrap() error { return e.err }

func isOAuthRefreshUncertain(err error) bool {
	var uncertain *oauthRefreshUncertainError
	return errors.As(err, &uncertain)
}

type oauthRefreshHTTPError struct {
	status int
	code   string
	body   string
}

func newOAuthRefreshHTTPError(status int, body []byte, rt string) error {
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	var code string
	if json.Unmarshal(body, &payload) == nil {
		if json.Unmarshal(payload.Error, &code) != nil {
			var detail struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(payload.Error, &detail) == nil {
				code = detail.Code
			}
		}
	}
	return &oauthRefreshHTTPError{status: status, code: strings.ToLower(strings.TrimSpace(code)), body: strings.ReplaceAll(string(body), rt, "[REDACTED]")}
}

func (e *oauthRefreshHTTPError) Error() string {
	return fmt.Sprintf("refresh failed (status %d): %s", e.status, e.body)
}
func (e *oauthRefreshHTTPError) PermanentRefreshFailure() bool {
	if e.code != "" {
		return permanentOAuthRefreshCode(e.code)
	}
	return isNonRetryable(errors.New(e.body))
}

func permanentOAuthRefreshCode(code string) bool {
	switch code {
	case "invalid_grant", "invalid_client", "unauthorized_client", "access_denied", "refresh_token_reused", "refresh_token_invalidated", "refresh_token_expired", "refresh_token_revoked", "token_invalidated":
		return true
	}
	return false
}

// isNonRetryable reports whether an auth error is not retryable
func isNonRetryable(err error) bool {
	if err == nil {
		return false
	}
	var classified permanentRefreshFailureClassifier
	if errors.As(err, &classified) {
		return classified.PermanentRefreshFailure()
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"invalid_grant", "invalid_client", "unauthorized_client", "access_denied",
		"refresh_token_reused", "refresh_token_invalidated", "refresh_token_expired", "refresh_token_revoked", "token_invalidated",
		"session has ended",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// parseIDToken parses the payload of the JWT id_token (signature not verified)
func parseIDToken(idToken string) *AccountInfo {
	if idToken == "" {
		return &AccountInfo{}
	}

	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return &AccountInfo{}
	}

	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}

	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return &AccountInfo{}
		}
	}

	var claims struct {
		Email      string `json:"email"`
		OpenAIAuth *struct {
			ChatGPTAccountID               string `json:"chatgpt_account_id"`
			UserID                         string `json:"user_id"`
			ChatGPTUserID                  string `json:"chatgpt_user_id"`
			PlanType                       string `json:"chatgpt_plan_type"`
			ChatGPTSubscriptionActiveUntil string `json:"chatgpt_subscription_active_until"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return &AccountInfo{}
	}

	info := &AccountInfo{Email: claims.Email}
	if claims.OpenAIAuth != nil {
		info.ChatGPTAccountID = claims.OpenAIAuth.ChatGPTAccountID
		info.UserID = firstNonEmptyTrimmed(claims.OpenAIAuth.UserID, claims.OpenAIAuth.ChatGPTUserID)
		info.PlanType = claims.OpenAIAuth.PlanType
		if s := claims.OpenAIAuth.ChatGPTSubscriptionActiveUntil; s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				info.SubscriptionExpiresAt = t
			}
		}
	}
	return info
}

// AccessTokenInfo is the result of parsing the AT JWT
type AccessTokenInfo struct {
	Email                 string
	ChatGPTAccountID      string
	UserID                string
	PlanType              string
	ExpiresAt             time.Time
	SubscriptionExpiresAt time.Time
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ParseAccessToken parses the JWT payload of the Access Token (signature not verified)
// The AT's email is under https://api.openai.com/profile, which differs from id_token
func ParseAccessToken(accessToken string) *AccessTokenInfo {
	if accessToken == "" {
		return nil
	}

	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return nil
	}

	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}

	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil
		}
	}

	var claims struct {
		Exp        int64 `json:"exp"`
		OpenAIAuth *struct {
			ChatGPTAccountID               string `json:"chatgpt_account_id"`
			UserID                         string `json:"user_id"`
			ChatGPTUserID                  string `json:"chatgpt_user_id"`
			PlanType                       string `json:"chatgpt_plan_type"`
			ChatGPTSubscriptionActiveUntil string `json:"chatgpt_subscription_active_until"`
		} `json:"https://api.openai.com/auth"`
		OpenAIProfile *struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return nil
	}

	info := &AccessTokenInfo{}
	if claims.OpenAIProfile != nil {
		info.Email = claims.OpenAIProfile.Email
	}
	if claims.OpenAIAuth != nil {
		info.ChatGPTAccountID = claims.OpenAIAuth.ChatGPTAccountID
		info.UserID = firstNonEmptyTrimmed(claims.OpenAIAuth.UserID, claims.OpenAIAuth.ChatGPTUserID)
		info.PlanType = claims.OpenAIAuth.PlanType
		if s := claims.OpenAIAuth.ChatGPTSubscriptionActiveUntil; s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				info.SubscriptionExpiresAt = t
			}
		}
	}
	if claims.Exp > 0 {
		info.ExpiresAt = time.Unix(claims.Exp, 0)
	}
	return info
}

// authClientPool is the connection pool for auth requests (grouped by proxyURL, with TTL cleanup)
var authClientPool sync.Map // map[string]*authPoolEntry

type authPoolEntry struct {
	client   *http.Client
	lastUsed atomic.Int64
}

func (e *authPoolEntry) touch() {
	e.lastUsed.Store(time.Now().UnixNano())
}

const (
	authClientPoolTTL             = 5 * time.Minute
	authClientPoolCleanupInterval = 60 * time.Second
)

// authClientPoolStop stops the cleanup goroutine (tests can call it to avoid a goroutine leak)
var authClientPoolStop = make(chan struct{})

func init() {
	go func() {
		ticker := time.NewTicker(authClientPoolCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				evictExpiredAuthClients()
			case <-authClientPoolStop:
				return
			}
		}
	}()
}

func evictExpiredAuthClients() {
	cutoff := time.Now().Add(-authClientPoolTTL).UnixNano()
	authClientPool.Range(func(key, value any) bool {
		entry := value.(*authPoolEntry)
		if entry.lastUsed.Load() < cutoff {
			authClientPool.Delete(key)
			entry.client.CloseIdleConnections()
		}
		return true
	})
}

// buildHTTPClient builds a proxy-capable HTTP client (connection pool reuse, with TTL cleanup)
func buildHTTPClient(proxyURL string) *http.Client {
	client, _ := buildHTTPClientChecked(proxyURL, false)
	return client
}

// buildHTTPClientChecked constructs a pooled auth client. When strict is true,
// an invalid proxy configuration is returned to the caller instead of silently
// falling back to a direct connection. Existing callers retain the historical
// best-effort behavior through buildHTTPClient.
func buildHTTPClientChecked(proxyURL string, strict bool) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if strict && proxyURL != "" {
		probe := &http.Transport{}
		if err := ConfigureTransportProxy(probe, proxyURL, &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}); err != nil {
			return nil, fmt.Errorf("configure proxy: %w", err)
		}
		probe.CloseIdleConnections()
	}
	if v, ok := authClientPool.Load(proxyURL); ok {
		entry := v.(*authPoolEntry)
		entry.touch()
		return entry.client, nil
	}

	transport := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		MaxConnsPerHost:     20,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	baseDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = baseDialer.DialContext
	if proxyURL != "" {
		if err := ConfigureTransportProxy(transport, proxyURL, baseDialer); err != nil {
			if strict {
				return nil, fmt.Errorf("configure proxy: %w", err)
			}
			transport.Proxy = nil
			transport.DialContext = baseDialer.DialContext
		}
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	entry := &authPoolEntry{client: client}
	entry.touch()

	if v, loaded := authClientPool.LoadOrStore(proxyURL, entry); loaded {
		e := v.(*authPoolEntry)
		e.touch()
		return e.client, nil
	}
	return client, nil
}

// BuildHTTPClient builds a proxy-aware HTTP client (exported for admin OAuth flow).
func BuildHTTPClient(proxyURL string) *http.Client {
	return buildHTTPClient(proxyURL)
}

// BuildHTTPClientChecked is the strict variant used by protocol adapters that
// must never silently bypass a configured account proxy.
func BuildHTTPClientChecked(proxyURL string) (*http.Client, error) {
	return buildHTTPClientChecked(proxyURL, true)
}

// ParseIDToken parses a JWT id_token payload (exported for admin OAuth flow).
func ParseIDToken(idToken string) *AccountInfo {
	return parseIDToken(idToken)
}

// HashAccountID generates a short hash from account_id (for logs)
func HashAccountID(accountID string) string {
	if accountID == "" {
		return ""
	}
	h := sha256.Sum256([]byte(accountID))
	return fmt.Sprintf("%x", h[:4])
}
