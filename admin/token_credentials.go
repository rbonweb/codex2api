package admin

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/openaiidentity"
)

type tokenCredentialSeed struct {
	refreshToken    string
	sessionToken    string
	accessToken     string
	accessTokenType string
	idToken         string
	accountID       string
	workspaceID     string
	// userID is the OpenAI user ID (user-...), stored only as account metadata.
	userID string
	// allowDuplicate allows duplicates only for accounts whose effective workspace (token workspace or request header override) is empty.
	allowDuplicate        bool
	email                 string
	planType              string
	expiresAt             time.Time
	expiresAtRaw          string
	expiresIn             int64
	subscriptionExpiresAt time.Time
	codex7DUsedPercent    string
	codex7DResetAt        string
	codex5HUsedPercent    string
	codex5HResetAt        string
	codex5HUsageUpdatedAt string
	codexUsageUpdatedAt   string
	customHeaders         map[string]string
}

func normalizeTokenCredentialSeed(seed tokenCredentialSeed) tokenCredentialSeed {
	seed.refreshToken = strings.TrimSpace(seed.refreshToken)
	seed.sessionToken = strings.TrimSpace(seed.sessionToken)
	seed.accessToken = strings.TrimSpace(seed.accessToken)
	seed.accessTokenType = strings.TrimSpace(seed.accessTokenType)
	seed.idToken = strings.TrimSpace(seed.idToken)
	seed.accountID = strings.TrimSpace(seed.accountID)
	seed.workspaceID = openaiidentity.NormalizeWorkspaceID(seed.workspaceID)
	seed.userID = strings.TrimSpace(seed.userID)
	seed.email = strings.TrimSpace(seed.email)
	seed.planType = strings.TrimSpace(seed.planType)
	seed.expiresAtRaw = strings.TrimSpace(seed.expiresAtRaw)
	seed.codex7DUsedPercent = strings.TrimSpace(seed.codex7DUsedPercent)
	seed.codex7DResetAt = strings.TrimSpace(seed.codex7DResetAt)
	seed.codex5HUsedPercent = strings.TrimSpace(seed.codex5HUsedPercent)
	seed.codex5HResetAt = strings.TrimSpace(seed.codex5HResetAt)
	seed.codex5HUsageUpdatedAt = strings.TrimSpace(seed.codex5HUsageUpdatedAt)
	seed.codexUsageUpdatedAt = strings.TrimSpace(seed.codexUsageUpdatedAt)
	if seed.accessTokenType == "" {
		seed.accessTokenType = accessTokenTypeForToken(seed.accessToken)
	}

	accessTokenForJWT := seed.accessToken
	if seed.accessTokenType == accessTokenTypeCodexAT {
		accessTokenForJWT = ""
	}
	tokenEmail, tokenWorkspaceID := openaiidentity.TokenIdentity(seed.idToken, accessTokenForJWT)
	if seed.workspaceID == "" {
		if tokenWorkspaceID != "" {
			if seed.email == "" {
				seed.email = tokenEmail
				seed.workspaceID = tokenWorkspaceID
			} else if strings.EqualFold(seed.email, tokenEmail) {
				seed.workspaceID = tokenWorkspaceID
			}
		}
	}
	if info := accountInfoFromTokens(seed.idToken, accessTokenForJWT); info != nil {
		if tokenWorkspaceID != "" {
			info.Email = tokenEmail
			info.ChatGPTAccountID = tokenWorkspaceID
		}
		if seed.accountID == "" {
			seed.accountID = info.ChatGPTAccountID
		}
		if seed.userID == "" {
			seed.userID = info.UserID
		}
		if seed.email == "" {
			seed.email = info.Email
		}
		if seed.planType == "" {
			seed.planType = info.PlanType
		}
		if seed.subscriptionExpiresAt.IsZero() && !info.SubscriptionExpiresAt.IsZero() {
			seed.subscriptionExpiresAt = info.SubscriptionExpiresAt
		}
	}
	// An expiry time already in the past on a paid plan is a stale JWT claim from before renewal, so it is not stored on import. (issue #360)
	if auth.StaleSubscriptionExpiry(seed.planType, seed.subscriptionExpiresAt, time.Now()) {
		seed.subscriptionExpiresAt = time.Time{}
	}

	if seed.expiresAt.IsZero() && seed.expiresIn > 0 {
		seed.expiresAt = time.Now().Add(time.Duration(seed.expiresIn) * time.Second)
	}
	if seed.expiresAt.IsZero() && seed.accessToken != "" && seed.accessTokenType != accessTokenTypeCodexAT {
		if info := auth.ParseAccessToken(seed.accessToken); info != nil && !info.ExpiresAt.IsZero() {
			seed.expiresAt = info.ExpiresAt
		}
	}
	if seed.expiresAt.IsZero() && seed.expiresAtRaw != "" {
		seed.expiresAt = parseCredentialExpiresAt(seed.expiresAtRaw)
	}
	if seed.expiresAt.IsZero() && seed.accessToken != "" {
		seed.expiresAt = time.Now().Add(time.Hour)
	}

	return seed
}

