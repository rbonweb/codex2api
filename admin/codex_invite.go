package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	inviteDefaultMaxEmails        = 10
	inviteUpperMaxEmails          = 50
	inviteRecipientCheckMaxEmails = 200
)

const (
	// v1: bump the version when the response structure changes, so entries written by old deployments in Redis expire naturally.
	inviteEligibilityCacheNamespace = "admin:codex-invite-eligibility:v1"
	inviteTrackingCacheNamespace    = "admin:codex-invite-tracking:v1"

	// The two TTLs have different jobs: the runtime cache only blocks repeats within seconds (switching accounts back and forth, several admins at the same time
	// with the page open), while the snapshot TTL decides how often the upstream is actually hit. The upstream's two endpoints sit
	// behind Cloudflare bot management, and the denser the requests, the more easily they get sent to a managed challenge,
	// so every request not sent is one less chance to be challenged.
	inviteRuntimeCacheTTL = 60 * time.Second
	// Eligibility is a monthly cumulative quota that only changes when "this gateway sends an invite" or "the user sends one on the official site",
	// the former actively invalidates it, so the window can be longer.
	inviteEligibilitySnapshotTTL = 15 * time.Minute
	// The redemption status of sent records is driven by the invitee's actions, which the gateway cannot see at all, so only a short window can cover it.
	inviteTrackingSnapshotTTL = 5 * time.Minute
)

