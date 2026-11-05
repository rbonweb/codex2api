package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// An unconverted handshake workspace-deactivated error must be returned as a 503 pool-level error, and must never be returned as 500 plus the raw handshake details
// leaked to the downstream.
func TestErrorToGinResponseHandshakeDeactivatedBecomes503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ErrorToGinResponse(c, errors.New(`websocket handshake failed: websocket: bad handshake (HTTP 402 Payment Required); Cf-Ray=x: {"detail":{"code":"deactivated_workspace"}}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After missing")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "account_pool_deactivated") {
		t.Fatalf("body = %s", body)
	}
	// The message must include the raw upstream error body, but the handshake internals (Cf-Ray and the like) must not leak.
	if !strings.Contains(body, "deactivated_workspace") {
		t.Fatalf("upstream detail missing: %s", body)
	}
	if strings.Contains(body, "websocket handshake failed") || strings.Contains(body, "Cf-Ray") {
		t.Fatalf("raw handshake detail leaked: %s", body)
	}

	// Ordinary errors still take the 500 fallback
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	ErrorToGinResponse(c, errors.New("dial tcp timeout"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("generic error status = %d, want 500", rec.Code)
	}
}
