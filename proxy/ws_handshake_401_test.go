package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// After a WS upstream handshake 401 is converted by wsrelay back into a real status-code response, the Responses main path must
// take the existing non-2xx branch: the usage log records 401, the account is put in cooldown as unauthorized, and downstream is
// rewritten to 503 account_pool_unauthorized per the issue #323 convention (instead of hiding the 401 under
// transport/598).
func TestResponsesWebsocketHandshake401CoolsAccountAndLogs401(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	ApplyRuntimeSettings(nextSettings)

	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })
	upstreamBody := `{"error":{"message":"Provided authentication token is expired. Please try signing in again.","type":"invalid_request_error","code":"token_expired"}}`
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		// Simulates the result of wsrelay.ExecuteRequestWebsocket converting a handshake 401.
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Unauthorized",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		}, nil
	}

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	db.SetUsageLogConfig("all", 1, 1)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	account := &auth.Account{DBID: 1, AccessToken: "expired-token", PlanType: "plus"}
	store.AddAccount(account)
	handler := NewHandler(store, db, nil, nil)

	body := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	// Downstream receives a 503 pool-level error, not a bare 401 (issue #323 semantics: an upstream account 401 is
	// an account-side problem and must not make downstream think its own key is invalid). The exact code depends on the wrap-up path
	// (retries exhausted vs no account available); both are acceptable.
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("downstream status = %d, want 503; body=%s", recorder.Code, recorder.Body.String())
	}
	if code := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); code != "account_pool_unauthorized" && code != "no_available_account" {
		t.Fatalf("downstream error.code = %q, want pool-level error; body=%s", code, recorder.Body.String())
	}

	// The account must be handled as unauthorized (disabled or cooling down) and must not stay in the scheduling pool where it keeps being dialed.
	if account.IsAvailable() {
		t.Fatal("account should be disabled/cooled down after handshake 401")
	}

	// The usage log must contain a real 401 row (the original defect: it was only recorded as 598/transport or silently dropped).
	deadline := time.Now().Add(5 * time.Second)
	for {
		logs, err := db.ListUsageLogsByTimeRange(context.Background(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("ListUsageLogsByTimeRange: %v", err)
		}
		var found bool
		for _, l := range logs {
			if l.StatusCode == http.StatusUnauthorized && l.Endpoint == "/v1/responses" {
				found = true
				if !strings.Contains(l.ErrorMessage, "token is expired") {
					t.Fatalf("401 row error message = %q, want upstream token_expired message", l.ErrorMessage)
				}
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no 401 usage log row recorded; rows=%d", len(logs))
		}
		time.Sleep(200 * time.Millisecond)
	}
}
