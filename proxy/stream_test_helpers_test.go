package proxy

import (
	"io"
	"net/http"
	"testing"
	"time"
)

func writeCodexSSE(w http.ResponseWriter, events ...string) {
	for _, event := range events {
		_, _ = io.WriteString(w, "data: "+event+"\n\n")
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// shortenDownstreamSSEKeepalive shortens the downstream keepalive interval to the millisecond range, so handler-level tests
// can observe heartbeats during upstream silences of tens of milliseconds; the default is restored when the test ends.
func shortenDownstreamSSEKeepalive(t *testing.T) {
	t.Helper()
	previousSSEInterval := downstreamSSEKeepaliveInterval
	previousRetryInterval := continuousRetryKeepaliveInterval
	t.Cleanup(func() {
		downstreamSSEKeepaliveInterval = previousSSEInterval
		continuousRetryKeepaliveInterval = previousRetryInterval
	})
	downstreamSSEKeepaliveInterval = 5 * time.Millisecond
	continuousRetryKeepaliveInterval = 5 * time.Millisecond
}
