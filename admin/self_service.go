package admin

import (
	"context"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// ==================== Public self-service account submission portal ====================
//
// Public page for external visitors: enter a contact email → generate a ChatGPT authorization link → after signing in, paste back the authorization code →
// The account is stored in the "pending review" state (enabled=false + tag self-service) and only enters the scheduling pool after an admin approves it.
// No human verification in this release; only per-IP rate limiting and email format checks. Credentials and tokens are never echoed back to the submitter.

const (
	selfServiceTag       = "self-service"
	selfServiceSource    = "self-service"
	selfServiceRateLimit = 10        // Maximum number of requests per IP within the window
	selfServiceRateWin   = time.Hour // Rate limit window
	selfServiceEmailMax  = 254       // Maximum email length
)

var selfServiceEmailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// selfServiceRateLimiter is a simple per-IP sliding-count rate limiter (in-memory, good enough for now).
type selfServiceRateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

var globalSelfServiceLimiter = &selfServiceRateLimiter{hits: make(map[string][]time.Time)}

// allow reports whether an IP still has quota within the window; if so, it records one request. now is passed in by the caller so tests can control time.
func (l *selfServiceRateLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-selfServiceRateWin)
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= selfServiceRateLimit {
		l.hits[ip] = kept
		return false
	}
	l.hits[ip] = append(kept, now)
	return true
}

// PublicAccountPortalPageEnabled reports whether the account self-service portal is enabled (off by default).
func (h *Handler) PublicAccountPortalPageEnabled(ctx context.Context) (bool, error) {
	if h == nil || h.db == nil {
		return false, nil
	}
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		return false, err
	}
	if settings == nil {
		return false, nil
	}
	return settings.PublicAccountPortalPageEnabled, nil
}

// accountPortalMiddleware gates the public portal API: when the switch is off, every request gets a 404; when it is on, requests are rate limited per IP.
func (h *Handler) accountPortalMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h == nil || h.db == nil {
			writeError(c, http.StatusServiceUnavailable, "service not ready")
			c.Abort()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		ok, err := h.PublicAccountPortalPageEnabled(ctx)
		cancel()
		if err != nil {
			writeInternalError(c, err)
			c.Abort()
			return
		}
		if !ok {
			writeError(c, http.StatusNotFound, "account self-service portal is not enabled")
			c.Abort()
			return
		}
		if !globalSelfServiceLimiter.allow(c.ClientIP(), time.Now()) {
			security.SecurityAuditLog("ACCOUNT_PORTAL_RATE_LIMITED", "ip="+security.SanitizeLog(c.ClientIP()))
			writeError(c, http.StatusTooManyRequests, "too many requests, please try again later")
			c.Abort()
			return
		}
		c.Next()
	}
}

func normalizeContactEmail(raw string) (string, bool) {
	email := strings.TrimSpace(security.SanitizeInput(raw))
	if email == "" || len(email) > selfServiceEmailMax || !selfServiceEmailRe.MatchString(email) {
		return "", false
	}
	return email, true
}

// GenerateAccountPortalAuthURL generates a ChatGPT authorization link (public).
// POST /api/account-portal/generate-auth-url  body: { contact_email }
func (h *Handler) GenerateAccountPortalAuthURL(c *gin.Context) {
	var req struct {
		ContactEmail string `json:"contact_email"`
	}
	_ = c.ShouldBindJSON(&req)

	email, ok := normalizeContactEmail(req.ContactEmail)
	if !ok {
		writeError(c, http.StatusBadRequest, "enter a valid contact email")
		return
	}

	state, err := oauthRandomHex(32)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "failed to generate state")
		return
	}
	codeVerifier, err := oauthRandomHex(64)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "failed to generate code_verifier")
		return
	}
	sessionID, err := oauthRandomHex(16)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "failed to generate session_id")
		return
	}

	authURL, err := buildOAuthAuthorizeURL(oauthDefaultRedirectURI, state, codeVerifier)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid client identity configuration: "+err.Error())
		return
	}
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        state,
		CodeVerifier: codeVerifier,
		RedirectURI:  oauthDefaultRedirectURI,
		ContactEmail: email,
		SelfService:  true,
		CreatedAt:    time.Now(),
	})

	c.JSON(http.StatusOK, gin.H{
		"auth_url":   authURL,
		"session_id": sessionID,
	})
}

