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

// shortenDownstreamSSEKeepalive 把下游保活间隔压到毫秒级，让处理器级测试
// 能在上游几十毫秒的静默里观察到心跳；测试结束恢复默认值。
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
