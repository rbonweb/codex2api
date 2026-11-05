package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// ==================== OAuth constants ====================

const (
	oauthAuthorizeURL       = "https://auth.openai.com/oauth/authorize"
	oauthTokenURL           = "https://auth.openai.com/oauth/token"
	oauthClientID           = "app_EMoamEEZ73f0CkXaXp7hrann"
	oauthDefaultRedirectURI = "http://localhost:1455/auth/callback"
	oauthDefaultScopes      = "openid profile email offline_access"
	oauthSessionTTL         = 30 * time.Minute
)

// ==================== In-memory session store ====================

type oauthSession struct {
	mu sync.Mutex // protects callback progress; authorization fields are immutable

	State        string
	CodeVerifier string
	RedirectURI  string
	ProxyURL     string
	ContactEmail string // contact email of the self-service portal submitter (written only by the self-service flow, as a note marker)
	SelfService  bool   // whether it came from the public self-service portal (decides the pending-review pool path)
	CreatedAt    time.Time

	// Fields for automatically capturing the callback
	CallbackCode   string    // authorization code received by the callback
	CallbackState  string    // state received by the callback
	CallbackAt     time.Time // callback time
	ExchangeResult *oauthExchangeResult
}

// claimExchange consumes a session before network I/O. Failed exchanges require
// a new authorization session because the upstream may already have used the code.
func (s *oauthSession) claimExchange(code, state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.CallbackAt.IsZero() || s.ExchangeResult != nil {
		return false
	}
	s.CallbackCode, s.CallbackState, s.CallbackAt = code, state, time.Now()
	return true
}

func (s *oauthSession) completeExchange(result *oauthExchangeResult) {
	s.mu.Lock()
	s.ExchangeResult = result
	s.mu.Unlock()
}

func (s *oauthSession) callbackSnapshot() (*oauthExchangeResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ExchangeResult != nil {
		result := *s.ExchangeResult
		return &result, true
	}
	return nil, !s.CallbackAt.IsZero()
}

// oauthExchangeResult is the exchange result after the automatic callback completes
type oauthExchangeResult struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	ID       int64  `json:"id,omitempty"`
	Email    string `json:"email,omitempty"`
	PlanType string `json:"plan_type,omitempty"`
	Error    string `json:"error,omitempty"`
}

type oauthSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*oauthSession
}

var globalOAuthStore = &oauthSessionStore{sessions: make(map[string]*oauthSession)}

func init() {
	go globalOAuthStore.cleanupLoop()
}

func (s *oauthSessionStore) set(id string, sess *oauthSession) {
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
}

func (s *oauthSessionStore) get(id string) (*oauthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok || time.Since(sess.CreatedAt) > oauthSessionTTL {
		return nil, false
	}
	return sess, true
}

func (s *oauthSessionStore) delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// findByState looks up a session by state (used by the callback endpoint; returns sessionID + session).
// It walks all entries and uses a constant-time comparison, so it does not return early based on match position.
func (s *oauthSessionStore) findByState(state string) (string, *oauthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var foundID string
	var foundSess *oauthSession
	for id, sess := range s.sessions {
		if !security.SecureCompare(sess.State, state) {
			continue
		}
		if time.Since(sess.CreatedAt) > oauthSessionTTL {
			continue
		}
		foundID, foundSess = id, sess
	}
	if foundSess == nil {
		return "", nil, false
	}
	return foundID, foundSess, true
}

func (s *oauthSessionStore) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		for id, sess := range s.sessions {
			if time.Since(sess.CreatedAt) > oauthSessionTTL {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}

// ==================== PKCE helper functions ====================

func oauthRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func oauthCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return strings.TrimRight(base64.URLEncoding.EncodeToString(h[:]), "=")
}

// ==================== Handlers ====================

