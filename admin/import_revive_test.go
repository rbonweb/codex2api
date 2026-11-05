package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// issue #618: after re-authorisation the RT changes; a bare RT import is merged into the old account by identity after the refresh, but the old account
// kept its earlier 401 unauthorized / error state. The user saw the new account vanish while the old account
// stayed flagged "unauthorized". Merging must clear the old account's error state (in the DB and in the cross-instance cooldown cache).
func TestMergeRefreshedDuplicateClearsStaleUnauthorizedState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()
	store := auth.NewStore(db, tokenCache, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{
		db:         db,
		store:      store,
		probeUsage: func(context.Context, *auth.Account) error { return nil },
	}
	ctx := context.Background()

	oldID, err := db.InsertAccountWithCredentials(ctx, "stale", map[string]interface{}{
		"refresh_token": "rt-dead",
		"access_token":  "at-stale",
		"email":         "revive@example.com",
		"workspace_id":  "ws-revive",
	}, "")
	if err != nil {
		t.Fatalf("Insert old: %v", err)
	}
	if err := store.LoadAccountByID(ctx, oldID); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	old := store.FindByID(oldID)
	if old == nil {
		t.Fatal("old runtime account missing")
	}
	store.MarkCooldownWithError(old, time.Hour, "unauthorized", "401 unauthorized")
	if got := old.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("precondition: runtime status = %q, want unauthorized", got)
	}
	preRow, err := db.GetAccountByID(ctx, oldID)
	if err != nil {
		t.Fatalf("GetAccountByID pre: %v", err)
	}
	if !strings.EqualFold(preRow.CooldownReason, "unauthorized") {
		t.Fatalf("precondition: db cooldown_reason = %q, want unauthorized", preRow.CooldownReason)
	}

	// A new RT imported after re-authorisation; after the refresh its identity matches the old account
	newID, err := db.InsertAccountWithCredentials(ctx, "reauth", map[string]interface{}{
		"refresh_token": "rt-fresh",
		"access_token":  "at-fresh",
		"email":         "revive@example.com",
		"workspace_id":  "ws-revive",
	}, "")
	if err != nil {
		t.Fatalf("Insert new: %v", err)
	}
	store.AddAccount(&auth.Account{DBID: newID, RefreshToken: "rt-fresh"})

	if merged := handler.mergeRefreshedDuplicateIntoExisting(newID, "test"); !merged {
		t.Fatal("expected re-authorized RT to merge into the stale account")
	}

	row, err := db.GetAccountByID(ctx, oldID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("refresh_token"); got != "rt-fresh" {
		t.Fatalf("refresh_token = %q, want rt-fresh", got)
	}
	if strings.EqualFold(row.Status, "error") {
		t.Fatalf("db status = %q, want cleared", row.Status)
	}
	if row.CooldownReason != "" || row.CooldownUntil.Valid {
		t.Fatalf("db cooldown = (%q, valid=%v), want cleared", row.CooldownReason, row.CooldownUntil.Valid)
	}
	survivor := store.FindByID(oldID)
	if survivor == nil {
		t.Fatal("survivor missing from runtime store")
	}
	if got := survivor.RuntimeStatus(); got == "unauthorized" || got == "error" {
		t.Fatalf("survivor runtime status = %q, want cleared", got)
	}
	survivor.Mu().RLock()
	tier := survivor.HealthTier
	survivor.Mu().RUnlock()
	if tier == auth.HealthTierBanned {
		t.Fatalf("survivor HealthTier = %q, want not banned", tier)
	}
	if store.FindByID(newID) != nil {
		t.Fatal("merged duplicate should have left the runtime store")
	}
}

