package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestQueryWhamUsage_ParsesPlusAccountResponse(t *testing.T) {
	body := `{
		"user_id": "user-abc",
		"account_id": "user-abc",
		"email": "rundown_consist_3o@icloud.com",
		"plan_type": "plus",
		"subscription_expires_at": "2026-07-17T09:30:23Z",
		"rate_limit": {
			"allowed": true,
			"limit_reached": false,
			"primary_window": {
				"used_percent": 83,
				"limit_window_seconds": 18000,
				"reset_after_seconds": 10778,
				"reset_at": 1779708117
			},
			"secondary_window": {
				"used_percent": 30,
				"limit_window_seconds": 604800,
				"reset_after_seconds": 474764,
				"reset_at": 1780172103
			}
		},
		"credits": {
			"has_credits": false,
			"unlimited": false,
			"overage_limit_reached": false,
			"balance": "0",
			"approx_local_messages": [0, 0],
			"approx_cloud_messages": [0, 0]
		},
		"spend_control": {"reached": false, "individual_limit": null}
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization header missing or malformed: %q", got)
		}
		if r.Header.Get("chatgpt-account-id") != "acc-1" {
			t.Errorf("chatgpt-account-id = %q, want acc-1", r.Header.Get("chatgpt-account-id"))
		}
		if r.Header.Get("Originator") != Originator {
			t.Errorf("Originator = %q, want %q", r.Header.Get("Originator"), Originator)
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "codex-tui/") {
			t.Errorf("User-Agent = %q, want codex-tui prefix", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	// Rewrite the URL to the test server by replacing it through a temporary variable
	oldURL := whamURLForTest
	whamURLForTest = server.URL
	defer func() { whamURLForTest = oldURL }()

	account := &auth.Account{DBID: 1, AccessToken: "at-123", AccountID: "acc-1"}
	usage, _, err := queryWhamUsageWithURL(context.Background(), account, "", whamURLForTest)
	if err != nil {
		t.Fatalf("QueryWhamUsage error: %v", err)
	}
	if usage.PlanType != "plus" {
		t.Errorf("PlanType = %q, want plus", usage.PlanType)
	}
	wantSubscriptionExpiresAt := time.Date(2026, 7, 17, 9, 30, 23, 0, time.UTC)
	if got := usage.SubscriptionExpiresAt(); !got.Equal(wantSubscriptionExpiresAt) {
		t.Errorf("SubscriptionExpiresAt() = %s, want %s", got.Format(time.RFC3339), wantSubscriptionExpiresAt.Format(time.RFC3339))
	}
	if usage.RateLimit.PrimaryWindow == nil || usage.RateLimit.PrimaryWindow.UsedPercent != 83 {
		t.Errorf("primary used_percent = %+v, want 83", usage.RateLimit.PrimaryWindow)
	}
	if usage.RateLimit.SecondaryWindow == nil || usage.RateLimit.SecondaryWindow.UsedPercent != 30 {
		t.Errorf("secondary used_percent = %+v, want 30", usage.RateLimit.SecondaryWindow)
	}
}

func TestQueryWhamUsage_UsesCustomHeaderAccountIDOverride(t *testing.T) {
	var gotAccountID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.Header.Get("chatgpt-account-id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type": "plus", "rate_limit": {}}`))
	}))
	defer server.Close()

	account := &auth.Account{
		DBID:          1,
		AccessToken:   "at-123",
		AccountID:     "acc-1",
		CustomHeaders: map[string]string{"Chatgpt-Account-Id": "acc-override"},
	}
	if _, _, err := queryWhamUsageWithURL(context.Background(), account, "", server.URL); err != nil {
		t.Fatalf("queryWhamUsageWithURL error: %v", err)
	}
	if gotAccountID != "acc-override" {
		t.Errorf("chatgpt-account-id = %q, want acc-override", gotAccountID)
	}
}

func TestApplyWhamUsage_SkipsIdentityWriteBackWhenAccountIDOverridden(t *testing.T) {
	account := &auth.Account{
		DBID:          1,
		AccessToken:   "at",
		AccountID:     "acc-real",
		CustomHeaders: map[string]string{"Chatgpt-Account-Id": "acc-override"},
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})

	usage := &WhamUsage{PlanType: "plus", AccountID: "acc-override", Email: "a@b.c"}
	ApplyWhamUsage(store, account, usage)

	if got := account.AccountID; got != "acc-real" {
		t.Errorf("AccountID = %q, want acc-real (identity must not be overwritten by overridden workspace)", got)
	}
}

func TestApplyWhamUsage_PersistsPlanAnd5h7d(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "wham-test", map[string]interface{}{"plan_type": "free"}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "free", AccountID: "acc"}

	now := time.Now()
	subscriptionExpiresAt := now.Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	reset5h := now.Add(3 * time.Hour).Unix()
	reset7d := now.Add(5 * 24 * time.Hour).Unix()
	usage := &WhamUsage{PlanType: "plus"}
	usage.SubscriptionExpiresAtRaw = whamTimeRaw(subscriptionExpiresAt.Format(time.RFC3339))
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 83, LimitWindowSeconds: 18000, ResetAt: reset5h}
	usage.RateLimit.SecondaryWindow = &WhamUsageWindow{UsedPercent: 30, LimitWindowSeconds: 604800, ResetAt: reset7d}

	result := ApplyWhamUsage(store, account, usage)

	if got := account.GetPlanType(); got != "plus" {
		t.Errorf("plan_type = %q, want plus (synced from wham)", got)
	}
	if !result.HasUsage5h || result.UsagePct5h != 83 {
		t.Errorf("5h result = %+v, want HasUsage5h && UsagePct5h=83", result)
	}
	if !result.HasUsage7d || result.UsagePct7d != 30 {
		t.Errorf("7d result = %+v, want HasUsage7d && UsagePct7d=30", result)
	}
	if result.Premium5hRateLimited {
		t.Error("expected NOT premium 5h rate limited (used_percent=83 < 100)")
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("plan_type"); got != "plus" {
		t.Errorf("persisted plan_type = %q, want plus", got)
	}
	wantSubscriptionExpiresAt := subscriptionExpiresAt
	if !account.SubscriptionExpiresAt.Equal(wantSubscriptionExpiresAt) {
		t.Errorf("account SubscriptionExpiresAt = %s, want %s", account.SubscriptionExpiresAt.Format(time.RFC3339), wantSubscriptionExpiresAt.Format(time.RFC3339))
	}
	if got := row.GetCredential("subscription_expires_at"); got != wantSubscriptionExpiresAt.Format(time.RFC3339) {
		t.Errorf("persisted subscription_expires_at = %q, want %q", got, wantSubscriptionExpiresAt.Format(time.RFC3339))
	}
}

