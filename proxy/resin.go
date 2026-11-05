package proxy

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
)

// ==================== Resin sticky proxy pool integration ====================

// ResinConfig holds the Resin proxy pool connection settings
type ResinConfig struct {
	BaseURL      string // full base address, for example http://127.0.0.1:2260/my-token
	PlatformName string // platform identifier, for example codex2api
}

// Global Resin config (atomic pointer, supports hot updates)
var resinCfg atomic.Pointer[ResinConfig]

// SetResinConfig sets the global Resin config; Resin is disabled when cfg is nil or BaseURL is empty.
// Enabling or disabling is synced to the egress flag in the auth package, so the scheduler's proxy pool fail-closed filter knows
// Codex accounts egress through Resin at this point. Addresses in logs keep only scheme://host, and the path segment
// is the Resin token, so it must not be written to disk in full (issue #679).
func SetResinConfig(cfg *ResinConfig) {
	wasEnabled := IsResinEnabled()
	if cfg != nil && strings.TrimSpace(cfg.BaseURL) != "" && strings.TrimSpace(cfg.PlatformName) != "" {
		resinCfg.Store(cfg)
		auth.SetResinEgressEnabled(true)
		log.Printf("[Resin] enabled: platform=%s endpoint=%s;all Codex channel egress goes through Resin, proxy pool/group proxy/account and global proxy_url no longer apply to Codex",
			cfg.PlatformName, MaskResinBaseURL(cfg.BaseURL))
		return
	}
	resinCfg.Store(nil)
	auth.SetResinEgressEnabled(false)
	if wasEnabled {
		log.Printf("[Resin] disabled; Codex channel egress resolves again as account > group > proxy pool > global > direct")
	}
}

// GetResinConfig returns the current Resin config, or nil when it is not configured
func GetResinConfig() *ResinConfig {
	return resinCfg.Load()
}

// IsResinEnabled reports whether the Resin proxy pool is enabled
func IsResinEnabled() bool {
	return GetResinConfig() != nil
}

// resinMaintenanceTarget for account maintenance side requests (wham usage / reset coupon / subscription expiry queries)
// decides the final URL and client. Like /responses, these requests carry the account identity to chatgpt.com,
// when Resin is enabled they must also go through the reverse proxy and reuse the Resin connection pool, otherwise every account shares the local machine's
// direct connection from the shared egress IP (issue #372). When it returns viaResin=true, the caller must set in the request headers
// X-Resin-Account; when disabled it returns the original URL and a nil client, and the caller falls back to the existing direct
// transport.
func resinMaintenanceTarget(account *auth.Account, targetURL string) (finalURL string, client *http.Client, viaResin bool) {
	egress := ResolveCodexEgress(account, targetURL, "")
	if !egress.ViaResin() {
		return targetURL, nil, false
	}
	return egress.URL, egress.Client(), true
}

// ==================== reverse proxy URL building ====================

// BuildReverseProxyURL converts a target URL into a Resin reverse proxy URL
// For example: https://chatgpt.com/backend-api/codex/responses
//
//	→ http://127.0.0.1:2260/my-token/codex2api/https/chatgpt.com/backend-api/codex/responses
func BuildReverseProxyURL(targetURL string) string {
	cfg := GetResinConfig()
	if cfg == nil {
		return targetURL
	}
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return targetURL
	}
	// <resin_base>/<platform>/<protocol>/<host><path+query>
	base := strings.TrimRight(cfg.BaseURL, "/")
	return fmt.Sprintf("%s/%s/%s/%s%s",
		base,
		cfg.PlatformName,
		parsed.Scheme,
		parsed.Host,
		parsed.RequestURI(),
	)
}

// BuildWebSocketURL converts a target WSS URL into a Resin WS reverse proxy URL
// For example: wss://chatgpt.com/backend-api/codex/responses
//
//	→ ws://127.0.0.1:2260/my-token/codex2api/https/chatgpt.com/backend-api/codex/responses
//
// Resin convention: the client-to-Resin side only supports ws://; the protocol path segment is http/https, matching the target ws/wss
func BuildWebSocketURL(targetURL string) string {
	cfg := GetResinConfig()
	if cfg == nil {
		return targetURL
	}
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return targetURL
	}
	resinParsed, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return targetURL
	}

	// wss → https, ws → http (the protocol field in the Resin path)
	protocol := "https"
	if parsed.Scheme == "ws" || parsed.Scheme == "http" {
		protocol = "http"
	}

	return fmt.Sprintf("ws://%s%s/%s/%s/%s%s",
		resinParsed.Host,
		resinParsed.Path,
		cfg.PlatformName,
		protocol,
		parsed.Host,
		parsed.RequestURI(),
	)
}

// ==================== account identity ====================

// ResinAccountID returns the stable account identifier in Resin (DBID as a string)
func ResinAccountID(account *auth.Account) string {
	return fmt.Sprintf("%d", account.DBID)
}

// ==================== lease inheritance ====================

// InheritLease hands the IP lease of a temporary identity over to the formal account identity
// Used for OAuth: the temporary identifier is used during authorization, then switched to the DBID after the account is created
func InheritLease(tempAccount, newAccount string) {
	cfg := GetResinConfig()
	if cfg == nil {
		return
	}

	inheritURL := fmt.Sprintf("%s/api/v1/%s/actions/inherit-lease",
		strings.TrimRight(cfg.BaseURL, "/"),
		cfg.PlatformName,
	)

	body := fmt.Sprintf(`{"parent_account":%q,"new_account":%q}`, tempAccount, newAccount)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, inheritURL, bytes.NewBufferString(body))
	if err != nil {
		log.Printf("[Resin] failed to build inherit-lease request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[Resin] inherit-lease request failed: %v", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[Resin] inherit-lease returned non-success status: %d (temp=%s new=%s)", resp.StatusCode, tempAccount, newAccount)
	} else {
		log.Printf("[Resin] inherit-lease succeeded: %s → %s", tempAccount, newAccount)
	}
}

// ==================== Resin connection pool ====================

// getResinHTTPClient returns a standard HTTP client that goes through the Resin reverse proxy (no uTLS)
// pool keys are isolated by accountID, reusing the underlying TCP connections
func getResinHTTPClient(account *auth.Account) *http.Client {
	key := fmt.Sprintf("resin|%d", account.ID())

	if v, ok := clientPool.Load(key); ok {
		entry := v.(*poolEntry)
		entry.touch()
		return entry.client
	}

	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 5,
		MaxConnsPerHost:     10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	entry := &poolEntry{
		client: &http.Client{
			Transport: transport,
			Timeout:   0, // no timeout for streaming responses
		},
	}
	entry.touch()

	if v, loaded := clientPool.LoadOrStore(key, entry); loaded {
		e := v.(*poolEntry)
		e.touch()
		return e.client
	}
	return entry.client
}
