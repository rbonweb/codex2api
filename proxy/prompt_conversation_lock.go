package proxy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	promptConversationLockedReasonCode = "conversation_cyber_locked"
	promptFingerprintReplayReasonCode  = "fingerprint_replay_cooldown"
	promptConversationLockedMessage    = "The current conversation is locked because of upstream CYB. This lock block does not count penalties again; wait for it to expire automatically, or have an administrator unlock it manually in 'Prompt Filter → Risk Profiles → session details'. Triggering CYB again after the lock is lifted may get the account deactivated."
	promptUserCyberCooldownReasonCode  = "user_cyber_cooldown"
	promptUserCyberCooldownMessage     = "This user is currently in a security cooldown period because of upstream CYB. New requests during the cooldown are not forwarded further and do not count penalties again; wait for it to expire automatically, or have an administrator lift the cooldown manually in 'Prompt Filter → Risk Profiles → user details'."
	promptConversationLockCacheTTL     = 30 * time.Second
)

type promptCyberRestriction struct {
	ReasonCode        string
	Message           string
	Scope             string
	LockedAt          time.Time
	ExpiresAt         time.Time
	RetryAfterSeconds int64
	IncidentID        string
	DecisionID        string
	RequestID         string
	AuditReference    string
	TriggerReasonCode string
}

type promptConversationLockIdentity struct {
	Kind               string
	LockKey            string
	Platform           string
	NewAPIUserID       string
	SessionFingerprint string
	SessionHash        string
}

func verifiedPromptUserCooldownIdentity(c *gin.Context, policyContext verifiedNewAPIPolicyContext) (promptConversationLockIdentity, bool) {
	if c == nil || !policyContext.MetaVerified {
		return promptConversationLockIdentity{}, false
	}
	platform := normalizedNewAPIPlatform(policyContext.Platform)
	userID := strings.TrimSpace(policyContext.Identity.UserID)
	if platform == "" || userID == "" {
		return promptConversationLockIdentity{}, false
	}
	digest := sha256.Sum256([]byte("prompt-user-cooldown-v1\x00" + platform + "\x00" + userID))
	return promptConversationLockIdentity{
		Kind:    database.PromptConversationLockIdentityNewAPI,
		LockKey: hex.EncodeToString(digest[:]), Platform: platform, NewAPIUserID: userID,
	}, true
}

// promptConversationLockFallbackPlatform marks the degraded identity source, so it is not confused with a real NewAPI
// platform code.
const promptConversationLockFallbackPlatform = "codex-local"

// promptSessionFingerprint32 produces a 32-digit hexadecimal session fingerprint that follows the same length convention as the NewAPI pass-through fingerprint,
// so the degraded identity can reuse the same storage and validation invariants.
func promptSessionFingerprint32(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("prompt-conversation-session-v1\x00" + strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:16])
}

// promptConversationSessionSignal extracts the stable session identifier that the Codex client sends itself. Codex
// requests always carry x-codex-* headers, where the window-id / installation-id headers together with the session-id header
// form a session identity that does not depend on NewAPI.
func promptConversationSessionSignal(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if value := promptSessionID(c); value != "" {
		return value
	}
	body := ingressRequestBody(c, nil)
	for _, path := range []string{"client_metadata.x-codex-window-id", "client_metadata.x-codex-installation-id"} {
		if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); value != "" {
			return value
		}
	}
	for _, name := range []string{"X-Codex-Window-Id", "X-Codex-Installation-Id", "Thread-Id"} {
		if value := strings.TrimSpace(c.GetHeader(name)); value != "" {
			return value
		}
	}
	return ""
}

// promptConversationLockFallbackSessionHash unifies the session
// hash derivation for the codex-local degraded identity. The lock table and the audit log must use the same signal extraction and fingerprint path, so the admin panel can use the audit
// record's session_hash to locate and unlock the matching conversation.
func promptConversationLockFallbackSessionHash(c *gin.Context) string {
	return hashRiskIdentity(promptSessionFingerprint32(promptConversationSessionSignal(c)))
}