func effectiveWorkspaceIDFromSeed(seed tokenCredentialSeed) string {
	seed = normalizeTokenCredentialSeed(seed)
	return openaiidentity.EffectiveWorkspaceID(seed.workspaceID, seed.customHeaders)
}

// patWhoAmIHydrateTimeout is the whoami budget for a single PAT. It is not deducted from the 30s context of the batch import:
// a batch of one hundred tokens is hydrated serially, so a few slow requests could burn the whole batch's storage budget.
const patWhoAmIHydrateTimeout = 6 * time.Second

// patWhoAmIHydrator fills in workspace identity for PATs within one batch import. A network-level failure (timeout / proxy unreachable)
// is very likely to fail the same way for the rest of the batch, so one hit trips the circuit breaker and the remaining tokens are stored directly as plain AT;
// a 4xx returned by the endpoint for a token only means that token lacks permission and does not affect other tokens.
type patWhoAmIHydrator struct {
	proxyURL string
	tripped  bool
}

func (w *patWhoAmIHydrator) hydrate(ctx context.Context, seed tokenCredentialSeed) tokenCredentialSeed {
	if w == nil || w.tripped {
		return seed
	}
	seed, err := hydrateSeedWithWhoAmI(ctx, seed, w.proxyURL)
	if err == nil {
		return seed
	}
	var statusErr *patWhoAmIStatusError
	if errors.As(err, &statusErr) {
		log.Printf("PAT whoami identity lookup failed (this token is stored as plain AT): %v", err)
		return seed
	}
	w.tripped = true
	log.Printf("PAT whoami network failure, the remaining tokens in this batch skip identity hydration: %v", err)
	return seed
}

// hydrateSeedWithWhoAmI uses /whoami to fill in the PAT's workspace / user / email / plan.
// The returned error is only for the caller to decide whether to trip the breaker; the seed is always usable (returned unchanged on failure).
func hydrateSeedWithWhoAmI(ctx context.Context, seed tokenCredentialSeed, proxyURL string) (tokenCredentialSeed, error) {
	if seed.accessTokenType != accessTokenTypeCodexAT || strings.TrimSpace(seed.accessToken) == "" {
		return seed, nil
	}
	if seed.workspaceID != "" && seed.email != "" && seed.planType != "" {
		return seed, nil
	}
	whoamiCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), patWhoAmIHydrateTimeout)
	defer cancel()
	meta, err := QueryPersonalAccessTokenMetadata(whoamiCtx, seed.accessToken, proxyURL)
	if err != nil {
		return seed, err
	}
	if meta == nil {
		return seed, nil
	}
	if seed.workspaceID == "" && meta.ChatGPTAccountID != "" {
		seed.workspaceID = meta.ChatGPTAccountID
	}
	if seed.accountID == "" && meta.ChatGPTAccountID != "" {
		seed.accountID = meta.ChatGPTAccountID
	}
	if seed.userID == "" && meta.ChatGPTUserID != "" {
		seed.userID = meta.ChatGPTUserID
	}
	if seed.email == "" && meta.Email != nil {
		seed.email = strings.TrimSpace(*meta.Email)
	}
	if seed.planType == "" && meta.ChatGPTPlanType != "" {
		seed.planType = meta.ChatGPTPlanType
	}
	return seed, nil
}

