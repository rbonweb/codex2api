package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

// bootstrapState tracks the runtime state of the bootstrap endpoint, mainly for:
//  1. Preventing duplicate writes under concurrency;
//  2. A simple global rate limit that only counts requests with a wrong token, so scanners cannot brute-force it.
var bootstrapState struct {
	mu sync.Mutex

	// rateBucket: a simple fixed-window rate limit, measured as at most maxPerWindow times per windowSec
	windowStart atomic.Int64 // unix seconds
	count       atomic.Int64
}

// bootstrapTokenState holds the one-time token needed to initialize the page. The token exists only in process memory,
// and is printed to the service log when generated: only the deployer who can read the server log can complete initialization, not
// anyone whose source IP merely looks like an internal network. Docker Desktop / rootless container port forwarding and
// private-network reverse proxies both turn the source of public requests into a private address.
var bootstrapTokenState struct {
	mu    sync.Mutex
	token string
}

const (
	bootstrapWindowSec  = 60
	bootstrapMaxPerWin  = 20
	bootstrapMinSecret  = 8
	bootstrapMaxSecret  = 256
	bootstrapTokenBytes = 16
)

// EnsureBootstrapToken returns the current one-time bootstrap token, generating it and printing it to the log if it has not been generated yet.
// The token is revoked after initialization succeeds; if the admin key is later cleared and the system returns to the pending-initialization state, a new token is generated.
func EnsureBootstrapToken() (string, error) {
	bootstrapTokenState.mu.Lock()
	defer bootstrapTokenState.mu.Unlock()
	if bootstrapTokenState.token != "" {
		return bootstrapTokenState.token, nil
	}
	buf := make([]byte, bootstrapTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	bootstrapTokenState.token = hex.EncodeToString(buf)
	log.Printf("[bootstrap] initialization token (bootstrap token): %s", bootstrapTokenState.token)
	log.Println("[bootstrap] The first-run setup page needs this token; it is valid only for the current process, is regenerated after a restart, and is revoked immediately once initialization succeeds.")
	return bootstrapTokenState.token, nil
}

// bootstrapTokenMatches compares the submitted token in constant time. It never matches while no token has been generated.
func bootstrapTokenMatches(submitted string) bool {
	submitted = strings.ToLower(strings.TrimSpace(submitted))
	bootstrapTokenState.mu.Lock()
	defer bootstrapTokenState.mu.Unlock()
	if bootstrapTokenState.token == "" || submitted == "" {
		return false
	}
	return security.SecureCompare(submitted, bootstrapTokenState.token)
}

// revokeBootstrapToken revokes the token after initialization succeeds.
func revokeBootstrapToken() {
	bootstrapTokenState.mu.Lock()
	bootstrapTokenState.token = ""
	bootstrapTokenState.mu.Unlock()
}

// bootstrapAuditIP builds the source field for the audit log: ip is the direct peer, which differs from the client IP resolved through a trusted proxy,
// so forwarded is recorded as well, to tell the real peer apart from the address claimed by the forwarding header.
func bootstrapAuditIP(c *gin.Context) string {
	peer := c.RemoteIP()
	forwarded := c.ClientIP()
	if forwarded == "" || forwarded == peer {
		return "ip=" + peer
	}
	return "ip=" + peer + " forwarded=" + forwarded
}

// bootstrapAllowRate implements the fixed-window rate limit with CAS:
//   - At any moment only one goroutine can successfully renew the window start; the other losers read
//     the latest renewed value, which prevents several goroutines from resetting count to 0 at the same time.
//   - Increment the count before checking, so the limit is never exceeded under high concurrency.
func bootstrapAllowRate() bool {
	now := time.Now().Unix()
	for {
		winStart := bootstrapState.windowStart.Load()
		if now-winStart < bootstrapWindowSec {
			break
		}
		// Advance only if windowStart is still the old value we read; if another goroutine has already
		// advanced it, exit the loop and reuse the latest window.
		if bootstrapState.windowStart.CompareAndSwap(winStart, now) {
			bootstrapState.count.Store(0)
			break
		}
	}
	newCount := bootstrapState.count.Add(1)
	return newCount <= bootstrapMaxPerWin
}

// bootstrapAllowClientIP reports whether this source IP may run page initialization. The caller must pass
// the direct TCP peer address (c.RemoteIP()) rather than c.ClientIP(): the latter, when the peer is a trusted proxy,
// is taken from X-Forwarded-For, and private subnets are trusted by default, so a requester can claim any internal address it likes.
// This gate is only defense in depth; the real authorization credential is the one-time initialization token.
//
// Rules:
//   - loopback is always allowed;
//   - when BOOTSTRAP_ALLOWED_CIDR is set, it takes precedence (comma-separated CIDRs; set to none to allow loopback only);
//   - when unset, private and link-local addresses are allowed by default: after Docker port mapping, the source IP of host access is
//     the bridge address (such as 172.17.0.1) rather than loopback; if only loopback were allowed, the most common local
//     Docker deployments would be kept off the initialization page (issue #199). Public IPs are still rejected by default.
func bootstrapAllowClientIP(clientIP string) bool {
	ip := net.ParseIP(strings.TrimSpace(clientIP))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	cidrEnv := strings.TrimSpace(os.Getenv("BOOTSTRAP_ALLOWED_CIDR"))
	if cidrEnv == "" {
		return ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	if strings.EqualFold(cidrEnv, "none") {
		return false
	}
	for _, cidr := range strings.Split(cidrEnv, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, network, err := net.ParseCIDR(cidr)
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func firstForwardedHeaderValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if idx := strings.Index(value, ","); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

func bootstrapPublicBaseURL(r *http.Request) string {
	if r == nil {
		return ""
	}
	proto := firstForwardedHeaderValue(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := firstForwardedHeaderValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		return ""
	}
	return strings.TrimRight(proto+"://"+host, "/")
}

func bootstrapDatabaseLocation(driver string) string {
	if strings.EqualFold(driver, "sqlite") {
		return strings.TrimSpace(os.Getenv("DATABASE_PATH"))
	}
	// DATABASE_URL wins over the split variables (config.Load); show only
	// host[:port]/name from it, never the credentials.
	if raw := strings.TrimSpace(os.Getenv("DATABASE_URL")); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			return ""
		}
		name := strings.TrimPrefix(parsed.Path, "/")
		if name == "" {
			return ""
		}
		return parsed.Host + "/" + name
	}
	host := strings.TrimSpace(os.Getenv("DATABASE_HOST"))
	name := strings.TrimSpace(os.Getenv("DATABASE_NAME"))
	port := strings.TrimSpace(os.Getenv("DATABASE_PORT"))
	if host == "" || name == "" {
		return ""
	}
	if port != "" {
		host += ":" + port
	}
	return host + "/" + name
}

func (h *Handler) bootstrapSetupHints(r *http.Request) gin.H {
	serviceURL := bootstrapPublicBaseURL(r)
	adminURL := ""
	apiBaseURL := ""
	if serviceURL != "" {
		adminURL = strings.TrimRight(serviceURL, "/") + "/admin/"
		apiBaseURL = strings.TrimRight(serviceURL, "/") + "/v1"
	}
	databaseDriver := h.databaseDriver
	databaseLabel := h.databaseLabel
	if databaseDriver == "" && h.db != nil {
		databaseDriver = h.db.Driver()
	}
	if databaseLabel == "" && h.db != nil {
		databaseLabel = h.db.Label()
	}
	cacheDriver := h.cacheDriver
	cacheLabel := h.cacheLabel
	if cacheDriver == "" && h.cache != nil {
		cacheDriver = h.cache.Driver()
	}
	if cacheLabel == "" && h.cache != nil {
		cacheLabel = h.cache.Label()
	}

	usageLogMode := database.UsageLogModeFull
	usageLogBatchSize := 200
	usageLogFlushIntervalSeconds := 5
	if h.db != nil {
		usageLogMode = h.db.GetUsageLogMode()
		usageLogBatchSize = h.db.GetUsageLogBatchSize()
		usageLogFlushIntervalSeconds = h.db.GetUsageLogFlushIntervalSeconds()
	}

	imageBackend := imagestore.CurrentConfig().Backend
	if imageBackend == "" {
		imageBackend = imagestore.BackendLocal
	}

	return gin.H{
		"service_url":  serviceURL,
		"admin_url":    adminURL,
		"api_base_url": apiBaseURL,
		"database": gin.H{
			"driver":   databaseDriver,
			"label":    databaseLabel,
			"location": bootstrapDatabaseLocation(databaseDriver),
		},
		"cache": gin.H{
			"driver": cacheDriver,
			"label":  cacheLabel,
		},
		"data": gin.H{
			"image_local_dir":       imageAssetDir(),
			"image_storage_backend": imageBackend,
		},
		"usage": gin.H{
			"log_mode":               usageLogMode,
			"batch_size":             usageLogBatchSize,
			"flush_interval_seconds": usageLogFlushIntervalSeconds,
		},
	}
}

// GetBootstrapStatus returns whether initialization is currently needed (GET /api/admin/bootstrap-status).
//
// This endpoint does not require auth; before showing the login screen, the frontend AuthGate polls this endpoint:
//   - ADMIN_SECRET is set via .env => needs_bootstrap=false, source="env"
//   - written to the database               => needs_bootstrap=false, source="database"
//   - both are empty                        => needs_bootstrap=true,  source="empty"
func (h *Handler) GetBootstrapStatus(c *gin.Context) {
	envSecret := strings.TrimSpace(h.adminSecretEnv)
	if envSecret != "" {
		c.JSON(http.StatusOK, gin.H{
			"needs_bootstrap": false,
			"source":          "env",
		})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		// On database errors, prefer fail-closed: do not allow bootstrap, and let operators check the DB first
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"needs_bootstrap": false,
			"source":          "error",
			"error":           "Failed to read system settings; check the database connection",
		})
		return
	}
	if settings != nil && strings.TrimSpace(settings.AdminSecret) != "" {
		c.JSON(http.StatusOK, gin.H{
			"needs_bootstrap": false,
			"source":          "database",
		})
		return
	}
	// When the admin key is cleared at runtime and the system re-enters the pending-initialization state, this part re-issues the token to the log.
	if _, err := EnsureBootstrapToken(); err != nil {
		log.Printf("[bootstrap] failed to generate initialization token: %v", err)
	}
	c.JSON(http.StatusOK, gin.H{
		"needs_bootstrap": true,
		"source":          "empty",
		"setup":           h.bootstrapSetupHints(c.Request),
	})
}

