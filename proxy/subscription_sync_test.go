package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

const testWorkspaceUUID = "288c5d93-a113-4ed3-b6a9-08b6a4d35417"

func TestQueryChatGPTSubscription_ParsesResponse(t *testing.T) {
	var gotAccountID, gotAuth, gotOrigin, gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.URL.Query().Get("account_id")
		gotAuth = r.Header.Get("Authorization")
		gotOrigin = r.Header.Get("Origin")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "22f6c975-8c24-4323-9e2d-97c4cb61905a",
			"plan_type": "plus",
			"active_start": "2026-01-29T14:54:57Z",
			"active_until": "2026-07-29T14:54:57Z",
			"billing_period": "monthly",
			"will_renew": true
		}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	account := &auth.Account{AccessToken: "test-at", AccountID: testWorkspaceUUID}
	sub, err := QueryChatGPTSubscription(context.Background(), account, "")
	if err != nil {
		t.Fatalf("QueryChatGPTSubscription: %v", err)
	}

	if sub.PlanType != "plus" {
		t.Fatalf("PlanType = %q, want plus", sub.PlanType)
	}
	want := time.Date(2026, 7, 29, 14, 54, 57, 0, time.UTC)
	if !sub.ActiveUntilTime().Equal(want) {
		t.Fatalf("ActiveUntilTime() = %v, want %v", sub.ActiveUntilTime(), want)
	}
	if !sub.WillRenew {
		t.Fatal("WillRenew = false, want true")
	}
	if gotAccountID != testWorkspaceUUID {
		t.Fatalf("query account_id = %q, want %q", gotAccountID, testWorkspaceUUID)
	}
	if gotAuth != "Bearer test-at" {
		t.Fatalf("Authorization = %q, want Bearer test-at", gotAuth)
	}
	if gotOrigin != "" {
		t.Fatalf("Origin = %q, want empty (Codex identity goes first, browser disguise only on 403)", gotOrigin)
	}
	if gotUA != MinimalCodexCLIUserAgentForHeaders() {
		t.Fatalf("User-Agent = %q, want Codex CLI UA", gotUA)
	}
}

// TestQueryChatGPTSubscription_RejectsUserAccountID verifies that an account ID in the user-... form
// (polluted historical data) fails right away without sending a request, because the upstream returns 500 for non-workspace UUIDs.
func TestQueryChatGPTSubscription_RejectsUserAccountID(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	account := &auth.Account{AccessToken: "test-at", AccountID: "user-MpgpQk00uRgvPiLR1iMGAiaX"}
	if _, err := QueryChatGPTSubscription(context.Background(), account, ""); err == nil {
		t.Fatal("QueryChatGPTSubscription should reject user-... account id")
	}
	if requested {
		t.Fatal("no request should be sent for user-... account id")
	}
}

// TestQueryChatGPTSubscription_FallsBackToJWTWorkspaceID verifies that when account_id is user-...
// (polluted history), it falls back to chatgpt_account_id in the AT JWT (the real workspace UUID).
func TestQueryChatGPTSubscription_FallsBackToJWTWorkspaceID(t *testing.T) {
	var gotAccountID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.URL.Query().Get("account_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","active_until":"2026-07-29T14:54:57Z","will_renew":true}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	payload, _ := json.Marshal(map[string]interface{}{
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": testWorkspaceUUID,
			"user_id":            "user-MpgpQk00uRgvPiLR1iMGAiaX",
			"chatgpt_plan_type":  "plus",
		},
	})
	jwt := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".fake_signature"

	account := &auth.Account{AccessToken: jwt, AccountID: "user-MpgpQk00uRgvPiLR1iMGAiaX"}
	sub, err := QueryChatGPTSubscription(context.Background(), account, "")
	if err != nil {
		t.Fatalf("QueryChatGPTSubscription: %v", err)
	}
	if sub.PlanType != "plus" {
		t.Fatalf("PlanType = %q, want plus", sub.PlanType)
	}
	if gotAccountID != testWorkspaceUUID {
		t.Fatalf("query account_id = %q, want JWT workspace uuid %q", gotAccountID, testWorkspaceUUID)
	}
}

