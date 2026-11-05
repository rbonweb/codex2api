package proxy

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

// ==================== HTTP Client pool for account maintenance requests (issue #446) ====================
//
// Background: wham usage/reset coupons, the Codex model list, alpha search, and subscription expiry sync, these
// "maintenance" bypass requests used to build `&http.Client{Transport: newCodexTransport(...)}` on every call,
// creating a one-off transport. Under the standard transport, leftover connections are reclaimed by IdleConnTimeout (90s);
// but the uTLS transport manages its own connection pool, so a one-off instance is discarded after use, and the HTTP/2 connections under it
// have no holder to close them, so the readLoop goroutine and the socket stay resident until the process exits.
// Background probes run periodically and /models is requested often by the Codex client, so the connection count, goroutines
// and memory grow linearly with runtime, eventually causing OOM.
//
// Fix: maintenance requests reuse a Client isolated by (purpose + account + proxy + transport mode).
// Reuse clientPool directly, which automatically gets the existing TTL eviction (clientPoolTTL) and
// the connection-closing logic of releaseEvictedClient, with no second cleanup goroutine needed.
//
// Why not share the /responses pool (getPooledClient) directly:
//   - maintenance requests use different UA/Originator semantics from business requests (subscription endpoints use a browser UA),
//     and mixing them on the same h2 connection would change the traffic profile upstream sees;
//   - when /responses fails, recyclePooledClient removes the whole Client, so one failed
//     background probe must not spill over onto business connections (and vice versa).

// Pool purpose identifiers for maintenance requests. Separating purposes lets "subscription sync (browser UA)" and
// "wham / model list / search (Codex CLI UA)" each hold their own connections, without sharing one h2 connection.
const (
	maintenancePurposeCodex        = "codex"        // wham / models / alpha search, Codex CLI UA
	maintenancePurposeSubscription = "subscription" // web subscription endpoints, browser UA
	maintenancePurposeInvite       = "invite"       // web referral invite endpoints, browser UA
)

// maintenanceClientKey builds the pool key for maintenance Clients. Its prefix differs from clientPoolKey,
// so it cannot overwrite the /responses main pool or the Resin pool.
func maintenanceClientKey(account *auth.Account, proxyURL, transportMode, purpose string) string {
	accountID := int64(0)
	if account != nil {
		accountID = account.ID()
	}
	return fmt.Sprintf("maint|%s|%d|%s|%s", purpose, accountID, strings.TrimSpace(proxyURL), transportMode)
}

// getMaintenanceClient returns a pooled Client dedicated to maintenance requests.
//
// When forceUTLS=true, CODEX_TRANSPORT_MODE is ignored and the uTLS Chrome fingerprint is forced,
// (subscription endpoints sit behind Cloudflare, where a regular fingerprint would be blocked).
func getMaintenanceClient(account *auth.Account, proxyURL, purpose string, forceUTLS bool) *http.Client {
	transportMode := codexTransportModeFromEnv()
	if forceUTLS {
		transportMode = codexTransportModeUTLSChrome
	}
	key := maintenanceClientKey(account, proxyURL, transportMode, purpose)

	if v, ok := clientPool.Load(key); ok {
		entry := v.(*poolEntry)
		entry.touch()
		return entry.client
	}

	var transport http.RoundTripper
	if forceUTLS {
		transport = NewUTLSTransport(proxyURL)
	} else {
		transport = newCodexTransport(proxyURL)
	}

	entry := &poolEntry{
		client: &http.Client{
			Transport: transport,
			// No overall timeout: each caller controls its timeout with context (wham 25s, model list 15s,
			// search 120s, subscription 15s), consistent with the semantics of the /responses pool.
			Timeout: 0,
		},
	}
	entry.touch()

	if v, loaded := clientPool.LoadOrStore(key, entry); loaded {
		// Lost the race: discard the transport we just built and release its connections (usually not yet dialed at this point,
		// but under uTLS, connections nobody holds may be left behind without an explicit close).
		releaseEvictedClient(entry.client)
		e := v.(*poolEntry)
		e.touch()
		return e.client
	}
	return entry.client
}

// getCodexMaintenanceClient is the pooled Client shared by wham, the model list, and alpha search.
// It uses the same transport as the gateway (including the uTLS Chrome fingerprint), keeping the fingerprint consistent with /responses.
func getCodexMaintenanceClient(account *auth.Account, proxyURL string) *http.Client {
	return getMaintenanceClient(account, proxyURL, maintenancePurposeCodex, false)
}
