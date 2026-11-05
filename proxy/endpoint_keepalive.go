package proxy

import (
	"context"
	"fmt"
	"io"

	"github.com/codex2api/database"
)

// Error responses are inspected as a bounded prefix. Success JSON has a
// separate, larger budget so media-bearing responses are not capped at 1 MiB.
const maxUpstreamResponseBodyBytes = 64 << 20

func readUpstreamErrorBody(ctx context.Context, reader io.Reader) ([]byte, error) {
	return readAllWithContinuousRetryKeepalive(ctx, io.LimitReader(reader, maxUpstreamErrorBodyBytes))
}

// upstreamResponseReadContext keeps a bounded usage catch-up read window after a normal request is canceled.
// A private continuous-retry attempt ends immediately with the downstream, and must not extend the retry lifetime because of the catch-up read.
func upstreamResponseReadContext(downstreamCtx, upstreamCtx context.Context, policy database.ContinuousRetryPolicy) context.Context {
	if continuousRetryBuffersAttempts(policy) || upstreamCtx == nil {
		return downstreamCtx
	}
	return upstreamCtx
}

// readAllLimitedWithContinuousRetryKeepalive combines the read limit with request-level keepalive
// for non-streaming successful responses and native protocol JSON aggregation, so there is no long stretch without downstream activity during body reading.
func readAllLimitedWithContinuousRetryKeepalive(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, fmt.Errorf("response body limit must be non-negative")
	}
	body, err := readAllWithContinuousRetryKeepalive(ctx, io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return body, nil
}

// setContinuousRetryKeepaliveActive enables or disables downstream keepalive as needed when a retry switches accounts.
func setContinuousRetryKeepaliveActive(ctx context.Context, active bool) {
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if keepalive == nil {
		return
	}
	if controller, ok := keepalive.(interface{ SetEnabled(bool) }); ok {
		controller.SetEnabled(active)
	}
	if active {
		activateContinuousRetryKeepalive(ctx)
	}
}