// SubmitAccountPortalCode exchanges the pasted-back authorization code and stores the account as "pending review" (public).
// POST /api/account-portal/submit-code  body: { session_id, code, state }
func (h *Handler) SubmitAccountPortalCode(c *gin.Context) {
	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		State     string `json:"state"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request format")
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Code = strings.TrimSpace(req.Code)
	req.State = strings.TrimSpace(req.State)
	if req.SessionID == "" || req.Code == "" || req.State == "" {
		writeError(c, http.StatusBadRequest, "session_id, code and state are all required")
		return
	}

	sess, ok := globalOAuthStore.get(req.SessionID)
	if !ok || !sess.SelfService {
		writeError(c, http.StatusBadRequest, "session does not exist or has expired (valid for 30 minutes), please start authorization again")
		return
	}
	if req.State != sess.State {
		writeError(c, http.StatusBadRequest, "state does not match, please start authorization again")
		return
	}
	if !sess.claimExchange(req.Code, req.State) {
		writeError(c, http.StatusBadRequest, "this authorization session has already been processed or is being processed, please start authorization again")
		return
	}
	defer sess.completeExchange(&oauthExchangeResult{Error: "please start authorization again"})

	proxyURL := strings.TrimSpace(sess.ProxyURL)
	if proxyURL == "" {
		proxyURL = h.store.GetProxyURL()
	}

	tokenResp, accountInfo, err := doOAuthCodeExchange(c.Request.Context(), req.Code, sess.CodeVerifier, sess.RedirectURI, proxyURL, "oauth-"+req.SessionID)
	if err != nil {
		writeError(c, http.StatusBadGateway, "authorization code exchange failed, please start authorization again and get a new authorization code")
		return
	}
	globalOAuthStore.delete(req.SessionID)

	if tokenResp.RefreshToken == "" {
		writeError(c, http.StatusBadGateway, "authorization did not return a refresh_token, please start authorization again")
		return
	}
	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
		refreshToken: tokenResp.RefreshToken,
		accessToken:  tokenResp.AccessToken,
		idToken:      tokenResp.IDToken,
		expiresIn:    tokenResp.ExpiresIn,
	})

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	name := seed.email
	if name == "" {
		name = "self-service-account"
	}

	if _, err := h.upsertSelfServiceAccount(ctx, name, proxyURL, seed, sess.ContactEmail); err != nil {
		if err == errDuplicateOAuthIdentity {
			// Already in the pool (or already submitted for review): return a neutral success response so the submitter cannot probe whether the account exists.
			c.JSON(http.StatusOK, gin.H{"message": "this account is already in the system, no need to submit it again"})
			return
		}
		writeError(c, http.StatusInternalServerError, "submission failed, please try again later")
		return
	}

	email := seed.email
	if accountInfo != nil && accountInfo.Email != "" {
		email = accountInfo.Email
	}
	security.SecurityAuditLog("ACCOUNT_PORTAL_SUBMITTED", "ip="+security.SanitizeLog(c.ClientIP())+" contact="+security.SanitizeLog(sess.ContactEmail)+" account="+security.SanitizeLog(email))

	// Do not echo the token or refresh_token; only report that the submission is now pending review.
	c.JSON(http.StatusOK, gin.H{
		"message": "Submitted. It will take effect after an admin reviews it. Thank you for your contribution!",
	})
}

// upsertSelfServiceAccount stores the account as "pending review": enabled=false + note (contact) + tag self-service,
// it is not added to the runtime scheduling pool (it is enabled only after admin approval). A duplicate account returns errDuplicateOAuthIdentity.
func (h *Handler) upsertSelfServiceAccount(ctx context.Context, name, proxyURL string, seed tokenCredentialSeed, contactEmail string) (int64, error) {
	seed = normalizeTokenCredentialSeed(seed)
	if seed.email != "" && effectiveWorkspaceIDFromSeed(seed) != "" {
		h.mergeDuplicateMu.Lock()
		defer h.mergeDuplicateMu.Unlock()
		if duplicateID, err := h.findOAuthIdentityDuplicate(ctx, seed, 0); err != nil {
			return 0, err
		} else if duplicateID > 0 {
			return 0, errDuplicateOAuthIdentity
		}
	}

	note := "self-service contact: " + contactEmail
	id, err := h.db.InsertPendingAccountWithCredentials(ctx, name, h.newCodexAccountCredentials(seed), proxyURL, note, []string{selfServiceTag})
	if err != nil {
		return 0, err
	}
	h.db.InsertAccountEventAsync(id, "added", selfServiceSource)
	// Public submissions do not go through the /api/admin/accounts* invalidation middleware; unless the snapshot is dropped explicitly,
	// the accounts page SWR cache keeps serving the old list, so the first query of the pending review panel returns 0.
	h.invalidateAccountSnapshotCaches()
	log.Printf("self-service portal added pending review account %d (%s, contact %s)", id, name, contactEmail)
	return id, nil
}
