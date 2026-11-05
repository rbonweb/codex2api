package proxy

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestWsAcquireAuditAccumulatesResetsAndTolerates(t *testing.T) {
	ctx := withWsAcquireAudit(context.Background())
	AddWsAcquireDuration(ctx, 150*time.Millisecond)
	AddWsAcquireDuration(ctx, 350*time.Millisecond)
	if got := wsAcquireAuditMs(ctx); got != 500 {
		t.Fatalf("wsAcquireAuditMs = %d, want 500 (accumulated)", got)
	}

	// Reset to zero per attempt: after an account-switch retry, the old value must not leak into the new attempt's log row
	resetWsAcquireAudit(ctx)
	if got := wsAcquireAuditMs(ctx); got != 0 {
		t.Fatalf("wsAcquireAuditMs after reset = %d, want 0", got)
	}

	// Non-positive durations and a ctx with no audit attached must both be safe no-ops
	AddWsAcquireDuration(ctx, 0)
	AddWsAcquireDuration(context.Background(), time.Second)
	if got := wsAcquireAuditMs(context.Background()); got != 0 {
		t.Fatalf("wsAcquireAuditMs on bare ctx = %d, want 0", got)
	}
}

func TestPopulateWsAcquireFirstTokenExclusion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newCtx := func(acquire time.Duration) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		attachWsAcquireAudit(c)
		AddWsAcquireDuration(c.Request.Context(), acquire)
		return c
	}
	setExclude := func(t *testing.T, enabled bool) {
		prev := CurrentRuntimeSettings()
		t.Cleanup(func() { ApplyRuntimeSettings(prev) })
		next := prev
		next.FirstTokenExcludesWsAcquire = enabled
		ApplyRuntimeSettings(next)
	}

	// Off by default: keeps the historical basis that includes connection acquisition
	setExclude(t, false)
	input := &database.UsageLogInput{FirstTokenMs: 1500}
	populateWsAcquireFromRequest(newCtx(900*time.Millisecond), input)
	if input.FirstTokenMs != 1500 || input.WsAcquireMs != 900 {
		t.Fatalf("disabled: first_token=%d acquire=%d, want 1500/900", input.FirstTokenMs, input.WsAcquireMs)
	}

	// Enabled: connection acquisition is subtracted; adding the two columns restores the original
	setExclude(t, true)
	input = &database.UsageLogInput{FirstTokenMs: 1500}
	populateWsAcquireFromRequest(newCtx(900*time.Millisecond), input)
	if input.FirstTokenMs != 600 || input.WsAcquireMs != 900 {
		t.Fatalf("enabled: first_token=%d acquire=%d, want 600/900", input.FirstTokenMs, input.WsAcquireMs)
	}

	// Not zeroed after subtraction: recorded rows are clamped to 1ms so the frontend does not treat 0 as not recorded
	input = &database.UsageLogInput{FirstTokenMs: 800}
	populateWsAcquireFromRequest(newCtx(900*time.Millisecond), input)
	if input.FirstTokenMs != 1 {
		t.Fatalf("clamped: first_token=%d, want 1", input.FirstTokenMs)
	}

	// Rows that were not recorded (0) stay unchanged: 0 means no first token, and it must not be turned into a negative or fake value
	input = &database.UsageLogInput{FirstTokenMs: 0}
	populateWsAcquireFromRequest(newCtx(900*time.Millisecond), input)
	if input.FirstTokenMs != 0 {
		t.Fatalf("unrecorded: first_token=%d, want 0", input.FirstTokenMs)
	}
}
