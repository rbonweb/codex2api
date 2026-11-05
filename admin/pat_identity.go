package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

const (
	openAIAuthAccountsBaseURL    = "https://auth.openai.com/api/accounts"
	personalAccessTokenWhoAmIURL = openAIAuthAccountsBaseURL + "/v1/user-auth-credential/whoami"
	patWhoAmITimeout             = 10 * time.Second
)

// patWhoAmIURLForTest lets tests replace the whoami endpoint URL. Production code must not assign it.
var patWhoAmIURLForTest = ""

// errPATWhoAmINoWorkspace means whoami succeeded but did not return a workspace ID.
var errPATWhoAmINoWorkspace = errors.New("whoami response has no chatgpt_account_id")

// PersonalAccessTokenMetadata is the identity metadata structure returned by /v1/user-auth-credential/whoami.
type PersonalAccessTokenMetadata struct {
	Email                   *string `json:"email"`
	ChatGPTUserID           string  `json:"chatgpt_user_id"`
	ChatGPTAccountID        string  `json:"chatgpt_account_id"`
	ChatGPTPlanType         string  `json:"chatgpt_plan_type"`
	ChatGPTAccountIsFedramp bool    `json:"chatgpt_account_is_fedramp"`
}

// patWhoAmIStatusError means the whoami endpoint returned a non-2xx status: the problem is with this token itself,
// (no permission / revoked), not a network failure, so a batch import must not trip the circuit breaker for the remaining tokens because of it.
type patWhoAmIStatusError struct {
	StatusCode int
}

func (e *patWhoAmIStatusError) Error() string {
	return fmt.Sprintf("whoami returned status %d", e.StatusCode)
}

// newPATWhoAmIClient reuses the strict client pooled per proxy (connection reuse; a proxy configuration failure is not silently bypassed),
// and only overrides the timeout and redirect policy: a request carrying a Bearer PAT must never automatically follow a redirect to another host.
func newPATWhoAmIClient(proxyURL string) (*http.Client, error) {
	base, err := auth.BuildHTTPClientChecked(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL for whoami: %w", err)
	}
	client := *base
	client.Timeout = patWhoAmITimeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client, nil
}

// QueryPersonalAccessTokenMetadata calls /v1/user-auth-credential/whoami on the OpenAI Auth API
// to get the PAT's workspace ID (ChatGPT-Account-Id), user ID, email and plan type.
func QueryPersonalAccessTokenMetadata(ctx context.Context, accessToken string, proxyURL string) (*PersonalAccessTokenMetadata, error) {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		return nil, errors.New("access token is empty")
	}

	endpoint := personalAccessTokenWhoAmIURL
	if patWhoAmIURLForTest != "" {
		endpoint = patWhoAmIURLForTest
	}

	client, err := newPATWhoAmIClient(proxyURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create whoami request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex2api")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute whoami request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &patWhoAmIStatusError{StatusCode: resp.StatusCode}
	}

	var meta PersonalAccessTokenMetadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&meta); err != nil {
		return nil, fmt.Errorf("decode whoami response: %w", err)
	}

	meta.ChatGPTAccountID = strings.TrimSpace(meta.ChatGPTAccountID)
	meta.ChatGPTUserID = strings.TrimSpace(meta.ChatGPTUserID)
	meta.ChatGPTPlanType = strings.TrimSpace(meta.ChatGPTPlanType)

	return &meta, nil
}