// TestMaybeSyncSubscriptionExpiry_SyncsAndThrottles verifies that for a paid plan with a missing expiry time,
// active_until is synced from the web endpoint into memory and the DB; the same account does not repeat requests within the throttle interval.
func TestMaybeSyncSubscriptionExpiry_SyncsAndThrottles(t *testing.T) {
	ctx := context.Background()
	activeUntil := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","active_until":"` + activeUntil.Format(time.RFC3339) + `","will_renew":true}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	id, err := db.InsertAccountWithCredentials(ctx, "subscription-sync", map[string]interface{}{"plan_type": "plus"}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID}

	if !MaybeSyncSubscriptionExpiry(ctx, store, account, "") {
		t.Fatal("MaybeSyncSubscriptionExpiry should update expiry on first call")
	}
	if !account.SubscriptionExpiresAt.Equal(activeUntil) {
		t.Fatalf("SubscriptionExpiresAt = %v, want %v", account.SubscriptionExpiresAt, activeUntil)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("subscription_expires_at"); got != activeUntil.Format(time.RFC3339) {
		t.Fatalf("persisted subscription_expires_at = %q, want %q", got, activeUntil.Format(time.RFC3339))
	}

	// With a far-future expiry time and inside the throttle window: the second call should not send another request.
	if MaybeSyncSubscriptionExpiry(ctx, store, account, "") {
		t.Fatal("second call should be a no-op")
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("upstream requests = %d, want 1 (throttled)", n)
	}
}

// TestMaybeSyncSubscriptionExpiry_SkipsPastActiveUntil verifies that a past active_until returned by the upstream
// (in grace period / degraded) is not written, to avoid conflicting with the stale-value cleanup logic.
func TestMaybeSyncSubscriptionExpiry_SkipsPastActiveUntil(t *testing.T) {
	ctx := context.Background()
	past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","active_until":"` + past.Format(time.RFC3339) + `","will_renew":false}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{DBID: 1, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID}

	if MaybeSyncSubscriptionExpiry(ctx, store, account, "") {
		t.Fatal("past active_until should not be applied")
	}
	if !account.SubscriptionExpiresAt.IsZero() {
		t.Fatalf("SubscriptionExpiresAt = %v, want zero", account.SubscriptionExpiresAt)
	}
}

// When Resin is enabled, the subscription expiry query must also go through the reverse proxy (issue #372); the fingerprint is handled on the Resin side.
func TestQueryChatGPTSubscriptionRoutesThroughResin(t *testing.T) {
	var gotPath, gotResinAccount, gotAccountIDQuery string
	fakeResin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotResinAccount = r.Header.Get("X-Resin-Account")
		gotAccountIDQuery = r.URL.Query().Get("account_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fakeResin.Close()

	SetResinConfig(&ResinConfig{BaseURL: fakeResin.URL, PlatformName: "test"})
	t.Cleanup(func() { SetResinConfig(nil) })

	account := &auth.Account{DBID: 11, AccessToken: "at-11", AccountID: testWorkspaceUUID}
	if _, err := QueryChatGPTSubscription(context.Background(), account, ""); err != nil {
		t.Fatalf("QueryChatGPTSubscription error: %v", err)
	}
	if want := "/test/https/chatgpt.com/backend-api/subscriptions"; gotPath != want {
		t.Fatalf("resin path = %q, want %q", gotPath, want)
	}
	if gotResinAccount != "11" {
		t.Fatalf("X-Resin-Account = %q, want %q", gotResinAccount, "11")
	}
	if gotAccountIDQuery != testWorkspaceUUID {
		t.Fatalf("account_id query = %q, want workspace uuid", gotAccountIDQuery)
	}
}

func newSubscriptionSyncTestStore(t *testing.T, plan string) (*auth.Store, *database.DB, int64) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	id, err := db.InsertAccountWithCredentials(ctx, "subscription-sync", map[string]interface{}{"plan_type": plan}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	return store, db, id
}

// Successful sync: writes the authoritative expiry time + confirmed/provider_api + auto-renewal + grace period;
// when the validity period is extended, records the renewal detection time; the first time it is obtained does not count as a renewal. The metadata is stored and can be read back.
func TestSyncSubscriptionExpiry_RecordsMetaOnSuccess(t *testing.T) {
	ctx := context.Background()
	first := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	renewed := first.Add(30 * 24 * time.Hour)
	grace := renewed.Add(7 * 24 * time.Hour)
	var body atomic.Value
	body.Store(`{"plan_type":"plus","active_until":"` + first.Format(time.RFC3339) + `","will_renew":true,"grace_period_end_timestamp":null}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	store, db, id := newSubscriptionSyncTestStore(t, "plus")
	account := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID}

	res := SyncSubscriptionExpiry(ctx, store, account, "")
	if res.Outcome != SubscriptionSyncUpdated || !res.ExpiresAt.Equal(first) {
		t.Fatalf("first sync = %+v, want updated %v", res, first)
	}
	meta := account.SubscriptionMetaSnapshot()
	if meta.SyncState != auth.SubscriptionSyncConfirmed || meta.Source != auth.SubscriptionSourceProviderAPI || meta.WillRenew != auth.SubscriptionAutoRenewEnabled {
		t.Fatalf("meta after first sync = %+v", meta)
	}
	if !meta.RenewalDetectedAt.IsZero() {
		t.Fatalf("first expiry must not count as renewal: %+v", meta)
	}
	if meta.LastKnownStatus != auth.SubscriptionStatusActive || meta.CheckedAt.IsZero() || meta.Error != "" {
		t.Fatalf("meta after first sync = %+v", meta)
	}

	// Renewal: validity period extended + a grace period field appears (unix seconds form).
	body.Store(`{"plan_type":"plus","active_until":"` + renewed.Format(time.RFC3339) + `","will_renew":false,"grace_period_end_timestamp":` + fmtInt(grace.Unix()) + `}`)
	res = SyncSubscriptionExpiry(ctx, store, account, "")
	if res.Outcome != SubscriptionSyncUpdated || !res.ExpiresAt.Equal(renewed) {
		t.Fatalf("renew sync = %+v, want updated %v", res, renewed)
	}
	meta = account.SubscriptionMetaSnapshot()
	if meta.RenewalDetectedAt.IsZero() || meta.WillRenew != auth.SubscriptionAutoRenewDisabled || !meta.GraceUntil.Equal(grace) {
		t.Fatalf("meta after renew = %+v", meta)
	}

	// Unchanged: the result is unchanged.
	res = SyncSubscriptionExpiry(ctx, store, account, "")
	if res.Outcome != SubscriptionSyncUnchanged {
		t.Fatalf("third sync = %+v, want unchanged", res)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	persisted := auth.SubscriptionMetaFromCredentials(row.GetCredential)
	if persisted.SyncState != auth.SubscriptionSyncConfirmed || persisted.Source != auth.SubscriptionSourceProviderAPI || !persisted.GraceUntil.Equal(grace) {
		t.Fatalf("persisted meta = %+v", persisted)
	}
	if row.GetCredential("subscription_expires_at") != renewed.Format(time.RFC3339) {
		t.Fatalf("persisted expiry = %q", row.GetCredential("subscription_expires_at"))
	}
	view := account.SubscriptionStatusView(time.Now())
	if view == nil || view.BusinessStatus != auth.SubscriptionStatusActive || view.AutoRenew != auth.SubscriptionAutoRenewDisabled {
		t.Fatalf("view = %+v", view)
	}
}

func fmtInt(v int64) string { return strconv.FormatInt(v, 10) }

// 404 "No subscription found" (k12/edu): not counted as a failure; marked unsupported; the original expiry time is kept.
func TestSyncSubscriptionExpiry_NoSubscriptionIsUnsupported(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"No subscription found"}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	store, _, id := newSubscriptionSyncTestStore(t, "k12")
	jwtExpiry := time.Now().Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	account := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "k12", AccountID: testWorkspaceUUID, SubscriptionExpiresAt: jwtExpiry}

	res := SyncSubscriptionExpiry(ctx, store, account, "")
	if res.Outcome != SubscriptionSyncNoSubscription || res.Err != nil {
		t.Fatalf("result = %+v, want no_subscription", res)
	}
	meta := account.SubscriptionMetaSnapshot()
	if meta.SyncState != auth.SubscriptionSyncUnsupported || meta.WillRenew != auth.SubscriptionAutoRenewUnsupported || meta.Error != "" {
		t.Fatalf("meta = %+v", meta)
	}
	if !account.GetSubscriptionExpiresAt().Equal(jwtExpiry) {
		t.Fatal("JWT expiry must be kept when provider has no record")
	}
	view := account.SubscriptionStatusView(time.Now())
	if view == nil || view.AutoRenew != auth.SubscriptionAutoRenewUnsupported || view.BusinessStatus != auth.SubscriptionStatusActive {
		t.Fatalf("view = %+v", view)
	}
}

// Query failure: when there is a known state, mark it failed and keep the expiry time; when there has never been a state, mark it unknown.
func TestSyncSubscriptionExpiry_FailureKeepsLastKnown(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<html>blocked</html>`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	store, _, id := newSubscriptionSyncTestStore(t, "plus")
	known := time.Now().Add(-2 * 24 * time.Hour).UTC().Truncate(time.Second)
	account := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID, SubscriptionExpiresAt: known}

	res := SyncSubscriptionExpiry(ctx, store, account, "")
	if res.Outcome != SubscriptionSyncFailed || res.Err == nil {
		t.Fatalf("result = %+v, want failed", res)
	}
	meta := account.SubscriptionMetaSnapshot()
	if meta.SyncState != auth.SubscriptionSyncFailed || meta.Error == "" {
		t.Fatalf("meta = %+v", meta)
	}
	if !account.GetSubscriptionExpiresAt().Equal(known) {
		t.Fatal("failure must keep last known expiry")
	}
	view := account.SubscriptionStatusView(time.Now())
	if view == nil || view.BusinessStatus != auth.SubscriptionStatusExpired || view.SyncState != auth.SubscriptionSyncFailed || view.Error == "" {
		t.Fatalf("view = %+v", view)
	}

	blank := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID}
	SyncSubscriptionExpiry(ctx, store, blank, "")
	if got := blank.SubscriptionMetaSnapshot().SyncState; got != auth.SubscriptionSyncUnknown {
		t.Fatalf("blank account sync state = %s, want unknown", got)
	}

	// A non-paid plan sends no request.
	free := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "free", AccountID: testWorkspaceUUID}
	if res := SyncSubscriptionExpiry(ctx, store, free, ""); res.Outcome != SubscriptionSyncUnsupported {
		t.Fatalf("free plan = %+v, want unsupported", res)
	}
}

