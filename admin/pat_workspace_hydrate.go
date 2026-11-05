package admin

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
)

// patWorkspaceHydrateBackoff is the retry interval after a whoami hydration failure. Most failures come from the token itself
// lacking whoami permission, so retrying in a short time will not help; a few hourly probes a day are enough.
const patWorkspaceHydrateBackoff = 6 * time.Hour

// needsPATWorkspaceHydration identifies AT-only accounts that do not have a workspace ID yet: file / batch / self-service
// imports, and PATs that existed before the upgrade, have never gone through whoami hydration on the paste entry, so wham stats have stayed off for them.
func needsPATWorkspaceHydration(account *auth.Account) bool {
	if account == nil || account.DBID <= 0 {
		return false
	}
	if !whamDailyUsageChannelSupported(account) {
		return false
	}
	if accessTokenTypeForToken(account.GetAccessToken()) != accessTokenTypeCodexAT {
		return false
	}
	return strings.TrimSpace(account.EffectiveAccountID()) == ""
}

// hydratePATWorkspace calls whoami for a single PAT account missing a workspace and writes the identity back to memory and the database.
// force=true skips the failure backoff (manual refresh). Returns whether the workspace ID is now present.
func (h *Handler) hydratePATWorkspace(ctx context.Context, account *auth.Account, force bool) bool {
	if !needsPATWorkspaceHydration(account) {
		return account != nil && strings.TrimSpace(account.EffectiveAccountID()) != ""
	}
	if h == nil || h.store == nil {
		return false
	}
	id := account.DBID
	if !force {
		h.whamDailyBackfillMu.Lock()
		failedAt, backedOff := h.patWhoAmIFailedAt[id]
		h.whamDailyBackfillMu.Unlock()
		if backedOff && time.Since(failedAt) < patWorkspaceHydrateBackoff {
			return false
		}
	}
	whoamiCtx, cancel := context.WithTimeout(ctx, patWhoAmIHydrateTimeout)
	defer cancel()
	meta, err := QueryPersonalAccessTokenMetadata(whoamiCtx, account.GetAccessToken(), h.store.ResolveProxyForAccount(account))
	if err == nil && (meta == nil || meta.ChatGPTAccountID == "") {
		err = errPATWhoAmINoWorkspace
	}
	if err != nil {
		h.whamDailyBackfillMu.Lock()
		if h.patWhoAmIFailedAt == nil {
			h.patWhoAmIFailedAt = map[int64]time.Time{}
		}
		h.patWhoAmIFailedAt[id] = time.Now()
		h.whamDailyBackfillMu.Unlock()
		log.Printf("PAT account %d whoami workspace hydration failed: %v", id, err)
		return false
	}
	h.whamDailyBackfillMu.Lock()
	delete(h.patWhoAmIFailedAt, id)
	h.whamDailyBackfillMu.Unlock()
	email := ""
	if meta.Email != nil {
		email = strings.TrimSpace(*meta.Email)
	}
	h.store.UpdateAccountIdentity(account, email, meta.ChatGPTAccountID)
	if meta.ChatGPTPlanType != "" {
		h.store.UpdateAccountPlanType(account, meta.ChatGPTPlanType)
	}
	log.Printf("PAT account %d hydrated workspace %s via whoami", id, auth.HashAccountID(meta.ChatGPTAccountID))
	return true
}

// hydratePATWorkspaces batch-hydrates PATs missing a workspace before each hourly probe round, so the accounts it fills in can
// enter the official usage candidates in this round. Concurrency matches the probe, so a large account pool cannot flood whoami all at once.
func (h *Handler) hydratePATWorkspaces(ctx context.Context) {
	if h == nil || h.store == nil {
		return
	}
	var pending []*auth.Account
	for _, account := range h.store.Accounts() {
		if needsPATWorkspaceHydration(account) {
			pending = append(pending, account)
		}
	}
	if len(pending) == 0 {
		return
	}
	sem := make(chan struct{}, whamDailyUsageProbeConcurrency)
	var wg sync.WaitGroup
	for _, account := range pending {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(acc *auth.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			h.hydratePATWorkspace(ctx, acc, false)
		}(account)
	}
	wg.Wait()
}
