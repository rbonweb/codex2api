package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestReviewAstraLongOnlyOverrideAppliesPricing(t *testing.T) {
	db := newTestAdminDB(t)
	t.Cleanup(func() { database.SetModelPricingOverrides(nil) })
	_, err := db.MutateModelPricingSettings(context.Background(), nil, func(m map[string]database.ModelPricingOverride) error {
		m["gpt-6-astra"] = database.ModelPricingOverride{Source: "custom", Input: 7}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/model-pricing", strings.NewReader(`{"model":"gpt-6-astra","pricing":{"input_long":20,"output_long":75}}`))
	h.UpdateModelPricing(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	settings, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	overrides, err := database.ParseModelPricingOverridesJSON(settings.ModelPricingOverrides)
	got := overrides["gpt-6-astra"]
	if err != nil || got.InputLong != 20 || got.OutputLong != 75 {
		t.Fatalf("pricing request did not apply long override: %+v", got)
	}
}

func TestReviewShortDiagnosticSecretsPreserveIDs(t *testing.T) {
	got := sanitizeCodexTestText(`{"response_id":"resp_0123","usage":1234,"api_key":"1","echo":"1 1"}`, []string{"1"})
	var parsed map[string]any
	if json.Unmarshal([]byte(got), &parsed) != nil || parsed["response_id"] != "resp_0123" || parsed["usage"] != float64(1234) {
		t.Fatalf("short placeholder corrupted diagnostics: %s", got)
	}
	if strings.Contains(got, `"1"`) || strings.Contains(got, `1 1`) {
		t.Fatal("standalone credential was exposed")
	}
}
