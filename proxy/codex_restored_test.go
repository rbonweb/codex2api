package proxy

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Codex coverage that phase 1 deleted together with relay and Grok tests. The Codex upstream is an
// httptest server reached through the Resin forwarder (SetResinConfig), because CodexBaseURL is a
// const in this tree. Tests that touch Resin or the runtime settings must not call t.Parallel().

const codexRestoredResinPlatform = "codex-restored"

func codexRestoredUseResin(t *testing.T, upstreamURL string) {
	t.Helper()
	previous := GetResinConfig()
	previousEgress := auth.ResinEgressEnabled()
	SetResinConfig(&ResinConfig{BaseURL: upstreamURL, PlatformName: codexRestoredResinPlatform})
	t.Cleanup(func() {
		SetResinConfig(previous)
		auth.SetResinEgressEnabled(previousEgress)
	})
}

// useResinForTest installs a Resin config for one test and restores both the config and the auth egress flag on cleanup.
func useResinForTest(t *testing.T, baseURL, platform string) {
	t.Helper()
	previous := GetResinConfig()
	previousEgress := auth.ResinEgressEnabled()
	SetResinConfig(&ResinConfig{BaseURL: baseURL, PlatformName: platform})
	t.Cleanup(func() {
		SetResinConfig(previous)
		auth.SetResinEgressEnabled(previousEgress)
	})
}

func codexRestoredCompactRequest(t *testing.T, handler *Handler, ctx context.Context, body string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader([]byte(body))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = req
	handler.ResponsesCompact(ginContext)
	return recorder, ginContext
}