func verifiedPromptConversationLockIdentity(c *gin.Context, policyContext verifiedNewAPIPolicyContext) (promptConversationLockIdentity, bool) {
	if c == nil || !policyContext.MetaVerified {
		return promptConversationLockIdentity{}, false
	}
	platform := normalizedNewAPIPlatform(policyContext.Platform)
	userID := strings.TrimSpace(policyContext.Identity.UserID)
	fingerprint := strings.ToLower(strings.TrimSpace(policyContext.Meta.SessionFingerprint))
	if platform == "" || userID == "" || len(fingerprint) != 32 {
		return promptConversationLockIdentity{}, false
	}
	lockMaterial := "prompt-conversation-lock-v1\x00" + platform + "\x00" + userID + "\x00" + fingerprint
	if policyContext.Meta.ChannelID > 0 {
		lockMaterial = "prompt-conversation-lock-v2\x00" + platform + "\x00" + userID + "\x00" + strconv.Itoa(policyContext.Meta.ChannelID) + "\x00" + fingerprint
	}
	digest := sha256.Sum256([]byte(lockMaterial))
	return promptConversationLockIdentity{
		Kind:    database.PromptConversationLockIdentityNewAPI,
		LockKey: hex.EncodeToString(digest[:]), Platform: platform, NewAPIUserID: userID,
		SessionFingerprint: fingerprint, SessionHash: hashRiskIdentity(fingerprint),
	}, true
}

func promptConversationLockTTL(cfg promptfilter.Config) time.Duration {
	hours := promptfilter.NormalizeAdvancedConfig(cfg.Advanced).Enforcement.ConversationLockTTLHours
	return time.Duration(hours) * time.Hour
}

func promptUserCyberCooldownTTL(cfg promptfilter.Config) time.Duration {
	minutes := promptfilter.NormalizeAdvancedConfig(cfg.Advanced).Enforcement.UserCyberCooldownMinutes
	return time.Duration(minutes) * time.Minute
}

func promptConversationLockExpired(item *database.PromptConversationLock, ttl time.Duration) bool {
	return item == nil || (ttl > 0 && !item.LockedAt.After(time.Now().UTC().Add(-ttl)))
}

func promptCyberRestrictionDecision(item *database.PromptConversationLock, cfg promptfilter.Config) promptCyberRestriction {
	result := promptCyberRestriction{
		ReasonCode: promptConversationLockedReasonCode,
		Message:    promptConversationLockedMessage,
		Scope:      database.PromptConversationRestrictionScopeConversation,
	}
	ttl := promptConversationLockTTL(cfg)
	if item != nil {
		result.LockedAt = item.LockedAt.UTC()
		result.IncidentID = strings.TrimSpace(item.IncidentID)
		result.DecisionID = strings.TrimSpace(item.DecisionID)
		result.RequestID = strings.TrimSpace(item.RequestID)
		result.TriggerReasonCode = strings.TrimSpace(item.ReasonCode)
		result.AuditReference = result.IncidentID
		if result.AuditReference == "" {
			result.AuditReference = result.RequestID
		}
		// Older local locks did not store a separate request_id, but decision_id has always used
		// local-block:<request_correlation_id>. After compatible derivation, historical locks can also directly
		// paste it to locate the original audit log without waiting for new data to be produced.
		if result.AuditReference == "" && strings.HasPrefix(result.DecisionID, "local-block:") {
			result.AuditReference = strings.TrimSpace(strings.TrimPrefix(result.DecisionID, "local-block:"))
		}
		if item.ReasonCode == promptUserCyberCooldownReasonCode {
			result.ReasonCode = promptUserCyberCooldownReasonCode
			result.Scope = database.PromptConversationRestrictionScopeUserCooldown
			ttl = promptUserCyberCooldownTTL(cfg)
		} else if item.IdentityKind == database.PromptConversationLockIdentityFingerprintReplay || item.ReasonCode == promptFingerprintReplayReasonCode {
			result.ReasonCode = promptFingerprintReplayReasonCode
			result.Scope = database.PromptConversationRestrictionScopeFingerprintReplay
			ttl = promptUserCyberCooldownTTL(cfg)
		}
	}
	if !result.LockedAt.IsZero() && ttl > 0 {
		result.ExpiresAt = result.LockedAt.Add(ttl)
		remaining := time.Until(result.ExpiresAt)
		if remaining > 0 {
			result.RetryAfterSeconds = int64((remaining + time.Second - 1) / time.Second)
		}
	}
	remainingText := promptCyberRestrictionRemainingText(result.RetryAfterSeconds)
	auditText := ""
	if result.AuditReference != "" {
		auditText = fmt.Sprintf("audit ID: %s; ", result.AuditReference)
	}
	if result.Scope == database.PromptConversationRestrictionScopeUserCooldown {
		result.Message = fmt.Sprintf("This user entered a security cooldown because of upstream CYB, about %s remaining; all new requests during the cooldown are not forwarded and do not count penalties again. %sAdministrators can lift the cooldown in 'Prompt Filter → Risk Profiles → user details'. Error code: %s.", remainingText, auditText, result.ReasonCode)
	} else if result.Scope == database.PromptConversationRestrictionScopeFingerprintReplay {
		result.Message = fmt.Sprintf("The same prompt fingerprint recently received upstream CYB, so the current request entered an exact replay cooldown, about %s remaining; the whole API key is not banned. %sAdministrators can unlock it manually in 'Prompt Filter → Risk Profiles → session details'. Error code: %s.", remainingText, auditText, result.ReasonCode)
	} else if result.TriggerReasonCode != "" && result.TriggerReasonCode != newAPIUpstreamCyberPolicyReasonCode {
		result.Message = fmt.Sprintf("The current conversation is locked by a local high-risk rule, about %s remaining; later requests are not forwarded and do not count penalties again. Trigger reason: %s; %sAdministrators can review and unlock it manually in 'Prompt Filter → Risk Profiles → session details'. Error code: %s.", remainingText, result.TriggerReasonCode, auditText, result.ReasonCode)
	} else {
		result.Message = fmt.Sprintf("The current conversation is locked because of upstream CYB, about %s remaining; later requests are not forwarded and do not count penalties again. %sAdministrators can unlock it manually in 'Prompt Filter → Risk Profiles → session details'. Error code: %s.", remainingText, auditText, result.ReasonCode)
	}
	return result
}

