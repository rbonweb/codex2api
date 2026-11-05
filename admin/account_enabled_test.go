package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestToggleAccountEnabledImmediatelySchedulesOnlyEnabledEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	credentials := func(name string) map[string]interface{} {
		return map[string]interface{}{
			"refresh_token": "rt-" + name,
			"access_token":  "at-" + name,
			"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"email":         name + "@example.com",
		}
	}

	disabledID, err := db.InsertAccountWithCredentials(ctx, "disabled", credentials("disabled"), "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials(disabled): %v", err)
	}
	enabledID, err := db.InsertAccountWithCredentials(ctx, "to-enable", credentials("healthy"), "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials(to-enable): %v", err)
	}
	for _, id := range []int64{disabledID, enabledID} {
		if err := db.SetAccountEnabled(ctx, id, false); err != nil {
			t.Fatalf("SetAccountEnabled(%d, false): %v", id, err)
		}
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:       1,
		FastSchedulerEnabled: true,
	})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}
	if got := store.Next(); got != nil {
		store.Release(got)
		t.Fatalf("Next() before enable = account %d, want nil", got.ID())
	}

	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(recorder)
	requestContext.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", enabledID)}}
	requestContext.Request = httptest.NewRequest(
		http.MethodPatch,
		fmt.Sprintf("/api/admin/accounts/%d/enabled", enabledID),
		strings.NewReader(`{"enabled":true}`),
	)
	requestContext.Request.Header.Set("Content-Type", "application/json")
	handler.ToggleAccountEnabled(requestContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("ToggleAccountEnabled status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil immediately after enabling the healthy endpoint")
	}
	defer store.Release(got)
	if got.ID() != enabledID {
		t.Fatalf("Next() = account %d, want newly enabled account %d", got.ID(), enabledID)
	}
}

func TestBatchEnableLoadsEndpointMissingFromRuntimePool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	accountID, err := db.InsertAccountWithCredentials(ctx, "batch-enable", map[string]interface{}{
		"refresh_token": "rt-batch",
		"access_token":  "at-batch",
		"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"email":         "batch@example.com",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	if err := db.SetAccountEnabled(ctx, accountID, false); err != nil {
		t.Fatalf("SetAccountEnabled(false): %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:       1,
		FastSchedulerEnabled: true,
	})
	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(recorder)
	requestContext.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/admin/accounts/batch-update",
		strings.NewReader(fmt.Sprintf(`{"ids":[%d],"enabled":true}`, accountID)),
	)
	requestContext.Request.Header.Set("Content-Type", "application/json")
	handler.BatchUpdateAccounts(requestContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("BatchUpdateAccounts status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil after batch-enabling an endpoint absent from the runtime pool")
	}
	defer store.Release(got)
	if got.ID() != accountID {
		t.Fatalf("Next() = account %d, want batch-enabled account %d", got.ID(), accountID)
	}
}