// A past active_until within the grace period can be written (stale-value cleanup skips the grace period).
func TestSyncSubscriptionExpiry_AppliesPastExpiryInsideGrace(t *testing.T) {
	ctx := context.Background()
	past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	grace := time.Now().Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","active_until":"` + past.Format(time.RFC3339) + `","will_renew":true,"is_delinquent":true,"grace_period_end_timestamp":"` + grace.Format(time.RFC3339) + `"}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	store, _, id := newSubscriptionSyncTestStore(t, "plus")
	account := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID}
	store.AddAccount(account)

	res := SyncSubscriptionExpiry(ctx, store, account, "")
	if res.Outcome != SubscriptionSyncUpdated || !res.ExpiresAt.Equal(past) {
		t.Fatalf("result = %+v, want updated with past expiry", res)
	}
	view := account.SubscriptionStatusView(time.Now())
	if view == nil || view.BusinessStatus != auth.SubscriptionStatusGracePeriod || view.GraceUntil == "" {
		t.Fatalf("view = %+v, want grace_period", view)
	}
	if store.ClearStaleSubscriptionExpiresAt(account) {
		t.Fatal("grace period expiry must survive stale clearing")
	}
}

// The stored failure reason must be readable: an HTML block page keeps only the status code plus a suspected-blocking hint.
func TestSubscriptionErrorForStorage(t *testing.T) {
	html := &SubscriptionHTTPError{Status: 403, Body: "<html>\n  <head><meta name=\"viewport\"></head><body>blocked</body></html>"}
	if got := subscriptionErrorForStorage(html); got != "HTTP 403: "+SubscriptionErrorAntiBotChallenge {
		t.Fatalf("html error = %q", got)
	}
	plain := &SubscriptionHTTPError{Status: 500, Body: `{"detail":"boom"}`}
	if got := subscriptionErrorForStorage(plain); got != `HTTP 500: {"detail":"boom"}` {
		t.Fatalf("plain error = %q", got)
	}
	if got := subscriptionErrorForStorage(context.DeadlineExceeded); got != context.DeadlineExceeded.Error() {
		t.Fatalf("generic error = %q", got)
	}
}