func promptCyberRestrictionRemainingText(seconds int64) string {
	if seconds <= 0 {
		return "under 1 minute"
	}
	minutes := (seconds + 59) / 60
	if minutes < 60 {
		return fmt.Sprintf("%d minutes", minutes)
	}
	hours := minutes / 60
	remainingMinutes := minutes % 60
	if remainingMinutes == 0 {
		return fmt.Sprintf("%d hours", hours)
	}
	return fmt.Sprintf("%d hours %d minutes", hours, remainingMinutes)
}

func promptCyberRestrictionDetails(restriction promptCyberRestriction, signedDetails gin.H) gin.H {
	details := gin.H{}
	for key, value := range signedDetails {
		details[key] = value
	}
	details["reason_code"] = restriction.ReasonCode
	details["restriction_scope"] = restriction.Scope
	details["retry_after_seconds"] = restriction.RetryAfterSeconds
	details["manual_unlock_path"] = "/admin/prompt-filter/profiles"
	if !restriction.LockedAt.IsZero() {
		details["locked_at"] = restriction.LockedAt.Format(time.RFC3339)
	}
	if !restriction.ExpiresAt.IsZero() {
		details["expires_at"] = restriction.ExpiresAt.Format(time.RFC3339)
	}
	if restriction.IncidentID != "" {
		details["incident_id"] = restriction.IncidentID
	}
	if restriction.DecisionID != "" {
		details["decision_id"] = restriction.DecisionID
	}
	if restriction.RequestID != "" {
		details["request_id"] = restriction.RequestID
	}
	if restriction.AuditReference != "" {
		details["audit_reference"] = restriction.AuditReference
	}
	if restriction.TriggerReasonCode != "" {
		details["trigger_reason_code"] = restriction.TriggerReasonCode
	}
	return details
}

func promptCyberRestrictionAPIError(restriction promptCyberRestriction, signedDetails gin.H) *api.APIError {
	return api.NewAPIErrorWithDetails(
		api.ErrorCode(restriction.ReasonCode), restriction.Message,
		api.ErrorTypeInvalidRequest, promptCyberRestrictionDetails(restriction, signedDetails),
	)
}

func writePromptCyberRestrictionHeaders(c *gin.Context, restriction promptCyberRestriction) {
	if c == nil {
		return
	}
	c.Header("X-Codex2API-Policy-Restriction-Scope", restriction.Scope)
	if restriction.RetryAfterSeconds > 0 {
		c.Header("Retry-After", strconv.FormatInt(restriction.RetryAfterSeconds, 10))
	}
	if !restriction.ExpiresAt.IsZero() {
		c.Header("X-Codex2API-Policy-Restriction-Expires-At", restriction.ExpiresAt.Format(time.RFC3339))
	}
}

