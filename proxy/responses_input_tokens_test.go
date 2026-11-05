package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// TestLocalInputTokensEndpointReturnsInputTokens 覆盖 issue #238：本地 token 计数
// 兼容端点对合法请求返回 200 + 正数 input_tokens，对非法 JSON 返回 400。
func TestLocalInputTokensEndpointReturnsInputTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"model":"gpt-5.5","input":"hello world","instructions":"be brief"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.ResponsesInputTokens(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := gjson.Get(rec.Body.String(), "input_tokens")
	if !got.Exists() || got.Int() <= 0 {
		t.Fatalf("input_tokens = %q, want positive; body=%s", got.Raw, rec.Body.String())
	}

	// 非法 JSON → 400
	rec = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not json"))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.ResponsesInputTokens(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid body status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
