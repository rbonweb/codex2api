package proxy

import (
	"context"
	"sync"
	"time"
)

const (
	// For plain SSE, when upstream thinks for a long time without producing any forwardable event, the downstream
	// link's idle timer must still be refreshed. SSE comments are not treated as model output by clients.
	defaultDownstreamHTTPKeepaliveInterval = 30 * time.Second
	defaultDownstreamSSEKeepaliveInterval  = defaultDownstreamHTTPKeepaliveInterval
	downstreamSSEKeepaliveComment          = ": keepalive\n\n"
	downstreamMessagesKeepaliveEvent       = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
)

// The variable form only shortens the wait for handler-level tests; production runs read it from the environment.
var downstreamSSEKeepaliveInterval = downstreamSSEKeepaliveIntervalFromEnv()

// downstreamSSEKeepaliveIntervalFromEnv reads the downstream HTTP/SSE keepalive period configuration.
func downstreamSSEKeepaliveIntervalFromEnv() time.Duration {
	return durationFromEnv("DOWNSTREAM_HTTP_KEEPALIVE_INTERVAL", defaultDownstreamSSEKeepaliveInterval)
}

// ConfigureDownstreamKeepaliveFromEnv refreshes the keepalive configuration after config.Load reads .env.
// Process environment variables still take effect at package initialization.
func ConfigureDownstreamKeepaliveFromEnv() {
	downstreamSSEKeepaliveInterval = downstreamSSEKeepaliveIntervalFromEnv()
	continuousRetryKeepaliveInterval = downstreamSSEKeepaliveInterval
	downstreamWSKeepaliveInterval = downstreamWSKeepaliveIntervalFromEnv()
}

// startDownstreamSSEKeepalive runs writeKeepalive periodically until the request is canceled or a write fails
// or stop is called. stop waits for the goroutine to exit completely, so that no writes happen concurrently after the stream has finished.
func startDownstreamSSEKeepalive(ctx context.Context, interval time.Duration, writeKeepalive func() bool) func() {
	if interval <= 0 || writeKeepalive == nil {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	stopCh := make(chan struct{})
	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if !writeKeepalive() {
					return
				}
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stopCh)
			<-done
		})
	}
}