// A rate-limit cooldown is not a "stale error state", so merging must not clear it along the way.
func TestMergeRefreshedDuplicateKeepsRateLimitCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{
		db:         db,
		store:      store,
		probeUsage: func(context.Context, *auth.Account) error { return nil },
	}
	ctx := context.Background()

	oldID, err := db.InsertAccountWithCredentials(ctx, "limited", map[string]interface{}{
		"refresh_token": "rt-old",
		"access_token":  "at-old",
		"email":         "limited@example.com",
		"workspace_id":  "ws-limited",
	}, "")
	if err != nil {
		t.Fatalf("Insert old: %v", err)
	}
	if err := store.LoadAccountByID(ctx, oldID); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	store.MarkCooldown(store.FindByID(oldID), time.Hour, "rate_limited")

	newID, err := db.InsertAccountWithCredentials(ctx, "reauth", map[string]interface{}{
		"refresh_token": "rt-new",
		"access_token":  "at-new",
		"email":         "limited@example.com",
		"workspace_id":  "ws-limited",
	}, "")
	if err != nil {
		t.Fatalf("Insert new: %v", err)
	}
	store.AddAccount(&auth.Account{DBID: newID, RefreshToken: "rt-new"})

	if merged := handler.mergeRefreshedDuplicateIntoExisting(newID, "test"); !merged {
		t.Fatal("expected merge")
	}
	row, err := db.GetAccountByID(ctx, oldID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if !strings.EqualFold(row.CooldownReason, "rate_limited") || !row.CooldownUntil.Valid {
		t.Fatalf("rate-limit cooldown = (%q, valid=%v), want preserved", row.CooldownReason, row.CooldownUntil.Valid)
	}
}

func newReviveTestHandler(t *testing.T) (*Handler, *database.DB, *auth.Store) {
	t.Helper()
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{
		db:    db,
		store: store,
		refreshAccount: func(_ context.Context, id int64) error {
			acc := store.FindByID(id)
			if acc == nil {
				return fmt.Errorf("account %d not found", id)
			}
			acc.Mu().Lock()
			acc.AccessToken = fmt.Sprintf("at-%d", id)
			acc.Mu().Unlock()
			return nil
		},
		probeUsage: func(context.Context, *auth.Account) error { return nil },
	}
	return handler, db, store
}