func TestApplyWhamUsage_PersistsIdentity(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "at-only", map[string]interface{}{"access_token": "at"}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at"}
	usage := &WhamUsage{
		UserID:    "user-from-wham",
		AccountID: "account-from-wham",
		Email:     "wham@example.com",
		PlanType:  "team",
	}

	ApplyWhamUsage(store, account, usage)

	if account.Email != "wham@example.com" {
		t.Fatalf("account.Email = %q, want wham@example.com", account.Email)
	}
	if account.AccountID != "account-from-wham" {
		t.Fatalf("account.AccountID = %q, want account-from-wham", account.AccountID)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("email"); got != "wham@example.com" {
		t.Fatalf("credentials.email = %q, want wham@example.com", got)
	}
	if got := row.GetCredential("account_id"); got != "account-from-wham" {
		t.Fatalf("credentials.account_id = %q, want account-from-wham", got)
	}
}

func TestApplyWhamUsage_PersistsSubscriptionExpiresAtWhenMemoryAlreadyMatches(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "wham-subscription-retry", map[string]interface{}{"plan_type": "plus"}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	expiresAt := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "plus", AccountID: "acc", SubscriptionExpiresAt: expiresAt}
	usage := &WhamUsage{PlanType: "plus"}
	usage.SubscriptionExpiresAtRaw = whamTimeRaw(expiresAt.Format(time.RFC3339))

	ApplyWhamUsage(store, account, usage)

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("subscription_expires_at"); got != expiresAt.Format(time.RFC3339) {
		t.Fatalf("persisted subscription_expires_at = %q, want %q", got, expiresAt.Format(time.RFC3339))
	}
}

// TestApplyWhamUsage_ClearsStaleSubscriptionExpiry verifies: wham authoritatively returns a paid plan_type
// while the subscription expiry stored on the account has already passed (the old value still in the JWT after renewal), the stale in-memory and DB values are cleared,
// so it no longer shows "expired". (issue #360)
func TestApplyWhamUsage_ClearsStaleSubscriptionExpiry(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	stale := time.Now().Add(-96 * time.Hour).UTC().Truncate(time.Second)
	id, err := db.InsertAccountWithCredentials(ctx, "wham-stale-subscription", map[string]interface{}{
		"plan_type":               "plus",
		"subscription_expires_at": stale.Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "plus", AccountID: "acc", SubscriptionExpiresAt: stale}
	// Measured: wham/usage returns no subscription expiry fields, so this stays unset to match the real response.
	usage := &WhamUsage{PlanType: "plus"}

	ApplyWhamUsage(store, account, usage)

	if !account.SubscriptionExpiresAt.IsZero() {
		t.Fatalf("account.SubscriptionExpiresAt = %v, want zero after stale clear", account.SubscriptionExpiresAt)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("subscription_expires_at"); got != "" {
		t.Fatalf("persisted subscription_expires_at = %q, want cleared", got)
	}
}

// TestApplyWhamUsage_KeepsExpiredSubscriptionForFreePlan verifies: when the plan has truly expired (wham returns
// free), the past subscription expiry is accurate information and is kept, not cleared.
func TestApplyWhamUsage_KeepsExpiredSubscriptionForFreePlan(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	expired := time.Now().Add(-96 * time.Hour).UTC().Truncate(time.Second)
	id, err := db.InsertAccountWithCredentials(ctx, "wham-real-expired", map[string]interface{}{
		"plan_type":               "plus",
		"subscription_expires_at": expired.Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "plus", AccountID: "acc", SubscriptionExpiresAt: expired}
	usage := &WhamUsage{PlanType: "free"}

	ApplyWhamUsage(store, account, usage)

	if !account.SubscriptionExpiresAt.Equal(expired) {
		t.Fatalf("account.SubscriptionExpiresAt = %v, want unchanged %v", account.SubscriptionExpiresAt, expired)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("subscription_expires_at"); got != expired.Format(time.RFC3339) {
		t.Fatalf("persisted subscription_expires_at = %q, want %q", got, expired.Format(time.RFC3339))
	}
}

func TestWhamUsageSubscriptionExpiresAtFallbacks(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Time
	}{
		{
			name: "subscription_active_until",
			body: `{"subscription_active_until":"2026-07-17T09:30:23.123Z"}`,
			want: time.Date(2026, 7, 17, 9, 30, 23, 123000000, time.UTC),
		},
		{
			name: "chatgpt_subscription_active_until",
			body: `{"chatgpt_subscription_active_until":"2026-07-17T09:30:23Z"}`,
			want: time.Date(2026, 7, 17, 9, 30, 23, 0, time.UTC),
		},
		{
			name: "unix_seconds",
			body: `{"subscription_expires_at":1784280623}`,
			want: time.Unix(1784280623, 0),
		},
		{
			name: "unix_milliseconds",
			body: `{"subscription_expires_at":1784280623000}`,
			want: time.UnixMilli(1784280623000),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var usage WhamUsage
			if err := json.Unmarshal([]byte(tc.body), &usage); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			if got := usage.SubscriptionExpiresAt(); !got.Equal(tc.want) {
				t.Fatalf("SubscriptionExpiresAt() = %s, want %s", got.Format(time.RFC3339Nano), tc.want.Format(time.RFC3339Nano))
			}
		})
	}
}

func TestApplyWhamUsage5hOnlyDoesNotRefreshStale7dProbeFreshness(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{
		DBID:                2,
		AccessToken:         "at",
		PlanType:            "plus",
		Status:              auth.StatusReady,
		UsagePercent7d:      40,
		UsagePercent7dValid: true,
		UsageUpdatedAt:      time.Now().Add(-20 * time.Minute),
	}

	now := time.Now()
	usage := &WhamUsage{PlanType: "plus"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 83, LimitWindowSeconds: 18000, ResetAt: now.Add(2 * time.Hour).Unix()}

	result := ApplyWhamUsage(store, account, usage)

	if !result.Used5hHeaders || !result.HasUsage5h {
		t.Fatalf("ApplyWhamUsage result = %+v, want 5h-only usage snapshot", result)
	}
	if result.HasUsage7d {
		t.Fatalf("ApplyWhamUsage result = %+v, want no 7d snapshot from 5h-only usage", result)
	}
	if !result.Persisted5hOnly {
		t.Fatalf("ApplyWhamUsage result = %+v, want 5h-only persistence path", result)
	}
	if !account.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = false, want true because 5h-only WHAM sync must not refresh stale 7d freshness")
	}
}

// Reproduces issue #168: on a free account's wham response, primary_window actually carries the 7d data
// (limit_window_seconds=604800), with secondary_window=null. The code must classify by
// limit_window_seconds rather than by field position, otherwise the 7d data is wrongly written into the 5h slot.
func TestApplyWhamUsage_FreeAccountPrimaryIs7d(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "wham-free", map[string]interface{}{"plan_type": "free"}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "free", AccountID: "acc"}

	reset7d := time.Now().Add(7 * 24 * time.Hour).Unix()
	usage := &WhamUsage{PlanType: "free"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 3, LimitWindowSeconds: 604800, ResetAfterSeconds: 604800, ResetAt: reset7d}
	usage.RateLimit.SecondaryWindow = nil

	result := ApplyWhamUsage(store, account, usage)

	if result.HasUsage5h {
		t.Errorf("expected HasUsage5h=false for free account (only 7d window), got result=%+v", result)
	}
	if result.Used5hHeaders {
		t.Errorf("expected Used5hHeaders=false (no 5h window in response), got result=%+v", result)
	}
	if !result.HasUsage7d || result.UsagePct7d != 3 {
		t.Errorf("7d result = %+v, want HasUsage7d && UsagePct7d=3", result)
	}
	if result.Persisted5hOnly {
		t.Error("expected Persisted5hOnly=false; should persist via 7d snapshot path")
	}

	if pct, ok := account.GetUsagePercent7d(); !ok || pct != 3 {
		t.Errorf("account 7d in-memory snapshot = (%v, %v), want (3, true)", pct, ok)
	}
	if _, ok := account.GetUsagePercent5h(); ok {
		t.Error("account 5h snapshot should remain unset for free account")
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("codex_7d_used_percent"); got != "3" {
		t.Errorf("persisted codex_7d_used_percent = %q, want %q", got, "3")
	}
}

// issue #382: when the upstream WHAM no longer returns the 5h window, the local stale 5h snapshot and the premium 5h rate limit must be cleared.
func TestApplyWhamUsage_ClearsStale5hWhenUpstreamOmitsWindow(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "wham-no-5h", map[string]interface{}{
		"plan_type":                 "plus",
		"codex_5h_used_percent":     80,
		"codex_5h_reset_at":         time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		"codex_5h_usage_updated_at": time.Now().Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "plus", AccountID: "acc"}
	account.SetUsageSnapshot5h(80, time.Now().Add(2*time.Hour))

	reset7d := time.Now().Add(7 * 24 * time.Hour).Unix()
	usage := &WhamUsage{PlanType: "plus"}
	// weekly only: simulates the WHAM response after OpenAI temporarily removed the 5h window
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 22, LimitWindowSeconds: 604800, ResetAfterSeconds: 604800, ResetAt: reset7d}
	usage.RateLimit.SecondaryWindow = nil

	result := ApplyWhamUsage(store, account, usage)

	if result.HasUsage5h || result.Used5hHeaders {
		t.Fatalf("result = %+v, want no 5h window", result)
	}
	if !result.Cleared5h {
		t.Fatal("Cleared5h = false, want true when stale 5h was present")
	}
	if !result.HasUsage7d || result.UsagePct7d != 22 {
		t.Fatalf("7d result = %+v, want UsagePct7d=22", result)
	}
	if _, ok := account.GetUsagePercent5h(); ok {
		t.Fatal("in-memory 5h snapshot should be cleared")
	}
	if account.IsPremium5hRateLimited() {
		t.Fatal("account should not remain premium 5h rate limited")
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("codex_5h_used_percent"); got != "" {
		t.Errorf("persisted codex_5h_used_percent = %q, want empty/cleared", got)
	}
	if got := row.GetCredential("codex_7d_used_percent"); got != "22" {
		t.Errorf("persisted codex_7d_used_percent = %q, want 22", got)
	}
}

func TestApplyWhamUsage_ClearsPremium5hCooldownWhenWindowGone(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 42, AccessToken: "at", PlanType: "plus", Status: auth.StatusReady}
	resetAt := time.Now().Add(3 * time.Hour)
	store.MarkPremium5hRateLimited(account, resetAt)
	if !account.IsPremium5hRateLimited() {
		t.Fatal("precondition: account should be premium 5h rate limited")
	}

	usage := &WhamUsage{PlanType: "plus"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{
		UsedPercent:        10,
		LimitWindowSeconds: 604800,
		ResetAfterSeconds:  604800,
		ResetAt:            time.Now().Add(7 * 24 * time.Hour).Unix(),
	}

	result := ApplyWhamUsage(store, account, usage)
	if !result.Cleared5h {
		t.Fatal("Cleared5h = false, want true")
	}
	if account.IsPremium5hRateLimited() {
		t.Fatal("premium 5h limit should be cleared when upstream omits 5h")
	}
	if got := account.RuntimeStatus(); got == "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want not rate_limited after clearing absent 5h", got)
	}
}