func (h *Handler) sendNewAPIPromptCyberRestriction(c *gin.Context, cfg promptfilter.Config, decision promptfilter.Decision, verdict promptfilter.Verdict, body []byte, endpoint, model string, signedBody []byte, restriction promptCyberRestriction) bool {
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody)
	if !verified {
		return false
	}
	metadata := buildNewAPIPolicyDecisionMetadataWithSecret(policyContext.Identity, decision, verdict, cfg, body, endpoint, model, "", policyContext.VerificationSecret)
	writeNewAPIPolicyDecisionHeaders(c, metadata)
	writePromptCyberRestrictionHeaders(c, restriction)
	apiErr := promptCyberRestrictionAPIError(restriction, newAPIPolicyDecisionDetails(metadata))
	api.SendErrorWithStatus(c, apiErr, http.StatusBadRequest)
	return true
}

func promptCyberRestrictionLock(item *database.PromptConversationLock, exactConversation bool) *database.PromptConversationLock {
	if item == nil {
		return nil
	}
	copy := *item
	if exactConversation {
		if item.IdentityKind == database.PromptConversationLockIdentityFingerprintReplay {
			copy.ReasonCode = promptFingerprintReplayReasonCode
		} else {
			copy.ReasonCode = promptConversationLockedReasonCode
		}
	} else {
		copy.ReasonCode = promptUserCyberCooldownReasonCode
	}
	return &copy
}

// promptConversationLockFallbackIdentity, used when there is no NewAPI pass-through signature, uses the downstream
// API key plus the Codex session identifier to form the lock identity. Without this fallback path, deployments that are not connected to NewAPI
// cannot lock a session at all, so blocking works only request by request and cannot shut down a session that is probing.
func promptConversationLockFallbackIdentity(c *gin.Context) (promptConversationLockIdentity, bool) {
	if c == nil {
		return promptConversationLockIdentity{}, false
	}
	apiKeyID := requestAPIKeyID(c)
	fingerprint := promptSessionFingerprint32(promptConversationSessionSignal(c))
	if apiKeyID == 0 || len(fingerprint) != 32 {
		return promptConversationLockIdentity{}, false
	}
	subject := fmt.Sprintf("apikey:%d", apiKeyID)
	digest := sha256.Sum256([]byte("prompt-conversation-lock-v1\x00" +
		database.PromptConversationLockIdentityCodexSession + "\x00" + subject + "\x00" + fingerprint))
	return promptConversationLockIdentity{
		Kind:               database.PromptConversationLockIdentityCodexSession,
		LockKey:            hex.EncodeToString(digest[:]),
		Platform:           promptConversationLockFallbackPlatform,
		NewAPIUserID:       subject,
		SessionFingerprint: fingerprint,
		SessionHash:        promptConversationLockFallbackSessionHash(c),
	}, true
}

// promptConversationFingerprintReplayIdentity is the last-resort identity for
// unsigned requests that have neither a verified NewAPI user nor a stable
// Codex session signal. It requires the client IP hash and scopes the lock to
// the API key, so a shared key cannot be globally blocked.
func (h *Handler) promptConversationFingerprintReplayIdentity(c *gin.Context, signedBody []byte, endpoint, model string) (promptConversationLockIdentity, bool) {
	if h == nil || c == nil {
		return promptConversationLockIdentity{}, false
	}
	apiKeyID := requestAPIKeyID(c)
	if apiKeyID == 0 {
		return promptConversationLockIdentity{}, false
	}
	audit := h.capturePromptFilterAuditContext(c)
	clientIPHash := strings.TrimSpace(audit.ClientIPHash)
	if clientIPHash == "" {
		return promptConversationLockIdentity{}, false
	}
	fingerprint := h.promptConversationReplayFingerprint(c, signedBody, endpoint, model)
	if len(fingerprint) != sha256.Size*2 {
		return promptConversationLockIdentity{}, false
	}
	subject := fmt.Sprintf("apikey:%d:ip:%s", apiKeyID, clientIPHash)
	scope := strings.Join([]string{subject, fingerprint}, "\x00")
	return promptConversationLockIdentity{
		Kind:               database.PromptConversationLockIdentityFingerprintReplay,
		LockKey:            promptfilter.StableEvidenceFingerprint("prompt-fingerprint-replay-lock", scope),
		Platform:           "codex-fingerprint",
		NewAPIUserID:       subject,
		SessionFingerprint: fingerprint[:32],
		SessionHash:        fingerprint,
	}, true
}

