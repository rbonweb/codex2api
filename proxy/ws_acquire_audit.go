package proxy

import (
	"context"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// ws_acquire_ms (follow-up to issue #413): records the time spent acquiring the upstream WS connection within one attempt,
// wall-clock time (busy queueing + liveness probe + handshake, including a rebuild after a failed send). It is attached to the downstream request ctx,
// wsrelay accumulates it and logUsageForRequest backfills usage_logs, so that in first_token_ms
// "gateway-side connection queueing" and "upstream generation" can be attributed separately. The HTTP/relay path is always 0.
// It mirrors userAgentAudit: the executor resets it to zero at each attempt's entry, and what finally gets stored is
// the value from the attempt that succeeded (or wrote a log row).
type wsAcquireAuditContextKey struct{}

type wsAcquireAudit struct {
	mu    sync.Mutex
	total time.Duration
}

func withWsAcquireAudit(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if wsAcquireAuditFromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, wsAcquireAuditContextKey{}, &wsAcquireAudit{})
}

func wsAcquireAuditFromContext(ctx context.Context) *wsAcquireAudit {
	if ctx == nil {
		return nil
	}
	audit, _ := ctx.Value(wsAcquireAuditContextKey{}).(*wsAcquireAudit)
	return audit
}

func resetWsAcquireAudit(ctx context.Context) {
	if audit := wsAcquireAuditFromContext(ctx); audit != nil {
		audit.mu.Lock()
		audit.total = 0
		audit.mu.Unlock()
	}
}

// AddWsAcquireDuration adds one WS connection acquisition duration (called by wsrelay; within one attempt
// a rebuild after a failed send acquires more than once, so the durations are summed into the total).
func AddWsAcquireDuration(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	if audit := wsAcquireAuditFromContext(ctx); audit != nil {
		audit.mu.Lock()
		audit.total += d
		audit.mu.Unlock()
	}
}

func wsAcquireAuditMs(ctx context.Context) int {
	audit := wsAcquireAuditFromContext(ctx)
	if audit == nil {
		return 0
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	return int(audit.total.Milliseconds())
}

func attachWsAcquireAudit(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request = c.Request.WithContext(withWsAcquireAudit(c.Request.Context()))
}

func populateWsAcquireFromRequest(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	input.WsAcquireMs = wsAcquireAuditMs(c.Request.Context())
	// Optional basis (off by default): first_token_ms subtracts the connection acquisition time and keeps only the "upstream generates first content" part,
	// so it is comparable with the HTTP path and with deployments where acquisition is always 0; the raw value = first_token_ms + ws_acquire_ms.
	// Rows that were recorded are clamped to a floor of 1ms after subtraction, so they are not zeroed and shown as "-" (not recorded) by the frontend.
	if CurrentRuntimeSettings().FirstTokenExcludesWsAcquire &&
		input.FirstTokenMs > 0 && input.WsAcquireMs > 0 {
		input.FirstTokenMs = max(input.FirstTokenMs-input.WsAcquireMs, 1)
	}
}