func TestApplyWhamUsage_EmptyWindowPayloadPreserves5h(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 43, AccessToken: "at", PlanType: "plus", Status: auth.StatusReady}
	resetAt := time.Now().Add(3 * time.Hour)
	account.SetUsageSnapshot5h(88, resetAt)

	usage := &WhamUsage{PlanType: "plus"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{}

	result := ApplyWhamUsage(store, account, usage)
	if result.Cleared5h || result.HasUsage5h || result.HasUsage7d {
		t.Fatalf("result = %+v, want malformed payload to be non-authoritative", result)
	}
	if pct, gotReset, ok := account.GetUsageSnapshot5h(); !ok || pct != 88 || !gotReset.Equal(resetAt) {
		t.Fatalf("5h snapshot = (%v, %v, %v), want preserved 88%% and reset %v", pct, gotReset, ok, resetAt)
	}
}

func TestApplyWhamUsage_UsedPercentOnlyPayloadPreserves5h(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 44, AccessToken: "at", PlanType: "plus", Status: auth.StatusReady}
	account.SetUsageSnapshot5h(77, time.Now().Add(2*time.Hour))

	usage := &WhamUsage{PlanType: "plus"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 12}

	result := ApplyWhamUsage(store, account, usage)
	if result.Cleared5h || result.HasUsage5h || result.HasUsage7d {
		t.Fatalf("result = %+v, want used-percent-only payload to be non-authoritative", result)
	}
	if _, _, ok := account.GetUsageSnapshot5h(); !ok {
		t.Fatal("5h snapshot should remain valid for a used-percent-only payload")
	}
}

// Defensive test: even if the backend swaps the order of the 5h/7d fields, classification must still follow limit_window_seconds.
func TestApplyWhamUsage_ClassifiesByWindowSeconds(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 1, AccessToken: "at", PlanType: "plus"}

	now := time.Now()
	usage := &WhamUsage{PlanType: "plus"}
	// Deliberately reversed: 7d (604800) in primary, 5h (18000) in secondary
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 30, LimitWindowSeconds: 604800, ResetAt: now.Add(5 * 24 * time.Hour).Unix()}
	usage.RateLimit.SecondaryWindow = &WhamUsageWindow{UsedPercent: 83, LimitWindowSeconds: 18000, ResetAt: now.Add(3 * time.Hour).Unix()}

	result := ApplyWhamUsage(store, account, usage)

	if !result.HasUsage5h || result.UsagePct5h != 83 {
		t.Errorf("5h result = %+v, want UsagePct5h=83 (classified by 18000s window)", result)
	}
	if !result.HasUsage7d || result.UsagePct7d != 30 {
		t.Errorf("7d result = %+v, want UsagePct7d=30 (classified by 604800s window)", result)
	}
}

