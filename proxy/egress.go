package proxy

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/codex2api/auth"
)

// ==================== Unified Codex egress resolution ====================
//
// Egress for the Codex channel has three outbound configurations that coexist (issue #679):
//
//  1. Resin reverse proxy (global, system settings resin_url + resin_platform_name)
//  2. Proxy pool / group proxy / account proxy_url / global proxy_url
//     (one chosen by auth.Store.ResolveProxyForAccount in the order account > group > proxy pool > global)
//  3. Direct connection
//
// Priority is fixed as Resin > proxy > direct connection, and Resin overrides the whole layer: once enabled, the Codex channel
// All outbound traffic that carries account identity (/responses, compact, WS, wham usage, subscription queries, telemetry,
// token refresh) all goes through Resin instead, and the proxy picked at layer 2 is kept only in logs and audit records and does not take part in dialing.
// An explicit template refresh can use the dedicated issuing proxy through ResolveCodexRequestEgress, and does not change the default path above.
//
// Every place that chooses a client for Codex outbound traffic must go through this file's resolver, and nobody may write their own
// `if IsResinEnabled()` branches: scattered branches are exactly the root cause of "three configurations overriding each other, and no one can tell which is in effect".

// CodexEgressKind identifies which path a Codex outbound request finally takes.
type CodexEgressKind string

const (
	// CodexEgressResin goes through the Resin reverse proxy; the URL is already rewritten, and the request header must carry X-Resin-Account.
	CodexEgressResin CodexEgressKind = "resin"
	// CodexEgressProxy goes straight to upstream through the proxy picked at layer 2.
	CodexEgressProxy CodexEgressKind = "proxy"
	// CodexEgressDirect connects directly to upstream without a proxy.
	CodexEgressDirect CodexEgressKind = "direct"
)

// CodexEgress describes the resolved egress: the final request address, the proxy used for dialing, and the client.
type CodexEgress struct {
	Kind CodexEgressKind
	// URL is the final request address; in Resin mode it has already been rewritten to the reverse-proxy path.
	URL string
	// ProxyURL is the proxy picked at layer 2. In Resin mode the original value is kept for audit, but it is not used for dialing.
	ProxyURL string
	// DialProxyURL is the proxy actually used for dialing: in Resin mode it is always empty.
	DialProxyURL string

	account *auth.Account
	client  *http.Client
}

// ResolveCodexEgress decides the path for one Codex HTTP outbound request that carries account identity.
// proxyURL is the proxy already resolved at layer 2 (may be empty); when Resin is enabled it is overridden for the whole layer.
// When account is nil, Resin cannot be used (Resin sticks to an account, and without an identity there is nothing to stick to), so it falls back to proxy/direct.
func ResolveCodexEgress(account *auth.Account, targetURL, proxyURL string) CodexEgress {
	proxyURL = strings.TrimSpace(proxyURL)
	if resinCarriesEgress(account) {
		return CodexEgress{
			Kind:     CodexEgressResin,
			URL:      BuildReverseProxyURL(targetURL),
			ProxyURL: proxyURL,
			account:  account,
			client:   getResinHTTPClient(account),
		}
	}
	kind := CodexEgressDirect
	if proxyURL != "" {
		kind = CodexEgressProxy
	}
	return CodexEgress{
		Kind:         kind,
		URL:          targetURL,
		ProxyURL:     proxyURL,
		DialProxyURL: proxyURL,
		account:      account,
	}
}

// resinCarriesEgress determines whether this account's outbound traffic is carried by Resin: Resin is enabled and the account has an identity
// (Resin sticks to an account, and without an identity there is nothing to stick to).
// It uses the same criteria as the fail-closed allow in auth.Store and the audit label in upstream_trace.
func resinCarriesEgress(account *auth.Account) bool {
	return IsResinEnabled() && account != nil
}

// ViaResin reports whether this outbound request goes through Resin.
func (e CodexEgress) ViaResin() bool {
	return e.Kind == CodexEgressResin
}

// Client returns the HTTP client for this path. Resin mode reuses the per-account isolated Resin connection pool;
// other modes reuse the gateway's main pool (uTLS + proxy). When account is nil and not Resin, it returns nil,
// and the caller handles it with its own fallback transport.
func (e CodexEgress) Client() *http.Client {
	if e.client != nil {
		return e.client
	}
	if e.account == nil {
		return nil
	}
	e.client = getPooledClient(e.account, e.DialProxyURL)
	return e.client
}

