package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
)

func gatewayLiveHandler(t *testing.T, upstream http.HandlerFunc, accounts int) (*Handler, []*auth.Account) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	previousURL := liveCallsURLForTest
	t.Cleanup(func() { liveCallsURLForTest = previousURL })
	liveCallsURLForTest = server.URL

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	list := make([]*auth.Account, 0, accounts)
	for i := 0; i < accounts; i++ {
		account := &auth.Account{DBID: int64(i + 1), AccessToken: "at", PlanType: "plus", AccountID: "acct-live"}
		store.AddAccount(account)
		list = append(list, account)
	}
	handler := NewHandler(store, nil, &config.Config{AdminSecret: "secret"}, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	return handler, list
}

func gatewayLiveRequest() *liveCallRequest {
	return &liveCallRequest{SDP: "v=0\r\n", Session: []byte(`{"model":"gpt-live-1-codex"}`)}
}

func TestCreateGatewayLiveCallSuccessIsProxyControlled(t *testing.T) {
	var logs atomic.Int32
	previous := liveUsageLogForTest
	t.Cleanup(func() { liveUsageLogForTest = previous })
	liveUsageLogForTest = func(*database.UsageLogInput) { logs.Add(1) }

	handler, accounts := gatewayLiveHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/backend-api/codex/rtc_gw_1")
		w.Header().Set("Content-Type", "application/sdp")
		_, _ = w.Write([]byte("v=0\r\nanswer\r\n"))
	}, 1)
	c, rec := liveTestContext(t, http.MethodPost, "/v1/realtime", nil, &database.APIKeyRow{
		ID: 21, Limits: database.APIKeyLimits{AllowLive: true, MaxConcurrency: 1},
	})
	call, gwErr := handler.createGatewayLiveCall(c, gatewayLiveRequest())
	if gwErr != nil {
		t.Fatalf("gateway error: %+v", gwErr)
	}
	if c.Writer.Written() || rec.Body.Len() != 0 {
		t.Fatalf("createGatewayLiveCall wrote to the client: %q", rec.Body.String())
	}
	if string(call.AnswerSDP) != "v=0\r\nanswer\r\n" || call.Record.CallID != "rtc_gw_1" {
		t.Fatalf("call=%+v answer=%q", call.Record, call.AnswerSDP)
	}
	stored, _, err := handler.liveCalls().get(context.Background(), "rtc_gw_1")
	if err != nil || stored.Controller != liveControllerProxy || stored.ControllerOwner == "" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	// An external sideband claim is refused while the gateway holds the call.
	if _, claimed, err := handler.liveCalls().claimProxy(context.Background(), call.Record); err != nil || claimed {
		t.Fatalf("external claim: claimed=%v err=%v", claimed, err)
	}
	if accounts[0].ActiveRequests.Load() != 1 {
		t.Fatalf("account reservation = %d, want 1 until finalize", accounts[0].ActiveRequests.Load())
	}
	// The key concurrency slot is held until finalize.
	c2, _ := liveTestContext(t, http.MethodPost, "/v1/realtime", nil, &database.APIKeyRow{
		ID: 21, Limits: database.APIKeyLimits{AllowLive: true, MaxConcurrency: 1},
	})
	if _, _, ok := handler.acquireAPIKeyConcurrencyForWebSocket(c2); ok {
		t.Fatal("key concurrency should be held while the call is open")
	}

	handler.liveCalls().finalize(call.Record)
	handler.liveCalls().finalize(call.Record)
	if accounts[0].ActiveRequests.Load() != 0 {
		t.Fatalf("account reservation after finalize = %d", accounts[0].ActiveRequests.Load())
	}
	if logs.Load() != 1 {
		t.Fatalf("usage logs = %d, want 1", logs.Load())
	}
	release, _, ok := handler.acquireAPIKeyConcurrencyForWebSocket(c2)
	if !ok {
		t.Fatal("key concurrency should be released after finalize")
	}
	release()
}