// When limit_window_seconds is missing or has an unknown value, fall back to classifying by field position.
func TestPickClassifiedWhamWindows_FallsBackToPositionForUnknownSeconds(t *testing.T) {
	primary := &WhamUsageWindow{UsedPercent: 50, LimitWindowSeconds: 0} // unknown or missing
	secondary := &WhamUsageWindow{UsedPercent: 20, LimitWindowSeconds: 0}

	w5h, w7d := pickClassifiedWhamWindows(primary, secondary, "plus", time.Now())
	if w5h != primary {
		t.Errorf("expected primary→5h via position fallback, got %v", w5h)
	}
	if w7d != secondary {
		t.Errorf("expected secondary→7d via position fallback, got %v", w7d)
	}
}

func TestPickClassifiedWhamWindows_FreeUnknownPrimaryFallsBackTo7d(t *testing.T) {
	primary := &WhamUsageWindow{UsedPercent: 100, LimitWindowSeconds: 0}

	w5h, w7d := pickClassifiedWhamWindows(primary, nil, "free", time.Now())
	if w5h != nil {
		t.Fatalf("expected no 5h window for free unknown primary, got %v", w5h)
	}
	if w7d != primary {
		t.Fatalf("expected primary→7d for free unknown primary, got %v", w7d)
	}
}

func TestPickClassifiedWhamWindows_LongResetPrimaryFallsBackTo7d(t *testing.T) {
	primary := &WhamUsageWindow{
		UsedPercent:        100,
		LimitWindowSeconds: 0,
		ResetAfterSeconds:  6 * 60 * 60,
	}

	w5h, w7d := pickClassifiedWhamWindows(primary, nil, "", time.Now())
	if w5h != nil {
		t.Fatalf("expected no 5h window for long reset primary, got %v", w5h)
	}
	if w7d != primary {
		t.Fatalf("expected primary→7d for long reset primary, got %v", w7d)
	}
}

// TestPickClassifiedWhamWindows_TeamMonthlyWindowRoutesTo7dSlot verifies that the team plan's
// monthly window (about 30 days = 2592000s) is routed into the long-window (7d) slot, not dropped or wrongly put into 5h.
func TestPickClassifiedWhamWindows_TeamMonthlyWindowRoutesTo7dSlot(t *testing.T) {
	now := time.Now()
	primary := &WhamUsageWindow{UsedPercent: 40, LimitWindowSeconds: 18000, ResetAt: now.Add(2 * time.Hour).Unix()}
	monthly := &WhamUsageWindow{UsedPercent: 12, LimitWindowSeconds: 2_592_000, ResetAt: now.Add(20 * 24 * time.Hour).Unix()}

	w5h, w7d := pickClassifiedWhamWindows(primary, monthly, "team", now)
	if w5h != primary {
		t.Fatalf("expected primary→5h, got %v", w5h)
	}
	if w7d != monthly {
		t.Fatalf("expected monthly(2592000s)→7d slot, got %v", w7d)
	}

	// 28–31 day tolerance: a 29-day window should also be recognized as monthly.
	tolMonthly := &WhamUsageWindow{UsedPercent: 5, LimitWindowSeconds: 29 * 24 * 60 * 60, ResetAt: now.Add(29 * 24 * time.Hour).Unix()}
	if _, w7dTol := pickClassifiedWhamWindows(primary, tolMonthly, "team", now); w7dTol != tolMonthly {
		t.Fatalf("expected 29d window→7d slot via tolerance, got %v", w7dTol)
	}
}

// TestApplyWhamUsage_CapturesMonthlyWindowLength verifies that the real period in seconds of the team monthly window is recorded on the account,
// so smart pacing computes with the real period (not a fixed 7 days).
func TestApplyWhamUsage_CapturesMonthlyWindowLength(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 1, AccessToken: "at", PlanType: "team"}

	now := time.Now()
	usage := &WhamUsage{PlanType: "team"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 40, LimitWindowSeconds: 18000, ResetAt: now.Add(2 * time.Hour).Unix()}
	usage.RateLimit.SecondaryWindow = &WhamUsageWindow{UsedPercent: 12, LimitWindowSeconds: 2_592_000, ResetAt: now.Add(20 * 24 * time.Hour).Unix()}

	ApplyWhamUsage(store, account, usage)

	if got := account.GetWindow7dSeconds(); got != 2_592_000 {
		t.Fatalf("Window7dSeconds=%d, want 2592000", got)
	}
	if kind := account.Window7dKind(); kind != "monthly" {
		t.Fatalf("Window7dKind=%q, want monthly", kind)
	}
}

// TestApplyWhamUsage_FreePlanMonthlyWindow verifies that the only rate-limit window of the free plan (the monthly window)
// is also recognized as monthly: free has only primary=2592000s and no secondary (issue #324,
// the frontend uses usage_window_7d_kind to show the label as 30d instead of 7d).
func TestApplyWhamUsage_FreePlanMonthlyWindow(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 1, AccessToken: "at", PlanType: "free"}

	now := time.Now()
	usage := &WhamUsage{PlanType: "free"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 63, LimitWindowSeconds: 2_592_000, ResetAt: now.Add(18 * 24 * time.Hour).Unix()}

	result := ApplyWhamUsage(store, account, usage)

	if !result.HasUsage7d {
		t.Fatal("expected free monthly window to land in the 7d slot")
	}
	if result.HasUsage5h {
		t.Fatal("free monthly window must not be classified as 5h")
	}
	if got := account.GetWindow7dSeconds(); got != 2_592_000 {
		t.Fatalf("Window7dSeconds=%d, want 2592000", got)
	}
	if kind := account.Window7dKind(); kind != "monthly" {
		t.Fatalf("Window7dKind=%q, want monthly", kind)
	}
}

func TestApplyWhamUsage_MarksPremium5hLimitedAt100Percent(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 1, AccessToken: "at", PlanType: "plus"}

	now := time.Now()
	usage := &WhamUsage{PlanType: "plus"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 100, LimitWindowSeconds: 18000, ResetAt: now.Add(2 * time.Hour).Unix()}

	result := ApplyWhamUsage(store, account, usage)
	if !result.Premium5hRateLimited {
		t.Errorf("expected Premium5hRateLimited=true for plus plan at 100%%, result=%+v", result)
	}
	if !account.IsPremium5hRateLimited() {
		t.Error("account should be in premium 5h rate-limited state after ApplyWhamUsage")
	}
}

func TestApplyWhamUsage_CreditAccountSkipsPremium5hWindowLimit(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.5"})
	account := &auth.Account{
		DBID:                  1,
		AccessToken:           "at",
		PlanType:              "plus",
		Status:                auth.StatusReady,
		CreditEnabled:         true,
		CreditSkipUsageWindow: true,
	}
	// The credits toggle now also requires that credits are actually available right now; a missing snapshot is treated as "no credits".
	account.SetCreditBalance("1000.0000000000", true, false, false)

	now := time.Now()
	usage := &WhamUsage{PlanType: "plus"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 100, LimitWindowSeconds: 18000, ResetAt: now.Add(2 * time.Hour).Unix()}

	result := ApplyWhamUsage(store, account, usage)
	if result.Premium5hRateLimited {
		t.Fatalf("Premium5hRateLimited = true, want false for credit account")
	}
	if account.IsPremium5hRateLimited() {
		t.Fatal("credit account should not be in premium 5h rate-limited state after ApplyWhamUsage")
	}
	pct5h, _, ok := account.GetUsageSnapshot5h()
	if !ok || pct5h != 100 {
		t.Fatalf("5h snapshot = (%v, %v), want 100 with valid snapshot", pct5h, ok)
	}
}