// inviteCachedPayload is the entry in the runtime cache. ObservedAt/ExpiresAt are stored along with the data,
// so Redis entries and database snapshots carry the same metadata; otherwise the "updated at" read from the two paths
// would differ. ExpiresAt is the data's validity period (minutes), unrelated to the TTL of the Redis key itself (60s).
type inviteCachedPayload struct {
	HTTPStatus int             `json:"http_status"`
	ObservedAt time.Time       `json:"observed_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
	Payload    json.RawMessage `json:"payload"`
}

// inviteCacheMeta is sent with the response, so the frontend can show when the data was fetched and whether it came from the cache.
type inviteCacheMeta struct {
	Source     string     `json:"source"` // upstream / runtime / snapshot
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

func inviteUpstreamMeta() *inviteCacheMeta {
	now := time.Now()
	return &inviteCacheMeta{Source: "upstream", ObservedAt: &now}
}

// inviteForceRefresh decides whether to bypass the cache and go straight to the upstream. The frontend's manual refresh button and the re-fetch after sending both
// pass this parameter.
func inviteForceRefresh(c *gin.Context) bool {
	switch strings.ToLower(strings.TrimSpace(c.Query("refresh"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// Cache scope: the full identifier of one result. Only normalized parameters go into the scope; otherwise "limit not passed"
// and "passing 100" would each store an identical entry.
func inviteEligibilityScope(programID, entrypoint string) string {
	return programID + "|" + entrypoint
}

func inviteTrackingScope(programID, period string, limit int) string {
	return fmt.Sprintf("%s|%s|%d", programID, period, limit)
}

// readInviteCache tries the runtime cache and then the database snapshot in turn, and on a hit decodes the result into dest.
// It returns nil on a miss. An error at either layer is only logged and the request continues to the upstream: the cache must not make the feature unavailable.
func (h *Handler) readInviteCache(ctx context.Context, namespace, kind string, accountID, generation int64, scope string, dest any) *inviteCacheMeta {
	now := time.Now()

	key := inviteCacheKey(accountID, generation, scope)
	var cached inviteCachedPayload
	if h.getRuntimeJSON(ctx, namespace, key, &cached) &&
		len(cached.Payload) > 0 && now.Before(cached.ExpiresAt) {
		if err := json.Unmarshal(cached.Payload, dest); err == nil {
			return &inviteCacheMeta{Source: "runtime", ObservedAt: &cached.ObservedAt, ExpiresAt: &cached.ExpiresAt}
		}
	}

	if h.db == nil {
		return nil
	}
	snap, err := h.db.GetCodexInviteSnapshot(ctx, accountID, kind, scope)
	if err != nil {
		log.Printf("failed to read invite snapshot: account=%d kind=%s err=%v", accountID, kind, err)
		return nil
	}
	// A different credential generation means the account was re-authorized, and the old snapshot describes eligibility for another identity.
	if snap == nil || snap.Expired(now) || snap.CredentialGeneration != generation {
		return nil
	}
	if err := json.Unmarshal(snap.Payload, dest); err != nil {
		return nil
	}
	// Backfill the runtime cache, so the same snapshot does not hit the database again for the next 60 seconds.
	h.setRuntimeJSON(ctx, namespace, key, inviteCachedPayload{
		HTTPStatus: snap.HTTPStatus,
		ObservedAt: snap.ObservedAt,
		ExpiresAt:  snap.ExpiresAt,
		Payload:    snap.Payload,
	}, inviteRuntimeCacheTTL)
	return &inviteCacheMeta{Source: "snapshot", ObservedAt: &snap.ObservedAt, ExpiresAt: &snap.ExpiresAt}
}

// writeInviteCache records one successful observation. It is only called when the upstream gives a business conclusion; failures are never cached,
// see the note on database.CodexInviteSnapshot.
func (h *Handler) writeInviteCache(ctx context.Context, namespace, kind string, accountID, generation int64, scope string, httpStatus int, ttl time.Duration, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		log.Printf("failed to encode invite cache: account=%d kind=%s err=%v", accountID, kind, err)
		return
	}
	now := time.Now()
	expiresAt := now.Add(ttl)

	h.setRuntimeJSON(ctx, namespace, inviteCacheKey(accountID, generation, scope), inviteCachedPayload{
		HTTPStatus: httpStatus,
		ObservedAt: now,
		ExpiresAt:  expiresAt,
		Payload:    payload,
	}, inviteRuntimeCacheTTL)

	if h.db == nil {
		return
	}
	if err := h.db.UpsertCodexInviteSnapshot(ctx, &database.CodexInviteSnapshot{
		AccountID:            accountID,
		Kind:                 kind,
		Scope:                scope,
		CredentialGeneration: generation,
		HTTPStatus:           httpStatus,
		Payload:              payload,
		ObservedAt:           now,
		ExpiresAt:            expiresAt,
	}); err != nil {
		log.Printf("failed to write invite snapshot: account=%d kind=%s err=%v", accountID, kind, err)
	}
}

func inviteCacheKey(accountID, generation int64, scope string) string {
	return fmt.Sprintf("%d|%d|%s", accountID, generation, scope)
}

// invalidateInviteCache clears the cache after the quota has actually changed (currently only the "sent successfully" case).
//
// Snapshots are deleted per account as a whole; this is the authoritative layer. Runtime cache keys include parameter combinations and cannot be enumerated,
// so only the default combination is deleted. Non-default parameters can only come from manual API calls, and at most they go stale for one runtime TTL;
// the frontend re-fetches with refresh=1 after sending, so it never reaches the cache.
func (h *Handler) invalidateInviteCache(ctx context.Context, accountID, generation int64, programID, entrypoint string) {
	if h.cache != nil {
		period, limit := proxy.NormalizeInviteTracking("", 0)
		targets := []struct {
			namespace string
			scope     string
		}{
			{inviteEligibilityCacheNamespace, inviteEligibilityScope(programID, entrypoint)},
			{inviteTrackingCacheNamespace, inviteTrackingScope(programID, period, limit)},
		}
		for _, target := range targets {
			if err := h.cache.DeleteRuntime(ctx, target.namespace, inviteCacheKey(accountID, generation, target.scope)); err != nil {
				log.Printf("failed to clear invite runtime cache: account=%d namespace=%s err=%v", accountID, target.namespace, err)
			}
		}
	}
	if h.db == nil {
		return
	}
	if err := h.db.DeleteCodexInviteSnapshots(ctx, accountID); err != nil {
		log.Printf("failed to clear invite snapshot: account=%d err=%v", accountID, err)
	}
}

var inviteEmailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
var inviteEmailSplitter = regexp.MustCompile(`[,;\r\n\t ]+`)

type inviteRequest struct {
	Emails     []string `json:"emails"`
	EmailsText string   `json:"emails_text"`
	// The upstream has split the old single-field referral_key into program_id + entrypoint; leaving them empty uses the default plan.
	ProgramID  string `json:"program_id"`
	Entrypoint string `json:"entrypoint"`
	ProxyURL   string `json:"proxy_url"`
	MaxEmails  int    `json:"max_emails"`
}

type inviteRecipientCheckRequest struct {
	Emails []string `json:"emails"`
}

// CheckInviteRecipients checks in bulk whether recipient emails have already been reserved or invited by this gateway.
// POST /api/accounts/invite/recipients/check
func (h *Handler) CheckInviteRecipients(c *gin.Context) {
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "Invite email ledger unavailable")
		return
	}

	var req inviteRecipientCheckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Failed to parse request body: "+err.Error())
		return
	}
	emails, err := collectInviteEmails(req.Emails, "", inviteRecipientCheckMaxEmails)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	recipients, err := h.db.ListCodexInviteRecipientsByEmails(c.Request.Context(), emails)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "Failed to query invite email status: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"recipients": recipients})
}

// SendInvite sends an invite email through the given account to the ChatGPT referral invite endpoint.
// POST /api/accounts/:id/invite
func (h *Handler) SendInvite(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return
	}

	var req inviteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Failed to parse request body: "+err.Error())
		return
	}

	maxEmails := inviteDefaultMaxEmails
	if req.MaxEmails > 0 && req.MaxEmails < maxEmails {
		maxEmails = req.MaxEmails
	}
	if maxEmails > inviteUpperMaxEmails {
		maxEmails = inviteUpperMaxEmails
	}

	emails, err := collectInviteEmails(req.Emails, req.EmailsText, maxEmails)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	account := h.findAccountByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return
	}
	if account.GetAccessToken() == "" {
		writeError(c, http.StatusBadRequest, "Account has no usable access token; refresh the account first")
		return
	}
	if h.db == nil {
		// Do not send the email before a persistent ledger exists: after the upstream succeeds, a repeat invite could not be prevented.
		writeError(c, http.StatusServiceUnavailable, "Invite email ledger unavailable")
		return
	}

	proxyURL := strings.TrimSpace(req.ProxyURL)
	if proxyURL == "" {
		proxyURL = h.store.ResolveProxyForAccount(account)
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()

	programID, entrypoint := proxy.NormalizeInviteProgram(req.ProgramID, req.Entrypoint)
	reservationID := uuid.NewString()
	if _, err := h.db.ReserveCodexInviteRecipients(ctx, reservationID, account.DBID, programID, entrypoint, emails); err != nil {
		var conflict *database.CodexInviteRecipientConflictError
		if errors.As(err, &conflict) {
			writeError(c, http.StatusConflict, "The following emails have already been invited or are being sent, and cannot be invited again: "+strings.Join(conflict.Emails, ", "))
			return
		}
		writeError(c, http.StatusInternalServerError, "Failed to reserve invite email: "+err.Error())
		return
	}

	sendInvite := h.sendCodexInvite
	if sendInvite == nil {
		sendInvite = proxy.SendCodexInvite
	}
	result, err := sendInvite(ctx, account, proxyURL, programID, entrypoint, emails)
	if err != nil {
		// A network error does not prove the upstream did not accept the request. To guarantee the same email is sent at most once, conservatively mark it as
		// unknown, and wait for tracking or an admin to check it, instead of releasing it automatically and allowing an immediate retry.
		if _, markErr := h.db.MarkCodexInviteRecipientsUnknown(c.Request.Context(), reservationID, "", 0); markErr != nil {
			log.Printf("failed to mark invite email result as unknown: reservation=%s err=%v", reservationID, markErr)
		}
		writeError(c, http.StatusBadGateway, "Invite request failed: "+err.Error())
		return
	}

	if result.Challenged {
		// The challenge page clearly did not reach the invite business endpoint, so it can be released safely; keep the input so the user can retry later.
		if _, releaseErr := h.db.ReleaseCodexInviteRecipients(c.Request.Context(), reservationID); releaseErr != nil {
			log.Printf("failed to release challenged invite email: reservation=%s err=%v", reservationID, releaseErr)
		}
	} else if !result.OK {
		// When the upstream explicitly rejects the business request, this send did not succeed. If the reason clearly says the recipient has already received
		// the invite, mark that email as known_invited; release the other reservations so they can be retried after correction.
		observedAt := time.Now()
		evidence := inviteRecipientEvidenceFromInviteItems(result.Invites, observedAt)
		if inviteFailureIndicatesAlreadyInvited(result.UpstreamMessage) && len(result.FailedEmails) > 0 {
			evidence = append(evidence, inviteRecipientEvidenceFromEmails(result.FailedEmails, observedAt)...)
		}
		canRelease := true
		if len(evidence) > 0 {
			if _, markErr := h.db.MarkCodexInviteRecipientsKnownInvited(c.Request.Context(), reservationID,
				result.RequestID, result.StatusCode, evidence, observedAt); markErr != nil {
				// When the upstream has given recipient-level evidence but the local side cannot record it precisely, the retry must not be unlocked;
				// turn the remaining reservations of this batch into unknown: a manual check is preferable to risking sending a second email.
				canRelease = false
				log.Printf("failed to record email already invited upstream: reservation=%s err=%v", reservationID, markErr)
				if _, unknownErr := h.db.MarkCodexInviteRecipientsUnknown(c.Request.Context(), reservationID,
					result.RequestID, result.StatusCode); unknownErr != nil {
					log.Printf("failed to conservatively lock invite email: reservation=%s err=%v", reservationID, unknownErr)
				}
			}
		}
		if canRelease {
			if _, releaseErr := h.db.ReleaseCodexInviteRecipients(c.Request.Context(), reservationID); releaseErr != nil {
				log.Printf("failed to release reservation of failed invite email: reservation=%s err=%v", reservationID, releaseErr)
			}
		}
	}

	// As long as the request reached the upstream, the quota may already have changed: a recipient-level rejection may also have used up a send.
	// The only case known to be unchanged is the one Cloudflare blocked at the door. It is better to hit the upstream one extra time than to
	// let the page keep showing the remaining count from before the send.
	if !result.Challenged {
		h.invalidateInviteCache(c.Request.Context(), account.DBID, account.GetCredentialGeneration(), programID, entrypoint)
	}

	if !result.OK {
		// Common case: accounts without referral eligibility get 403. Pass the upstream response through for the frontend to display.
		c.JSON(http.StatusOK, gin.H{
			"ok":     false,
			"result": result,
		})
		return
	}

	invitedAt := time.Now()
	evidence := inviteRecipientEvidenceFromInviteItems(result.Invites, invitedAt)
	if _, finalizeErr := h.db.FinalizeCodexInviteRecipients(c.Request.Context(), reservationID,
		result.RequestID, result.StatusCode, evidence, invitedAt); finalizeErr != nil {
		// The email has already been sent, so never turn the success response into a retryable failure. The reservation row still prevents duplicate sends.
		log.Printf("failed to finalize invite email success status: reservation=%s err=%v", reservationID, finalizeErr)
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":              true,
		"result":          result,
		"recorded_emails": emails,
	})
}

func inviteFailureIndicatesAlreadyInvited(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	if strings.Contains(normalized, "已") &&
		(strings.Contains(normalized, "邀请") || strings.Contains(normalized, "邀請")) {
		return true
	}
	for _, marker := range []string{
		"already invited",
		"already received an invite",
		"already received a referral",
		"has already been invited",
		"previously invited",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func inviteRecipientEvidenceFromEmails(emails []string, observedAt time.Time) []database.CodexInviteRecipientEvidence {
	evidence := make([]database.CodexInviteRecipientEvidence, 0, len(emails))
	for _, email := range emails {
		if strings.TrimSpace(email) == "" {
			continue
		}
		evidence = append(evidence, database.CodexInviteRecipientEvidence{
			Email:     email,
			InvitedAt: observedAt,
		})
	}
	return evidence
}

func inviteRecipientEvidenceFromInviteItems(items []proxy.CodexInviteItem, observedAt time.Time) []database.CodexInviteRecipientEvidence {
	evidence := make([]database.CodexInviteRecipientEvidence, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.Email) == "" {
			continue
		}
		evidence = append(evidence, database.CodexInviteRecipientEvidence{
			Email:      item.Email,
			ReferralID: item.ReferralID,
			InviteURL:  item.InviteURL,
			InvitedAt:  observedAt,
		})
	}
	return evidence
}

// rememberInviteTrackingRecipients merges past invites that the upstream tracking proves exist into the global ledger.
// Whether an invite is redeemed, expired, or resendable, the product rule is "one email is invited only once", so all of them are kept.
func (h *Handler) rememberInviteTrackingRecipients(ctx context.Context, accountID int64, programID, entrypoint string, result *proxy.CodexInviteTracking) {
	if h.db == nil || result == nil || !result.OK || result.Challenged || len(result.Items) == 0 {
		return
	}
	evidence := make([]database.CodexInviteRecipientEvidence, 0, len(result.Items))
	for _, item := range result.Items {
		if strings.TrimSpace(item.Email) == "" {
			continue
		}
		invitedAt := time.Time{}
		if item.CreatedAt != "" {
			invitedAt, _ = time.Parse(time.RFC3339Nano, item.CreatedAt)
		}
		evidence = append(evidence, database.CodexInviteRecipientEvidence{
			Email:                   item.Email,
			ReferralID:              item.ReferralID,
			InviteURL:               item.InviteURL,
			UpstreamRecipientStatus: item.Status,
			InvitedAt:               invitedAt,
		})
	}
	if len(evidence) == 0 {
		return
	}
	_, err := h.db.UpsertCodexInviteRecipientsFromTracking(ctx, accountID, programID, entrypoint, result.StatusCode, evidence)
	if err != nil {
		log.Printf("failed to backfill invite email ledger: account=%d err=%v", accountID, err)
	}
}

// GetInviteEligibility queries the referral invite eligibility and remaining quota of the given account.
// GET /api/accounts/:id/invite/eligibility
func (h *Handler) GetInviteEligibility(c *gin.Context) {
	account, proxyURL, ok := h.resolveInviteAccount(c)
	if !ok {
		return
	}

	programID, entrypoint := proxy.NormalizeInviteProgram(
		strings.TrimSpace(c.Query("program_id")), strings.TrimSpace(c.Query("entrypoint")))
	scope := inviteEligibilityScope(programID, entrypoint)
	generation := account.GetCredentialGeneration()
	reqCtx := c.Request.Context()

	if !inviteForceRefresh(c) {
		var cached proxy.CodexInviteEligibility
		if meta := h.readInviteCache(reqCtx, inviteEligibilityCacheNamespace,
			database.CodexInviteSnapshotEligibility, account.DBID, generation, scope, &cached); meta != nil {
			c.JSON(http.StatusOK, gin.H{"ok": cached.OK, "result": cached, "cache": meta})
			return
		}
	}

	ctx, cancel := context.WithTimeout(reqCtx, 30*time.Second)
	defer cancel()

	result, err := proxy.QueryCodexInviteEligibility(ctx, account, proxyURL, programID, entrypoint)
	if err != nil {
		writeError(c, http.StatusBadGateway, "Eligibility query failed: "+err.Error())
		return
	}

	// Only cache when the upstream gives a business conclusion. The challenge page borrows 403, the same code as "not eligible",
	// so caching it would turn one Cloudflare block into a fifteen-minute "this account cannot send invites".
	// 200 + should_show=false is a real conclusion and is cached as usual.
	if result.OK && !result.Challenged {
		h.writeInviteCache(reqCtx, inviteEligibilityCacheNamespace, database.CodexInviteSnapshotEligibility,
			account.DBID, generation, scope, result.StatusCode, inviteEligibilitySnapshotTTL, result)
	}

	// A non-2xx upstream response is not a gateway error; pass it through for the frontend to display (such as the 403 for accounts without eligibility).
	c.JSON(http.StatusOK, gin.H{"ok": result.OK, "result": result, "cache": inviteUpstreamMeta()})
}

// GetInviteTracking queries the invites already sent by the given account and their redemption status.
// GET /api/accounts/:id/invite/tracking
func (h *Handler) GetInviteTracking(c *gin.Context) {
	account, proxyURL, ok := h.resolveInviteAccount(c)
	if !ok {
		return
	}

	rawLimit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	programID, entrypoint := proxy.NormalizeInviteProgram(strings.TrimSpace(c.Query("program_id")), "")
	period, limit := proxy.NormalizeInviteTracking(strings.TrimSpace(c.Query("period")), rawLimit)
	scope := inviteTrackingScope(programID, period, limit)
	generation := account.GetCredentialGeneration()
	reqCtx := c.Request.Context()

	if !inviteForceRefresh(c) {
		var cached proxy.CodexInviteTracking
		if meta := h.readInviteCache(reqCtx, inviteTrackingCacheNamespace,
			database.CodexInviteSnapshotTracking, account.DBID, generation, scope, &cached); meta != nil {
			h.rememberInviteTrackingRecipients(reqCtx, account.DBID, programID, entrypoint, &cached)
			c.JSON(http.StatusOK, gin.H{"ok": cached.OK, "result": cached, "cache": meta})
			return
		}
	}

	ctx, cancel := context.WithTimeout(reqCtx, 30*time.Second)
	defer cancel()

	result, err := proxy.QueryCodexInviteTracking(ctx, account, proxyURL, programID, period, limit)
	if err != nil {
		writeError(c, http.StatusBadGateway, "Invite record query failed: "+err.Error())
		return
	}

	if result.OK && !result.Challenged {
		h.rememberInviteTrackingRecipients(reqCtx, account.DBID, programID, entrypoint, result)
		h.writeInviteCache(reqCtx, inviteTrackingCacheNamespace, database.CodexInviteSnapshotTracking,
			account.DBID, generation, scope, result.StatusCode, inviteTrackingSnapshotTTL, result)
	}

	c.JSON(http.StatusOK, gin.H{"ok": result.OK, "result": result, "cache": inviteUpstreamMeta()})
}

// resolveInviteAccount parses the account ID from the path, validates the credential, and resolves the proxy.
// On validation failure it has already written the error response and returns ok=false.
func (h *Handler) resolveInviteAccount(c *gin.Context) (*auth.Account, string, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid account ID")
		return nil, "", false
	}
	account := h.findAccountByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return nil, "", false
	}
	if account.GetAccessToken() == "" {
		writeError(c, http.StatusBadRequest, "Account has no usable access token; refresh the account first")
		return nil, "", false
	}

	proxyURL := strings.TrimSpace(c.Query("proxy_url"))
	if proxyURL == "" {
		proxyURL = h.store.ResolveProxyForAccount(account)
	}
	return account, proxyURL, true
}

// collectInviteEmails merges emails[] and text sources, de-duplicates them (case-insensitive), validates the format, and clamps to the limit.
func collectInviteEmails(list []string, text string, maxEmails int) ([]string, error) {
	raw := make([]string, 0, len(list))
	raw = append(raw, list...)
	if strings.TrimSpace(text) != "" {
		raw = append(raw, inviteEmailSplitter.Split(text, -1)...)
	}

	seen := make(map[string]struct{})
	emails := make([]string, 0, len(raw))
	for _, e := range raw {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		key := strings.ToLower(e)
		if _, ok := seen[key]; ok {
			continue
		}
		if !inviteEmailPattern.MatchString(e) {
			return nil, fmt.Errorf("invalid email format: %s", e)
		}
		seen[key] = struct{}{}
		emails = append(emails, e)
	}

	if len(emails) == 0 {
		return nil, fmt.Errorf("at least one email is required")
	}
	if len(emails) > maxEmails {
		return nil, fmt.Errorf("number of emails exceeds the limit of %d", maxEmails)
	}
	return emails, nil
}
