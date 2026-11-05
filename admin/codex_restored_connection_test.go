package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// codexRestoredUpstream starts an httptest server that stands in for the Codex upstream.
// proxy.CodexBaseURL is a constant on this branch, so the server is reached through the Resin reverse-proxy seam that the other admin tests use: with Resin enabled every Codex request (responses and WHAM) goes to <server>/<platform>/https/chatgpt.com/backend-api/codex/....
// The WHAM usage endpoint answers 404 so the batch pre-flight never marks the account.
// Tests that use it must not call t.Parallel() because Resin is a process-wide setting.
func codexRestoredUpstream(t *testing.T, upstream http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/wham/usage") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		upstream(w, r)
	}))
	t.Cleanup(server.Close)
	previousResin := proxy.GetResinConfig()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "codex-restored-test"})
	t.Cleanup(func() { proxy.SetResinConfig(previousResin) })
	return server
}

// codexRestoredStatusUpstream answers every Codex request with a fixed status and JSON body.
func codexRestoredStatusUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return codexRestoredUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

// codexRestoredAccount builds a ready ChatGPT Codex OAuth account with an access token.
func codexRestoredAccount(id int64) *auth.Account {
	return &auth.Account{
		DBID:        id,
		AccessToken: "at-restored-secret",
		AccountID:   fmt.Sprintf("acct-restored-%d", id),
		PlanType:    "plus",
		Status:      auth.StatusReady,
		HealthTier:  auth.HealthTierHealthy,
	}
}

func codexRestoredHandler(account *auth.Account) *Handler {
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(account)
	return &Handler{store: store}
}

func codexRestoredServeConnectionTest(handler *Handler, id int64) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/admin/accounts/:id/test", handler.TestConnection)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/accounts/%d/test", id), nil))
	return recorder
}

func codexRestoredErrorMsg(account *auth.Account) string {
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	return account.ErrorMsg
}

func codexRestoredDecodeEvents(t *testing.T, body string) []testEvent {
	t.Helper()
	var events []testEvent
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event testEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

const codexRestoredCompletedSSE = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"

const codexRestoredResponseFailedSSE = "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"model unavailable\",\"code\":\"model_not_available\"}}}\n\n"

func codexRestoredSSEUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return codexRestoredUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

func TestConnectionUnauthorizedRecordsErrorMessage_CodexUpstream(t *testing.T) {
	codexRestoredStatusUpstream(t, http.StatusUnauthorized,
		`{"error":{"message":"Your authentication token has been invalidated.","code":"token_invalidated"},"status":401}`)
	account := codexRestoredAccount(42)
	handler := codexRestoredHandler(account)

	beforeGeneration := handler.accountCachesGen.Load()
	recorder := codexRestoredServeConnectionTest(handler, 42)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "token_invalidated") {
		t.Fatalf("SSE response %q does not contain token_invalidated", recorder.Body.String())
	}
	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
	if errorMsg := codexRestoredErrorMsg(account); !strings.Contains(errorMsg, "token_invalidated") {
		t.Fatalf("ErrorMsg = %q, want token_invalidated", errorMsg)
	}
	if got := handler.accountCachesGen.Load(); got <= beforeGeneration {
		t.Fatalf("account cache generation = %d, want > %d after stateful connection test", got, beforeGeneration)
	}
}

func TestConnectionPaymentRequiredMarksError_CodexUpstream(t *testing.T) {
	codexRestoredStatusUpstream(t, http.StatusPaymentRequired, `{"detail":{"code":"deactivated_workspace"}}`)
	account := codexRestoredAccount(42)
	handler := codexRestoredHandler(account)

	recorder := codexRestoredServeConnectionTest(handler, 42)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := account.RuntimeStatus(); got != "error" {
		t.Fatalf("RuntimeStatus() = %q, want error", got)
	}
	if errorMsg := codexRestoredErrorMsg(account); !strings.Contains(errorMsg, "402") {
		t.Fatalf("ErrorMsg = %q, want 402", errorMsg)
	}
}

func TestConnectionBarePaymentRequiredMarksError_CodexUpstream(t *testing.T) {
	codexRestoredStatusUpstream(t, http.StatusPaymentRequired, `{"detail":"Payment Required"}`)
	account := codexRestoredAccount(43)
	handler := codexRestoredHandler(account)

	recorder := codexRestoredServeConnectionTest(handler, 43)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := account.RuntimeStatus(); got != "error" {
		t.Fatalf("RuntimeStatus() = %q, want error", got)
	}
}

