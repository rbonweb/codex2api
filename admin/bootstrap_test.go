package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func TestDefaultBootstrapSettingsUseRecommendedPromptFilterPreset(t *testing.T) {
	settings := defaultBootstrapSettings()
	if settings.CodexMinCLIVersion != "0.160.0" {
		t.Fatalf("fresh install minimum Codex CLI version = %q, want 0.160.0", settings.CodexMinCLIVersion)
	}
	if settings.PromptFilterEnabled {
		t.Fatal("fresh install must keep the prompt-filter master switch explicit")
	}
	if settings.PromptFilterMode != promptfilter.ModeBlock || !settings.PromptFilterStrictTerminalEnabled {
		t.Fatalf("prompt filter bootstrap = mode:%q strict_terminal:%t", settings.PromptFilterMode, settings.PromptFilterStrictTerminalEnabled)
	}
	advanced, err := promptfilter.ParseAdvancedConfig(settings.PromptFilterAdvancedConfig)
	if err != nil {
		t.Fatalf("parse recommended advanced config: %v", err)
	}
	if !advanced.Normalization.Enabled || advanced.Normalization.MaxDecodeRuns != 2 {
		t.Fatalf("recommended bootstrap normalization = %+v", advanced.Normalization)
	}
	if advanced.Output.Enabled || advanced.Sidecar.Enabled || advanced.Intelligence.Enabled {
		t.Fatalf("latency-sensitive bootstrap layers must remain opt-in: %+v", advanced)
	}
}

// issue #199: after Docker port mapping, the source IP of host access is the bridge address (private network) rather than loopback,
// so when BOOTSTRAP_ALLOWED_CIDR is not set, private-network sources must be allowed by default, otherwise local Docker deployments
// cannot finish page initialization.
func TestBootstrapAllowClientIPDefaultsAllowPrivate(t *testing.T) {
	t.Setenv("BOOTSTRAP_ALLOWED_CIDR", "")

	allowed := []string{
		"127.0.0.1", "::1", // loopback
		"172.17.0.1", "172.18.0.1", // docker bridge
		"10.0.0.5", "192.168.1.100", // common private networks
		"fd00::1",      // ULA IPv6 private network
		"169.254.10.1", // link-local
	}
	for _, ip := range allowed {
		if !bootstrapAllowClientIP(ip) {
			t.Errorf("bootstrapAllowClientIP(%q) = false, want true (default private allow)", ip)
		}
	}

	denied := []string{"203.0.113.7", "8.8.8.8", "2001:4860:4860::8888", "", "not-an-ip"}
	for _, ip := range denied {
		if bootstrapAllowClientIP(ip) {
			t.Errorf("bootstrapAllowClientIP(%q) = true, want false (public/invalid must be denied)", ip)
		}
	}
}

func TestBootstrapAllowClientIPExplicitCIDROverridesDefault(t *testing.T) {
	t.Setenv("BOOTSTRAP_ALLOWED_CIDR", "203.0.113.0/24")

	if !bootstrapAllowClientIP("203.0.113.7") {
		t.Error("IP inside configured CIDR should be allowed")
	}
	// Once explicitly configured, the default private-network allowance no longer applies; only the configured subnets and loopback are accepted
	if bootstrapAllowClientIP("172.17.0.1") {
		t.Error("private IP outside configured CIDR should be denied when CIDR is set")
	}
	if !bootstrapAllowClientIP("127.0.0.1") {
		t.Error("loopback must always be allowed")
	}
}

func TestBootstrapAllowClientIPNoneDisablesDefault(t *testing.T) {
	t.Setenv("BOOTSTRAP_ALLOWED_CIDR", "none")

	if bootstrapAllowClientIP("172.17.0.1") {
		t.Error("BOOTSTRAP_ALLOWED_CIDR=none should deny private IPs")
	}
	if !bootstrapAllowClientIP("127.0.0.1") {
		t.Error("loopback must always be allowed even with none")
	}
}

// resetBootstrapGlobals clears the package-level token and rate limit state, so test cases do not pollute each other.
func resetBootstrapGlobals(t *testing.T) {
	t.Helper()
	reset := func() {
		bootstrapTokenState.mu.Lock()
		bootstrapTokenState.token = ""
		bootstrapTokenState.mu.Unlock()
		bootstrapState.windowStart.Store(0)
		bootstrapState.count.Store(0)
	}
	reset()
	t.Cleanup(reset)
}