// GenerateOAuthURL generates a Codex CLI PKCE OAuth authorization URL
// POST /api/admin/oauth/generate-auth-url
func (h *Handler) GenerateOAuthURL(c *gin.Context) {
	var req struct {
		ProxyURL    string `json:"proxy_url"`
		RedirectURI string `json:"redirect_uri"`
	}
	_ = c.ShouldBindJSON(&req)

	redirectURI := strings.TrimSpace(req.RedirectURI)
	if redirectURI == "" {
		// OpenAI OAuth only registers the localhost:1455 callback, so the fixed default is always used
		// so a different port in the request Host (e.g. localhost:3000) does not make the callback check fail (#80)
		redirectURI = oauthDefaultRedirectURI
	}

	state, err := oauthRandomHex(32)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to generate state")
		return
	}
	codeVerifier, err := oauthRandomHex(64)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to generate code_verifier")
		return
	}
	sessionID, err := oauthRandomHex(16)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to generate session_id")
		return
	}

	authURL, err := buildOAuthAuthorizeURL(redirectURI, state, codeVerifier)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid client identity configuration: "+err.Error())
		return
	}
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        state,
		CodeVerifier: codeVerifier,
		RedirectURI:  redirectURI,
		ProxyURL:     strings.TrimSpace(req.ProxyURL),
		CreatedAt:    time.Now(),
	})

	c.JSON(http.StatusOK, gin.H{
		"auth_url":   authURL,
		"session_id": sessionID,
	})
}

// buildOAuthAuthorizeURL assembles the Codex CLI PKCE authorization link (shared by the admin and the public self-service portal).
func buildOAuthAuthorizeURL(redirectURI, state, codeVerifier string) (string, error) {
	headers, err := proxy.CodexOAuthIdentityHeaders(nil)
	if err != nil {
		return "", err
	}
	params := neturl.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", oauthClientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", oauthDefaultScopes)
	params.Set("state", state)
	params.Set("code_challenge", oauthCodeChallenge(codeVerifier))
	params.Set("code_challenge_method", "S256")
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")
	params.Set("originator", headers.Get("Originator"))
	return oauthAuthorizeURL + "?" + params.Encode(), nil
}