// TestConnectionDeletedAgentRuntimeMarksBanned_CodexUpstream checks that a connection test marks an account whose agent runtime was deleted as banned for 24 hours.
func TestConnectionDeletedAgentRuntimeMarksBanned_CodexUpstream(t *testing.T) {
	codexRestoredStatusUpstream(t, http.StatusForbidden,
		`{"error":{"message":"Agent runtime has been deleted.","type":null,"code":"biscuit_baker_service_agent_error_status","param":null},"status":403}`)
	account := codexRestoredAccount(42)
	handler := codexRestoredHandler(account)

	recorder := codexRestoredServeConnectionTest(handler, 42)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
	cooldownReason, cooldownUntil := account.GetCooldownSnapshot()
	if cooldownReason != "unauthorized" {
		t.Fatalf("cooldown reason = %q, want unauthorized", cooldownReason)
	}
	if remaining := time.Until(cooldownUntil); remaining < 23*time.Hour+59*time.Minute || remaining > 24*time.Hour {
		t.Fatalf("cooldown remaining = %s, want approximately 24h", remaining)
	}
	if errorMsg := codexRestoredErrorMsg(account); !strings.Contains(errorMsg, "Agent runtime has been deleted") {
		t.Fatalf("ErrorMsg = %q, want deleted runtime message", errorMsg)
	}
}

func TestConnectionCodexEmitsDiagnosticsFramesAroundSuccess_CodexUpstream(t *testing.T) {
	account := codexRestoredAccount(42)
	handler := codexRestoredHandler(account)
	codexRestoredUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-request-id", "req_ok")
		w.Header().Set("x-codex-plan-type", "plus")
		w.Header().Set("x-codex-primary-used-percent", "5")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		w.Header().Set("x-codex-primary-reset-after-seconds", "100")
		w.WriteHeader(http.StatusOK)
		// Echo the bearer secret back so the preview must redact it.
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_ok\",\"model\":\"gpt-5.5\",\"auth\":\"" + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + "\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":1,\"input_tokens_details\":{\"cached_tokens\":0}}}}\n\n"))
	})

	recorder := codexRestoredServeConnectionTest(handler, 42)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	events := codexRestoredDecodeEvents(t, recorder.Body.String())
	if len(events) < 5 || events[0].Type != "test_start" || events[1].Type != "diagnostics" {
		t.Fatalf("expected test_start then initial diagnostics: %+v", events)
	}
	initial := events[1].CodexDiagnostics
	if initial == nil || initial.HTTPStatus != 200 || initial.DurationMS != nil || initial.RequestID != "req_ok" || initial.PlanType != "plus" || initial.PrimaryWindow == nil {
		t.Fatalf("initial diagnostics must carry headers without duration: %+v", initial)
	}
	terminal, final := events[len(events)-2], events[len(events)-1]
	if terminal.Type != "test_complete" || !terminal.Success || final.Type != "diagnostics" {
		t.Fatalf("terminal result must be followed by final diagnostics: %+v", events)
	}
	d := final.CodexDiagnostics
	if d == nil || d.DurationMS == nil || d.FirstContentMS == nil || d.ResponseID != "resp_ok" || d.ResponseModel != "gpt-5.5" || d.ResponseStatus != "completed" {
		t.Fatalf("final diagnostics incomplete: %+v", d)
	}
	if d.Usage == nil || d.Usage.InputTokens == nil || *d.Usage.InputTokens != 7 || d.Usage.CachedTokens == nil || *d.Usage.CachedTokens != 0 {
		t.Fatalf("usage not carried: %+v", d.Usage)
	}
	if strings.Contains(d.ResponseBody, "at-restored-secret") || !strings.Contains(d.ResponseBody, "[REDACTED]") {
		t.Fatalf("access token leaked into body preview: %s", d.ResponseBody)
	}
	if !strings.Contains(d.ResponseBody, "response.output_text.delta") {
		t.Fatalf("body preview must include the streamed frames: %s", d.ResponseBody)
	}
	for _, event := range events {
		if event.Type == "content" && strings.Contains(event.Text, "took") {
			return
		}
	}
	t.Fatalf("existing duration trailer must be preserved: %+v", events)
}

