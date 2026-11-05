package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func patchAccountScheduler(t *testing.T, handler *Handler, accountID int64, body string) *httptest.ResponseRecorder {
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

func accountFingerprintCredential(t *testing.T, db *database.DB, accountID int64) string {
	t.Helper()
	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	return row.GetCredential(auth.CodexFingerprintModeCredentialKey)
}

func TestUpdateAccountSchedulerPersistsCodexFingerprintMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	handler := &Handler{db: db, store: store}

	// When not configured, it defaults to off, and outbound behavior matches the behavior before the upgrade.
	if got := accountFingerprintCredential(t, db, accountID); got != "" {
		t.Fatalf("initial credential = %q, want empty", got)
	}

	recorder := patchAccountScheduler(t, handler, accountID, `{"codex_fingerprint_mode":"session"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := accountFingerprintCredential(t, db, accountID); got != auth.CodexFingerprintModeSession {
		t.Fatalf("credential = %q, want %q", got, auth.CodexFingerprintModeSession)
	}

	// null means reset to the default level, off.
	recorder = patchAccountScheduler(t, handler, accountID, `{"codex_fingerprint_mode":null}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := accountFingerprintCredential(t, db, accountID); got != auth.CodexFingerprintModeOff {
		t.Fatalf("credential after reset = %q, want %q", got, auth.CodexFingerprintModeOff)
	}
}

func TestUpdateAccountSchedulerRejectsInvalidCodexFingerprintMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	recorder := patchAccountScheduler(t, handler, accountID, `{"codex_fingerprint_mode":"converge"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if got := accountFingerprintCredential(t, db, accountID); got != "" {
		t.Fatalf("credential = %q, want the rejected update to leave it untouched", got)
	}
}

// TestUpdateAccountSchedulerSyncsRuntimeCodexFingerprintMode verifies that a change takes effect immediately for runtime accounts
// without waiting for the next full reload.
func TestUpdateAccountSchedulerSyncsRuntimeCodexFingerprintMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	if err := store.LoadAccountByID(context.Background(), accountID); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	handler := &Handler{db: db, store: store}

	runtime := store.FindByID(accountID)
	if runtime == nil {
		t.Fatal("runtime account not loaded")
	}
	if got := runtime.EffectiveCodexFingerprintMode(); got != auth.CodexFingerprintModeOff {
		t.Fatalf("runtime mode = %q, want %q before any update", got, auth.CodexFingerprintModeOff)
	}

	recorder := patchAccountScheduler(t, handler, accountID, `{"codex_fingerprint_mode":"device"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := runtime.EffectiveCodexFingerprintMode(); got != auth.CodexFingerprintModeDevice {
		t.Fatalf("runtime mode = %q, want %q", got, auth.CodexFingerprintModeDevice)
	}
}

func TestAccountResponseExposesCodexFingerprintMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	handler := &Handler{db: db, store: store}

	if recorder := patchAccountScheduler(t, handler, accountID, `{"codex_fingerprint_mode":"full"}`); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}

	detailed := handler.buildAccountResponse(row, nil, nil, nil, nil, true)
	if detailed.CodexFingerprintMode != auth.CodexFingerprintModeFull {
		t.Fatalf("detailed mode = %q, want %q", detailed.CodexFingerprintMode, auth.CodexFingerprintModeFull)
	}

	// The summary response must also carry the fingerprint mode, for the list and quick drawer to render.
	summary := handler.buildAccountResponse(row, nil, nil, nil, nil, false)
	if summary.CodexFingerprintMode != auth.CodexFingerprintModeFull {
		t.Fatalf("summary mode = %q, want %q", summary.CodexFingerprintMode, auth.CodexFingerprintModeFull)
	}
}