func TestApplyWhamUsage_Marks7dLimitedAt100Percent(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 1, AccessToken: "at", PlanType: "team", Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy}

	now := time.Now()
	usage := &WhamUsage{PlanType: "team"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 20, LimitWindowSeconds: 18000, ResetAt: now.Add(2 * time.Hour).Unix()}
	usage.RateLimit.SecondaryWindow = &WhamUsageWindow{UsedPercent: 100, LimitWindowSeconds: 604800, ResetAt: now.Add(5 * 24 * time.Hour).Unix()}

	result := ApplyWhamUsage(store, account, usage)
	if !result.Usage7dRateLimited {
		t.Fatalf("Usage7dRateLimited = false, result=%+v", result)
	}
	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
}

func TestApplyWhamUsage_CreditAccountSkips7dWindowLimit(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.5"})
	account := &auth.Account{
		DBID:                  1,
		AccessToken:           "at",
		PlanType:              "team",
		Status:                auth.StatusReady,
		HealthTier:            auth.HealthTierHealthy,
		CreditEnabled:         true,
		CreditSkipUsageWindow: true,
	}
	// The credits toggle now also requires that credits are actually available right now; a missing snapshot is treated as "no credits".
	account.SetCreditBalance("1000.0000000000", true, false, false)

	now := time.Now()
	usage := &WhamUsage{PlanType: "team"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 20, LimitWindowSeconds: 18000, ResetAt: now.Add(2 * time.Hour).Unix()}
	usage.RateLimit.SecondaryWindow = &WhamUsageWindow{UsedPercent: 100, LimitWindowSeconds: 604800, ResetAt: now.Add(5 * 24 * time.Hour).Unix()}

	result := ApplyWhamUsage(store, account, usage)
	if result.Usage7dRateLimited {
		t.Fatalf("Usage7dRateLimited = true, want false for credit account")
	}
	// The window is fully used but credits still cover it: the display still shows rate limited, and scheduling is not affected,
	// the frontend uses UsingCredits to show a credits badge after the rate-limit badge.
	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited for credit account", got)
	}
	if !account.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true while credits cover the window")
	}
	if !account.UsingCredits() {
		t.Fatal("UsingCredits() = false, want true while credits cover the window")
	}
	pct7d, ok := account.GetUsagePercent7d()
	if !ok || pct7d != 100 {
		t.Fatalf("7d snapshot = (%v, %v), want 100 with valid snapshot", pct7d, ok)
	}
}

func TestApplyWhamUsage_IgnoredUsageStatusRemainsMetadataOnly(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		TestConcurrency:        1,
		TestModel:              "gpt-5.4",
		IgnoreUsageLimitStatus: true,
	})
	account := &auth.Account{DBID: 2, AccessToken: "at", PlanType: "team", Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy}
	store.AddAccount(account)

	now := time.Now()
	usage := &WhamUsage{PlanType: "team"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 100, LimitWindowSeconds: 18000, ResetAt: now.Add(2 * time.Hour).Unix()}
	usage.RateLimit.SecondaryWindow = &WhamUsageWindow{UsedPercent: 100, LimitWindowSeconds: 604800, ResetAt: now.Add(5 * 24 * time.Hour).Unix()}

	result := ApplyWhamUsage(store, account, usage)
	if !result.UsageWindowLimitsIgnored {
		t.Fatal("UsageWindowLimitsIgnored = false, want true")
	}
	if result.Premium5hRateLimited || result.Usage7dRateLimited {
		t.Fatalf("WHAM metadata created a cooldown: %+v", result)
	}
	if account.IsAvailable() {
		t.Fatal("WHAM 100% metadata must fence the account from fresh scheduling")
	}
	store.BindSessionAffinity("working-turn", account, "")
	continued, _ := store.NextForContinuationWithFilter("working-turn", 0, nil, nil)
	if continued != account {
		t.Fatal("WHAM 100% metadata prevented the existing turn from continuing")
	}
	store.Release(continued)
	if pct5h, _, ok := account.GetUsageSnapshot5h(); !ok || pct5h != 100 {
		t.Fatalf("5h snapshot = (%v, %v), want 100 and valid", pct5h, ok)
	}
	if pct7d, ok := account.GetUsagePercent7d(); !ok || pct7d != 100 {
		t.Fatalf("7d snapshot = (%v, %v), want 100 and valid", pct7d, ok)
	}
}

func TestWhamUsageJSON_RoundTrip(t *testing.T) {
	in := WhamUsage{PlanType: "plus"}
	in.RateLimit.Allowed = true
	in.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 50}

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out WhamUsage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.RateLimit.PrimaryWindow == nil || out.RateLimit.PrimaryWindow.UsedPercent != 50 {
		t.Errorf("roundtrip lost primary window")
	}
}