func codexRestoredCompactUpstream(t *testing.T, seenBody *[]byte, seenPath *string, model string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readUpstreamRequestBody(r)
		mu.Lock()
		*seenBody = body
		*seenPath = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_compact_codex","object":"response","model":"` + model + `","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestBodySignalCompactFilters_CodexUpstream(t *testing.T) {
	codex := &auth.Account{DBID: 2, AccessToken: "at-codex"}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	handler := NewHandler(store, nil, nil, nil)
	if handler.storeHasAvailableCodexAccount() {
		t.Fatal("empty pool should report no codex account")
	}
	store.AddAccount(codex)
	if !handler.storeHasAvailableCodexAccount() {
		t.Fatal("pool with a codex account should report available")
	}
}

func TestAccountCompactionDomain_CodexUpstream(t *testing.T) {
	native := &auth.Account{DBID: 1, AccessToken: "at"}
	agentless := &auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro"}
	for _, account := range []*auth.Account{native, agentless} {
		if got := accountCompactionDomain(account); got != nativeCodexCompactionDomain {
			t.Fatalf("accountCompactionDomain(%d) = %q, want %q", account.DBID, got, nativeCodexCompactionDomain)
		}
	}
	if got := accountCompactionDomain(nil); got != "" {
		t.Fatalf("accountCompactionDomain(nil) = %q, want empty", got)
	}

	filter := compactionDomainFilter(nativeCodexCompactionDomain, func(account *auth.Account) bool { return account.DBID != 99 })
	if !filter(native) || !filter(agentless) {
		t.Fatal("codex accounts must pass the native compaction domain filter")
	}
	if filter(&auth.Account{DBID: 99, AccessToken: "blocked"}) {
		t.Fatal("the inner filter must still apply after the domain check")
	}
	if filter(nil) {
		t.Fatal("nil account must be rejected")
	}
	if compactionDomainFilter("responses:https://other.example/v1", nil)(native) {
		t.Fatal("a codex account must not pass a foreign domain filter")
	}
}

func TestResponsesHTTPFallbackRetryKeepsCorrelationThroughSecondAccount_CodexUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	previousLogWriter := log.Writer()
	previousLogFlags := log.Flags()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
		log.SetOutput(previousLogWriter)
		log.SetFlags(previousLogFlags)
	})
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	ApplyRuntimeSettings(nextSettings)

	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       errReadCloser{err: errors.New("websocket read error: websocket: close 1009 (message too big)")},
		}, nil
	}

	var httpCalls atomic.Int32
	var tokensMu sync.Mutex
	var bearerTokens []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokensMu.Lock()
		bearerTokens = append(bearerTokens, r.Header.Get("Authorization"))
		tokensMu.Unlock()
		if httpCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"retry me"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"type":"response.output_text.delta","delta":"codex-success"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n",
		))
	}))
	defer upstream.Close()
	codexRestoredUseResin(t, upstream.URL)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 1, TestConcurrency: 1, TestModel: "gpt-5.5"})
	defer store.Stop()
	primary := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	primary.SetDispatchCountLimit(1)
	store.AddAccount(primary)
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	body := []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "codex-success") {
		t.Fatalf("fallback retry response = status %d body %s", recorder.Code, recorder.Body.String())
	}
	if got := httpCalls.Load(); got != 2 {
		t.Fatalf("HTTP calls = %d, want failed fallback plus successful retry on the second Codex account", got)
	}
	tokensMu.Lock()
	gotTokens := append([]string(nil), bearerTokens...)
	tokensMu.Unlock()
	// The scheduler picks the order; what matters is that the retry moved to the other Codex account.
	if len(gotTokens) != 2 || gotTokens[0] == gotTokens[1] || !slices.Contains(gotTokens, "Bearer at-1") || !slices.Contains(gotTokens, "Bearer at-2") {
		t.Fatalf("upstream bearer tokens = %v, want one call per Codex account (at-1 and at-2)", gotTokens)
	}
	completionLines := make([]string, 0, 2)
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "HTTP 降级尝试结束") {
			completionLines = append(completionLines, line)
		}
	}
	if len(completionLines) != 2 {
		t.Fatalf("fallback completion log lines = %d, want failed and final attempts; logs=%s", len(completionLines), logs.String())
	}
	if !strings.Contains(completionLines[0], "status=500") || !strings.Contains(completionLines[1], "status=200") {
		t.Fatalf("fallback completion statuses are not failure then success: %v", completionLines)
	}
	firstID := strings.SplitN(strings.SplitN(completionLines[0], "fallback_id=", 2)[1], ",", 2)[0]
	secondID := strings.SplitN(strings.SplitN(completionLines[1], "fallback_id=", 2)[1], ",", 2)[0]
	if firstID == "" || firstID != secondID {
		t.Fatalf("fallback ids differ across HTTP retry: %q vs %q; logs=%v", firstID, secondID, completionLines)
	}
}

// The relay version of this test also covered per-account ModelMapping applied before the suffix
// fallback. Codex accounts do not apply account-level mapping (only the store-level Codex mapping),
// so that part is not restored; this checks the Codex path strips the suffix and reaches the compact endpoint.
func TestResponsesCompactAppliesSuffixFallback_CodexUpstream(t *testing.T) {
	var seenBody []byte
	var seenPath string
	upstream := codexRestoredCompactUpstream(t, &seenBody, &seenPath, "gpt-5.5")
	codexRestoredUseResin(t, upstream.URL)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", Status: auth.StatusReady})
	handler := NewHandler(store, nil, nil, nil)

	requestCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	recorder, _ := codexRestoredCompactRequest(t, handler, requestCtx, `{"model":"gpt-5.5-openai-compact","input":"hello","stream":true}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.HasSuffix(seenPath, "/backend-api/codex/responses/compact") {
		t.Fatalf("upstream path = %q, want compact endpoint", seenPath)
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-5.5" {
		t.Fatalf("upstream model = %q, want gpt-5.5; body=%s", model, seenBody)
	}
}

// Regression (PR #350): a stale suffixed-to-suffixed identity rule must not leak the synthetic
// alias upstream. Ported to the store-level Codex mapping, which is the mapping Codex accounts use.
func TestResponsesCompactStaleSuffixIdentityRuleKeepsBaseModelUpstream_CodexUpstream(t *testing.T) {
	var seenBody []byte
	var seenPath string
	upstream := codexRestoredCompactUpstream(t, &seenBody, &seenPath, "gpt-5.5")
	codexRestoredUseResin(t, upstream.URL)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", Status: auth.StatusReady})
	store.SetCodexModelMapping(`{"gpt-5.5-openai-compact":"gpt-5.5-openai-compact"}`)
	handler := NewHandler(store, nil, nil, nil)

	requestCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	recorder, _ := codexRestoredCompactRequest(t, handler, requestCtx, `{"model":"gpt-5.5","input":"hello","stream":true}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-5.5" {
		t.Fatalf("upstream model = %q, want base gpt-5.5; body=%s", model, seenBody)
	}
}

