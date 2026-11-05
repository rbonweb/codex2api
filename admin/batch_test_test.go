package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestShouldMarkBatchTestAccountError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       bool
	}{
		{
			name:       "forbidden is account scoped",
			statusCode: http.StatusForbidden,
			body:       []byte(`{"error":{"code":"unsupported_country_region_territory"}}`),
			want:       true,
		},
		{
			name:       "payment required deactivated workspace is account scoped",
			statusCode: http.StatusPaymentRequired,
			body:       []byte(`{"detail":{"code":"deactivated_workspace"}}`),
			want:       true,
		},
		{
			name:       "bare payment required is account scoped",
			statusCode: http.StatusPaymentRequired,
			body:       []byte(`{"detail":"Payment Required"}`),
			want:       true,
		},
		{
			name:       "invalid grant bad request is account scoped",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":"invalid_grant"}`),
			want:       true,
		},
		{
			name:       "model version bad request is global",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"detail":"The 'gpt-5.5' model requires a newer version of Codex"}`),
			want:       false,
		},
		{
			name:       "server error is not marked as account error",
			statusCode: http.StatusBadGateway,
			body:       []byte(`bad gateway`),
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldMarkBatchTestAccountError(tt.statusCode, tt.body); got != tt.want {
				t.Fatalf("shouldMarkBatchTestAccountError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBatchOperationHTTPStatus(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  string
		message string
		want    int
	}{
		{name: "success", status: "success", message: "test passed", want: http.StatusOK},
		{name: "upstream chinese", status: "failed", message: "upstream returned 402: workspace deactivated", want: http.StatusPaymentRequired},
		{name: "http prefix", status: "failed", message: "HTTP 500: upstream failed", want: http.StatusInternalServerError},
		{name: "embedded status", status: "failed", message: "token endpoint returned status 401", want: http.StatusUnauthorized},
		{name: "status code", status: "failed", message: "token endpoint returned status code 403", want: http.StatusForbidden},
		{name: "status equals", status: "failed", message: "OAuth refresh failed (status=429)", want: http.StatusTooManyRequests},
		{name: "application error", status: "failed", message: "response.failed: model unavailable", want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := batchOperationHTTPStatus(tt.status, tt.message); got != tt.want {
				t.Fatalf("batchOperationHTTPStatus(%q, %q) = %d, want %d", tt.status, tt.message, got, tt.want)
			}
		})
	}
}

func TestEmitBatchTestProgressIncludesStructuredResult(t *testing.T) {
	var completed, success, failed, banned, rateLimited int64
	atomic.StoreInt64(&failed, 1)
	atomic.StoreInt64(&rateLimited, 1)

	var got batchOperationEvent
	handler := &Handler{}
	handler.emitBatchTestProgress(
		func(event batchOperationEvent) {
			got = event
		},
		42,
		3,
		&completed,
		&success,
		&failed,
		&banned,
		&rateLimited,
		"rate_limited",
		"upstream returned 429: account hit rate limiting",
	)

	if got.Type != "progress" || got.Action != "batch_test" {
		t.Fatalf("event type/action = %q/%q, want progress/batch_test", got.Type, got.Action)
	}
	if got.AccountID != 42 || got.Status != "rate_limited" || got.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("event account/status/http = %d/%q/%d, want 42/rate_limited/429", got.AccountID, got.Status, got.HTTPStatus)
	}
	if got.Current != 1 || got.Total != 3 || got.Failed != 1 || got.RateLimited != 1 {
		t.Fatalf("event counters = current:%d total:%d failed:%d rate_limited:%d", got.Current, got.Total, got.Failed, got.RateLimited)
	}
}

func TestResolveBatchTestAccountsDefaultsToAllAccounts(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "token-1", Status: auth.StatusReady})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "token-2", Status: auth.StatusReady})

	accounts, missing := resolveBatchTestAccounts(store, nil)
	if missing != 0 {
		t.Fatalf("missing = %d, want 0", missing)
	}
	if len(accounts) != 2 {
		t.Fatalf("len(accounts) = %d, want 2", len(accounts))
	}
}

func TestResolveBatchTestAccountsUsesSelectedIDs(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "token-1", Status: auth.StatusReady})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "token-2", Status: auth.StatusReady})

	ids := []int64{2, 99, 2, 1}
	accounts, missing := resolveBatchTestAccounts(store, &ids)
	if missing != 1 {
		t.Fatalf("missing = %d, want 1", missing)
	}
	if len(accounts) != 2 {
		t.Fatalf("len(accounts) = %d, want 2", len(accounts))
	}
	if accounts[0].DBID != 2 || accounts[1].DBID != 1 {
		t.Fatalf("account order = [%d, %d], want [2, 1]", accounts[0].DBID, accounts[1].DBID)
	}
}

func TestApplyUsageLimitedTestStateMarks7dRateLimited(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:        1,
		AccessToken: "token",
		PlanType:    "team",
		Status:      auth.StatusReady,
		HealthTier:  auth.HealthTierHealthy,
	}
	account.SetUsagePercent7d(100)
	account.SetReset7dAt(time.Now().Add(time.Hour))
	store.AddAccount(account)

	applyUsageLimitedTestState(store, account, proxy.CodexUsageSyncResult{
		HasUsage7d: true,
		UsagePct7d: 100,
	})

	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	reason, until := account.GetCooldownSnapshot()
	if reason != "rate_limited" || until.IsZero() {
		t.Fatalf("cooldown = (%q, %s), want active rate_limited cooldown", reason, until)
	}
}

func TestApplyUsageLimitedTestStatePreservesBannedAccount(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:       1,
		PlanType:   "team",
		Status:     auth.StatusReady,
		HealthTier: auth.HealthTierBanned,
	}
	account.SetUsagePercent7d(100)
	account.SetReset7dAt(time.Now().Add(time.Hour))
	store.AddAccount(account)

	applyUsageLimitedTestState(store, account, proxy.CodexUsageSyncResult{
		HasUsage7d: true,
		UsagePct7d: 100,
	})

	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
}

func TestRunSingleBatchTestSkipsDeactivatedWorkspaceHandshake(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"detail":{"code":"deactivated_workspace"}}`))
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, nil)
	trigger := &auth.Account{
		DBID:        1,
		AccessToken: "at-trigger",
		AccountID:   "org-ws",
		PlanType:    "team",
		Status:      auth.StatusReady,
		HealthTier:  auth.HealthTierHealthy,
	}
	k12Seat := &auth.Account{
		DBID:        2,
		AccessToken: "at-k12",
		AccountID:   "seat-k12",
		PlanType:    "k12",
		Status:      auth.StatusReady,
		HealthTier:  auth.HealthTierHealthy,
	}
	k12Seat.CustomHeaders = map[string]string{"Chatgpt-Account-Id": "org-ws"}
	store.AddAccount(trigger)
	store.AddAccount(k12Seat)
	store.MarkDeactivatedWorkspace(trigger, "WHAM usage probe returned 402: deactivated_workspace")

	handler := &Handler{store: store}
	status, msg := handler.runSingleBatchTest(context.Background(), k12Seat)
	if status != "failed" {
		t.Fatalf("status = %q, message = %q, want failed", status, msg)
	}
	if !strings.Contains(msg, "workspace-linked") {
		t.Fatalf("message = %q, want linked workspace skip", msg)
	}
	if hits.Load() != 0 {
		t.Fatalf("deactivated workspace sibling still probed upstream %d times", hits.Load())
	}
	if k12Seat.RuntimeStatus() != "error" {
		t.Fatalf("k12 status = %q, want error", k12Seat.RuntimeStatus())
	}
}