func TestConnectionCodexNon200CarriesErrorDiagnostics_CodexUpstream(t *testing.T) {
	account := codexRestoredAccount(42)
	handler := codexRestoredHandler(account)
	codexRestoredUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("cf-ray", "ray-401")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Your authentication token has been invalidated.","code":"token_invalidated","type":"invalid_request_error"}}`))
	})

	events := codexRestoredDecodeEvents(t, codexRestoredServeConnectionTest(handler, 42).Body.String())
	if len(events) < 4 || events[len(events)-2].Type != "error" || events[len(events)-1].Type != "diagnostics" {
		t.Fatalf("error must be followed by final diagnostics: %+v", events)
	}
	d := events[len(events)-1].CodexDiagnostics
	if d == nil || d.HTTPStatus != 401 || d.CFRay != "ray-401" || d.ErrorCode != "token_invalidated" || d.ErrorType != "invalid_request_error" || d.DurationMS == nil {
		t.Fatalf("final diagnostics incomplete: %+v", d)
	}
	if !strings.Contains(d.ResponseBody, "token_invalidated") {
		t.Fatalf("body preview missing: %s", d.ResponseBody)
	}
	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized (state handling must be unchanged)", got)
	}
}

func TestConnectionCodexStreamFailureRecordsErrorType_CodexUpstream(t *testing.T) {
	account := codexRestoredAccount(42)
	handler := codexRestoredHandler(account)
	codexRestoredSSEUpstream(t,
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fail\"}}\n\n"+
			"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_fail\",\"status\":\"failed\",\"error\":{\"type\":\"server_error\",\"code\":\"upstream_unavailable\",\"message\":\"boom\"}}}\n\n")

	events := codexRestoredDecodeEvents(t, codexRestoredServeConnectionTest(handler, 42).Body.String())
	if len(events) < 4 || events[len(events)-2].Type != "error" || events[len(events)-1].Type != "diagnostics" {
		t.Fatalf("error must be followed by final diagnostics: %+v", events)
	}
	d := events[len(events)-1].CodexDiagnostics
	if d == nil || d.ResponseID != "resp_fail" || d.ResponseStatus != "failed" || d.ErrorType != "server_error" || d.ErrorCode != "upstream_unavailable" || d.FirstContentMS != nil {
		t.Fatalf("final diagnostics incomplete: %+v", d)
	}
}

func TestConnectionCodexRequestFailureCarriesDiagnostics_CodexUpstream(t *testing.T) {
	account := codexRestoredAccount(42)
	handler := codexRestoredHandler(account)
	server := codexRestoredUpstream(t, func(http.ResponseWriter, *http.Request) {})
	server.Close()

	events := codexRestoredDecodeEvents(t, codexRestoredServeConnectionTest(handler, 42).Body.String())
	if len(events) != 2 || events[0].Type != "test_start" || events[1].Type != "error" {
		t.Fatalf("request failure must yield test_start then error: %+v", events)
	}
	d := events[1].CodexDiagnostics
	if d == nil || d.DurationMS == nil || d.HTTPStatus != 0 || d.Model == "" {
		t.Fatalf("request failure must still attach duration diagnostics: %+v", d)
	}
	if !strings.HasPrefix(events[1].Error, "request failed") {
		t.Fatalf("error text = %q", events[1].Error)
	}
}

func TestRunSingleBatchTestTimesOutSlowStreamingBody_CodexUpstream(t *testing.T) {
	previousTimeout := batchTestAccountTimeout
	batchTestAccountTimeout = 20 * time.Millisecond
	t.Cleanup(func() { batchTestAccountTimeout = previousTimeout })

	codexRestoredUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	account := codexRestoredAccount(1)
	handler := codexRestoredHandler(account)

	start := time.Now()
	status, msg := handler.runSingleBatchTest(context.Background(), account)
	elapsed := time.Since(start)

	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if !strings.Contains(msg, "test timed out") {
		t.Fatalf("message = %q, want timeout message", msg)
	}
	if elapsed > time.Second {
		t.Fatalf("batch test took %s, want bounded timeout", elapsed)
	}
	if account.Status == auth.StatusError {
		t.Fatal("timeout should not mark account as permanent error")
	}
	if account.LastTimeoutAt.IsZero() {
		t.Fatal("timeout should be recorded in scheduler health")
	}
}

