package admin

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func codexRestoredPatchScheduler(t *testing.T, handler *Handler, accountID int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID),
		strings.NewReader(body),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateAccountScheduler(ctx)
	return recorder
}

func codexRestoredFingerprintCredential(t *testing.T, db *database.DB, accountID int64) string {
	t.Helper()
	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	return row.GetCredential(auth.CodexFingerprintModeCredentialKey)
}

func codexRestoredAgentPrivateKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Ed25519 key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("encode PKCS8 key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// TestUpdateSettingsPersistsCodexFingerprintDefaultMode_CodexUpstream checks that the default fingerprint mode for new accounts saved through PUT /settings agrees in the response, the database and the runtime store.
func TestUpdateSettingsPersistsCodexFingerprintDefaultMode_CodexUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/admin/settings",
		strings.NewReader(`{"codex_fingerprint_default_mode":"session"}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateSettings(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response settingsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.CodexFingerprintDefaultMode != auth.CodexFingerprintModeSession {
		t.Fatalf("response mode = %q, want %q", response.CodexFingerprintDefaultMode, auth.CodexFingerprintModeSession)
	}
	if got := store.GetCodexFingerprintDefaultMode(); got != auth.CodexFingerprintModeSession {
		t.Fatalf("store mode = %q, want %q", got, auth.CodexFingerprintModeSession)
	}
	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if persisted.CodexFingerprintDefaultMode != auth.CodexFingerprintModeSession {
		t.Fatalf("persisted mode = %q, want %q", persisted.CodexFingerprintDefaultMode, auth.CodexFingerprintModeSession)
	}

	// An invalid mode is rejected as a whole and changes nothing.
	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/admin/settings",
		strings.NewReader(`{"codex_fingerprint_default_mode":"converge"}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateSettings(ctx)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if got := store.GetCodexFingerprintDefaultMode(); got != auth.CodexFingerprintModeSession {
		t.Fatalf("store mode after rejected update = %q, want %q", got, auth.CodexFingerprintModeSession)
	}
}

// TestNewCodexAccountCredentialsStampsDefaultFingerprintMode_CodexUpstream checks that new accounts get the system default mode in both credentials and runtime state, while tokenCredentialMap (the update path) never carries the key so a re-import cannot overwrite a mode the user adjusted.
func TestNewCodexAccountCredentialsStampsDefaultFingerprintMode_CodexUpstream(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	handler := &Handler{db: db, store: store}
	seed := tokenCredentialSeed{refreshToken: "rt-stamp"}

	// The default is off: no key is written, exactly as before the setting existed.
	if _, ok := handler.newCodexAccountCredentials(seed)[auth.CodexFingerprintModeCredentialKey]; ok {
		t.Fatal("the codex_fingerprint_mode key must not be written while the default is off")
	}

	store.SetCodexFingerprintDefaultMode(auth.CodexFingerprintModeSession)
	credentials := handler.newCodexAccountCredentials(seed)
	if got := credentials[auth.CodexFingerprintModeCredentialKey]; got != auth.CodexFingerprintModeSession {
		t.Fatalf("credential = %v, want %q", got, auth.CodexFingerprintModeSession)
	}
	account := handler.newCodexAccountFromSeed(7, "", seed)
	if got := account.EffectiveCodexFingerprintMode(); got != auth.CodexFingerprintModeSession {
		t.Fatalf("runtime mode = %q, want %q", got, auth.CodexFingerprintModeSession)
	}

	if _, ok := tokenCredentialMap(seed)[auth.CodexFingerprintModeCredentialKey]; ok {
		t.Fatal("tokenCredentialMap must not carry codex_fingerprint_mode, or the update path would overwrite an existing account's mode")
	}
}

func TestBatchUpdateAccountsAppliesCodexFingerprintMode_CodexUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	first := insertTestAccount(t, db)
	second, err := db.InsertAccountWithCredentials(context.Background(), "second", map[string]interface{}{
		"refresh_token": "rt-second",
		"email":         "second@example.com",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	store := auth.NewStore(db, nil, nil)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/api/admin/accounts/batch",
		strings.NewReader(fmt.Sprintf(`{"ids":[%d,%d],"codex_fingerprint_mode":"session"}`, first, second)),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.BatchUpdateAccounts(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	for _, id := range []int64{first, second} {
		if got := codexRestoredFingerprintCredential(t, db, id); got != auth.CodexFingerprintModeSession {
			t.Fatalf("account %d credential = %q, want %q", id, got, auth.CodexFingerprintModeSession)
		}
	}
}

func TestSessionIdentityFingerprintPersistenceAndDefaults_CodexUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{auth.CodexFingerprintModeSingleMachineMultiWindow} {
		t.Run(mode, func(t *testing.T) {
			db := newTestAdminDB(t)
			settings := defaultBootstrapSettings()
			if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
				t.Fatal(err)
			}
			tc := cache.NewMemory(4)
			t.Cleanup(func() { _ = tc.Close() })
			store := auth.NewStore(db, tc, settings)
			t.Cleanup(store.Stop)
			h := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "test-secret")
			id := insertTestAccount(t, db)
			if err := store.LoadAccountByID(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			// Single-account API, persisted credentials and immediate runtime all agree.
			rec := codexRestoredPatchScheduler(t, h, id, fmt.Sprintf(`{"codex_fingerprint_mode":%q}`, mode))
			if rec.Code != http.StatusOK {
				t.Fatal(rec.Code, rec.Body.String())
			}
			if codexRestoredFingerprintCredential(t, db, id) != mode || store.FindByID(id).EffectiveCodexFingerprintMode() != mode {
				t.Fatal("account persistence/runtime mismatch")
			}
			rec = codexRestoredPatchScheduler(t, h, id, `{"codex_fingerprint_mode":null}`)
			if rec.Code != http.StatusOK || codexRestoredFingerprintCredential(t, db, id) != "off" {
				t.Fatal("reset failed")
			}
			// Batch API uses the same validation and credential storage.
			rec = httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/batch", strings.NewReader(fmt.Sprintf(`{"ids":[%d],"codex_fingerprint_mode":%q}`, id, mode)))
			c.Request.Header.Set("Content-Type", "application/json")
			h.BatchUpdateAccounts(c)
			if rec.Code != http.StatusOK || codexRestoredFingerprintCredential(t, db, id) != mode {
				t.Fatal("batch failed", rec.Body.String())
			}
			// The system default must survive database normalization and stamp only new accounts.
			rec = httptest.NewRecorder()
			c, _ = gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(fmt.Sprintf(`{"codex_fingerprint_default_mode":%q}`, mode)))
			c.Request.Header.Set("Content-Type", "application/json")
			h.UpdateSettings(c)
			if rec.Code != http.StatusOK {
				t.Fatal(rec.Code, rec.Body.String())
			}
			persisted, err := db.GetSystemSettings(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if persisted.CodexFingerprintDefaultMode != mode || store.GetCodexFingerprintDefaultMode() != mode {
				t.Fatal("default persistence/runtime mismatch")
			}
			seed := tokenCredentialSeed{refreshToken: "rt-test"}
			if h.newCodexAccountCredentials(seed)[auth.CodexFingerprintModeCredentialKey] != mode {
				t.Fatal("new-account default missing")
			}
			if _, exists := tokenCredentialMap(seed)[auth.CodexFingerprintModeCredentialKey]; exists {
				t.Fatal("refresh path overwrites mode")
			}
		})
	}
}

func TestWhamDailyUsageChannelSupportedAcceptsCodexAccounts_CodexUpstream(t *testing.T) {
	oauth := &auth.Account{DBID: 7, AccessToken: "at"}
	if !whamDailyUsageChannelSupported(oauth) {
		t.Fatal("the channel gate must accept a Codex OAuth account")
	}
	agent := &auth.Account{
		DBID:            8,
		CodexAuthMode:   auth.CodexAuthModeAgentIdentity,
		AgentRuntimeID:  "agent-rt-1",
		AgentPrivateKey: "pk",
	}
	if !agent.IsCodexAgentIdentity() || !whamDailyUsageChannelSupported(agent) {
		t.Fatal("the channel gate must accept an Agent-Identity Codex account")
	}
	if whamDailyUsageChannelSupported(nil) {
		t.Fatal("the channel gate must reject a nil account")
	}
}

// TestCodexAuthKindFilterMatchesOAuthAndAgentIdentity_CodexUpstream checks the paged account list: an Agent-Identity account is an OAuth-kind Codex account, so auth_kind=oauth keeps it, and an API-key filter is rejected because no such accounts exist any more.
func TestCodexAuthKindFilterMatchesOAuthAndAgentIdentity_CodexUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	codex := database.UpstreamChannelCodex
	item := &accountListSnapshotItem{Status: "active", Enabled: true}
	if !accountListItemMatches(item, accountPageQuery{AuthKind: "oauth"}, codex) {
		t.Fatal("auth_kind=oauth must match a Codex account")
	}
	if accountListItemMatches(item, accountPageQuery{AuthKind: "api_key"}, codex) {
		t.Fatal("auth_kind=api_key must match nothing")
	}
	summary, _ := summarizeAccountList([]*accountListSnapshotItem{item}, codex)
	if summary.OAuth != 1 {
		t.Fatalf("summary = %+v, want OAuth=1", summary)
	}

	// The same through the HTTP handler with a stored Agent-Identity account next to a Codex OAuth account.
	db := newTestAdminDB(t)
	ctx := context.Background()
	oauthID, err := db.InsertAccountWithCredentials(ctx, "codex-oauth", map[string]interface{}{
		"refresh_token": "secret-codex-oauth",
		"email":         "oauth@example.com",
		"plan_type":     "plus",
	}, "")
	if err != nil {
		t.Fatalf("insert oauth account: %v", err)
	}
	agentID, err := db.InsertAccountWithCredentials(ctx, "codex-agent", map[string]interface{}{
		"auth_mode":         auth.CodexAuthModeAgentIdentity,
		"agent_runtime_id":  "agent-restored",
		"agent_private_key": codexRestoredAgentPrivateKey(t),
		"account_id":        "acc-agent-restored",
		"chatgpt_user_id":   "user-agent-restored",
		"email":             "agent@example.com",
		"plan_type":         "plus",
	}, "")
	if err != nil {
		t.Fatalf("insert agent identity account: %v", err)
	}
	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	t.Cleanup(store.Stop)
	tokenCache := cache.NewMemory(1)
	t.Cleanup(func() { _ = tokenCache.Close() })
	handler := NewHandler(store, db, tokenCache, nil, "")

	recorder := invokeListAccounts(t, handler, "/api/admin/accounts?view=page&channel=codex&page=1&page_size=50&auth_kind=oauth")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var page accountsPageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	ids := map[int64]accountResponse{}
	for _, row := range page.Accounts {
		ids[row.ID] = row
	}
	if page.Total != 2 || len(ids) != 2 {
		t.Fatalf("auth_kind=oauth rows = %+v, want the OAuth and the Agent-Identity account", page.Accounts)
	}
	if _, ok := ids[oauthID]; !ok {
		t.Fatalf("OAuth account %d missing from auth_kind=oauth page", oauthID)
	}
	agentRow, ok := ids[agentID]
	if !ok {
		t.Fatalf("Agent-Identity account %d missing from auth_kind=oauth page", agentID)
	}
	if !agentRow.AgentIdentity {
		t.Fatalf("Agent-Identity row lost its agent_identity flag: %+v", agentRow)
	}

	recorder = invokeListAccounts(t, handler, "/api/admin/accounts?view=page&channel=codex&page=1&page_size=50&auth_kind=api_key")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("auth_kind=api_key status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
}

func TestCleanPlanAllowNormalizesDropsUnknownAndDedupes_CodexUpstream(t *testing.T) {
	input := []string{
		"Plus", " PRO ", "team", "free", "go", "k12", "enterprise", "business", "prolite", "promax",
		"plus", "Team", "", "  ", "unknown", "api", "supergrok", "claude", "max-5x",
	}
	want := []string{
		"plus", "pro", "team", "free", "go", "k12", "enterprise", "business", "prolite", "promax",
	}
	if got := cleanPlanAllow(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanPlanAllow() = %#v, want %#v", got, want)
	}
	if got := cleanPlanAllow(nil); got != nil {
		t.Fatalf("cleanPlanAllow(nil) = %#v, want nil", got)
	}
}
