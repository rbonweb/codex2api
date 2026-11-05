package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/database"
)

// TestKeepaliveReadCancellationPreservesDrainBudget checks the stream pump,
// including a body that stays silent past the downstream cancellation.
func TestKeepaliveReadCancellationPreservesDrainBudget(t *testing.T) {
	for _, continuous := range []bool{false, true} {
		name := "ordinary"
		if continuous {
			name = "continuous-retry"
		}
		t.Run(name, func(t *testing.T) {
			clientCtx, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			keepalive := &requestContinuousRetryKeepalive{ctx: clientCtx}
			keepalive.Activate()
			clientCtx = context.WithValue(clientCtx, continuousRetryKeepaliveContextKey{}, continuousRetryKeepalive(keepalive))
			upstreamCtx, cancelUpstream := newDrainableUpstreamContext(clientCtx, 200*time.Millisecond)
			defer cancelUpstream()
			readCtx := upstreamResponseReadContext(clientCtx, upstreamCtx, database.ContinuousRetryPolicy{Enabled: continuous})
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- readSSEStreamWithContinuousRetryKeepalive(readCtx, reader, func(_ string, _ []byte) bool {
					close(started)
					return true
				})
			}()
			_, err := io.WriteString(writer, "data: {\"type\":\"response.created\"}\n\n")
			if err != nil {
				t.Fatal(err)
			}
			<-started
			cancelClient()
			if !continuous {
				select {
				case err := <-done:
					t.Fatalf("ordinary read skipped its drain window: %v", err)
				case <-time.After(30 * time.Millisecond):
				}
			}
			timeout := time.Second
			if continuous {
				timeout = 100 * time.Millisecond
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("read cancellation = %v", err)
				}
			case <-time.After(timeout):
				t.Fatal("stream read exceeded its cancellation budget")
			}
		})
	}
}

type cancelOnContentWriter struct {
	*httptest.ResponseRecorder
	cancel                context.CancelFunc
	once                  sync.Once
	heartbeatsAfterCancel int
	failHeartbeat         bool
	disconnected          bool
}

func (w *cancelOnContentWriter) WriteString(value string) (int, error) {
	return w.Write([]byte(value))
}

func (w *cancelOnContentWriter) Write(p []byte) (int, error) {
	if w.disconnected && (strings.Contains(string(p), ": keepalive") || strings.Contains(string(p), "event: ping")) {
		w.heartbeatsAfterCancel++
		if w.failHeartbeat {
			return 0, io.ErrClosedPipe
		}
	}
	n, err := w.ResponseRecorder.Write(p)
	if strings.Contains(string(p), "review-content") {
		w.disconnected = true
		if !w.failHeartbeat {
			w.once.Do(w.cancel)
		}
	}
	return n, err
}

// TestDownstreamKeepaliveDrainsUsageAfterClientCancel covers native Responses and the Chat translator with downstream heartbeats.
func TestDownstreamKeepaliveDrainsUsageAfterClientCancel(t *testing.T) {
	previous := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 5 * time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previous })
	for _, tc := range []struct {
		name, path, body string
	}{
		{"responses-codex", "/v1/responses", `{"model":"gpt-6-astra","input":"hi","stream":true}`},
		{"chat", "/v1/chat/completions", `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":true}`},
	} {
		for _, disconnect := range []string{"context-canceled", "heartbeat-write-failed"} {
			t.Run(tc.name+"/"+disconnect, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_review\"}}\n\n"+
						"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\"}}\n\n"+
						"data: {\"type\":\"response.output_text.delta\",\"delta\":\"review-content\"}\n\n")
					w.(http.Flusher).Flush()
					time.Sleep(60 * time.Millisecond)
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_review\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1000,\"output_tokens\":40}}}\n\n")
				}))
				t.Cleanup(upstream.Close)
				h, _, router := newModelQuotaTestHandler(t, 2, upstream.URL)
				clientCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)).WithContext(clientCtx)
				req.Header.Set("Authorization", "Bearer "+modelQuotaTestKey)
				req.Header.Set("Content-Type", "application/json")
				w := &cancelOnContentWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, failHeartbeat: disconnect == "heartbeat-write-failed"}
				router.ServeHTTP(w, req)
				if !w.disconnected {
					t.Fatalf("fixture never canceled downstream after content: status=%d body=%q", w.Code, w.Body.String())
				}
				wantHeartbeats := 0
				if w.failHeartbeat {
					wantHeartbeats = 1
				}
				if w.heartbeatsAfterCancel != wantHeartbeats {
					t.Fatalf("sent %d heartbeats after client cancellation", w.heartbeatsAfterCancel)
				}
				h.db.FlushUsageLogs()
				logs, err := h.db.ListUsageLogsByFilter(context.Background(), database.UsageLogFilter{Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), IncludeCanceled: true})
				if err != nil {
					t.Fatal(err)
				}
				if len(logs) != 1 {
					t.Fatalf("usage log count=%d, want 1", len(logs))
				}
				entry := logs[0]
				t.Logf("status=%d prompt=%d completion=%d total=%d", entry.StatusCode, entry.PromptTokens, entry.CompletionTokens, entry.TotalTokens)
				if entry.PromptTokens != 1000 || entry.CompletionTokens != 40 {
					t.Fatalf("terminal usage lost after client cancel: prompt=%d completion=%d, want 1000/40", entry.PromptTokens, entry.CompletionTokens)
				}
			})
		}
	}
}