const accessTokenTypeCodexAT = "codex_at"

func accessTokenTypeForToken(accessToken string) string {
	if strings.HasPrefix(strings.TrimSpace(accessToken), "at-") {
		return accessTokenTypeCodexAT
	}
	return ""
}

func accountInfoFromTokens(idToken, accessToken string) *auth.AccountInfo {
	info := auth.ParseIDToken(strings.TrimSpace(idToken))
	if info == nil {
		info = &auth.AccountInfo{}
	}
	if atInfo := auth.ParseAccessToken(strings.TrimSpace(accessToken)); atInfo != nil {
		if info.ChatGPTAccountID == "" {
			info.ChatGPTAccountID = atInfo.ChatGPTAccountID
		}
		if info.UserID == "" {
			info.UserID = atInfo.UserID
		}
		if info.Email == "" {
			info.Email = atInfo.Email
		}
		if info.PlanType == "" {
			info.PlanType = atInfo.PlanType
		}
		if info.SubscriptionExpiresAt.IsZero() && !atInfo.SubscriptionExpiresAt.IsZero() {
			info.SubscriptionExpiresAt = atInfo.SubscriptionExpiresAt
		}
	}
	return info
}

func tokenCredentialMap(seed tokenCredentialSeed) map[string]interface{} {
	seed = normalizeTokenCredentialSeed(seed)
	credentials := make(map[string]interface{})
	if seed.refreshToken != "" {
		credentials["refresh_token"] = seed.refreshToken
	}
	if seed.sessionToken != "" {
		credentials["session_token"] = seed.sessionToken
	}
	if seed.accessToken != "" {
		credentials["access_token"] = seed.accessToken
	}
	if seed.accessTokenType != "" {
		credentials["access_token_type"] = seed.accessTokenType
	}
	if seed.idToken != "" {
		credentials["id_token"] = seed.idToken
	}
	if !seed.expiresAt.IsZero() {
		credentials["expires_at"] = seed.expiresAt.Format(time.RFC3339)
	}
	if seed.accountID != "" {
		credentials["account_id"] = seed.accountID
	}
	if seed.workspaceID != "" {
		credentials["workspace_id"] = seed.workspaceID
	}
	if seed.userID != "" {
		credentials["user_id"] = seed.userID
	}
	if seed.allowDuplicate && effectiveWorkspaceIDFromSeed(seed) == "" {
		credentials["allow_duplicate"] = "true"
	}
	if seed.email != "" {
		credentials["email"] = seed.email
	}
	if seed.planType != "" {
		credentials["plan_type"] = seed.planType
	}
	if !seed.subscriptionExpiresAt.IsZero() {
		credentials["subscription_expires_at"] = seed.subscriptionExpiresAt.Format(time.RFC3339)
	}
	if seed.codex7DUsedPercent != "" {
		credentials["codex_7d_used_percent"] = seed.codex7DUsedPercent
	}
	if seed.codex7DResetAt != "" {
		credentials["codex_7d_reset_at"] = seed.codex7DResetAt
	}
	if seed.codex5HUsedPercent != "" {
		credentials["codex_5h_used_percent"] = seed.codex5HUsedPercent
	}
	if seed.codex5HResetAt != "" {
		credentials["codex_5h_reset_at"] = seed.codex5HResetAt
	}
	if seed.codex5HUsageUpdatedAt != "" {
		credentials["codex_5h_usage_updated_at"] = seed.codex5HUsageUpdatedAt
	}
	if seed.codexUsageUpdatedAt != "" {
		credentials["codex_usage_updated_at"] = seed.codexUsageUpdatedAt
	}
	if len(seed.customHeaders) > 0 {
		credentials["custom_headers"] = cloneCustomHeaders(seed.customHeaders)
	}
	return credentials
}