func TestRunSingleBatchTestSuccessRecoversBannedAccount_CodexUpstream(t *testing.T) {
	codexRestoredSSEUpstream(t, codexRestoredCompletedSSE)
	account := codexRestoredAccount(1)
	account.Status = auth.StatusCooldown
	account.CooldownUtil = time.Now().Add(time.Hour)
	account.CooldownReason = "unauthorized"
	account.HealthTier = auth.HealthTierBanned
	account.FailureStreak = 3
	account.LastFailureAt = time.Now().Add(-time.Minute)
	account.LastUnauthorizedAt = time.Now().Add(-time.Minute)
	atomic.StoreInt32(&account.Disabled, 1)
	handler := codexRestoredHandler(account)

	status, msg := handler.runSingleBatchTest(context.Background(), account)
	if status != "success" {
		t.Fatalf("status = %q, message = %q, want success", status, msg)
	}

	account.Mu().RLock()
	accountStatus := account.Status
	healthTier := account.HealthTier
	cooldownUntil := account.CooldownUtil
	cooldownReason := account.CooldownReason
	failureStreak := account.FailureStreak
	successStreak := account.SuccessStreak
	lastSuccessAt := account.LastSuccessAt
	account.Mu().RUnlock()

	if atomic.LoadInt32(&account.Disabled) != 0 {
		t.Fatal("successful batch test should clear disabled flag")
	}
	if accountStatus != auth.StatusReady {
		t.Fatalf("Status = %v, want ready", accountStatus)
	}
	if healthTier == auth.HealthTierBanned {
		t.Fatal("successful batch test should recover banned health tier")
	}
	if !cooldownUntil.IsZero() || cooldownReason != "" {
		t.Fatalf("cooldown = (%s, %q), want cleared", cooldownUntil, cooldownReason)
	}
	if failureStreak != 0 {
		t.Fatalf("FailureStreak = %d, want 0", failureStreak)
	}
	if successStreak == 0 || lastSuccessAt.IsZero() {
		t.Fatal("successful batch test should record scheduler success")
	}
}

func TestRunSingleBatchTestResponseFailedDoesNotRecoverCooldown_CodexUpstream(t *testing.T) {
	codexRestoredSSEUpstream(t, codexRestoredResponseFailedSSE)
	account := codexRestoredAccount(1)
	account.Status = auth.StatusCooldown
	account.CooldownUtil = time.Now().Add(time.Hour)
	account.CooldownReason = "rate_limited"
	account.HealthTier = auth.HealthTierRisky
	handler := codexRestoredHandler(account)

	status, msg := handler.runSingleBatchTest(context.Background(), account)
	if status == "success" {
		t.Fatalf("status = success, want failure for response.failed; message=%q", msg)
	}
	if !strings.Contains(msg, "model unavailable") {
		t.Fatalf("message = %q, want upstream failure detail", msg)
	}
	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
}

func TestRunSingleBatchTestResponseFailedMarksReadyAccountError_CodexUpstream(t *testing.T) {
	codexRestoredSSEUpstream(t, codexRestoredResponseFailedSSE)
	account := codexRestoredAccount(1)
	handler := codexRestoredHandler(account)

	status, msg := handler.runSingleBatchTest(context.Background(), account)
	if status == "success" {
		t.Fatalf("status = success, want failure for response.failed; message=%q", msg)
	}
	if got := account.RuntimeStatus(); got != "error" {
		t.Fatalf("RuntimeStatus() = %q, want error", got)
	}
	if errorMsg := codexRestoredErrorMsg(account); !strings.Contains(errorMsg, "model unavailable") {
		t.Fatalf("ErrorMsg = %q, want model unavailable", errorMsg)
	}
}

func TestRunSingleBatchTestUnauthorizedRecordsErrorMessage_CodexUpstream(t *testing.T) {
	codexRestoredStatusUpstream(t, http.StatusUnauthorized,
		`{"error":{"code":"token_invalidated","message":"token invalidated"},"status":401}`)
	account := codexRestoredAccount(1)
	handler := codexRestoredHandler(account)

	status, msg := handler.runSingleBatchTest(context.Background(), account)
	if status != "banned" {
		t.Fatalf("status = %q, message = %q, want banned", status, msg)
	}
	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
	if errorMsg := codexRestoredErrorMsg(account); !strings.Contains(errorMsg, "token_invalidated") {
		t.Fatalf("ErrorMsg = %q, want token_invalidated", errorMsg)
	}
}

func TestRunSingleBatchTestPaymentRequiredMarksError_CodexUpstream(t *testing.T) {
	codexRestoredStatusUpstream(t, http.StatusPaymentRequired, `{"detail":"Payment Required"}`)
	account := codexRestoredAccount(1)
	handler := codexRestoredHandler(account)

	status, msg := handler.runSingleBatchTest(context.Background(), account)
	if status != "failed" {
		t.Fatalf("status = %q, message = %q, want failed", status, msg)
	}
	if got := account.RuntimeStatus(); got != "error" {
		t.Fatalf("RuntimeStatus() = %q, want error", got)
	}
	if errorMsg := codexRestoredErrorMsg(account); !strings.Contains(errorMsg, "402") {
		t.Fatalf("ErrorMsg = %q, want 402", errorMsg)
	}
}

