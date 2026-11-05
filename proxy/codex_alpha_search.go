package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexAlphaSearchURL is the standalone Codex web search endpoint on the ChatGPT backend.
// The Codex CLI calls it when web_search = "live" (or --search) is enabled
// POST {base_url}/alpha/search performs the standalone search; this is not the same path as the
// hosted tool (tools: [{"type":"web_search"}]) in Responses requests.
const codexAlphaSearchURL = "https://chatgpt.com/backend-api/codex/alpha/search"

// codexAlphaSearchURLForTest lets tests replace the default URL. Production code must not assign it.
var codexAlphaSearchURLForTest = ""

// Search requests and results are structured JSON and are normally far smaller than this value; it is only a read guard.
const codexAlphaSearchBodyLimit int64 = 4 << 20

// codexAlphaSearchUnsupportedFields lists the fields the standalone search endpoint rejects and that must be stripped before forwarding.
// Newer codex clients also put Responses-style session and cache fields into the search body, but the upstream
// /alpha/search schema is narrower and rejects them with 400 "Unknown parameter" (issue #433):
// The same batch of /responses requests accepts prompt_cache_key, but only the search endpoint rejects it. These fields are meaningless for a single search
// call, so stripping them does not change search semantics.
var codexAlphaSearchUnsupportedFields = []string{"prompt_cache_key"}

// sanitizeCodexAlphaSearchBody removes fields the search endpoint does not support before forwarding, to avoid an upstream 400.
// It only removes known incompatible fields and passes the rest of the request body through unchanged.
func sanitizeCodexAlphaSearchBody(rawBody []byte) []byte {
	sanitized := rawBody
	for _, field := range codexAlphaSearchUnsupportedFields {
		if !gjson.GetBytes(sanitized, field).Exists() {
			continue
		}
		if next, err := sjson.DeleteBytes(sanitized, field); err == nil {
			sanitized = next
		}
	}
	return sanitized
}

// CodexAlphaSearchHandler passes through the Codex CLI standalone web search (issue #359).
//
// The endpoint exists only on the ChatGPT backend. It forwards live using the credentials of one schedulable ChatGPT OAuth account,
// and the request body and response (including upstream error status codes) pass through unchanged, with no local parsing and no fabricated search results.
func (h *Handler) CodexAlphaSearchHandler(c *gin.Context) {
	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, codexAlphaSearchBodyLimit))
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Failed to read request body", api.ErrorTypeInvalidRequest))
		return
	}
	h.capturePromptRequestIngress(c, rawBody)
	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	if h.inspectPromptFilterOpenAI(c, rawBody, "/v1/alpha/search", model) {
		return
	}
	continuousRetryPolicy := continuousRetryPolicyForCall(nil)
	rememberContinuousRetryPolicyForRequest(c, continuousRetryPolicy)
	stopRetryDeadline := installContinuousRetryHTTPDeadline(c, continuousRetryPolicy, continuousRetryProtocolResponses)
	defer stopRetryDeadline()
	stopRetryKeepalive := installContinuousRetryHTTPInformationalKeepalive(c)
	defer stopRetryKeepalive()
	activateContinuousRetryKeepalive(c.Request.Context())

	apiKeyID := requestAPIKeyID(c)
	searchFilter := applyAffinityGroupRouting(c, resolveRequestSessionIdentity(c.Request.Header, rawBody), func(a *auth.Account) bool {
		return true
	})
	account := h.store.NextExcludingWithFilter(apiKeyID, nil, searchFilter)
	if account == nil {
		api.SendError(c, api.ErrServiceUnavailable)
		return
	}
	defer h.store.Release(account)

	apiKey := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	resp, err := ForwardCodexAlphaSearch(
		c.Request.Context(),
		account,
		h.store.ResolveProxyForAccount(account),
		rawBody,
		c.Request.Header,
		h.deviceCfg,
		apiKey,
	)
	if err != nil {
		// A version-unavailable error is caught before the request goes out; it is a local configuration problem and must not be reported as an upstream 502.
		var identityErr *Error
		if errors.As(err, &identityErr) && identityErr.Code == ErrorCodeCodexClientVersionUnavailable {
			api.SendErrorWithStatus(c,
				api.NewAPIError(api.ErrCodeServiceUnavailable, identityErr.Message, api.ErrorTypeServer),
				identityErr.HTTPStatus)
			return
		}
		api.SendErrorWithStatus(c,
			api.NewAPIError(api.ErrCodeUpstreamError, fmt.Sprintf("codex alpha search: %v", err), api.ErrorTypeUpstream),
			http.StatusBadGateway)
		return
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logUpstreamError("/v1/alpha/search", resp.StatusCode, "", account.ID(), resp.Body)
	}
	contentType := resp.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(resp.StatusCode, contentType, resp.Body)
}

// CodexAlphaSearchResponse carries the raw upstream search response for the handler to pass through unchanged.
type CodexAlphaSearchResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

// ForwardCodexAlphaSearch forwards a standalone search request to the ChatGPT backend using the account credentials.
// The upstream status code and response body are returned unchanged (including 4xx/5xx), so the CLI gets the real error semantics;
// only transport-level failures return an error.
func ForwardCodexAlphaSearch(ctx context.Context, account *auth.Account, proxyURL string, rawBody []byte, downstreamHeaders http.Header, deviceCfg *DeviceProfileConfig, apiKey string) (*CodexAlphaSearchResponse, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, fmt.Errorf("account has no access token")
	}

	// Strip fields the search endpoint does not support (such as prompt_cache_key sent by the client), otherwise upstream returns 400 (issue #433).
	rawBody = sanitizeCodexAlphaSearchBody(rawBody)

	endpoint := codexAlphaSearchURL
	if codexAlphaSearchURLForTest != "" {
		endpoint = codexAlphaSearchURLForTest
	}

	// A standalone search is a model-driven retrieval turn, and upstream can take tens of seconds.
	reqCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(string(rawBody)))
	if err != nil {
		return nil, fmt.Errorf("build codex search request: %w", err)
	}

	if deviceCfg == nil {
		deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
	}
	identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Account: account, APIKey: apiKey, DeviceConfig: deviceCfg, Headers: downstreamHeaders})
	if err != nil {
		return nil, err
	}
	userAgent, version := identity.UserAgent, identity.Version
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Originator", CodexOriginatorForGeneratedUserAgent(userAgent))
	if version != "" {
		req.Header.Set("Version", version)
	}
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}

	// Reuse the same gateway transport (with uTLS Chrome fingerprint support) as /responses and the manifest passthrough.
	// Pool the transport instead of creating one per request, to avoid leaking connections from one-off uTLS transports (issue #446).
	client := getCodexMaintenanceClient(account, proxyURL)
	resp, err := executeHTTPWithContinuousRetryKeepalive(reqCtx, func() (*http.Response, error) {
		return client.Do(req)
	})
	if err != nil {
		return nil, fmt.Errorf("codex search request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readAllLimitedWithContinuousRetryKeepalive(reqCtx, resp.Body, codexAlphaSearchBodyLimit)
	if err != nil {
		return nil, fmt.Errorf("read codex search response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("codex alpha search upstream returned %d: %s", resp.StatusCode, truncateForLog(body, 512))
	}
	return &CodexAlphaSearchResponse{
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
	}, nil
}

func truncateForLog(body []byte, limit int) string {
	text := strings.TrimSpace(string(body))
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}