func TestQueryWhamUsage_ParsesRateLimitResetCredits(t *testing.T) {
	body := `{
		"plan_type": "plus",
		"subscription_expires_at": "2026-07-17T09:30:23Z",
		"rate_limit": {"allowed": true},
		"rate_limit_reset_credits": {"available_count": 4}
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	oldURL := whamURLForTest
	whamURLForTest = server.URL
	defer func() { whamURLForTest = oldURL }()

	account := &auth.Account{DBID: 1, AccessToken: "at-123", AccountID: "acc-1"}
	usage, _, err := queryWhamUsageWithURL(context.Background(), account, "", whamURLForTest)
	if err != nil {
		t.Fatalf("QueryWhamUsage error: %v", err)
	}
	if usage.RateLimitResetCredits == nil || usage.RateLimitResetCredits.AvailableCount != 4 {
		t.Fatalf("RateLimitResetCredits = %+v, want available_count=4", usage.RateLimitResetCredits)
	}

	// ApplyWhamUsage should write the counts to the account.
	ApplyWhamUsage(nil, account, usage)
	if count, ok := account.GetRateLimitResetCredits(); !ok || count != 4 {
		t.Fatalf("account reset credits = (%d,%v), want (4,true)", count, ok)
	}
}

// TestQueryWhamResetCredits_ParsesAndFilters verifies parsing and filtering of the reset-credit list endpoint
// (issue #322): only keep reset credits with reset_type=codex_rate_limits, status=available, and an expires_at,
// with the same request shape as the wham query (Bearer + chatgpt-account-id).
func TestQueryWhamResetCredits_ParsesAndFilters(t *testing.T) {
	body := `{
		"available_count": 3,
		"credits": [
			{"id": "c1", "reset_type": "codex_rate_limits", "status": "available", "granted_at": "2026-06-29T00:00:00Z", "expires_at": "2026-07-19T00:42:09Z"},
			{"id": "c2", "reset_type": "codex_rate_limits", "status": "available", "granted_at": "2026-07-01T00:00:00Z", "expires_at": "2026-07-21T08:00:00Z"},
			{"id": "c6", "reset_type": "codex_rate_limits", "status": "available", "granted_at": "2026-07-02T00:00:00Z", "consumable_until": "2026-07-22T08:00:00Z"},
			{"id": "c3", "reset_type": "codex_rate_limits", "status": "redeemed", "granted_at": "2026-06-20T00:00:00Z", "expires_at": "2026-07-10T00:00:00Z"},
			{"id": "c4", "reset_type": "other_type", "status": "available", "granted_at": "2026-06-29T00:00:00Z", "expires_at": "2026-07-19T00:00:00Z"},
			{"id": "c5", "reset_type": "codex_rate_limits", "status": "available", "granted_at": "2026-06-29T00:00:00Z", "expires_at": ""}
		]
	}`

	var gotMethod, gotAuth, gotAccountID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotAccountID = r.Header.Get("chatgpt-account-id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	oldURL := whamResetCreditsURLForTest
	whamResetCreditsURLForTest = server.URL
	defer func() { whamResetCreditsURLForTest = oldURL }()

	account := &auth.Account{DBID: 1, AccessToken: "at-123", AccountID: "acc-1"}
	list, _, err := QueryWhamResetCredits(context.Background(), account, "")
	if err != nil {
		t.Fatalf("QueryWhamResetCredits error: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("Authorization = %q, want Bearer prefix", gotAuth)
	}
	if gotAccountID != "acc-1" {
		t.Errorf("chatgpt-account-id = %q, want acc-1", gotAccountID)
	}
	if list.AvailableCount != 3 {
		t.Errorf("AvailableCount = %d, want 3", list.AvailableCount)
	}

	credits := list.AvailableCodexCredits()
	if len(credits) != 3 {
		t.Fatalf("AvailableCodexCredits len = %d, want 3 (accept consumable_until; filter redeemed/other_type/no-expiry)", len(credits))
	}
	if credits[0].ID != "c1" || credits[1].ID != "c2" || credits[2].ID != "c6" {
		t.Errorf("credits ids = %s,%s,%s, want c1,c2,c6", credits[0].ID, credits[1].ID, credits[2].ID)
	}
	if credits[0].ExpiresAt != "2026-07-19T00:42:09Z" {
		t.Errorf("credits[0].ExpiresAt = %q", credits[0].ExpiresAt)
	}
	if credits[2].EffectiveConsumableUntil() != "2026-07-22T08:00:00Z" {
		t.Errorf("credits[2].EffectiveConsumableUntil = %q", credits[2].EffectiveConsumableUntil())
	}
}

func TestWhamResetCreditItemEffectiveConsumableUntilPrefersCanonicalField(t *testing.T) {
	credit := WhamResetCreditItem{
		ExpiresAt:       "2026-07-20T00:00:00Z",
		ConsumableUntil: "2026-07-19T00:00:00Z",
	}
	if got := credit.EffectiveConsumableUntil(); got != "2026-07-19T00:00:00Z" {
		t.Fatalf("EffectiveConsumableUntil() = %q, want consumable_until", got)
	}
}

func TestConsumeResetCredit_PostsRedeemRequestID(t *testing.T) {
	var gotMethod, gotAuth, gotAccountID, gotRedeemID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotAccountID = r.Header.Get("chatgpt-account-id")
		var payload struct {
			RedeemRequestID string `json:"redeem_request_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		gotRedeemID = payload.RedeemRequestID
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	oldURL := whamConsumeURLForTest
	whamConsumeURLForTest = server.URL
	defer func() { whamConsumeURLForTest = oldURL }()

	account := &auth.Account{DBID: 1, AccessToken: "at-123", AccountID: "acc-1"}
	resp, err := ConsumeResetCredit(context.Background(), account, "")
	if err != nil {
		t.Fatalf("ConsumeResetCredit error: %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("resp = %+v, want 200", resp)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("Authorization = %q, want Bearer prefix", gotAuth)
	}
	if gotAccountID != "acc-1" {
		t.Errorf("chatgpt-account-id = %q, want acc-1", gotAccountID)
	}
	if gotRedeemID == "" {
		t.Errorf("redeem_request_id missing in request body")
	}
}

func TestConsumeResetCredit_NonOKReturnsResp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()

	oldURL := whamConsumeURLForTest
	whamConsumeURLForTest = server.URL
	defer func() { whamConsumeURLForTest = oldURL }()

	account := &auth.Account{DBID: 1, AccessToken: "at-123", AccountID: "acc-1"}
	resp, err := ConsumeResetCredit(context.Background(), account, "")
	if err == nil {
		t.Fatal("expected error on non-2xx")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("resp = %+v, want 401 surfaced to caller", resp)
	}
	_ = resp.Body.Close()
}

func TestConsumeResetCredit_NoToken(t *testing.T) {
	account := &auth.Account{DBID: 1}
	if _, err := ConsumeResetCredit(context.Background(), account, ""); err == nil {
		t.Fatal("expected error when account has no access token")
	}
}

// TestConsumeResetCreditWithID_ReusesProvidedIdempotencyKey verifies that the redeem_request_id provided by the caller
// is sent upstream unchanged: retrying the same reset reuses it, letting upstream idempotent deduplication work,
// so a reset is not consumed twice.
func TestConsumeResetCreditWithID_ReusesProvidedIdempotencyKey(t *testing.T) {
	var gotIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			RedeemRequestID string `json:"redeem_request_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		gotIDs = append(gotIDs, payload.RedeemRequestID)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	oldURL := whamConsumeURLForTest
	whamConsumeURLForTest = server.URL
	defer func() { whamConsumeURLForTest = oldURL }()

	account := &auth.Account{DBID: 1, AccessToken: "at-123", AccountID: "acc-1"}
	const fixedID = "fixed-redeem-id-123"

	// Both calls reuse the same ID (simulates a retry after a refresh).
	for i := 0; i < 2; i++ {
		resp, err := ConsumeResetCreditWithID(context.Background(), account, "", fixedID)
		if err != nil {
			t.Fatalf("ConsumeResetCreditWithID error: %v", err)
		}
		_ = resp.Body.Close()
	}

	if len(gotIDs) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(gotIDs))
	}
	for i, id := range gotIDs {
		if id != fixedID {
			t.Errorf("request %d redeem_request_id = %q, want %q (idempotency key must be reused)", i, id, fixedID)
		}
	}
}

// TestConsumeResetCreditWithID_EmptyIDFallsBackToGenerated verifies that an empty ID still generates one,
// so an empty idempotency key is never sent.
func TestConsumeResetCreditWithID_EmptyIDFallsBackToGenerated(t *testing.T) {
	var gotID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			RedeemRequestID string `json:"redeem_request_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		gotID = payload.RedeemRequestID
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	oldURL := whamConsumeURLForTest
	whamConsumeURLForTest = server.URL
	defer func() { whamConsumeURLForTest = oldURL }()

	account := &auth.Account{DBID: 1, AccessToken: "at-123", AccountID: "acc-1"}
	resp, err := ConsumeResetCreditWithID(context.Background(), account, "", "   ")
	if err != nil {
		t.Fatalf("ConsumeResetCreditWithID error: %v", err)
	}
	_ = resp.Body.Close()
	if gotID == "" {
		t.Fatal("redeem_request_id should fall back to a generated value, got empty")
	}
}