// The relay version asserted c.Get("x-model"); on the Codex path the display model reaches the usage log instead, so this reads the persisted row.
func TestResponsesCompactSuffixOnlyRequestLogsBaseModel_CodexUpstream(t *testing.T) {
	var seenBody []byte
	var seenPath string
	upstream := codexRestoredCompactUpstream(t, &seenBody, &seenPath, "gpt-5.5")
	codexRestoredUseResin(t, upstream.URL)

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", Status: auth.StatusReady})
	handler := NewHandler(store, db, nil, nil)

	requestCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	recorder, _ := codexRestoredCompactRequest(t, handler, requestCtx, `{"model":"gpt-5.5-openai-compact","input":"hello","stream":true}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	db.FlushUsageLogs()
	page, err := db.ListUsageLogsByTimeRangePaged(context.Background(), database.UsageLogFilter{
		Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), Page: 1, PageSize: 10,
		Endpoint: "/v1/responses/compact", IncludeCanceled: true,
	})
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged: %v", err)
	}
	if len(page.Logs) != 1 {
		t.Fatalf("usage logs = %d, want 1", len(page.Logs))
	}
	if got := page.Logs[0]; got.Model != "gpt-5.5" || strings.Contains(got.EffectiveModel, "->") {
		t.Fatalf("usage log model=%q effective=%q, want base gpt-5.5 without a mapping arrow", got.Model, got.EffectiveModel)
	}
}

func TestResponsesCompactReadCancellationDoesNotPenalizeAccount_CodexUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
	})
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
			Enabled:    true,
			Categories: []string{database.ContinuousRetryCategoryTransport},
		}
		return current
	})

	responseStarted := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "128")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"partial"`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		responseStarted <- struct{}{}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	codexRestoredUseResin(t, upstream.URL)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0, MaxRateLimitRetries: 0})
	defer store.Stop()
	account := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", Models: []string{"gpt-5.5"}, Status: auth.StatusReady}
	store.AddAccount(account)
	handler := NewHandler(store, nil, nil, nil)

	requestCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewBufferString(`{"model":"gpt-5.5","input":"hello"}`)).WithContext(requestCtx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = req

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ResponsesCompact(ginContext)
	}()
	select {
	case <-responseStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("timed out waiting for compact response body")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("compact handler did not stop after downstream cancellation")
	}

	if account.FailureStreak != 0 || account.LastFailureKind != "" {
		t.Fatalf("downstream cancellation penalized account: streak=%d kind=%q", account.FailureStreak, account.LastFailureKind)
	}
	if got := account.ActiveRequests.Load(); got != 0 {
		t.Fatalf("ActiveRequests after cancellation = %d, want 0", got)
	}
}