func (h *Handler) promptConversationReplayFingerprint(c *gin.Context, signedBody []byte, endpoint, model string) string {
	if c == nil || h == nil {
		return ""
	}
	if _, exists := c.Get("raw_body"); !exists {
		if body := ingressRequestBody(c, signedBody); len(body) > 0 {
			setRawRequestBody(c, body)
		}
	}
	body := ingressRequestBody(c, signedBody)
	if len(body) == 0 {
		if raw, exists := c.Get("raw_body"); exists {
			body, _ = raw.([]byte)
		}
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = "/v1/responses"
	}
	if strings.TrimSpace(model) == "" {
		model = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	captured, ok := h.promptRuleLearningEvidenceForUpstreamFailure(c, endpoint, model)
	if !ok {
		return ""
	}
	learningSourceText := strings.TrimSpace(envelopeDirectCurrentUserText(captured.Envelope))
	if learningSourceText == "" && captured.PrimaryOrigin == string(promptfilter.OriginApplicationCandidate) {
		learningSourceText = strings.TrimSpace(captured.Text)
	}
	if learningSourceText == "" && len(captured.Envelope.Segments) == 0 {
		learningSourceText = strings.TrimSpace(captured.Text)
	}
	if learningSourceText == "" {
		_, learningSourceText = promptPolicyLearningContext(captured.Envelope)
	}
	return promptfilter.PromptEvidenceFingerprint(learningSourceText)
}

func (h *Handler) unsignedUpstreamCyberLockIdentity(c *gin.Context, signedBody []byte, endpoint, model string) (promptConversationLockIdentity, string, bool) {
	if identity, ok := promptConversationLockFallbackIdentity(c); ok {
		return identity, newAPIUpstreamCyberPolicyReasonCode, true
	}
	if identity, ok := h.promptConversationFingerprintReplayIdentity(c, signedBody, endpoint, model); ok {
		return identity, promptFingerprintReplayReasonCode, true
	}
	return promptConversationLockIdentity{}, "", false
}

// resolvePromptConversationLockIdentity prefers the verified NewAPI identity and, when it is missing,
// falls back to the Codex session identity. Behavior for NewAPI deployments is the same as before the upgrade.
func (h *Handler) resolvePromptConversationLockIdentity(c *gin.Context, cfg promptfilter.Config, signedBody []byte) (promptConversationLockIdentity, bool) {
	if h == nil || c == nil {
		return promptConversationLockIdentity{}, false
	}
	if policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody); verified {
		if identity, ok := verifiedPromptConversationLockIdentity(c, policyContext); ok {
			return identity, true
		}
	}
	return promptConversationLockFallbackIdentity(c)
}

// Existence gate for the fingerprint replay cooldown: the fallback identity needs a full request envelope (the whole body
// traversal and redaction), yet most of the time there is no active fingerprint cooldown globally. A short-TTL cache of the
// conclusion 'is there an active fingerprint lock' replaces the per-request envelope build with a lightweight
// existence query, run at most once every 10 seconds. This instance sets the flag immediately when it creates a fingerprint lock; a new lock from another instance becomes visible only after up to one gate TTL,
// which is acceptable for a best-effort cooldown.
const promptFingerprintReplayGateTTL = 10 * time.Second

func (h *Handler) hasActiveFingerprintReplayLocks(ctx context.Context, cooldownTTL time.Duration) bool {
	if h == nil || h.db == nil {
		return false
	}
	now := time.Now()
	h.fpReplayGateMu.Lock()
	if !h.fpReplayGateAt.IsZero() && now.Sub(h.fpReplayGateAt) < promptFingerprintReplayGateTTL {
		active := h.fpReplayGateActive
		h.fpReplayGateMu.Unlock()
		return active
	}
	h.fpReplayGateMu.Unlock()
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	active, err := h.db.HasActivePromptFingerprintReplayLocks(checkCtx, cooldownTTL)
	if err != nil {
		// A failed query does not cache the conclusion, and lets later checks pass as 'a lock may exist': cooldown semantics take priority over saving CPU.
		return true
	}
	h.fpReplayGateMu.Lock()
	h.fpReplayGateAt = now
	h.fpReplayGateActive = active
	h.fpReplayGateMu.Unlock()
	return active
}

func (h *Handler) markFingerprintReplayLockCreated() {
	if h == nil {
		return
	}
	h.fpReplayGateMu.Lock()
	h.fpReplayGateAt = time.Now()
	h.fpReplayGateActive = true
	h.fpReplayGateMu.Unlock()
}