// TestConsumeResetCreditParsed_ReturnsWindowsResetAndCredit verifies that a successful response is parsed into
// WhamResetResult (windows_reset / credit), so callers get instant feedback without querying usage again after the reset.
func TestConsumeResetCreditParsed_ReturnsWindowsResetAndCredit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":"ok","windows_reset":2,"credit":{"id":"cr-1","reset_type":"manual","status":"redeemed","redeemed_at":"2026-06-16T00:00:00Z"}}`))
	}))
	defer server.Close()

	oldURL := whamConsumeURLForTest
	whamConsumeURLForTest = server.URL
	defer func() { whamConsumeURLForTest = oldURL }()

	account := &auth.Account{DBID: 1, AccessToken: "at-123", AccountID: "acc-1"}
	result, resp, err := ConsumeResetCreditParsed(context.Background(), account, "", "rid-1")
	if err != nil {
		t.Fatalf("ConsumeResetCreditParsed error: %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("resp = %+v, want 200", resp)
	}
	if result == nil {
		t.Fatal("result is nil, want parsed WhamResetResult")
	}
	if result.WindowsReset != 2 {
		t.Errorf("WindowsReset = %d, want 2", result.WindowsReset)
	}
	if result.Code != "ok" {
		t.Errorf("Code = %q, want ok", result.Code)
	}
	if result.Credit == nil || result.Credit.Status != "redeemed" {
		t.Errorf("Credit = %+v, want status=redeemed", result.Credit)
	}
}

// TestConsumeResetCreditParsed_NonOKReturnsRespForCaller verifies that on a non-2xx response result is nil,
// and resp is kept (body not closed) so the caller can read the error details.
func TestConsumeResetCreditParsed_NonOKReturnsRespForCaller(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()

	oldURL := whamConsumeURLForTest
	whamConsumeURLForTest = server.URL
	defer func() { whamConsumeURLForTest = oldURL }()

	account := &auth.Account{DBID: 1, AccessToken: "at-123", AccountID: "acc-1"}
	result, resp, err := ConsumeResetCreditParsed(context.Background(), account, "", "rid-1")
	if err == nil {
		t.Fatal("expected error on non-2xx")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on non-2xx", result)
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("resp = %+v, want 401 surfaced to caller", resp)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if len(body) == 0 {
		t.Error("expected error body to remain readable for caller")
	}
}

// When Resin is enabled, the wham usage query must go through the Resin reverse proxy and carry X-Resin-Account,
// not connect directly to chatgpt.com (issue #372: account maintenance requests skipped Resin,
// so many accounts shared the local egress IP).
func TestQueryWhamUsageRoutesThroughResin(t *testing.T) {
	var gotPath, gotResinAccount string
	fakeResin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotResinAccount = r.Header.Get("X-Resin-Account")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus"}`))
	}))
	defer fakeResin.Close()

	SetResinConfig(&ResinConfig{BaseURL: fakeResin.URL, PlatformName: "test"})
	t.Cleanup(func() { SetResinConfig(nil) })

	account := &auth.Account{DBID: 7, AccessToken: "at-7"}
	usage, _, err := QueryWhamUsage(context.Background(), account, "")
	if err != nil {
		t.Fatalf("QueryWhamUsage error: %v", err)
	}
	if usage == nil || usage.PlanType != "plus" {
		t.Fatalf("unexpected usage result: %+v", usage)
	}
	if want := "/test/https/chatgpt.com/backend-api/wham/usage"; gotPath != want {
		t.Fatalf("resin path = %q, want %q", gotPath, want)
	}
	if gotResinAccount != "7" {
		t.Fatalf("X-Resin-Account = %q, want %q", gotResinAccount, "7")
	}
}

// The reset-credit list and consume requests use the same request shape as the wham query, so they must also go through Resin.
func TestConsumeResetCreditRoutesThroughResin(t *testing.T) {
	var gotPath, gotResinAccount string
	fakeResin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotResinAccount = r.Header.Get("X-Resin-Account")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fakeResin.Close()

	SetResinConfig(&ResinConfig{BaseURL: fakeResin.URL, PlatformName: "test"})
	t.Cleanup(func() { SetResinConfig(nil) })

	account := &auth.Account{DBID: 9, AccessToken: "at-9"}
	if _, _, err := ConsumeResetCreditParsed(context.Background(), account, "", "req-1"); err != nil {
		t.Fatalf("ConsumeResetCreditParsed error: %v", err)
	}
	if want := "/test/https/chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"; gotPath != want {
		t.Fatalf("resin path = %q, want %q", gotPath, want)
	}
	if gotResinAccount != "9" {
		t.Fatalf("X-Resin-Account = %q, want %q", gotResinAccount, "9")
	}
}

// The reset-credit list endpoint must also go through Resin (so all three endpoints, usage query, consume and list, are covered).
func TestQueryWhamResetCreditsRoutesThroughResin(t *testing.T) {
	var gotPath, gotResinAccount string
	fakeResin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotResinAccount = r.Header.Get("X-Resin-Account")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credits":[]}`))
	}))
	defer fakeResin.Close()

	SetResinConfig(&ResinConfig{BaseURL: fakeResin.URL, PlatformName: "test"})
	t.Cleanup(func() { SetResinConfig(nil) })

	account := &auth.Account{DBID: 13, AccessToken: "at-13"}
	if _, _, err := QueryWhamResetCredits(context.Background(), account, ""); err != nil {
		t.Fatalf("QueryWhamResetCredits error: %v", err)
	}
	if want := "/test/https/chatgpt.com/backend-api/wham/rate-limit-reset-credits"; gotPath != want {
		t.Fatalf("resin path = %q, want %q", gotPath, want)
	}
	if gotResinAccount != "13" {
		t.Fatalf("X-Resin-Account = %q, want %q", gotResinAccount, "13")
	}
}