// ExchangeOAuthCode exchanges the authorization code for a token and writes a new account
// POST /api/admin/oauth/exchange-code
func (h *Handler) ExchangeOAuthCode(c *gin.Context) {
	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		State     string `json:"state"`
		Name      string `json:"name"`
		ProxyURL  string `json:"proxy_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	if req.SessionID == "" || req.Code == "" || req.State == "" {
		writeError(c, http.StatusBadRequest, "session_id, code and state are all required")
		return
	}

	sess, ok := globalOAuthStore.get(req.SessionID)
	if !ok {
		writeError(c, http.StatusBadRequest, "OAuth session does not exist or has expired (valid for 30 minutes)")
		return
	}
	if req.State != sess.State {
		writeError(c, http.StatusBadRequest, "state does not match; start authorization again")
		return
	}
	if !sess.claimExchange(req.Code, req.State) {
		writeError(c, http.StatusBadRequest, "This authorization session has already been processed or is being processed; start authorization again")
		return
	}
	defer sess.completeExchange(&oauthExchangeResult{Error: "Start authorization again"})

	proxyURL := sess.ProxyURL
	if trimmed := strings.TrimSpace(req.ProxyURL); trimmed != "" {
		proxyURL = trimmed
	}
	if proxyURL == "" {
		proxyURL = h.store.GetProxyURL()
	}

	// Temporary Resin identity for the OAuth exchange (the new account has no DBID yet)
	resinTempID := "oauth-" + req.SessionID
	tokenResp, accountInfo, err := doOAuthCodeExchange(c.Request.Context(), req.Code, sess.CodeVerifier, sess.RedirectURI, proxyURL, resinTempID)
	if err != nil {
		writeError(c, http.StatusBadGateway, "Authorization code exchange failed: "+err.Error())
		return
	}
	globalOAuthStore.delete(req.SessionID)

	if tokenResp.RefreshToken == "" {
		writeError(c, http.StatusBadGateway, "The authorization server did not return a refresh_token; confirm the offline_access scope is enabled")
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

	name := strings.TrimSpace(req.Name)
	if name == "" && seed.email != "" {
		name = seed.email
	}
	if name == "" {
		name = "oauth-account"
	}

	id, updated, err := h.upsertOAuthIdentityAccount(ctx, name, proxyURL, seed, "oauth")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to write account to the database: "+err.Error())
		return
	}
	if proxy.IsResinEnabled() && !updated {
		go proxy.InheritLease(resinTempID, fmt.Sprintf("%d", id))
	}

	email := ""
	planType := ""
	if accountInfo != nil {
		email = accountInfo.Email
		planType = accountInfo.PlanType
	}
	if email == "" {
		email = seed.email
	}
	if planType == "" {
		planType = seed.planType
	}

	message := fmt.Sprintf("OAuth account %s added successfully", name)
	if updated {
		message = fmt.Sprintf("OAuth account already exists; updated account %d", id)
	}
	c.JSON(http.StatusOK, gin.H{
		"message":   message,
		"id":        id,
		"email":     email,
		"plan_type": planType,
		"updated":   updated,
	})
}

var errDuplicateOAuthIdentity = errors.New("duplicate oauth identity")
var errDuplicateCredentialWorkspaceRoute = errors.New("duplicate credential workspace route")

func oauthIdentityDuplicateMessage(id int64) string {
	return fmt.Sprintf("OAuth account already exists (id=%d); update the existing account", id)
}

func (h *Handler) findOAuthIdentityDuplicate(ctx context.Context, seed tokenCredentialSeed, excludeID int64) (int64, error) {
	if h == nil || h.db == nil {
		return 0, nil
	}
	seed = normalizeTokenCredentialSeed(seed)
	effectiveWorkspaceID := effectiveWorkspaceIDFromSeed(seed)
	if seed.email == "" || effectiveWorkspaceID == "" {
		return 0, nil
	}
	id, err := h.db.FindActiveAccountByOAuthRouteIdentity(ctx, seed.email, effectiveWorkspaceID, excludeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}

func (h *Handler) upsertOAuthIdentityAccount(ctx context.Context, name, proxyURL string, seed tokenCredentialSeed, source string) (int64, bool, error) {
	id, updated, _, err := h.upsertOAuthIdentityAccountWithRuntime(ctx, name, proxyURL, seed, source, true, overwriteAccountProxy)
	return id, updated, err
}

// upsertOAuthIdentityAccountDeferred keeps newly inserted accounts out of the
// runtime pool until the caller can commit the whole import batch atomically.
// Existing-account updates still reload immediately so rotated credentials are
// visible without waiting for the rest of the import.
func (h *Handler) upsertOAuthIdentityAccountDeferred(ctx context.Context, name, proxyURL string, seed tokenCredentialSeed, source string, proxyPolicy proxyOverwritePolicy) (int64, bool, *auth.Account, error) {
	return h.upsertOAuthIdentityAccountWithRuntime(ctx, name, proxyURL, seed, source, false, proxyPolicy)
}

func (h *Handler) upsertOAuthIdentityAccountWithRuntime(ctx context.Context, name, proxyURL string, seed tokenCredentialSeed, source string, loadRuntime bool, proxyPolicy proxyOverwritePolicy) (int64, bool, *auth.Account, error) {
	seed = normalizeTokenCredentialSeed(seed)
	if seed.email == "" || effectiveWorkspaceIDFromSeed(seed) == "" {
		id, err := h.db.InsertAccountWithCredentials(ctx, name, h.newCodexAccountCredentials(seed), proxyURL)
		if err != nil {
			return 0, false, nil, err
		}
		if !loadRuntime {
			return id, false, h.newCodexAccountFromSeed(id, proxyURL, seed), nil
		}
		h.db.InsertAccountEventAsync(id, "added", source)
		h.loadInsertedTokenAccount(id, proxyURL, seed, source)
		return id, false, nil, nil
	}
	h.mergeDuplicateMu.Lock()
	defer h.mergeDuplicateMu.Unlock()

	if duplicateID, err := h.findOAuthIdentityDuplicate(ctx, seed, 0); err != nil {
		return 0, false, nil, err
	} else if duplicateID > 0 {
		row, err := h.db.GetAccountByID(ctx, duplicateID)
		if err != nil {
			return 0, false, nil, err
		}
		// Empty values always keep the existing binding. preserveAccountProxy goes further: an account already bound to a proxy
		// is not overwritten: the proxy in the import file is passive data, while the target may already have done fine-grained
		// proxy assignment (automatic balancing, etc.); silently overwriting it would be data corruption.
		effectiveProxyURL := strings.TrimSpace(proxyURL)
		existingProxyURL := strings.TrimSpace(row.ProxyURL)
		if effectiveProxyURL == "" || (proxyPolicy == preserveAccountProxy && existingProxyURL != "") {
			effectiveProxyURL = existingProxyURL
		}
		if err := h.db.UpdateOAuthAccountCredentials(ctx, duplicateID, tokenCredentialMap(seed), effectiveProxyURL); err != nil {
			return 0, false, nil, err
		}
		// When valid credentials are re-imported and the account was previously in an error or banned (401) state, the error state is cleared,
		// so the reloaded runtime account is no longer banned and is re-evaluated by a later probe.
		// Only for error / unauthorized, so a legitimate rate limit cooldown (rate_limited) is not cleared by mistake.
		h.clearReimportedAccountErrorState(ctx, row, "re-import")
		if err := h.reloadTokenAccount(ctx, duplicateID, source); err != nil {
			return 0, false, nil, err
		}
		h.db.InsertAccountEventAsync(duplicateID, "updated", source)
		return duplicateID, true, nil, nil
	}

	id, err := h.db.InsertAccountWithCredentials(ctx, name, h.newCodexAccountCredentials(seed), proxyURL)
	if err != nil {
		return 0, false, nil, err
	}
	if !loadRuntime {
		return id, false, h.newCodexAccountFromSeed(id, proxyURL, seed), nil
	}
	h.db.InsertAccountEventAsync(id, "added", source)
	h.loadInsertedTokenAccount(id, proxyURL, seed, source)
	return id, false, nil, nil
}

// accountErrorStateNeedsReset reports whether an existing account is in a state that should be cleared once valid credentials are re-imported:
// error or ban state: status='error' (RT invalid, auth failed, etc.) or a 401 unauthorized cooldown.
// Rate limit cooldowns (rate_limited*) are excluded, so re-importing does not wrongly clear a legitimate rate limit window.
func accountErrorStateNeedsReset(row *database.AccountRow) bool {
	if row == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(row.Status), "error") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(row.CooldownReason), "unauthorized")
}

// clearReimportedAccountErrorState runs after a re-import, re-authorization or merged credential obtains valid credentials,
// and clears the error / unauthorized state of the existing account in place. It returns whether it actually cleared anything (an account not in
// that state is left alone, and rate limit cooldowns are never touched).
//
// The database and the cross-instance cooldown cache must be cleared together: the caller then calls reloadTokenAccount to rebuild from the database
// the runtime account; once the database is clean, memory is clean too. But when the scheduler picks an account it also reads the cooldown from the cache
// and writes it back onto the account; clearing only the database would leave the account blocked by the unauthorized cache until its TTL expires.
// The in-memory FailureStreak / PermanentRefreshFailures / Disabled flags are reset along with the reload
// so they need no separate handling here.
func (h *Handler) clearReimportedAccountErrorState(ctx context.Context, row *database.AccountRow, action string) bool {
	if h == nil || h.db == nil || !accountErrorStateNeedsReset(row) {
		return false
	}
	if err := h.db.ClearError(ctx, row.ID); err != nil {
		log.Printf("%s: failed to clear error state of account %d: %v", action, row.ID, err)
		return false
	}
	if h.store != nil {
		h.store.ForgetCachedAccountCooldown(row.ID)
	}
	return true
}

// reviveReimportedAccount handles the case where a duplicate import hits an existing account that is in error / unauthorized state:
// the credentials are identical, so it would normally just count one duplicate and skip; but when the user imports the same credentials again,
// that is an explicit request to bring this account back and re-evaluate it (issue #618). Here the old state is cleared,
// the runtime account is reloaded and probed again to return it to the pool; if the probe fails again, the account is marked as usual
// as unauthorized / error, at the cost of a single upstream probe.
//
// If the account itself is in a normal state (including a rate limit cooldown), it does nothing and returns false, so the caller counts it as an ordinary duplicate
// count: re-importing a normal account should have no side effects.
func (h *Handler) reviveReimportedAccount(ctx context.Context, row *database.AccountRow, source string) bool {
	if h == nil || h.db == nil || row == nil || !accountErrorStateNeedsReset(row) {
		return false
	}
	if !h.clearReimportedAccountErrorState(ctx, row, "duplicate import revive") {
		return false
	}
	if h.store != nil {
		// reloadTokenAccount removes the account first and then rebuilds it from the database; if the rebuild fails, the account disappears from the runtime pool.
		// In that case it must not report "revived" externally: put the original runtime object back (it is still in an error state, consistent with
		// the not-revived outcome), and return false so the caller counts it as an ordinary duplicate.
		prev := h.store.FindByID(row.ID)
		if err := h.reloadTokenAccount(ctx, row.ID, source); err != nil {
			log.Printf("reloading runtime for account %d after duplicate import revive failed; keeping the original runtime state: %v", row.ID, err)
			if prev != nil && h.store.FindByID(row.ID) == nil {
				h.store.AddAccount(prev)
			}
			return false
		}
		if acc := h.store.FindByID(row.ID); acc != nil &&
			acc.GetAccessToken() == "" && !acc.IsCodexAgentIdentity() &&
			!h.store.GetLazyMode() && strings.HasPrefix(source, "import") {
			// Reloads from import sources do not refresh the token automatically (reloadTokenAccount leaves import* sources
			// to the import flow to queue), so a bare RT account is handled here and goes through the import concurrency gate.
			id := row.ID
			h.runImportProbeTask(func(ctx context.Context) {
				h.refreshImportedAccountAndProbe(ctx, id, source+"_refresh")
			})
		}
	}
	h.db.InsertAccountEventAsync(row.ID, "updated", source+"_revive")
	log.Printf("duplicate import hit existing account %d in an abnormal state; cleared the error/401 state and re-probing (source=%s)", row.ID, source)
	return true
}

func (h *Handler) loadInsertedTokenAccount(id int64, proxyURL string, seed tokenCredentialSeed, source string) {
	if h == nil || h.store == nil {
		return
	}
	newAcc := h.newCodexAccountFromSeed(id, proxyURL, seed)
	h.store.AddAccount(newAcc)
	h.triggerTokenAccountProbe(id, source)
}

func (h *Handler) reloadTokenAccount(ctx context.Context, id int64, source string) error {
	if h == nil || h.store == nil {
		return nil
	}
	h.store.RemoveAccount(id)
	if err := h.store.LoadAccountByID(ctx, id); err != nil {
		log.Printf("failed to reload runtime after updating account %d: %v", id, err)
		return err
	}
	h.triggerTokenAccountProbe(id, source)
	return nil
}

func (h *Handler) triggerTokenAccountProbe(id int64, source string) {
	if h == nil || h.store == nil {
		return
	}
	if account := h.store.FindByID(id); account != nil && account.GetAccessToken() != "" {
		h.triggerImportedAccountUsageProbe(id, source)
	} else if !h.store.GetLazyMode() && !strings.HasPrefix(source, "import") {
		h.startDBBackgroundTask(func(ctx context.Context) {
			h.refreshImportedAccountAndProbe(ctx, id, source+"_refresh")
		})
	}
}

// UpdateOAuthAccountCode updates the authorization parameters of an existing OAuth account using an authorization code.
// POST /api/admin/accounts/:id/oauth/exchange-code
func (h *Handler) UpdateOAuthAccountCode(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		State     string `json:"state"`
		ProxyURL  string `json:"proxy_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Code = strings.TrimSpace(req.Code)
	req.State = strings.TrimSpace(req.State)
	req.ProxyURL = strings.TrimSpace(req.ProxyURL)
	if req.SessionID == "" || req.Code == "" || req.State == "" {
		writeError(c, http.StatusBadRequest, "session_id, code and state are all required")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "Account not found")
			return
		}
		writeInternalError(c, err)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(row.Type), "oauth") {
		writeError(c, http.StatusBadRequest, "The current account is not an OAuth account, so it cannot be re-authorized")
		return
	}

	sess, ok := globalOAuthStore.get(req.SessionID)
	if !ok {
		writeError(c, http.StatusBadRequest, "OAuth session does not exist or has expired (valid for 30 minutes)")
		return
	}
	if req.State != sess.State {
		writeError(c, http.StatusBadRequest, "state does not match; start authorization again")
		return
	}
	if !sess.claimExchange(req.Code, req.State) {
		writeError(c, http.StatusBadRequest, "This authorization session has already been processed or is being processed; start authorization again")
		return
	}
	defer sess.completeExchange(&oauthExchangeResult{Error: "Start authorization again"})

	proxyURL := sess.ProxyURL
	if req.ProxyURL != "" {
		proxyURL = req.ProxyURL
	}
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(row.ProxyURL)
	}
	if proxyURL == "" && h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}

	resinAccountID := fmt.Sprintf("%d", id)
	tokenResp, accountInfo, err := doOAuthCodeExchange(c.Request.Context(), req.Code, sess.CodeVerifier, sess.RedirectURI, proxyURL, resinAccountID)
	if err != nil {
		writeError(c, http.StatusBadGateway, "Authorization code exchange failed: "+err.Error())
		return
	}
	globalOAuthStore.delete(req.SessionID)

	if tokenResp.RefreshToken == "" {
		writeError(c, http.StatusBadGateway, "The authorization server did not return a refresh_token; confirm the offline_access scope is enabled")
		return
	}

	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
		refreshToken:  tokenResp.RefreshToken,
		accessToken:   tokenResp.AccessToken,
		idToken:       tokenResp.IDToken,
		expiresIn:     tokenResp.ExpiresIn,
		customHeaders: row.GetCredentialStringMap("custom_headers"),
	})
	if seed.email != "" && effectiveWorkspaceIDFromSeed(seed) != "" {
		h.mergeDuplicateMu.Lock()
		defer h.mergeDuplicateMu.Unlock()
	}
	if duplicateID, err := h.findOAuthIdentityDuplicate(ctx, seed, id); err != nil {
		writeInternalError(c, err)
		return
	} else if duplicateID > 0 {
		writeError(c, http.StatusConflict, oauthIdentityDuplicateMessage(duplicateID))
		return
	}
	if err := h.db.UpdateOAuthAccountCredentials(ctx, id, tokenCredentialMap(seed), proxyURL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "Account not found")
			return
		}
		writeError(c, http.StatusInternalServerError, "Failed to write token to the database: "+err.Error())
		return
	}
	// Aligned with the re-import merge path (upsertOAuthIdentityAccount): once new credentials are obtained,
	// the error / 401 state is cleared in place and left for a probe to re-judge, instead of leaving the account stuck at "abnormal"
	// waiting on one asynchronous probe that may fail (issue #493). Rate limit cooldowns are outside the scope of the clearing.
	h.clearReimportedAccountErrorState(ctx, row, "re-authorize")

	if err := h.reloadTokenAccount(ctx, id, "oauth_reauth"); err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to reload runtime account: "+err.Error())
		return
	}
	h.db.InsertAccountEventAsync(id, "updated", "oauth_reauth")

	email := ""
	planType := ""
	if accountInfo != nil {
		email = accountInfo.Email
		planType = accountInfo.PlanType
	}
	if email == "" {
		email = seed.email
	}
	if planType == "" {
		planType = seed.planType
	}

	c.JSON(http.StatusOK, gin.H{
		"message":   "OAuth account authorization parameters updated successfully",
		"id":        id,
		"email":     email,
		"plan_type": planType,
	})
}

