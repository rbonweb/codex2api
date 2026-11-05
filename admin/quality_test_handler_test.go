package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func qualityTestRouter(h *Handler) *gin.Engine {
	router := gin.New()
	router.GET("/accounts/:id/quality-test/options", h.QualityTestOptions)
	router.POST("/accounts/:id/quality-test", h.QualityTest)
	return router
}

func qualityTestHTTPRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/accounts/42/quality-test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestQualityTestRejectsInvalidInputBeforeUpstream(t *testing.T) {
	h, _ := newCodexDiagnosticsTestHandler(t)
	for _, body := range []string{
		`{`,
		`{"model":"gpt-4o-mini","prompt":" "}`,
		`{"model":"other-model","prompt":"HTML"}`,
		`{"model":"gpt-4o-mini","prompt":"HTML","reasoning_effort":"invalid"}`,
		`{"model":"gpt-4o-mini","prompt":"` + strings.Repeat("x", qualityTestPromptLimit+1) + `"}`,
	} {
		recorder := httptest.NewRecorder()
		qualityTestRouter(h).ServeHTTP(recorder, qualityTestHTTPRequest(body))
		if recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Header().Get("Content-Type"), "event-stream") {
			t.Fatalf("invalid request status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func TestQualityTestChannelPayloadsAndOptions(t *testing.T) {
	h := &Handler{store: auth.NewStore(nil, nil, nil)}
	codex := &auth.Account{Models: []string{"gpt-5.5", "gpt-image-1", "gpt-5.5(high)"}}
	options := h.qualityTestOptionsForAccount(context.Background(), codex)
	if len(options.Models) != 1 || options.Models[0] != "gpt-5.5" {
		t.Fatalf("invalid options: %+v", options)
	}
	body, err := buildQualityTestPayload(codex, "gpt-5.5", qualityTestRequest{Prompt: "HTML"})
	if err != nil || gjson.GetBytes(body, "reasoning").Exists() || gjson.GetBytes(body, "store").Bool() {
		t.Fatalf("default effort/store behavior changed: %s", body)
	}
}

func TestQualityTestPreviewUsesAnIndependentOpaqueSandbox(t *testing.T) {
	router := gin.New()
	router.GET("/preview", serveQualityTestPreview)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/preview?html=must-not-be-reflected", nil))
	policy := recorder.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"sandbox allow-scripts", "frame-ancestors 'self'", "connect-src 'none'", "script-src 'unsafe-inline'"} {
		if !strings.Contains(policy, directive) {
			t.Fatalf("missing preview policy %q: %s", directive, policy)
		}
	}
	if strings.Contains(policy, "allow-same-origin") || strings.Contains(recorder.Body.String(), "must-not-be-reflected") || recorder.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatal("preview must remain isolated and never reflect request data")
	}
}
