package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type scopedModelJSON struct {
	ID      string `json:"id"`
	Owner   string `json:"owned_by"`
	Created int64  `json:"created"`
}

func scopedModelByID(models []scopedModelJSON, id string) (string, int64, bool) {
	for _, model := range models {
		if model.ID == id {
			return model.Owner, model.Created, true
		}
	}
	return "", 0, false
}

func listScopedModelsForTest(t *testing.T, handler *Handler, row *database.APIKeyRow) []scopedModelJSON {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(contextAPIKeyRow, row)
	handler.ListModels(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Data []scopedModelJSON `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	return payload.Data
}

func TestScopedModelsPlanMissingFailsClosedAndEmptyIs200(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, AccessToken: "at", RefreshToken: "rt", Models: []string{"grok-4.5"}, PlanType: "free", CredentialGeneration: 1})
	handler := NewHandler(store, nil, nil, nil)
	models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 1, Limits: database.APIKeyLimits{PlanAllow: []string{"free"}}})
	if len(models) != 0 {
		t.Fatalf("archive plan leaked through live plan gate: %+v", models)
	}
}

func TestFilterCodexManifestRetainsUnknownFieldsAndStableETag(t *testing.T) {
	body := []byte(`{"models":[{"slug":"gpt-a","display_name":"A"},{"slug":"gpt-b","new_field":{"x":1}}],"future":true}`)
	filtered, etag, err := filterCodexManifest(body, `W/"upstream"`, func(slug string) bool { return slug == "gpt-b" })
	if err != nil {
		t.Fatalf("filterCodexManifest: %v", err)
	}
	if etag == "" {
		t.Fatal("gateway ETag is empty")
	}
	var payload struct {
		Models []map[string]any `json:"models"`
		Future bool             `json:"future"`
	}
	if err := json.Unmarshal(filtered, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Future || len(payload.Models) != 1 || payload.Models[0]["slug"] != "gpt-b" || payload.Models[0]["new_field"] == nil {
		t.Fatalf("filtered payload lost schema fields: %s", filtered)
	}
	filteredAgain, etagAgain, err := filterCodexManifest(body, `W/"upstream"`, func(slug string) bool { return slug == "gpt-b" })
	if err != nil || string(filteredAgain) != string(filtered) || etagAgain != etag {
		t.Fatalf("filtered manifest is unstable: err=%v etag=%q/%q", err, etag, etagAgain)
	}
}

func TestFilterCodexManifestUnknownSchemaFailsClosed(t *testing.T) {
	for _, body := range [][]byte{[]byte(`{"data":[]}`), []byte(`{"models":[{"id":"gpt-a"}]}`), []byte(`not json`)} {
		if _, _, err := filterCodexManifest(body, "", func(string) bool { return true }); err == nil {
			t.Fatalf("filterCodexManifest(%s) accepted unsafe schema", body)
		}
	}
}

func TestScopedModelRecordsDoesNotNeedDatabase(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai"})
	handler := NewHandler(store, nil, nil, nil)
	if got := handler.scopedModelRecords(context.Background(), &database.APIKeyRow{ID: 1}); len(got) == 0 {
		t.Fatal("conservative Grok defaults missing")
	}
}

// A model the upstream rejected for an account ("The 'gpt-5.5' model is not
// supported when using Codex with a ChatGPT account") must not stay listed for
// that account while the unsupported cooldown is active, or clients pick a model
// that can only fail. Other accounts that still serve it keep it listed, and a
// rate-limit cooldown keeps the list stable as before.
func TestScopedModelsHidesModelWithActiveUnsupportedCooldown(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	first := &auth.Account{DBID: 1, AccessToken: "codex-1", PlanType: "plus"}
	second := &auth.Account{DBID: 2, AccessToken: "codex-2", PlanType: "plus"}
	store.AddAccount(first)
	store.AddAccount(second)
	handler := NewHandler(store, nil, nil, nil)
	row := &database.APIKeyRow{ID: 31}
	listed := func(id string) bool {
		_, _, ok := scopedModelByID(listScopedModelsForTest(t, handler, row), id)
		return ok
	}
	if !listed("gpt-5.5") || !listed("gpt-5.6-luna") {
		t.Fatalf("baseline catalog must list gpt-5.5 and gpt-5.6-luna, got %+v", listScopedModelsForTest(t, handler, row))
	}

	until := time.Now().Add(time.Hour)
	first.SetModelCooldownUntil("gpt-5.5", auth.ModelCooldownReasonUnsupported, until)
	second.SetModelCooldownUntil("gpt-5.5", auth.ModelCooldownReasonUnsupported, until)
	if listed("gpt-5.5") {
		t.Fatal("gpt-5.5 is still listed although every account reports it unsupported")
	}
	if !listed("gpt-5.6-luna") {
		t.Fatal("an unsupported cooldown on gpt-5.5 hid an unrelated model")
	}

	second.ClearModelCooldown("gpt-5.5")
	if !listed("gpt-5.5") {
		t.Fatal("gpt-5.5 must reappear while one account can still serve it")
	}

	second.SetModelCooldownUntil("gpt-5.5", "rate_limited", until)
	if !listed("gpt-5.5") {
		t.Fatal("a rate-limit cooldown must keep the model listed")
	}
}