// ==================== Internal HTTP calls ====================

type rawOAuthTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func doOAuthCodeExchange(ctx context.Context, code, codeVerifier, redirectURI, proxyURL string, resinTempID ...string) (*rawOAuthTokenResp, *auth.AccountInfo, error) {
	form := neturl.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", oauthClientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", codeVerifier)

	// Resin reverse-proxy mode: rewrite the URL
	targetURL := oauthTokenURL
	tempID := ""
	if len(resinTempID) > 0 {
		tempID = resinTempID[0]
	}
	if proxy.IsResinEnabled() && tempID != "" {
		targetURL = proxy.BuildReverseProxyURL(oauthTokenURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	// Resin reverse proxy: inject the temporary account identity header
	if proxy.IsResinEnabled() && tempID != "" {
		req.Header.Set("X-Resin-Account", tempID)
	}

	var client *http.Client
	if proxy.IsResinEnabled() && tempID != "" {
		client = &http.Client{Timeout: 30 * time.Second}
	} else {
		client = auth.BuildUTLSHTTPClient(proxyURL)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("token exchange failed (HTTP %d)", resp.StatusCode)
	}

	var tokenResp rawOAuthTokenResp
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, nil, fmt.Errorf("failed to parse response: %w", err)
	}
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return nil, nil, fmt.Errorf("token exchange response is missing access_token")
	}

	info := accountInfoFromTokens(tokenResp.IDToken, tokenResp.AccessToken)
	return &tokenResp, info, nil
}

