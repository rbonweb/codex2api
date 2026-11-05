package proxy

import (
	"testing"

	"github.com/codex2api/auth"
)

// issue #446: maintenance-class bypass requests (wham usage/reset coupons, model list, alpha search, subscription sync)
// used to build a new one-off transport on every call. In uTLS mode those transports are discarded after use,
// and their HTTP/2 connections are never closed, so the background probe and /models keep pushing up the connection count, goroutines,
// and memory. The tests below lock in the invariant "reuse the same pooled Client".

func TestGetCodexMaintenanceClientIsReusedPerAccount(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "utls_chrome")
	clearMaintenanceClients(t)

	acc := &auth.Account{DBID: 4460}

	first := getCodexMaintenanceClient(acc, "")
	second := getCodexMaintenanceClient(acc, "")

	if first != second {
		t.Fatal("the same account got different Clients on two calls: one-off transports keep leaking connections and goroutines")
	}
	if _, ok := first.Transport.(*utlsRoundTripper); !ok {
		t.Fatalf("in utls_chrome mode transport = %T, want *utlsRoundTripper", first.Transport)
	}
}

// Accounts must be isolated: the same TCP connection reused across different tokens is recognizable upstream.
func TestGetCodexMaintenanceClientIsolatesAccountsAndProxies(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "utls_chrome")
	clearMaintenanceClients(t)

	accA := &auth.Account{DBID: 4461}
	accB := &auth.Account{DBID: 4462}

	if getCodexMaintenanceClient(accA, "") == getCodexMaintenanceClient(accB, "") {
		t.Fatal("different accounts shared the same maintenance Client (connections would be reused across tokens)")
	}
	if getCodexMaintenanceClient(accA, "") == getCodexMaintenanceClient(accA, "http://proxy:8080") {
		t.Fatal("different proxies shared the same maintenance Client (egress IPs would get mixed)")
	}
}

// Maintenance requests must be kept separate from the /responses main pool: when /responses fails, the whole Client is removed,
// so one failed background probe must not drag down business connections, and vice versa.
func TestMaintenanceClientKeyDoesNotCollideWithResponsesPool(t *testing.T) {
	acc := &auth.Account{DBID: 4463}

	maintKey := maintenanceClientKey(acc, "", codexTransportModeUTLSChrome, maintenancePurposeCodex)
	responsesKey := clientPoolKey(acc, "", codexTransportModeUTLSChrome)
	subKey := maintenanceClientKey(acc, "", codexTransportModeUTLSChrome, maintenancePurposeSubscription)

	if maintKey == responsesKey {
		t.Fatalf("maintenance pool key conflicts with the /responses pool key: %q", maintKey)
	}
	if maintKey == subKey {
		t.Fatal("subscription sync (browser UA) and Codex maintenance requests (CLI UA) shared a connection")
	}
}

// Subscription endpoints sit behind Cloudflare, so uTLS fingerprinting must be forced, independent of CODEX_TRANSPORT_MODE.
func TestSubscriptionMaintenanceClientForcesUTLS(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	clearMaintenanceClients(t)

	acc := &auth.Account{DBID: 4464}
	client := getMaintenanceClient(acc, "", maintenancePurposeSubscription, true)

	if _, ok := client.Transport.(*utlsRoundTripper); !ok {
		t.Fatalf("subscription sync transport = %T, want *utlsRoundTripper (a regular fingerprint would be blocked by Cloudflare)", client.Transport)
	}
}

// clearMaintenanceClients empties the maintenance Client pool so test cases do not contaminate each other.
func clearMaintenanceClients(t *testing.T) {
	t.Helper()
	clientPool.Range(func(key, value any) bool {
		if k, ok := key.(string); ok && len(k) >= 6 && k[:6] == "maint|" {
			clientPool.Delete(key)
			if entry, ok := value.(*poolEntry); ok {
				releaseEvictedClient(entry.client)
			}
		}
		return true
	})
}