func waitForAccountStatusCleared(t *testing.T, db *database.DB, id int64) *database.AccountRow {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		row, err := db.GetAccountByID(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAccountByID: %v", err)
		}
		if !accountErrorStateNeedsReset(row) {
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("account %d still in error state: status=%q cooldown_reason=%q", id, row.Status, row.CooldownReason)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// issue #618: a duplicate import with byte-identical credentials, where the old account is sitting in the error state, should not only count once
// as a duplicate and stop there: the user wants it brought back. This is the bare RT file import path.
func TestImportAccountsCommonRevivesErroredDuplicate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler, db, store := newReviveTestHandler(t)
	ctx := context.Background()

	existingID, err := db.InsertAccountWithCredentials(ctx, "dead", map[string]interface{}{
		"refresh_token": "rt-same",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	if err := store.LoadAccountByID(ctx, existingID); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	store.MarkError(store.FindByID(existingID), "refresh failed")

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	handler.importAccountsCommon(ginCtx, []importToken{{refreshToken: "rt-same"}}, importSettings{})

	payload := recorder.Body.String()
	if !strings.Contains(payload, `"type":"complete"`) {
		t.Fatalf("payload = %q, want complete event", payload)
	}
	complete := lastImportEvent(t, payload)
	if complete.Updated != 1 || complete.Duplicate != 0 || complete.Success != 0 || complete.Failed != 0 {
		t.Fatalf("complete = %+v, want updated=1 duplicate=0 success=0 failed=0", complete)
	}

	row := waitForAccountStatusCleared(t, db, existingID)
	if row.ErrorMessage != "" {
		t.Fatalf("error_message = %q, want cleared", row.ErrorMessage)
	}
	rows, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != existingID {
		t.Fatalf("active rows = %d, want only existing id %d", len(rows), existingID)
	}
	acc := store.FindByID(existingID)
	if acc == nil {
		t.Fatal("revived account missing from runtime store")
	}
	if got := acc.RuntimeStatus(); got == "error" || got == "unauthorized" {
		t.Fatalf("runtime status = %q, want cleared", got)
	}
}

// A JSON import with the same identity and the same credentials: when the old account is in 401 cooldown it is also revived and counted as an update.
func TestImportAccountsCommonRevivesUnauthorizedOAuthIdentityDuplicate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler, db, store := newReviveTestHandler(t)
	ctx := context.Background()

	existingID, err := db.InsertAccountWithCredentials(ctx, "existing", map[string]interface{}{
		"refresh_token": "rt-same",
		"access_token":  "at-same",
		"email":         "same@example.com",
		"account_id":    "acc-same",
		"workspace_id":  "acc-same",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	if err := store.LoadAccountByID(ctx, existingID); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	store.MarkCooldownWithError(store.FindByID(existingID), time.Hour, "unauthorized", "401 unauthorized")

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	handler.importAccountsCommon(ginCtx, []importToken{{
		refreshToken: "rt-same",
		accessToken:  "at-same",
		idToken:      makeOAuthTestIDToken("Same@Example.com", "acc-same", ""),
		email:        "Same@Example.com",
		accountID:    "acc-same",
	}}, importSettings{})

	complete := lastImportEvent(t, recorder.Body.String())
	if complete.Updated != 1 || complete.Duplicate != 0 || complete.Success != 0 {
		t.Fatalf("complete = %+v, want updated=1 duplicate=0 success=0", complete)
	}
	row := waitForAccountStatusCleared(t, db, existingID)
	if row.CooldownReason != "" {
		t.Fatalf("cooldown_reason = %q, want cleared", row.CooldownReason)
	}
	acc := store.FindByID(existingID)
	if acc == nil {
		t.Fatal("revived account missing from runtime store")
	}
	acc.Mu().RLock()
	tier := acc.HealthTier
	acc.Mu().RUnlock()
	if tier == auth.HealthTierBanned {
		t.Fatalf("HealthTier = %q, want not banned", tier)
	}
}

// Manually adding the same RT in bulk: when the old account is in the error state it is revived and counted as updated; in the normal state it is still a duplicate.
func TestAddAccountRevivesErroredDuplicate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler, db, store := newReviveTestHandler(t)
	ctx := context.Background()

	existingID, err := db.InsertAccountWithCredentials(ctx, "existing", map[string]interface{}{
		"refresh_token": "rt-existing",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	if err := store.LoadAccountByID(ctx, existingID); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}

	doAdd := func(body string) map[string]interface{} {
		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)
		ginCtx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts", strings.NewReader(body))
		ginCtx.Request.Header.Set("Content-Type", "application/json")
		handler.AddAccount(ginCtx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
		}
		var resp map[string]interface{}
		if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	// Normal state: still skipped as a duplicate, with no side effects
	resp := doAdd(`{"refresh_token":"rt-existing"}`)
	if resp["duplicate"] != float64(1) || resp["updated"] != float64(0) {
		t.Fatalf("healthy duplicate resp = %v, want duplicate=1 updated=0", resp)
	}

	store.MarkError(store.FindByID(existingID), "refresh failed")

	resp = doAdd(`{"refresh_token":"rt-existing"}`)
	if resp["updated"] != float64(1) || resp["duplicate"] != float64(0) || resp["success"] != float64(0) {
		t.Fatalf("errored duplicate resp = %v, want updated=1 duplicate=0 success=0", resp)
	}
	waitForAccountStatusCleared(t, db, existingID)
	if rows, _ := db.ListActive(ctx); len(rows) != 1 {
		t.Fatalf("active rows = %d, want 1 (revived in place, no new row)", len(rows))
	}
}

func lastImportEvent(t *testing.T, payload string) importEvent {
	t.Helper()
	var last importEvent
	found := false
	for _, line := range strings.Split(payload, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event importEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			t.Fatalf("decode SSE event %q: %v", line, err)
		}
		if event.Type == "complete" {
			last = event
			found = true
		}
	}
	if !found {
		t.Fatalf("no complete event in payload: %q", payload)
	}
	return last
}