// ApplyHeaders adds the request headers this path requires: Resin mode injects the account identity header, and other modes leave headers unchanged.
func (e CodexEgress) ApplyHeaders(h http.Header) {
	if h == nil || !e.ViaResin() || e.account == nil {
		return
	}
	h.Set("X-Resin-Account", ResinAccountID(e.account))
}

// ResolveCodexWebsocketEgress decides the path for Codex WS outbound traffic: it returns the final WS address and
// the proxy used for dialing. In Resin mode the WS address is rewritten to Resin's ws:// reverse-proxy path, and dialing does not go through a proxy;
// the caller injects the account identity header with ApplyHeaders.
func ResolveCodexWebsocketEgress(account *auth.Account, wsURL, proxyURL string) CodexEgress {
	proxyURL = strings.TrimSpace(proxyURL)
	if resinCarriesEgress(account) {
		return CodexEgress{
			Kind:     CodexEgressResin,
			URL:      BuildWebSocketURL(wsURL),
			ProxyURL: proxyURL,
			account:  account,
		}
	}
	kind := CodexEgressDirect
	if proxyURL != "" {
		kind = CodexEgressProxy
	}
	return CodexEgress{
		Kind:         kind,
		URL:          wsURL,
		ProxyURL:     proxyURL,
		DialProxyURL: proxyURL,
		account:      account,
	}
}

// CodexDialProxyURL returns the proxy actually used for dialing: when Resin carries the outbound traffic it is always empty (the address is already the reverse-proxy
// path, and wrapping it in another proxy would only push Resin itself behind a proxy). It serves callers that only need to know whether dialing goes through a proxy,
// and whose URL has already been rewritten (WS connection setup), so the rewritten address is not rewritten a second time.
func CodexDialProxyURL(account *auth.Account, proxyURL string) string {
	if resinCarriesEgress(account) {
		return ""
	}
	return strings.TrimSpace(proxyURL)
}

// CodexEgressSummary is the global egress summary for the settings API and logs: it only answers "who carries Codex channel egress now
// carries egress?". Per-account proxy selection is still decided by auth.Store and is not expanded here.
type CodexEgressSummary struct {
	// Mode is resin or proxy_chain; proxy_chain means resolution in the order account > group > proxy pool > global > direct connection.
	Mode string `json:"mode"`
	// When ResinEnabled is true, the Codex channel's proxy pool and proxy_url do not take part in outbound traffic.
	ResinEnabled bool `json:"resin_enabled"`
	// ResinEndpoint is the masked Resin address (with the token in the path removed), used for display only.
	ResinEndpoint string `json:"resin_endpoint,omitempty"`
	// ResinPlatformName is the platform identifier on the Resin side.
	ResinPlatformName string `json:"resin_platform_name,omitempty"`
}

// CurrentCodexEgressSummary returns the Codex egress summary currently in effect.
func CurrentCodexEgressSummary() CodexEgressSummary {
	cfg := GetResinConfig()
	if cfg == nil {
		return CodexEgressSummary{Mode: "proxy_chain"}
	}
	return CodexEgressSummary{
		Mode:              "resin",
		ResinEnabled:      true,
		ResinEndpoint:     MaskResinBaseURL(cfg.BaseURL),
		ResinPlatformName: cfg.PlatformName,
	}
}

// MaskResinBaseURL masks the token (path segment) in the Resin base address, keeping only the scheme and
// host:port, for display in logs and the UI. A resin_url looks like http://host:port/<token>, and printing the whole
// string is the same as writing the credential into the log (issue #679).
func MaskResinBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "***"
	}
	masked := parsed.Scheme + "://" + parsed.Host
	if strings.Trim(parsed.Path, "/") != "" {
		masked += "/***"
	}
	return masked
}

// ResolveCodexRequestEgress picks the websocket or HTTP egress for one request.
func ResolveCodexRequestEgress(ctx context.Context, account *auth.Account, targetURL, proxyURL string, websocket bool) CodexEgress {
	if boundProxy, ok := AccountBoundEgress(ctx); ok {
		boundProxy = strings.TrimSpace(boundProxy)
		kind := CodexEgressDirect
		if boundProxy != "" {
			kind = CodexEgressProxy
		}
		egress := CodexEgress{Kind: kind, URL: targetURL, ProxyURL: boundProxy, DialProxyURL: boundProxy, account: account}
		if !websocket && account != nil {
			egress.client = getRequestClient(ctx, account, boundProxy)
		}
		return egress
	}
	if websocket {
		return ResolveCodexWebsocketEgress(account, targetURL, proxyURL)
	}
	return ResolveCodexEgress(account, targetURL, proxyURL)
}