// newBootstrapTestEngine wires the routes using the config's default trusted proxies (loopback + private networks),
// matching the ClientIP resolution of the default production deployment.
func newBootstrapTestEngine(t *testing.T, h *Handler) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if err := r.SetTrustedProxies([]string{"127.0.0.1", "::1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}
	r.GET("/api/admin/bootstrap-status", h.GetBootstrapStatus)
	r.POST("/api/admin/bootstrap", h.PostBootstrap)
	return r
}

type bootstrapReq struct {
	remote string
	xff    string
	token  string
	secret string
}

func doBootstrap(t *testing.T, r *gin.Engine, in bootstrapReq) (int, map[string]any) {
	t.Helper()
	secret := in.secret
	if secret == "" {
		secret = "attacker-chosen-secret"
	}
	payload, _ := json.Marshal(map[string]string{"admin_secret": secret, "bootstrap_token": in.token})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = in.remote
	if in.xff != "" {
		req.Header.Set("X-Forwarded-For", in.xff)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func storedAdminSecret(t *testing.T, h *Handler) string {
	t.Helper()
	settings, err := h.db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if settings == nil {
		return ""
	}
	return settings.AdminSecret
}

// Private-network sources (Docker Desktop / rootless port forwarding, private reverse proxies) pass the IP gate by default,
// so at that point the one-time token must guard initialization; whoever arrives first must not set the key.
func TestPostBootstrapRequiresOneTimeToken(t *testing.T) {
	t.Setenv("BOOTSTRAP_ALLOWED_CIDR", "")
	resetBootstrapGlobals(t)
	h := &Handler{db: newTestAdminDB(t)}
	r := newBootstrapTestEngine(t, h)

	for _, token := range []string{"", "00000000000000000000000000000000"} {
		code, body := doBootstrap(t, r, bootstrapReq{remote: "192.168.97.1:40000", token: token})
		if code != http.StatusUnauthorized || body["code"] != "bootstrap_token_invalid" {
			t.Fatalf("token=%q: status=%d body=%v, want 401 bootstrap_token_invalid", token, code, body)
		}
	}
	if got := storedAdminSecret(t, h); got != "" {
		t.Fatalf("admin secret written without a valid token: %q", got)
	}

	token, err := EnsureBootstrapToken()
	if err != nil {
		t.Fatalf("EnsureBootstrapToken: %v", err)
	}
	code, body := doBootstrap(t, r, bootstrapReq{remote: "192.168.97.1:40000", token: " " + token + " ", secret: "operator-secret"})
	if code != http.StatusOK {
		t.Fatalf("valid token: status=%d body=%v, want 200", code, body)
	}
	if got := storedAdminSecret(t, h); got != "operator-secret" {
		t.Fatalf("stored admin secret = %q, want operator-secret", got)
	}

	// A token is void after use: replaying the same token gets only 409, and no new token is generated.
	code, _ = doBootstrap(t, r, bootstrapReq{remote: "192.168.97.1:40000", token: token})
	if code != http.StatusConflict {
		t.Fatalf("replayed token: status=%d, want 409", code)
	}
	if bootstrapTokenMatches(token) {
		t.Fatal("bootstrap token must be revoked after a successful bootstrap")
	}
}

// The IP gate must look at the direct TCP peer: when the peer is in a default-trusted private subnet, X-Forwarded-For is entirely controlled by
// the requester, and must not let it disguise the request as BOOTSTRAP_ALLOWED_CIDR or pose as an internal network.
func TestPostBootstrapIPGateIgnoresForwardedFor(t *testing.T) {
	resetBootstrapGlobals(t)
	h := &Handler{db: newTestAdminDB(t)}
	r := newBootstrapTestEngine(t, h)

	t.Setenv("BOOTSTRAP_ALLOWED_CIDR", "203.0.113.0/24")
	// A private reverse proxy passes the forged header through unchanged: the old implementation judged by ClientIP and matched the allowlist.
	code, body := doBootstrap(t, r, bootstrapReq{remote: "172.18.0.5:40000", xff: "203.0.113.7"})
	if code != http.StatusForbidden || body["code"] != "bootstrap_ip_forbidden" {
		t.Fatalf("spoofed XFF into allowed CIDR: status=%d body=%v, want 403", code, body)
	}
	if msg, _ := body["error"].(string); !bytes.Contains([]byte(msg), []byte("172.18.0.5")) {
		t.Fatalf("403 should echo the direct peer IP, got %q", msg)
	}

	t.Setenv("BOOTSTRAP_ALLOWED_CIDR", "")
	// For direct public connections the forwarding header is never accepted in the first place, so it is still rejected.
	if code, _ := doBootstrap(t, r, bootstrapReq{remote: "203.0.113.9:40000", xff: "192.168.1.77"}); code != http.StatusForbidden {
		t.Fatalf("public peer with private XFF: status=%d, want 403", code)
	}
	// A private-network peer claiming to be a public address does not change the decision: after passing the IP gate, the token is still required.
	if code, _ := doBootstrap(t, r, bootstrapReq{remote: "192.168.97.1:40000", xff: "8.8.8.8"}); code != http.StatusUnauthorized {
		t.Fatalf("private peer with public XFF: status=%d, want 401 (gate passes, token missing)", code)
	}
}

// The rate limit only counts requests with a wrong token: after scanners fill the window, a deployer holding the correct token can still complete initialization.
func TestPostBootstrapRateLimitDoesNotLockOutValidToken(t *testing.T) {
	t.Setenv("BOOTSTRAP_ALLOWED_CIDR", "")
	resetBootstrapGlobals(t)
	h := &Handler{db: newTestAdminDB(t)}
	r := newBootstrapTestEngine(t, h)

	for i := 0; i < bootstrapMaxPerWin; i++ {
		if code, _ := doBootstrap(t, r, bootstrapReq{remote: "10.0.0.8:40000", token: "wrong"}); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status=%d, want 401", i+1, code)
		}
	}
	if code, _ := doBootstrap(t, r, bootstrapReq{remote: "10.0.0.8:40000", token: "wrong"}); code != http.StatusTooManyRequests {
		t.Fatalf("attempt over limit: status=%d, want 429", code)
	}

	token, err := EnsureBootstrapToken()
	if err != nil {
		t.Fatalf("EnsureBootstrapToken: %v", err)
	}
	if code, body := doBootstrap(t, r, bootstrapReq{remote: "10.0.0.8:40000", token: token, secret: "operator-secret"}); code != http.StatusOK {
		t.Fatalf("valid token while rate limited: status=%d body=%v, want 200", code, body)
	}
}

func TestPostBootstrapEnvSecretSkipsToken(t *testing.T) {
	t.Setenv("BOOTSTRAP_ALLOWED_CIDR", "")
	resetBootstrapGlobals(t)
	h := &Handler{db: newTestAdminDB(t), adminSecretEnv: "from-env-secret"}
	r := newBootstrapTestEngine(t, h)

	if code, _ := doBootstrap(t, r, bootstrapReq{remote: "127.0.0.1:40000"}); code != http.StatusConflict {
		t.Fatalf("env secret configured: status=%d, want 409", code)
	}
	if bootstrapTokenState.token != "" {
		t.Fatal("no bootstrap token should be generated when ADMIN_SECRET comes from env")
	}
}

// When the admin key is cleared at runtime and the system returns to pending initialization, bootstrap-status reissues the token.
func TestGetBootstrapStatusIssuesTokenWhenPending(t *testing.T) {
	resetBootstrapGlobals(t)
	h := &Handler{db: newTestAdminDB(t)}
	r := newBootstrapTestEngine(t, h)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/bootstrap-status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"needs_bootstrap":true`)) {
		t.Fatalf("bootstrap-status = %d %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte(bootstrapTokenState.token)) {
		t.Fatal("bootstrap-status must never expose the token")
	}
	if len(bootstrapTokenState.token) != bootstrapTokenBytes*2 {
		t.Fatalf("bootstrap token = %q, want %d hex chars", bootstrapTokenState.token, bootstrapTokenBytes*2)
	}
}

func TestPostBootstrapPersistsWebSocketRetentionDefaultsAndExplicitZero(t *testing.T) {
	for _, existingZero := range []bool{false, true} {
		name := "fresh database"
		if existingZero {
			name = "existing explicit zero"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("BOOTSTRAP_ALLOWED_CIDR", "none")
			resetBootstrapGlobals(t)
			h := &Handler{db: newTestAdminDB(t)}
			if existingZero {
				settings := defaultBootstrapSettings()
				settings.CodexWSStatelessSlots = 0
				settings.CodexWSDownstreamKeepaliveSlots = 0
				if err := h.db.UpdateSystemSettings(context.Background(), settings); err != nil {
					t.Fatal(err)
				}
			}
			token, err := EnsureBootstrapToken()
			if err != nil {
				t.Fatal(err)
			}
			code, _ := doBootstrap(t, newBootstrapTestEngine(t, h), bootstrapReq{remote: "127.0.0.1:40000", token: token, secret: "synthetic-bootstrap-operator"})
			if code != http.StatusOK {
				t.Fatalf("bootstrap status=%d", code)
			}
			stored, err := h.db.GetSystemSettings(context.Background())
			if err != nil || stored == nil {
				t.Fatalf("bootstrap settings unavailable: %v", err)
			}
			wantBlank, wantChat := proxy.DefaultRuntimeSettings().CodexWSStatelessSlots, database.DefaultCodexWSDownstreamKeepaliveSlots
			if existingZero {
				wantBlank, wantChat = 0, 0
			}
			if stored.CodexWSStatelessSlots != wantBlank || stored.CodexWSDownstreamKeepaliveSlots != wantChat {
				t.Fatalf("bootstrap WS retention = blank:%d/chat:%d, want blank:%d/chat:%d", stored.CodexWSStatelessSlots, stored.CodexWSDownstreamKeepaliveSlots, wantBlank, wantChat)
			}
		})
	}
}
