package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestNormalizeContactEmail(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"user@example.com", "user@example.com", true},
		{"  User@Example.com  ", "User@Example.com", true},
		{"", "", false},
		{"not-an-email", "", false},
		{"a@b", "", false},
		{"a@@b.com", "", false},
		{"a b@x.com", "", false},
	}
	for _, tc := range cases {
		got, ok := normalizeContactEmail(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("normalizeContactEmail(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSelfServiceRateLimiter(t *testing.T) {
	l := &selfServiceRateLimiter{hits: make(map[string][]time.Time)}
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < selfServiceRateLimit; i++ {
		if !l.allow("1.2.3.4", base) {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	if l.allow("1.2.3.4", base) {
		t.Fatal("requests over the limit should be rate limited")
	}
	// Another IP is not affected
	if !l.allow("9.9.9.9", base) {
		t.Fatal("other IPs should be allowed")
	}
	// Allowed again outside the window
	if !l.allow("1.2.3.4", base.Add(selfServiceRateWin+time.Second)) {
		t.Fatal("requests should be allowed again after the window passes")
	}
}

func newSelfServiceHandler(t *testing.T, enabled bool) (*Handler, *database.DB) {
	t.Helper()
	db := newTestAdminDB(t)
	tc := cache.NewMemory(1)
	t.Cleanup(func() { tc.Close() })
	store := auth.NewStore(db, tc, nil)
	handler := NewHandler(store, db, tc, nil, "admin-secret")

	ctx := context.Background()
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if settings == nil {
		settings = &database.SystemSettings{}
	}
	settings.PublicAccountPortalPageEnabled = enabled
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	return handler, db
}

func TestAccountPortalDisabledReturns404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newSelfServiceHandler(t, false)
	router := gin.New()
	handler.RegisterRoutes(router)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/account-portal/generate-auth-url",
		strings.NewReader(`{"contact_email":"user@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("portal disabled should return 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAccountPortalGenerateAuthURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newSelfServiceHandler(t, true)
	router := gin.New()
	handler.RegisterRoutes(router)

	// Invalid email → 400
	badRec := httptest.NewRecorder()
	badReq := httptest.NewRequest(http.MethodPost, "/api/account-portal/generate-auth-url",
		strings.NewReader(`{"contact_email":"nope"}`))
	badReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("invalid email should return 400, got %d", badRec.Code)
	}

	// Valid email → 200 and returns auth_url + session_id
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/account-portal/generate-auth-url",
		strings.NewReader(`{"contact_email":"user@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid email should return 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "auth_url") || !strings.Contains(body, "session_id") {
		t.Fatalf("response should contain auth_url and session_id: %s", body)
	}
	if !strings.Contains(body, "auth.openai.com/oauth/authorize") {
		t.Fatalf("auth_url should point to the OpenAI authorization endpoint: %s", body)
	}
}

func TestUpsertSelfServiceAccountPendingAndNotScheduled(t *testing.T) {
	handler, db := newSelfServiceHandler(t, true)
	ctx := context.Background()

	seed := tokenCredentialSeed{
		refreshToken: "rt-abc",
		accessToken:  "at-abc",
		email:        "provider@example.com",
		accountID:    "acct-1",
		workspaceID:  "workspace-1",
	}
	id, err := handler.upsertSelfServiceAccount(ctx, "provider@example.com", "", seed, "contact@x.com")
	if err != nil {
		t.Fatalf("upsertSelfServiceAccount: %v", err)
	}
	if id <= 0 {
		t.Fatalf("id = %d, want > 0", id)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if row.Enabled {
		t.Fatal("self-service account should be disabled (pending review)")
	}
	if !strings.Contains(row.Note, "contact@x.com") {
		t.Fatalf("note should contain the contact email, got %q", row.Note)
	}
	hasTag := false
	for _, tag := range row.Tags {
		if tag == selfServiceTag {
			hasTag = true
		}
	}
	if !hasTag {
		t.Fatalf("should carry the self-service tag, tags=%v", row.Tags)
	}
	// Not in the scheduling pool
	if handler.store.FindByID(id) != nil {
		t.Fatal("pending review accounts should not enter the runtime scheduling pool")
	}

	// Duplicate submission → errDuplicateOAuthIdentity
	if _, err := handler.upsertSelfServiceAccount(ctx, "provider@example.com", "", seed, "contact2@x.com"); err != errDuplicateOAuthIdentity {
		t.Fatalf("duplicate submission should return errDuplicateOAuthIdentity, got %v", err)
	}
}

func TestSelfServiceFailedInsertAllowsRetry(t *testing.T) {
	handler, db := newSelfServiceHandler(t, true)
	ctx := context.Background()
	seed := tokenCredentialSeed{
		refreshToken: "rt-rb",
		accessToken:  "at-rb",
		email:        "rollback@example.com",
		accountID:    "acct-rb",
		workspaceID:  "workspace-rb",
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if id, err := handler.upsertSelfServiceAccount(canceled, "rollback@example.com", "", seed, "c@x.com"); err == nil || id != 0 {
		t.Fatalf("canceled submission = (%d, %v), want no inserted account", id, err)
	}
	if id, err := db.FindActiveAccountByOAuthIdentity(ctx, seed.email, seed.workspaceID); !errors.Is(err, sql.ErrNoRows) || id != 0 {
		t.Fatalf("failed submission left a duplicate identity: id=%d err=%v", id, err)
	}

	if _, err := handler.upsertSelfServiceAccount(ctx, "rollback@example.com", "", seed, "c@x.com"); err != nil {
		t.Fatalf("after rollback the same identity should be submittable again, got %v", err)
	}
}

func TestApproveSelfServiceAccountLoadsIntoPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db := newSelfServiceHandler(t, true)
	router := gin.New()
	handler.RegisterRoutes(router)
	ctx := context.Background()

	seed := tokenCredentialSeed{refreshToken: "rt-x", accessToken: "at-x", email: "p2@example.com", accountID: "acct-2", workspaceID: "workspace-2"}
	id, err := handler.upsertSelfServiceAccount(ctx, "p2@example.com", "", seed, "c@x.com")
	if err != nil {
		t.Fatalf("upsertSelfServiceAccount: %v", err)
	}
	if handler.store.FindByID(id) != nil {
		t.Fatal("account should not be in the pool before approval")
	}

	// Admin approval (enable=true) should load the account into the scheduling pool
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/"+strconv.FormatInt(id, 10)+"/enable",
		strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Key", "admin-secret")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approval should return 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if handler.store.FindByID(id) == nil {
		t.Fatal("account should enter the runtime scheduling pool after approval")
	}
	row, _ := db.GetAccountByID(ctx, id)
	if !row.Enabled {
		t.Fatal("enabled should be true after approval")
	}
}

func TestUpsertSelfServiceInvalidatesAccountListSnapshot(t *testing.T) {
	handler, _ := newSelfServiceHandler(t, true)
	ctx := context.Background()
	if _, err := handler.rebuildAccountListSnapshot(ctx, database.UpstreamChannelCodex); err != nil {
		t.Fatalf("warm snapshot: %v", err)
	}

	seed := tokenCredentialSeed{
		refreshToken: "rt-snap",
		accessToken:  "at-snap",
		email:        "snap@example.com",
		accountID:    "acct-snap",
		workspaceID:  "workspace-snap",
	}
	id, err := handler.upsertSelfServiceAccount(ctx, "snap@example.com", "", seed, "review@x.com")
	if err != nil {
		t.Fatalf("upsertSelfServiceAccount: %v", err)
	}

	snapshot, err := handler.getAccountListSnapshot(ctx, database.UpstreamChannelCodex)
	if err != nil {
		t.Fatalf("getAccountListSnapshot: %v", err)
	}
	found := false
	for _, item := range snapshot.Items {
		if item.ID == id {
			found = true
			if item.Enabled {
				t.Fatal("pending review account should be disabled in the snapshot")
			}
			if !containsString(item.Tags, selfServiceTag) {
				t.Fatalf("snapshot tags=%v, want self-service", item.Tags)
			}
			break
		}
	}
	if !found {
		t.Fatal("the list snapshot should include the pending review account right after submission")
	}
	if snapshot.Summary.SelfServicePending != 1 {
		t.Fatalf("SelfServicePending=%d, want 1", snapshot.Summary.SelfServicePending)
	}
}