func TestScopedModelsDeterministicOrderAndAliasOwner_CodexUpstream(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "codex-2", PlanType: "plus"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "codex-1", PlanType: "plus"})
	store.SetCodexModelMapping(`{"global-alias":"gpt-5.5","dead-global":"missing-model"}`)
	handler := NewHandler(store, nil, nil, nil)
	models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 9})

	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, strings.ToLower(model.ID))
	}
	if len(ids) == 0 || !slices.IsSorted(ids) {
		t.Fatalf("models are not deterministic sorted: %v", ids)
	}
	if owner, _, ok := scopedModelByID(models, "gpt-5.5"); !ok || owner != "openai" {
		t.Fatalf("catalog model owner = %q ok=%v, want openai", owner, ok)
	}
	if owner, _, ok := scopedModelByID(models, "global-alias"); !ok || owner != "codex2api" {
		t.Fatalf("alias owner = %q ok=%v, want codex2api", owner, ok)
	}
	if _, _, ok := scopedModelByID(models, "dead-global"); ok {
		t.Fatal("alias with an unrouteable target was exposed")
	}
}

func TestScopedModelsModelAllowAppliesToAliasName_CodexUpstream(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "codex-1", PlanType: "plus"})
	store.SetCodexModelMapping(`{"public-alias":"gpt-5.5"}`)
	handler := NewHandler(store, nil, nil, nil)
	models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 1, Limits: database.APIKeyLimits{ModelAllow: []string{"public-alias"}}})
	if len(models) != 1 || models[0].ID != "public-alias" || models[0].Owner != "codex2api" {
		t.Fatalf("models = %+v, want alias only", models)
	}
}

func TestScopedModelsAppliesKeyAndGroupGates_CodexUpstream(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	allowed := &auth.Account{DBID: 1, AccessToken: "codex-1", PlanType: "plus", Models: []string{"gpt-5.5"}, GroupIDs: []int64{10}}
	allowed.SetAllowedAPIKeyIDs([]int64{77})
	keyBlocked := &auth.Account{DBID: 2, AccessToken: "codex-2", PlanType: "plus", Models: []string{"gpt-5.6-luna"}, GroupIDs: []int64{10}}
	keyBlocked.SetAllowedAPIKeyIDs([]int64{78})
	otherGroup := &auth.Account{DBID: 3, AccessToken: "codex-3", PlanType: "plus", Models: []string{"gpt-5.4"}, GroupIDs: []int64{20}}
	store.AddAccount(allowed)
	store.AddAccount(keyBlocked)
	store.AddAccount(otherGroup)
	handler := NewHandler(store, nil, nil, nil)
	row := &database.APIKeyRow{ID: 77, AllowedGroupIDs: []int64{10}}

	models := listScopedModelsForTest(t, handler, row)
	if len(models) != 1 || models[0].ID != "gpt-5.5" {
		t.Fatalf("models = %+v, want only gpt-5.5 from the key- and group-eligible account", models)
	}
	if allowed.ActiveRequests.Load() != 0 || allowed.TotalRequests.Load() != 0 {
		t.Fatalf("model listing reserved account: active=%d total=%d", allowed.ActiveRequests.Load(), allowed.TotalRequests.Load())
	}
}

func TestScopedModelsHidePausedAccount_CodexUpstream(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	defer store.Stop()
	paused := &auth.Account{DBID: 1, AccessToken: "codex-1", PlanType: "plus", Models: []string{"gpt-5.5"}}
	atomic.StoreInt32(&paused.DispatchPaused, 1)
	store.AddAccount(paused)
	handler := NewHandler(store, nil, nil, nil)
	if models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 1}); len(models) != 0 {
		t.Fatalf("paused account models leaked: %+v", models)
	}

	atomic.StoreInt32(&paused.DispatchPaused, 0)
	cooled := &auth.Account{DBID: 2, AccessToken: "codex-2", PlanType: "plus", Models: []string{"gpt-5.6-luna"}}
	cooled.SetCooldownWithReason(time.Minute, "rate_limited")
	store.AddAccount(cooled)
	models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 1})
	if _, _, ok := scopedModelByID(models, "gpt-5.6-luna"); !ok {
		t.Fatalf("a transient cooldown must keep the model listed: %+v", models)
	}
	if _, _, ok := scopedModelByID(models, "gpt-5.5"); !ok {
		t.Fatalf("resumed account model missing: %+v", models)
	}
}