func TestApplyWhamUsage_PersistsIndependentSparkSnapshot(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "spark-wham", map[string]interface{}{
		"access_token": "at",
		"plan_type":    "pro",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "pro", Status: auth.StatusReady}

	now := time.Now()
	reset5h := now.Add(2 * time.Hour).Unix()
	resetSpark := now.Add(4 * time.Hour).Unix()
	usage := &WhamUsage{PlanType: "pro"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 100, LimitWindowSeconds: 18000, ResetAt: reset5h}
	usage.RateLimit.SecondaryWindow = &WhamUsageWindow{UsedPercent: 20, LimitWindowSeconds: 604800, ResetAt: now.Add(6 * 24 * time.Hour).Unix()}
	usage.AdditionalRateLimits = []WhamAdditionalRateLimit{{
		LimitName: "spark",
		RateLimit: &WhamRateLimitInfo{
			PrimaryWindow: &WhamUsageWindow{UsedPercent: 40, LimitWindowSeconds: 18000, ResetAt: resetSpark},
		},
	}}

	result := ApplyWhamUsage(store, account, usage)
	if !result.Premium5hRateLimited {
		t.Fatal("main 5h=100% should still mark the account rate limited")
	}
	if !account.IsPremium5hRateLimited() {
		t.Fatal("account should stay in premium 5h rate-limited state")
	}
	pct, resetAt, ok := account.GetUsageSnapshotSpark()
	if !ok {
		t.Fatal("spark snapshot missing")
	}
	if pct != 40 {
		t.Fatalf("spark percent = %v, want 40", pct)
	}
	if resetAt.Unix() != resetSpark {
		t.Fatalf("spark reset = %d, want %d", resetAt.Unix(), resetSpark)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("codex_spark_used_percent"); got != "40" {
		t.Fatalf("persisted spark percent = %q, want 40", got)
	}
}

func TestApplyWhamUsage_ClearsStaleSparkWhenUpstreamOmitsWindow(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "spark-clear", map[string]interface{}{
		"access_token":                 "at",
		"plan_type":                    "pro",
		"codex_spark_used_percent":     80,
		"codex_spark_reset_at":         time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		"codex_spark_usage_updated_at": time.Now().Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "pro"}
	account.SetUsageSnapshotSpark(80, time.Now().Add(2*time.Hour))

	usage := &WhamUsage{PlanType: "pro"}
	usage.RateLimit.PrimaryWindow = &WhamUsageWindow{UsedPercent: 10, LimitWindowSeconds: 18000, ResetAt: time.Now().Add(3 * time.Hour).Unix()}
	usage.RateLimit.SecondaryWindow = &WhamUsageWindow{UsedPercent: 5, LimitWindowSeconds: 604800, ResetAt: time.Now().Add(5 * 24 * time.Hour).Unix()}

	ApplyWhamUsage(store, account, usage)
	if _, ok := account.GetUsagePercentSpark(); ok {
		t.Fatal("authoritative WHAM without spark should clear the stale spark snapshot")
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("codex_spark_used_percent"); got != "" {
		t.Fatalf("persisted spark percent = %q, want cleared", got)
	}
}

func TestPickSparkWhamWindow_ParsesCamelCaseAdditionalLimits(t *testing.T) {
	raw := []byte(`{
		"additionalRateLimits": [{
			"limitName": "spark-five-hour",
			"rateLimit": {
				"primaryWindow": {
					"usedPercent": "37.5",
					"limitWindowSeconds": 18000,
					"resetAt": 1779708117
				}
			}
		}]
	}`)
	var usage WhamUsage
	if err := json.Unmarshal(raw, &usage); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	window := pickSparkWhamWindow(usage.additionalRateLimits())
	if window == nil {
		t.Fatal("expected spark window from camelCase additionalRateLimits")
	}
	if window.UsedPercent != 37.5 {
		t.Fatalf("used percent = %v, want 37.5", window.UsedPercent)
	}
	if window.LimitWindowSeconds != 18000 {
		t.Fatalf("window seconds = %d, want 18000", window.LimitWindowSeconds)
	}
}

func TestQueryWhamUsage_ParsesTeamMemberNullBalanceCredits(t *testing.T) {
	body := `{
		"user_id": "user-team",
		"account_id": "acc-team",
		"email": "team@example.com",
		"plan_type": "team",
		"credits": {
			"has_credits": true,
			"unlimited": false,
			"overage_limit_reached": false,
			"balance": null
		},
		"spend_control": {"reached": false, "individual_limit": null},
		"rate_limit_reached_type": null
	}`

	var usage WhamUsage
	if err := json.Unmarshal([]byte(body), &usage); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}

	if usage.Credits == nil {
		t.Fatal("usage.Credits is nil, want non-nil")
	}
	if !usage.Credits.HasCredits {
		t.Fatal("usage.Credits.HasCredits = false, want true")
	}
	if usage.Credits.Balance != nil {
		t.Fatalf("usage.Credits.Balance = %v, want nil", *usage.Credits.Balance)
	}

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "team-acc", map[string]interface{}{"plan_type": "team"}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{
		DBID:                  id,
		AccessToken:           "at",
		PlanType:              "team",
		CreditEnabled:         true,
		CreditSkipUsageWindow: true,
	}

	ApplyWhamUsage(store, account, &usage)

	credits, ok := account.GetCreditBalance()
	if !ok || !credits.HasCredits || credits.Balance != nil || credits.Unlimited || credits.OverageLimitReached ||
		(credits.SpendControlReached != nil && *credits.SpendControlReached) || credits.RateLimitReachedType != "" {
		t.Fatalf("unexpected credit state: %+v, ok=%t", credits, ok)
	}

	// Should bypass 7d / 5h limits because has_credits is true and no hard stop
	account.SetUsagePercent7d(100)
	if !account.SkipsUsageWindowLimits() {
		t.Fatal("SkipsUsageWindowLimits() = false, want true for team account with has_credits=true")
	}
	if !account.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true for team account with has_credits=true")
	}
}

func TestApplyWhamUsage_WorkspaceHardStopPreventsCreditBypass(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "team-acc-stop", map[string]interface{}{"plan_type": "team"}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{
		DBID:                  id,
		AccessToken:           "at",
		PlanType:              "team",
		CreditEnabled:         true,
		CreditSkipUsageWindow: true,
	}

	body := `{
		"user_id": "user-team",
		"account_id": "acc-team",
		"plan_type": "team",
		"credits": {
			"has_credits": true,
			"unlimited": false,
			"overage_limit_reached": false,
			"balance": null
		},
		"spend_control": {"reached": false},
		"rate_limit_reached_type": {"type": "workspace_member_credits_depleted"}
	}`
	var usage WhamUsage
	if err := json.Unmarshal([]byte(body), &usage); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}

	ApplyWhamUsage(store, account, &usage)

	account.SetUsagePercent7d(100)
	if account.SkipsUsageWindowLimits() {
		t.Fatal("SkipsUsageWindowLimits() = true, want false due to workspace_member_credits_depleted")
	}

	// Test spend_control.reached = true
	bodySC := `{
		"user_id": "user-team",
		"account_id": "acc-team",
		"plan_type": "team",
		"credits": {
			"has_credits": true,
			"unlimited": false,
			"overage_limit_reached": false,
			"balance": null
		},
		"spend_control": {"reached": true}
	}`
	var usageSC WhamUsage
	if err := json.Unmarshal([]byte(bodySC), &usageSC); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}

	ApplyWhamUsage(store, account, &usageSC)

	if account.SkipsUsageWindowLimits() {
		t.Fatal("SkipsUsageWindowLimits() = true, want false due to spend_control.reached=true")
	}
}