func (h *Handler) activePromptConversationLock(c *gin.Context, cfg promptfilter.Config, signedBody []byte, endpoint, model string) (*database.PromptConversationLock, bool) {
	if h == nil || h.db == nil || c == nil || !cfg.Advanced.Enforcement.ConversationLockEnabled {
		return nil, false
	}
	lockTTL := promptConversationLockTTL(cfg)
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody)
	if verified {
		userIdentity, ok := verifiedPromptUserCooldownIdentity(c, policyContext)
		if !ok {
			return nil, false
		}
		conversationIdentity, hasConversationIdentity := verifiedPromptConversationLockIdentity(c, policyContext)
		lockKey := ""
		if hasConversationIdentity {
			lockKey = conversationIdentity.LockKey
		}
		if hasConversationIdentity && h.cache != nil {
			if raw, found, err := h.cache.GetRuntime(c.Request.Context(), database.PromptConversationLockCacheNamespace, conversationIdentity.LockKey); err == nil && found {
				var item database.PromptConversationLock
				if json.Unmarshal(raw, &item) == nil && item.Status == database.PromptConversationLockStatusActive && !promptConversationLockExpired(&item, lockTTL) {
					return promptCyberRestrictionLock(&item, true), true
				}
				_ = h.cache.DeleteRuntime(c.Request.Context(), database.PromptConversationLockCacheNamespace, conversationIdentity.LockKey)
			}
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		item, exactConversation, err := h.db.GetActivePromptConversationRestriction(
			ctx, lockKey, userIdentity.Platform, userIdentity.NewAPIUserID,
			lockTTL, promptUserCyberCooldownTTL(cfg),
		)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false
		}
		if err != nil {
			log.Printf("check prompt conversation restriction failed identity=%s: %v", userIdentity.LockKey[:12], err)
			return nil, false
		}
		if exactConversation {
			h.cachePromptConversationLock(c.Request.Context(), item, lockTTL)
		}
		return promptCyberRestrictionLock(item, exactConversation), true
	}

	// Deployments that are not connected to a signed NewAPI lock only the current session by API key + Codex session; they do not apply
	// a user-level cooldown, so different users under a shared key do not affect each other.
	identity, ok := promptConversationLockFallbackIdentity(c)
	if !ok && h.hasActiveFingerprintReplayLocks(c.Request.Context(), promptUserCyberCooldownTTL(cfg)) {
		identity, ok = h.promptConversationFingerprintReplayIdentity(c, signedBody, endpoint, model)
	}
	if !ok {
		return nil, false
	}
	effectiveTTL := lockTTL
	if identity.Kind == database.PromptConversationLockIdentityFingerprintReplay {
		effectiveTTL = promptUserCyberCooldownTTL(cfg)
	}
	if h.cache != nil {
		if raw, found, err := h.cache.GetRuntime(c.Request.Context(), database.PromptConversationLockCacheNamespace, identity.LockKey); err == nil && found {
			var item database.PromptConversationLock
			if json.Unmarshal(raw, &item) == nil && item.Status == database.PromptConversationLockStatusActive && !promptConversationLockExpired(&item, effectiveTTL) {
				return promptCyberRestrictionLock(&item, true), true
			}
			_ = h.cache.DeleteRuntime(c.Request.Context(), database.PromptConversationLockCacheNamespace, identity.LockKey)
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	item, err := h.db.GetActivePromptConversationLockWithTTL(ctx, identity.LockKey, effectiveTTL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		log.Printf("check fallback prompt conversation lock failed identity=%s: %v", identity.LockKey[:12], err)
		return nil, false
	}
	h.cachePromptConversationLock(c.Request.Context(), item, effectiveTTL)
	return promptCyberRestrictionLock(item, true), true
}

func (h *Handler) cachePromptConversationLock(ctx context.Context, item *database.PromptConversationLock, lockTTL time.Duration) {
	if h == nil || h.cache == nil || item == nil || item.Status != database.PromptConversationLockStatusActive {
		return
	}
	cacheTTL := promptConversationLockCacheTTL
	if lockTTL > 0 {
		remaining := time.Until(item.LockedAt.Add(lockTTL))
		if remaining <= 0 {
			return
		}
		if remaining < cacheTTL {
			cacheTTL = remaining
		}
	}
	raw, err := json.Marshal(item)
	if err == nil {
		_ = h.cache.SetRuntime(ctx, database.PromptConversationLockCacheNamespace, item.LockKey, raw, cacheTTL)
	}
}

func (h *Handler) lockPromptConversationAfterUpstreamCYB(c *gin.Context, endpoint, model, incidentID string, metadata newAPIPolicyDecisionMetadata) bool {
	if h == nil || h.db == nil || c == nil || metadata.ReasonCode != newAPIUpstreamCyberPolicyReasonCode || metadata.DecisionID == "" {
		return false
	}
	cfg := h.promptFilterConfigForRequest(c)
	if !cfg.Advanced.Enforcement.ConversationLockEnabled {
		return false
	}
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, ingressRequestBody(c, nil))
	identity := promptConversationLockIdentity{}
	exactConversation := false
	if verified {
		userIdentity, ok := verifiedPromptUserCooldownIdentity(c, policyContext)
		if !ok {
			return false
		}
		identity = userIdentity
		if conversationIdentity, ok := verifiedPromptConversationLockIdentity(c, policyContext); ok {
			identity = conversationIdentity
			exactConversation = true
		}
	} else {
		var ok bool
		identity, ok = promptConversationLockFallbackIdentity(c)
		if !ok {
			return false
		}
		exactConversation = true
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	item, _, err := h.db.LockPromptConversation(ctx, database.PromptConversationLockInput{
		LockKey: identity.LockKey, IdentityKind: identity.Kind,
		Platform: identity.Platform, NewAPIUserID: identity.NewAPIUserID,
		SessionFingerprint: identity.SessionFingerprint, SessionHash: identity.SessionHash,
		IncidentID: incidentID, DecisionID: metadata.DecisionID, RequestID: metadata.RequestID,
		ReasonCode: metadata.ReasonCode, Endpoint: endpoint, Model: model, LockedAt: time.Now().UTC(),
	})
	if err != nil {
		log.Printf("persist prompt conversation lock failed decision=%s: %v", metadata.DecisionID, err)
		return false
	}
	if !exactConversation {
		return false
	}
	h.cachePromptConversationLock(c.Request.Context(), item, promptConversationLockTTL(cfg))
	return item != nil && item.Status == database.PromptConversationLockStatusActive
}

// lockPromptConversationAfterUnsignedUpstreamCYB applies only a scoped
// session/fingerprint replay lock. It never emits a NewAPI strike because the
// request has no verified identity. A stable Codex session gets conversation
// scope; otherwise the exact prompt fingerprint is paired with API key + IP.
func (h *Handler) lockPromptConversationAfterUnsignedUpstreamCYB(c *gin.Context, endpoint, model, incidentID string) bool {
	if h == nil || h.db == nil || c == nil {
		return false
	}
	cfg := h.promptFilterConfigForRequest(c)
	if !cfg.Advanced.Enforcement.ConversationLockEnabled {
		return false
	}
	signedBody := ingressRequestBody(c, nil)
	identity, reasonCode, ok := h.unsignedUpstreamCyberLockIdentity(c, signedBody, endpoint, model)
	if !ok {
		return false
	}
	requestID := ensurePromptPolicyRequestCorrelationID(c)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	item, _, err := h.db.LockPromptConversation(ctx, database.PromptConversationLockInput{
		LockKey: identity.LockKey, IdentityKind: identity.Kind,
		Platform: identity.Platform, NewAPIUserID: identity.NewAPIUserID,
		SessionFingerprint: identity.SessionFingerprint, SessionHash: identity.SessionHash,
		IncidentID: incidentID, DecisionID: "upstream-cy:" + requestID, RequestID: requestID,
		ReasonCode: reasonCode, Endpoint: endpoint, Model: model, LockedAt: time.Now().UTC(),
	})
	if err != nil {
		log.Printf("persist unsigned upstream CY replay lock failed request=%s: %v", requestID, err)
		return false
	}
	lockTTL := promptConversationLockTTL(cfg)
	if identity.Kind == database.PromptConversationLockIdentityFingerprintReplay {
		lockTTL = promptUserCyberCooldownTTL(cfg)
		h.markFingerprintReplayLockCreated()
	}
	h.cachePromptConversationLock(c.Request.Context(), item, lockTTL)
	return item != nil && item.Status == database.PromptConversationLockStatusActive
}

// promptGuardBlockHasLocalEvidence reports whether the final block is supported by local detection evidence (rule hits /
// final category / classifier signals). A block produced only by the external review layer, where the review model flagged a request that local checks allowed, or where the review call itself failed under fail-closed,
// only rejects that one request and does not trigger a session
// lock. Probabilistic review scores (such as per-category moderation thresholds) produce false positives for normal sessions,
// and finalizePromptGuardDecision already classifies such hits as weak evidence that does not count toward violation strikes.
// A single false positive would lock an entire session for seven days, a cost that does not match the strength of the evidence (issue #527).
func promptGuardBlockHasLocalEvidence(decision promptfilter.Decision, verdict promptfilter.Verdict) bool {
	if decision.PrimaryDetector == promptGuardDetectorExternalReview {
		return false
	}
	if verdict.Reviewed && verdict.ReviewError != "" && len(verdict.Matched) == 0 &&
		!verdict.TerminalStrictHit && !verdict.TerminalCategoryHit {
		return false
	}
	return true
}

// lockPromptConversationOnLocalBlock locks the session immediately when a local rule decides to block,
// so that the risk is stopped **before it is sent to the upstream provider**.
//
// Why it must come first: a pure regex single-turn hit rate is limited (see the promptfilter adversarial baseline, where 12 bypass
// variants, only 3 of them were blocked). An attacker only needs to reword, split words, or switch languages to get the next request past the local
// rules and onto upstream, producing a real cyber_policy account-ban signal. Locking only after upstream returns CYB means the
// risk has already leaked. Session-level blocking turns 'hit each request' into 'one hit shuts down the whole session', which is the one
// layer in defense in depth that covers unknown variants.
func (h *Handler) lockPromptConversationOnLocalBlock(c *gin.Context, cfg promptfilter.Config, signedBody []byte, endpoint, model string, decision promptfilter.Decision, verdict promptfilter.Verdict) bool {
	if h == nil || h.db == nil || c == nil || !cfg.Advanced.Enforcement.ConversationLockEnabled {
		return false
	}
	if !promptGuardBlockHasLocalEvidence(decision, verdict) {
		return false
	}
	identity, ok := h.resolvePromptConversationLockIdentity(c, cfg, signedBody)
	if !ok {
		return false
	}
	reasonCode := strings.TrimSpace(decision.ReasonCode)
	if reasonCode == "" {
		reasonCode = "local_prompt_block"
	}
	// Each local block uses its own decision ID, so trigger_count accurately reflects the number of hits;
	// a replay of the same request stays idempotent.
	decisionID := "local-block:" + ensurePromptPolicyRequestCorrelationID(c)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	item, _, err := h.db.LockPromptConversation(ctx, database.PromptConversationLockInput{
		LockKey: identity.LockKey, IdentityKind: identity.Kind,
		Platform: identity.Platform, NewAPIUserID: identity.NewAPIUserID,
		SessionFingerprint: identity.SessionFingerprint, SessionHash: identity.SessionHash,
		DecisionID: decisionID, ReasonCode: reasonCode,
		Endpoint: endpoint, Model: model, LockedAt: time.Now().UTC(),
	})
	if err != nil {
		log.Printf("persist local-block prompt conversation lock failed decision=%s: %v", decisionID, err)
		return false
	}
	h.cachePromptConversationLock(c.Request.Context(), item, promptConversationLockTTL(cfg))
	return item != nil && item.Status == database.PromptConversationLockStatusActive
}

func (h *Handler) rejectLockedPromptConversation(c *gin.Context, cfg promptfilter.Config, signedBody, responseBody []byte, endpoint, model string) bool {
	item, locked := h.activePromptConversationLock(c, cfg, signedBody, endpoint, model)
	if !locked {
		return false
	}
	restriction := promptCyberRestrictionDecision(item, cfg)
	profile := strings.ToLower(strings.TrimSpace(cfg.Advanced.Guard.DefaultProfile))
	switch profile {
	case promptfilter.GuardProfileBalanced, promptfilter.GuardProfileStrict, promptfilter.GuardProfileResearch:
	default:
		profile = promptfilter.GuardProfileBalanced
	}
	decision := promptfilter.Decision{
		Action: promptfilter.ActionBlock, Profile: profile,
		ReasonCode: restriction.ReasonCode, StrikeEligible: false, Terminal: true,
	}
	verdict := promptfilter.Verdict{Action: promptfilter.ActionBlock, Reason: restriction.Message, FullText: restriction.ReasonCode}
	if h.sendNewAPIPromptCyberRestriction(c, cfg, decision, verdict, responseBody, endpoint, model, signedBody, restriction) {
		return true
	}
	writePromptCyberRestrictionHeaders(c, restriction)
	apiErr := promptCyberRestrictionAPIError(restriction, nil)
	api.SendErrorWithStatus(c, apiErr, http.StatusBadRequest)
	return true
}