// ==================== OAuth automatic callback capture ====================

// OAuthCallback receives the OpenAI OAuth callback, automatically completes the code exchange and adds the account
// GET /auth/callback?code=xxx&state=xxx
func (h *Handler) OAuthCallback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")

	if code == "" || state == "" {
		c.HTML(http.StatusBadRequest, "", nil)
		c.String(http.StatusBadRequest, oauthCallbackPage("Authorization failed", "missing code or state parameter", false))
		return
	}

	sessionID, sess, ok := globalOAuthStore.findByState(state)
	if !ok {
		c.String(http.StatusBadRequest, oauthCallbackPage("Authorization failed", "OAuth session does not exist or has expired; start authorization again", false))
		return
	}
	if !sess.claimExchange(code, state) {
		c.String(http.StatusBadRequest, oauthCallbackPage("Authorization failed", "This authorization callback was already processed; start authorization again", false))
		return
	}
	result := &oauthExchangeResult{Error: "Authorization failed; start authorization again"}
	defer func() { sess.completeExchange(result) }()

	// Run the code exchange (Resin temporary identity)
	proxyURL := sess.ProxyURL
	if proxyURL == "" {
		proxyURL = h.store.GetProxyURL()
	}
	resinTempID := "oauth-" + sessionID
	tokenResp, accountInfo, err := doOAuthCodeExchange(c.Request.Context(), code, sess.CodeVerifier, sess.RedirectURI, proxyURL, resinTempID)
	if err != nil {
		log.Printf("OAuth callback token exchange failed: %s", security.SanitizeLog(err.Error()))
		result = &oauthExchangeResult{
			Success: false,
			Error:   "Token exchange failed; start authorization again",
		}
		c.String(http.StatusOK, oauthCallbackPage("Authorization failed", result.Error, false))
		return
	}

	if tokenResp.RefreshToken == "" {
		result = &oauthExchangeResult{
			Success: false,
			Error:   "The authorization server did not return a refresh_token",
		}
		c.String(http.StatusOK, oauthCallbackPage("Authorization failed", "refresh_token was not obtained; confirm offline_access is enabled", false))
		return
	}
	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
		refreshToken: tokenResp.RefreshToken,
		accessToken:  tokenResp.AccessToken,
		idToken:      tokenResp.IDToken,
		expiresIn:    tokenResp.ExpiresIn,
	})

	// Sessions started from the public self-service portal must go through pending review on entry; they must not, because the state is carried over to the public
	// /auth/callback, bypass the enabled=false branch of SubmitAccountPortalCode.
	if sess.SelfService {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		defer cancel()
		name := seed.email
		if name == "" {
			name = "self-service-account"
		}
		id, err := h.upsertSelfServiceAccount(ctx, name, proxyURL, seed, sess.ContactEmail)
		if err == errDuplicateOAuthIdentity {
			result = &oauthExchangeResult{
				Success: true,
				Message: "This account is already in the system; no need to submit again",
			}
			c.String(http.StatusOK, oauthCallbackPage("Submitted successfully", "This account is already in the system; no need to submit it again.", true))
			return
		}
		if err != nil {
			log.Printf("OAuth callback account write failed: %s", security.SanitizeLog(err.Error()))
			result = &oauthExchangeResult{
				Success: false,
				Error:   "Failed to save the account; contact the administrator and start authorization again",
			}
			c.String(http.StatusOK, oauthCallbackPage("Authorization failed", result.Error, false))
			return
		}
		email := seed.email
		if accountInfo != nil && accountInfo.Email != "" {
			email = accountInfo.Email
		}
		security.SecurityAuditLog("ACCOUNT_PORTAL_SUBMITTED", "ip="+security.SanitizeLog(c.ClientIP())+" contact="+security.SanitizeLog(sess.ContactEmail)+" account="+security.SanitizeLog(email))
		result = &oauthExchangeResult{
			Success: true,
			Message: "Account submitted; waiting for administrator review",
			ID:      id,
			Email:   email,
		}
		log.Printf("self-service portal callback added a pending-review account: id=%d email=%s", id, email)
		c.String(http.StatusOK, oauthCallbackPage("Submitted successfully", "Submitted. It will take effect after administrator review. Thank you for your contribution!", true))
		return
	}

	// Add the account automatically
	name := ""
	if seed.email != "" {
		name = seed.email
	}
	if name == "" {
		name = "oauth-account"
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	id, updated, err := h.upsertOAuthIdentityAccount(ctx, name, proxyURL, seed, "oauth_callback")
	if err != nil {
		log.Printf("OAuth callback account write failed: %s", security.SanitizeLog(err.Error()))
		result = &oauthExchangeResult{
			Success: false,
			Error:   "Failed to save the account; contact the administrator and start authorization again",
		}
		c.String(http.StatusOK, oauthCallbackPage("Authorization failed", result.Error, false))
		return
	}

	if proxy.IsResinEnabled() && !updated {
		go proxy.InheritLease(resinTempID, fmt.Sprintf("%d", id))
	}

	email := ""
	planType := ""
	if accountInfo != nil {
		email = accountInfo.Email
		planType = accountInfo.PlanType
	}
	if email == "" {
		email = seed.email
	}
	if planType == "" {
		planType = seed.planType
	}

	message := fmt.Sprintf("Account %s added successfully", name)
	pageMessage := fmt.Sprintf("Account %s was added automatically. You can close this page.", name)
	if updated {
		message = fmt.Sprintf("Account already exists; updated account %d", id)
		pageMessage = fmt.Sprintf("Account already exists; updated account %d. You can close this page.", id)
	}
	result = &oauthExchangeResult{
		Success:  true,
		Message:  message,
		ID:       id,
		Email:    email,
		PlanType: planType,
	}

	log.Printf("OAuth callback automatically added account successfully: id=%d email=%s", id, email)
	c.String(http.StatusOK, oauthCallbackPage("Authorization succeeded", pageMessage, true))
}