// defaultCodexFingerprintModeForNewAccount returns the default fingerprint convergence level for newly imported accounts, as configured in system settings
// (off/device/session/full, default off).
func (h *Handler) defaultCodexFingerprintModeForNewAccount() string {
	if h == nil || h.store == nil {
		return auth.CodexFingerprintModeOff
	}
	return auth.NormalizeCodexFingerprintMode(h.store.GetCodexFingerprintDefaultMode())
}

// newCodexAccountCredentials generates credentials for newly created or imported Codex accounts and stamps them with
// the system default fingerprint convergence level. It is only used when inserting new accounts; updating existing account credentials still goes through tokenCredentialMap,
// so that re-importing does not overwrite a level the user adjusted manually on the account.
func (h *Handler) newCodexAccountCredentials(seed tokenCredentialSeed) map[string]interface{} {
	credentials := tokenCredentialMap(seed)
	if mode := h.defaultCodexFingerprintModeForNewAccount(); mode != auth.CodexFingerprintModeOff {
		credentials[auth.CodexFingerprintModeCredentialKey] = mode
	}
	return credentials
}

// newCodexAccountFromSeed builds the in-memory state of a just-inserted account; its fingerprint level matches
// what newCodexAccountCredentials stores (the default level takes effect without a reload).
func (h *Handler) newCodexAccountFromSeed(id int64, proxyURL string, seed tokenCredentialSeed) *auth.Account {
	account := accountFromCredentialSeed(id, proxyURL, seed)
	account.CodexFingerprintMode = h.defaultCodexFingerprintModeForNewAccount()
	return account
}

func accountFromCredentialSeed(id int64, proxyURL string, seed tokenCredentialSeed) *auth.Account {
	seed = normalizeTokenCredentialSeed(seed)
	account := &auth.Account{
		DBID:                  id,
		RefreshToken:          seed.refreshToken,
		SessionToken:          seed.sessionToken,
		AccessToken:           seed.accessToken,
		ExpiresAt:             seed.expiresAt,
		AccountID:             seed.accountID,
		Email:                 seed.email,
		PlanType:              seed.planType,
		ProxyURL:              proxyURL,
		CustomHeaders:         cloneCustomHeaders(seed.customHeaders),
		Status:                auth.StatusReady,
		SubscriptionExpiresAt: seed.subscriptionExpiresAt,
	}
	if pct, ok := parseSeedUsagePercent(seed.codex7DUsedPercent); ok {
		updatedAt := parseSeedRFC3339(seed.codexUsageUpdatedAt)
		account.SetUsageSnapshot(pct, updatedAt)
		if resetAt := parseSeedRFC3339(seed.codex7DResetAt); !resetAt.IsZero() {
			account.SetReset7dAt(resetAt)
		}
	}
	if pct, ok := parseSeedUsagePercent(seed.codex5HUsedPercent); ok {
		account.SetUsageSnapshot5hAt(pct, parseSeedRFC3339(seed.codex5HResetAt), parseSeedRFC3339(seed.codex5HUsageUpdatedAt))
	}
	return account
}

func parseSeedUsagePercent(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func parseSeedRFC3339(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func parseCredentialExpiresAt(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}

	if unixSeconds, err := strconv.ParseFloat(raw, 64); err == nil && unixSeconds > 0 {
		if unixSeconds >= 1e12 {
			return time.UnixMilli(int64(unixSeconds))
		}
		return time.Unix(int64(unixSeconds), 0)
	}

	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}