// GetSetupHints returns the deployment check information shown after login.
//
// Unlike the public bootstrap-status, this endpoint is registered under /api/admin and requires the admin key,
// so it is safe to return deployment diagnostics such as the database location and image directory.
func (h *Handler) GetSetupHints(c *gin.Context) {
	c.JSON(http.StatusOK, h.bootstrapSetupHints(c.Request))
}

// PostBootstrap receives the initial admin key the user enters in the browser and writes it to the database.
//
// Security constraints:
//  1. Available only when ADMIN_SECRET is not configured; otherwise always 409;
//  2. Must carry the one-time initialization token from the startup log; the source IP gate judges by the direct peer and is only defense in depth;
//  3. Use a mutex plus a double check to prevent concurrent writes;
//  4. Requests with a wrong token count toward the global rate limit; requests with the correct token are not affected by it, so a scanner that fills the limit cannot block the deployer;
//  5. Validate the minimum length (8 runes) to avoid weak keys;
//  6. Audit log throughout.
func (h *Handler) PostBootstrap(c *gin.Context) {
	auditIP := bootstrapAuditIP(c)
	if peer := c.RemoteIP(); !bootstrapAllowClientIP(peer) {
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_IP", auditIP)
		c.JSON(http.StatusForbidden, gin.H{
			"error": "The current source IP (" + peer + ") is not allowed to run page initialization. Choose one: 1) set ADMIN_SECRET in .env and rebuild the container, then skip page initialization and sign in directly; 2) set the BOOTSTRAP_ALLOWED_CIDR environment variable to allow this source subnet (for example BOOTSTRAP_ALLOWED_CIDR=203.0.113.0/24). This check only looks at the TCP peer connected directly to the service and does not read X-Forwarded-For; when accessing through a reverse proxy, allow the subnet the proxy is in.",
			"code":  "bootstrap_ip_forbidden",
		})
		return
	}

	envSecret := strings.TrimSpace(h.adminSecretEnv)
	if envSecret != "" {
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_ENV", auditIP)
		c.JSON(http.StatusConflict, gin.H{
			"error": "ADMIN_SECRET is configured through an environment variable; no page initialization is needed",
		})
		return
	}

	// Confirm the system is really pending initialization before checking the token: once initialized, the token is long revoked, so return 409 directly
	// which is more accurate than "invalid token"; bootstrap-status already exposes this information publicly.
	preCtx, preCancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	current, err := h.db.GetSystemSettings(preCtx)
	preCancel()
	if err != nil {
		security.SecurityAuditLog("BOOTSTRAP_DB_READ_ERROR", auditIP+" err="+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read system settings"})
		return
	}
	if current != nil && strings.TrimSpace(current.AdminSecret) != "" {
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_ALREADY_INITIALIZED", auditIP)
		c.JSON(http.StatusConflict, gin.H{
			"error": "ADMIN_SECRET is already configured, so initialization cannot run again. To reset it, sign in with the existing key and change it on the 'Settings' page.",
		})
		return
	}
	if _, err := EnsureBootstrapToken(); err != nil {
		security.SecurityAuditLog("BOOTSTRAP_TOKEN_ERROR", auditIP+" err="+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate initialization token"})
		return
	}

	var body struct {
		AdminSecret    string `json:"admin_secret"`
		BootstrapToken string `json:"bootstrap_token"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body format"})
		return
	}
	if !bootstrapTokenMatches(body.BootstrapToken) {
		if !bootstrapAllowRate() {
			security.SecurityAuditLog("BOOTSTRAP_RATE_LIMITED", auditIP)
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many requests, please try again later"})
			return
		}
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_TOKEN", auditIP)
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "Invalid initialization token. Search the service log for bootstrap token to get the token (for example docker compose logs codex2api | grep \"bootstrap token\"); restarting the service generates a new token.",
			"code":  "bootstrap_token_invalid",
		})
		return
	}
	secret := strings.TrimSpace(body.AdminSecret)
	if secret == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Admin key cannot be empty"})
		return
	}
	if utf8.RuneCountInString(secret) < bootstrapMinSecret {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Admin key must be at least 8 characters",
		})
		return
	}
	if len(secret) > bootstrapMaxSecret {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Admin key is too long"})
		return
	}

	bootstrapState.mu.Lock()
	defer bootstrapState.mu.Unlock()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	// Double check: read again after entering the critical section, to avoid concurrent writes
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		security.SecurityAuditLog("BOOTSTRAP_DB_READ_ERROR", auditIP+" err="+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read system settings"})
		return
	}
	if settings != nil && strings.TrimSpace(settings.AdminSecret) != "" {
		security.SecurityAuditLog("BOOTSTRAP_REJECTED_ALREADY_INITIALIZED", auditIP)
		c.JSON(http.StatusConflict, gin.H{
			"error": "ADMIN_SECRET is already configured, so initialization cannot run again. To reset it, sign in with the existing key and change it on the 'Settings' page.",
		})
		return
	}
	if settings == nil {
		settings = defaultBootstrapSettings()
	}
	settings.AdminSecret = secret

	if err := h.db.UpdateSystemSettings(ctx, settings); err != nil {
		security.SecurityAuditLog("BOOTSTRAP_DB_WRITE_ERROR", auditIP+" err="+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to write system settings"})
		return
	}

	revokeBootstrapToken()
	security.SecurityAuditLog("BOOTSTRAP_SUCCESS", auditIP)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// defaultBootstrapSettings returns the safe default values first recorded in the settings table.
// Keep these consistent with step 3 in main.go, so that PostBootstrap, while the database has no records yet,
// does not write empty values that leave later business settings missing.
func defaultBootstrapSettings() *database.SystemSettings {
	promptFilterCfg := promptfilter.RecommendedConfig()
	return &database.SystemSettings{
		SiteName:                          database.DefaultSiteName,
		MaxConcurrency:                    2,
		CodexTelemetryEnabled:             false, // experimental: simulated telemetry is not sent out by default; the deployer must turn it on explicitly
		GlobalRPM:                         0,
		TestModel:                         auth.DefaultTestModel,
		TestContent:                       auth.DefaultTestContent,
		TestConcurrency:                   50,
		BackgroundRefreshIntervalMinutes:  2,
		UsageProbeMaxAgeMinutes:           10,
		RecoveryProbeIntervalMinutes:      30,
		LazyMode:                          false,
		PgMaxConns:                        50,
		RedisPoolSize:                     30,
		PromptFilterMode:                  promptFilterCfg.Mode,
		PromptFilterThreshold:             promptFilterCfg.Threshold,
		PromptFilterStrictThreshold:       promptFilterCfg.StrictThreshold,
		PromptFilterStrictTerminalEnabled: promptFilterCfg.StrictTerminalEnabled,
		PromptFilterAdvancedConfig:        promptfilter.MarshalAdvancedConfig(promptFilterCfg.Advanced),
		PromptFilterLogMatches:            true,
		PromptFilterMaxTextLength:         81920,
		PromptFilterCustomPatterns:        "[]",
		PromptFilterDisabledPatterns:      "[]",
		ClientCompatMode:                  proxy.ClientCompatModePreserve,
		CodexMinCLIVersion:                "0.160.0",
		UsageLogMode:                      database.UsageLogModeFull,
		UsageLogBatchSize:                 200,
		UsageLogFlushIntervalSeconds:      5,
		StreamFlushPolicy:                 proxy.StreamFlushPolicyImmediate,
		StreamFlushIntervalMS:             20,
		FirstTokenMode:                    proxy.FirstTokenModeLoose,
		FirstTokenTimeoutSeconds:          0,
		BillingTierPolicy:                 proxy.BillingTierPolicyActual,
		AffinityMode:                      "bounded",
		PublicKeyUsagePageEnabled:         true,
		PublicImageStudioPageEnabled:      true,
		CodexWSHideUpstreamErrors:         true,
		CodexWSSilentRetryEnabled:         true,
		CodexWSSilentMaxRetries:           2,
		CodexWSStatelessSlots:             proxy.DefaultRuntimeSettings().CodexWSStatelessSlots,
		CodexWSDownstreamKeepaliveSlots:   database.DefaultCodexWSDownstreamKeepaliveSlots,
		AutoPause5hGuardBandPercent:       5,
		AutoPause5hGuardConcurrency:       1,
		SmartPacingMinConcurrency:         1,
		SmartPacingWindows:                "5h,7d",
		AutoResetCreditsBeforeExpiryMin:   60,
		UTLSShutdownTimeoutMinutes:        30,
	}
}