// TestRunSingleBatchTestDeletedAgentRuntimeMarksBanned_CodexUpstream checks that a batch test marks an account whose agent runtime was deleted as banned for 24 hours.
func TestRunSingleBatchTestDeletedAgentRuntimeMarksBanned_CodexUpstream(t *testing.T) {
	codexRestoredStatusUpstream(t, http.StatusForbidden,
		`{"error":{"message":"Agent runtime has been deleted.","type":null,"code":"biscuit_baker_service_agent_error_status","param":null},"status":403}`)
	account := codexRestoredAccount(1)
	handler := codexRestoredHandler(account)

	status, msg := handler.runSingleBatchTest(context.Background(), account)
	if status != "banned" {
		t.Fatalf("status = %q, message = %q, want banned", status, msg)
	}
	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
	_, cooldownUntil := account.GetCooldownSnapshot()
	if remaining := time.Until(cooldownUntil); remaining < 23*time.Hour+59*time.Minute || remaining > 24*time.Hour {
		t.Fatalf("cooldown remaining = %s, want approximately 24h", remaining)
	}
	if errorMsg := codexRestoredErrorMsg(account); !strings.Contains(errorMsg, "Agent runtime has been deleted") {
		t.Fatalf("ErrorMsg = %q, want deleted runtime message", errorMsg)
	}
}

// TestScheduledConnectionTestsOverlapAndShutdown_CodexUpstream keeps the original overlap and shutdown coverage with Codex OAuth accounts.
// The original polled the database until the second minute was claimed while the first batch was still blocked; this version has no timer or polling loop and proves the overlap through the final state instead.
// After the release every account must have exactly one successful attempt per minute and the global concurrency must stay at four.
func TestScheduledConnectionTestsOverlapAndShutdown_CodexUpstream(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%v", shutdown), func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx, cancel := context.WithCancel(context.Background())
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			started := make(chan struct{}, 16)
			var active, peak, calls atomic.Int32
			codexRestoredUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				started <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_scheduled\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			})
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "scheduled.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
			t.Cleanup(store.Stop)
			h := &Handler{store: store, db: db}
			t.Cleanup(func() {
				cancel()
				unblock()
				h.WaitScheduledConnectionTests()
			})
			loc, err := time.LoadLocation(scheduledTestTimezone)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 26, 12, 0, 0, 0, loc)
			plan, _ := json.Marshal(accountScheduledTest{Enabled: true, Model: "gpt-5.5", Content: "hi", WeekdayTimes: []string{"12:00", "12:01"}})
			var ids []int64
			for i := 0; i < 5; i++ {
				id, err := db.InsertAccountWithCredentials(ctx, fmt.Sprintf("scheduled-%d", i), map[string]interface{}{
					"access_token": "test-token", "plan_type": "plus", scheduledTestCredentialKey: string(plan),
				}, "")
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
				account := codexRestoredAccount(id)
				account.AccessToken = "test-token"
				store.AddAccount(account)
			}
			h.dispatchScheduledConnectionTests(ctx, now)
			for i := 0; i < 4; i++ {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("first batch did not start four attempts")
				}
			}
			// Dispatching the next minute while the first batch is blocked must return at once and must not deadlock; the claim itself is only proven by the end state below because production code offers no claim hook and polling is not allowed.
			h.dispatchScheduledConnectionTests(ctx, now.Add(time.Minute))
			if shutdown {
				cancel()
			} else {
				unblock()
				// Re-dispatching a slot cannot send another upstream request.
				h.dispatchScheduledConnectionTests(ctx, now)
			}
			done := make(chan struct{})
			go func() { h.WaitScheduledConnectionTests(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("workers did not finish after release/cancellation")
			}
			if peak.Load() > 4 {
				t.Fatalf("global concurrency=%d, want at most 4", peak.Load())
			}
			if !shutdown {
				if calls.Load() != 10 {
					t.Fatalf("end state after release: upstream attempts=%d, want 10 (one per account per minute, so both overlapping minutes ran)", calls.Load())
				}
				for _, id := range ids {
					jobs, err := db.ListOneShotMaintenanceJobs(ctx, id, scheduledTestJobPrefix, 10)
					if err != nil || len(jobs) != 2 {
						t.Fatalf("end state after release: account %d jobs=%v err=%v, want two claimed minute slots", id, jobs, err)
					}
					for _, job := range jobs {
						var result scheduledTestRecord
						if json.Unmarshal([]byte(job.LastError), &result) != nil || !result.Success {
							t.Fatalf("unsuccessful slot for %d: %s", id, job.LastError)
						}
					}
				}
			}
		})
	}
}