func TestCreateGatewayLiveCallRequiresAllowLive(t *testing.T) {
	handler, _ := gatewayLiveHandler(t, func(http.ResponseWriter, *http.Request) {
		t.Error("upstream must not be called")
	}, 1)
	c, _ := liveTestContext(t, http.MethodPost, "/v1/realtime", nil, &database.APIKeyRow{ID: 22})
	_, gwErr := handler.createGatewayLiveCall(c, gatewayLiveRequest())
	if gwErr == nil || gwErr.Status != http.StatusForbidden || gwErr.Err == nil {
		t.Fatalf("gwErr=%+v", gwErr)
	}
	if c.Writer.Written() {
		t.Fatal("wrote to the client")
	}
}

func TestCreateGatewayLiveCallRejectsInvalidRequest(t *testing.T) {
	handler, _ := gatewayLiveHandler(t, func(http.ResponseWriter, *http.Request) {
		t.Error("upstream must not be called")
	}, 1)
	c, _ := liveTestContext(t, http.MethodPost, "/v1/realtime", nil, &database.APIKeyRow{
		ID: 23, Limits: database.APIKeyLimits{AllowLive: true},
	})
	_, gwErr := handler.createGatewayLiveCall(c, &liveCallRequest{SDP: ""})
	if gwErr == nil || gwErr.Status != http.StatusBadRequest {
		t.Fatalf("gwErr=%+v", gwErr)
	}
}

func TestCreateGatewayLiveCallUpstreamRejectionKeepsStatusAndReleasesKey(t *testing.T) {
	handler, accounts := gatewayLiveHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad session"}}`))
	}, 1)
	row := &database.APIKeyRow{ID: 24, Limits: database.APIKeyLimits{AllowLive: true, MaxConcurrency: 1}}
	c, rec := liveTestContext(t, http.MethodPost, "/v1/realtime", nil, row)
	_, gwErr := handler.createGatewayLiveCall(c, gatewayLiveRequest())
	if gwErr == nil || gwErr.Status != http.StatusBadRequest || gwErr.Err.Message != "bad session" {
		t.Fatalf("gwErr=%+v", gwErr)
	}
	if c.Writer.Written() || rec.Body.Len() != 0 {
		t.Fatalf("wrote to the client: %q", rec.Body.String())
	}
	if accounts[0].ActiveRequests.Load() != 0 {
		t.Fatalf("account reservation = %d", accounts[0].ActiveRequests.Load())
	}
	c2, _ := liveTestContext(t, http.MethodPost, "/v1/realtime", nil, row)
	release, _, ok := handler.acquireAPIKeyConcurrencyForWebSocket(c2)
	if !ok {
		t.Fatal("key concurrency not released after failure")
	}
	release()
}

func TestCreateGatewayLiveCallExhaustedAccountsReturn503(t *testing.T) {
	var calls atomic.Int32
	handler, accounts := gatewayLiveHandler(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}, 2)
	row := &database.APIKeyRow{ID: 25, Limits: database.APIKeyLimits{AllowLive: true, MaxConcurrency: 1}}
	c, _ := liveTestContext(t, http.MethodPost, "/v1/realtime", nil, row)
	_, gwErr := handler.createGatewayLiveCall(c, gatewayLiveRequest())
	if gwErr == nil || gwErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("gwErr=%+v", gwErr)
	}
	if c.Writer.Written() {
		t.Fatal("wrote to the client")
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want one per account", calls.Load())
	}
	for _, account := range accounts {
		if account.ActiveRequests.Load() != 0 {
			t.Fatalf("account %d reservation = %d", account.ID(), account.ActiveRequests.Load())
		}
	}
	c2, _ := liveTestContext(t, http.MethodPost, "/v1/realtime", nil, row)
	release, _, ok := handler.acquireAPIKeyConcurrencyForWebSocket(c2)
	if !ok {
		t.Fatal("key concurrency not released after failure")
	}
	release()
}