// On 403 with the Codex identity, fall back to the browser disguise; it only counts as a failure when both identities get 403; 401 does not retry with another identity.
func TestQueryChatGPTSubscription_FallsBackToBrowserOn403(t *testing.T) {
	ctx := context.Background()
	activeUntil := time.Now().Add(15 * 24 * time.Hour).UTC().Truncate(time.Second)
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Originator") != "" {
			seen = append(seen, "codex")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html><head></head><body>Enable JavaScript and cookies to continue</body></html>"))
			return
		}
		seen = append(seen, "browser:"+r.Header.Get("Origin"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","active_until":"` + activeUntil.Format(time.RFC3339) + `","will_renew":true}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	account := &auth.Account{DBID: 7, AccessToken: "test-at", PlanType: "pro", AccountID: testWorkspaceUUID}
	sub, err := QueryChatGPTSubscription(ctx, account, "")
	if err != nil || !sub.ActiveUntilTime().Equal(activeUntil) {
		t.Fatalf("fallback query = %+v, %v", sub, err)
	}
	if len(seen) != 2 || seen[0] != "codex" || seen[1] != "browser:https://chatgpt.com" {
		t.Fatalf("attempt order = %v, want codex then browser", seen)
	}

	// Both identities get 403: report a 403 error, and the stored text is the human-verification hint.
	seen = nil
	both := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><head></head><body>Enable JavaScript and cookies to continue</body></html>"))
	}))
	defer both.Close()
	defer SetSubscriptionsURLForTest(both.URL)()
	if _, err := QueryChatGPTSubscription(ctx, account, ""); err == nil || subscriptionErrorForStorage(err) != "HTTP 403: "+SubscriptionErrorAntiBotChallenge {
		t.Fatalf("both-403 err = %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("both-403 attempts = %d, want 2", len(seen))
	}

	// 401: not worth switching identity; only one request is sent.
	seen = nil
	unauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Could not parse your authentication token."}`))
	}))
	defer unauth.Close()
	defer SetSubscriptionsURLForTest(unauth.URL)()
	if _, err := QueryChatGPTSubscription(ctx, account, ""); err == nil {
		t.Fatal("401 should surface as error")
	}
	if len(seen) != 1 {
		t.Fatalf("401 attempts = %d, want 1", len(seen))
	}
}