// PollOAuthCallback is polled by the frontend for the callback result
// GET /api/admin/oauth/poll-callback?session_id=xxx
func (h *Handler) PollOAuthCallback(c *gin.Context) {
	sessionID := c.Query("session_id")
	if sessionID == "" {
		writeError(c, http.StatusBadRequest, "session_id is required")
		return
	}

	sess, ok := globalOAuthStore.get(sessionID)
	if !ok {
		writeError(c, http.StatusNotFound, "OAuth session does not exist or has expired")
		return
	}

	result, processing := sess.callbackSnapshot()
	if result != nil {
		// Callback completed: return the result and clean up the session
		c.JSON(http.StatusOK, gin.H{
			"status": "completed",
			"result": result,
		})
		globalOAuthStore.delete(sessionID)
		return
	}

	if processing {
		// Callback received but the exchange is not finished yet (rare race)
		c.JSON(http.StatusOK, gin.H{
			"status": "processing",
		})
		return
	}

	// Callback not received yet
	c.JSON(http.StatusOK, gin.H{
		"status": "waiting",
	})
}

// oauthCallbackPage generates a simple HTML callback result page.
// title/message may come from database errors or token claims, so they must be escaped before insertion.
func oauthCallbackPage(title, message string, success bool) string {
	color := "#e53e3e"
	icon := "&#10060;"
	if success {
		color = "#38a169"
		icon = "&#10004;"
	}
	title = html.EscapeString(title)
	message = html.EscapeString(message)
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>%s</title>
<style>
body{font-family:-apple-system,sans-serif;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0;background:#f7fafc}
.card{background:#fff;border-radius:12px;padding:40px;box-shadow:0 4px 20px rgba(0,0,0,.08);text-align:center;max-width:420px}
.icon{font-size:48px;margin-bottom:16px}
h1{color:%s;font-size:24px;margin:0 0 12px}
p{color:#4a5568;line-height:1.6;margin:0}
</style></head>
<body><div class="card"><div class="icon">%s</div><h1>%s</h1><p>%s</p></div></body></html>`,
		title, color, icon, title, message)
}
