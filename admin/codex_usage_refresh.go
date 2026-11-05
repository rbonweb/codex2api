package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// BatchRefreshCodexUsage refreshes progress-bar snapshots for the entire loaded
// Codex pool. It queries WHAM only; token refresh and Responses probes remain
// separate management operations.
func (h *Handler) BatchRefreshCodexUsage(c *gin.Context) {
	if h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "Account pool unavailable")
		return
	}
	if !h.codexUsageRefreshRunning.CompareAndSwap(false, true) {
		writeError(c, http.StatusConflict, "Codex usage is being refreshed; wait for the current operation to finish")
		return
	}
	defer h.codexUsageRefreshRunning.Store(false)

	accounts := make([]*auth.Account, 0)
	for _, account := range h.store.Accounts() {
		if supportsCodexWhamRefresh(account) {
			accounts = append(accounts, account)
		}
	}
	stream := strings.EqualFold(c.Query("stream"), "true")
	state := batchOperationEvent{Type: "start", Action: "batch_usage_refresh", Total: len(accounts)}
	if stream {
		setupSSE(c)
		sendSSEJSON(c, state)
	}

	ctx := c.Request.Context()
	concurrency := min(h.store.GetUsageProbeConcurrency(), len(accounts))
	results := make(chan batchOperationEvent, concurrency)
	go func() {
		defer close(results)
		h.runCodexUsageRefresh(ctx, accounts, concurrency, results)
	}()
	for event := range results {
		state.Current++
		if event.Status == "success" {
			state.Success++
		} else {
			state.Failed++
		}
		event.Type, event.Action = "progress", state.Action
		event.Current, event.Total = state.Current, state.Total
		event.Success, event.Failed = state.Success, state.Failed
		if stream && ctx.Err() == nil {
			sendSSEJSON(c, event)
		}
	}
	// Expire projection/analysis caches before the completion event, so the
	// browser's immediate reload observes the new quota values even in big pools.
	h.invalidateAccountSnapshotCaches()
	if ctx.Err() != nil {
		return
	}
	state.Type = "complete"
	if stream {
		sendSSEJSON(c, state)
	} else {
		c.JSON(http.StatusOK, state)
	}
}

func supportsCodexWhamRefresh(account *auth.Account) bool {
	if account == nil {
		return false
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	upstream := strings.ToLower(strings.TrimSpace(account.UpstreamType))
	return (upstream == "" || upstream == "codex") &&
		!strings.EqualFold(strings.TrimSpace(account.CodexAuthMode), auth.CodexAuthModeAgentIdentity) &&
		strings.TrimSpace(account.AccessToken) != ""
}

func (h *Handler) runCodexUsageRefresh(ctx context.Context, accounts []*auth.Account, concurrency int, results chan<- batchOperationEvent) {
	jobs := make(chan *auth.Account)
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for account := range jobs {
				if ctx.Err() != nil {
					return
				}
				name, email := runtimeAccountOperationIdentity(account)
				event := batchOperationEvent{AccountID: account.DBID, AccountName: name, AccountEmail: email, Status: "success", HTTPStatus: http.StatusOK, Message: "Usage updated"}
				if err := h.refreshCodexWhamSnapshot(ctx, account); err != nil {
					event.Status, event.Error = "failed", err.Error()
					event.Message = event.Error
					event.HTTPStatus = batchOperationHTTPStatus(event.Status, event.Error)
				}
				select {
				case results <- event:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
feed:
	for _, account := range accounts {
		select {
		case jobs <- account:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	workers.Wait()
}

func (h *Handler) refreshCodexWhamSnapshot(ctx context.Context, account *auth.Account) error {
	// Recheck at execution time: another management action may have removed or
	// changed an account while it was queued.
	if h.store.FindByID(account.DBID) != account || !supportsCodexWhamRefresh(account) {
		return errors.New("account was removed or no longer supports WHAM usage queries")
	}
	if !account.TryBeginUsageProbe() {
		return errors.New("account usage is being refreshed, please retry later")
	}
	defer account.FinishUsageProbe()
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	usage, response, err := proxy.QueryWhamUsage(probeCtx, account, h.store.ResolveProxyForAccount(account))
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if response != nil && response.StatusCode != http.StatusOK {
		// A WHAM 401 alone cannot invalidate a codex_at account. Report the
		// query failure without changing authentication or invoking Responses.
		return fmt.Errorf("WHAM upstream returned %d", response.StatusCode)
	}
	if err != nil {
		if probeCtx.Err() != nil {
			return fmt.Errorf("WHAM usage query aborted: %w", probeCtx.Err())
		}
		return errors.New("WHAM usage query failed, check the proxy or network connection")
	}
	state := proxy.ApplyWhamUsage(h.store, account, usage)
	if !state.HasUsage5h && !state.HasUsage7d {
		return errors.New("WHAM returned no valid usage window; keeping the existing usage snapshot")
	}
	return nil
}
