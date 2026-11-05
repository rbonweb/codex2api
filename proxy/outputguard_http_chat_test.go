package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security/outputguard"
	"github.com/gin-gonic/gin"
)

// The Output Guard runtime config has a database-level minimum (channel >= 128KiB). The integration test
// therefore uses the real minimum threshold and is triggered by real upstream events larger than 128KiB, rather than a faked small value.
const (
	guardHTTPTestMaxToolBytes    = int64(16 * 1024)
	guardHTTPTestMaxChannelBytes = int64(128 * 1024)
	guardHTTPTestMaxRequestBytes = int64(256 * 1024)
)

func init() {
	gin.SetMode(gin.TestMode)
}

// setupOutputGuardHTTPTestEnv loads a valid guard config that can be triggered by real events,
// and ensures the global ready state is cleared when the test ends, so that it does not pollute other cases in the package.
func setupOutputGuardHTTPTestEnv(t *testing.T) {
	t.Helper()
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)

	enabled := true
	if !ApplyOutputGuardSettings(database.OutputGuardSettings{
		Enabled:               &enabled,
		MaxToolArgumentBytes:  guardHTTPTestMaxToolBytes,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       guardHTTPTestMaxChannelBytes,
		MaxRequestBytes:       guardHTTPTestMaxRequestBytes,
		ConfigGeneration:      1,
	}) {
		t.Fatal("ApplyOutputGuardSettings rejected the integration-test guard config")
	}
}

// newOutputGuardHTTPTestStore uses a real auth.Store and real accounts, going through the same
// Resin outbound path as production (Codex OAuth accounts cannot point BaseURL directly at httptest).
func newOutputGuardHTTPTestStore(t *testing.T, maxRetries int) *auth.Store {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.5",
		MaxRetries:          maxRetries,
		MaxRateLimitRetries: 0,
		RetryIntervalMS:     0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:        1,
		AccessToken: "at-guard-test",
		PlanType:    "pro",
		AccountID:   "acct-guard-test",
	})
	return store
}

func enableOutputGuardHTTPTestResin(t *testing.T, upstreamURL string) {
	t.Helper()
	previous := resinCfg.Load()
	t.Cleanup(func() { SetResinConfig(previous) })
	SetResinConfig(&ResinConfig{BaseURL: upstreamURL, PlatformName: "guard-test"})
}

func outputGuardTestEvent(payload map[string]any) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return "data: " + string(raw) + "\n\n"
}

func outputGuardTestDelta(itemID string, delta string) string {
	return outputGuardTestEvent(map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       itemID,
		"content_index": 0,
		"delta":         delta,
	})
}

// outputGuardTestRandomText generates low-periodicity text to avoid triggering repetition detection,
// so that the assertion hits the byte-budget path.
func outputGuardTestRandomText(seed int64, n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	rng := rand.New(rand.NewSource(seed))
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(buf)
}

func newOutputGuardResponsesTestContext(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder
}

func outputGuardTestBodySummary(body string) string {
	const max = 400
	if len(body) <= max {
		return body
	}
	return body[:max] + fmt.Sprintf("... [truncated %d bytes]", len(body)-max)
}

func decodeOutputGuardErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode guard error response: %v; body=%s", err, outputGuardTestBodySummary(string(body)))
	}
	return payload.Error.Code
}

// 1. Responses HTTP streaming trips the Guard before the first token: it must return a 502 JSON error response,
// and the upstream must be cancelled immediately.
func TestResponsesHTTPStreamGuardTrippedPreToken(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	overflow := outputGuardTestRandomText(11, int(guardHTTPTestMaxChannelBytes)+12*1024)
	var upstreamCalls atomic.Int32
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_pre", overflow))
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-time.After(3 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)
	enableOutputGuardHTTPTestResin(t, upstream.URL)

	handler := NewHandler(newOutputGuardHTTPTestStore(t, 0), nil, &config.Config{AllowAnonymousV1: true}, nil)
	c, recorder := newOutputGuardResponsesTestContext(t, `{"model":"gpt-5.5","stream":true,"input":"test"}`)

	handler.Responses(c)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502 before first token, got %d, body: %s", recorder.Code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if code := decodeOutputGuardErrorCode(t, recorder.Body.Bytes()); code != outputguard.TriggeredCode {
		t.Fatalf("expected error code %q, got %q; body=%s", outputguard.TriggeredCode, code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected upstream request context to be canceled immediately upon trip")
	}
}

// guardDeadlineRecorder makes httptest.ResponseRecorder support write deadlines,
// because 04a's in-stream terminal write uses http.ResponseController with a bounded timeout.
type guardDeadlineRecorder struct {
	*httptest.ResponseRecorder
}

func (r *guardDeadlineRecorder) SetWriteDeadline(time.Time) error { return nil }

// 2. ChatCompletions streaming trips the Guard after the first token: downstream must receive an in-stream 502
// error frame, and the [DONE] success sentinel must never appear.
func TestChatCompletionsStreamGuardTrippedPostTokenNoDone(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	overflow := outputGuardTestRandomText(22, int(guardHTTPTestMaxChannelBytes)+12*1024)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_post", "Hello "))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(10 * time.Millisecond)
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_post", overflow))
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)
	enableOutputGuardHTTPTestResin(t, upstream.URL)

	handler := NewHandler(newOutputGuardHTTPTestStore(t, 0), nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := &guardDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ChatCompletions(c)

	body := outputGuardTestBodySummary(recorder.Body.String())
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected committed stream status 200, got %d; body=%s", recorder.Code, body)
	}
	if !strings.Contains(body, `"content":"Hello "`) {
		t.Fatalf("expected the already-delivered first token to remain visible, body: %s", outputGuardTestBodySummary(body))
	}
	if !strings.Contains(body, outputguard.TriggeredCode) {
		t.Fatalf("expected in-stream guard terminal containing %q, body: %s", outputguard.TriggeredCode, outputGuardTestBodySummary(body))
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("tripped stream MUST NOT contain the [DONE] sentinel, body: %s", outputGuardTestBodySummary(body))
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
}

// 3. Responses HTTP non-streaming: a runaway snapshot must block the aggregated result from being written, returning 502 directly.
func TestResponsesHTTPNonStreamGuardTripped(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	overflow := outputGuardTestRandomText(33, int(guardHTTPTestMaxChannelBytes)+12*1024)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_nonstream", overflow))
		_, _ = io.WriteString(w, outputGuardTestEvent(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_guard_nonstream", "status": "completed"},
		}))
	}))
	t.Cleanup(upstream.Close)
	enableOutputGuardHTTPTestResin(t, upstream.URL)

	handler := NewHandler(newOutputGuardHTTPTestStore(t, 0), nil, &config.Config{AllowAnonymousV1: true}, nil)
	c, recorder := newOutputGuardResponsesTestContext(t, `{"model":"gpt-5.5","stream":false,"input":"test"}`)

	handler.Responses(c)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502 for tripped non-stream response, got %d, body: %s", recorder.Code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if code := decodeOutputGuardErrorCode(t, recorder.Body.Bytes()); code != outputguard.TriggeredCode {
		t.Fatalf("expected error code %q, got %q; body=%s", outputguard.TriggeredCode, code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
}

// 3b. ChatCompletions non-streaming: if the Guard trips before aggregation, it must block the compact result from being written,
// returning a 502 JSON instead of aggregating already-runaway content into a 200.
func TestChatCompletionsNonStreamGuardTripped(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	overflow := outputGuardTestRandomText(36, int(guardHTTPTestMaxChannelBytes)+12*1024)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_chat_nonstream", overflow))
		_, _ = io.WriteString(w, outputGuardTestEvent(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_guard_chat_nonstream", "status": "completed"},
		}))
	}))
	t.Cleanup(upstream.Close)
	enableOutputGuardHTTPTestResin(t, upstream.URL)

	handler := NewHandler(newOutputGuardHTTPTestStore(t, 0), nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.ChatCompletions(c)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502 for tripped non-stream chat response, got %d, body: %s", recorder.Code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if code := decodeOutputGuardErrorCode(t, recorder.Body.Bytes()); code != outputguard.TriggeredCode {
		t.Fatalf("expected error code %q, got %q; body=%s", outputguard.TriggeredCode, code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", got)
	}
}

// 4. Cold-start gate: when config loading fails (lastSyncError recorded, not ready), both Responses and
// ChatCompletions must hard-block with 503 service_unavailable and must not enter account scheduling.
func TestOutputGuardHTTPReadinessGate503(t *testing.T) {
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)
	recordOutputGuardConfigSyncError(errors.New("cold start: output guard settings unavailable"))

	tests := []struct {
		name string
		body string
		call func(*Handler, *gin.Context)
	}{
		{
			name: "responses",
			body: `{"model":"gpt-5.5","input":"test"}`,
			call: func(h *Handler, c *gin.Context) { h.Responses(c) },
		},
		{
			name: "chat_completions",
			body: `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`,
			call: func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewHandler(nil, nil, &config.Config{AllowAnonymousV1: true}, nil)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")

			tc.call(handler, c)

			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected status 503 while guard is not ready, got %d; body=%s", recorder.Code, outputGuardTestBodySummary(recorder.Body.String()))
			}
		})
	}
}

// 5. Cross-attempt budget inheritance: bytes already consumed but not triggered in the first attempt must accumulate into the second
// attempt; otherwise rebuilding the state machine on each attempt would let a runaway stream through across attempts.
func TestResponsesHTTPNonStreamGuardBudgetInheritedAcrossAttempts(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)

	// Each attempt alone is below the 128KiB channel limit, but the two accumulated together exceed it.
	halfBudget := guardHTTPTestMaxChannelBytes/2 + 6*1024
	firstDelta := outputGuardTestRandomText(44, int(halfBudget))
	secondDelta := outputGuardTestRandomText(55, int(halfBudget))

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_budget", firstDelta))
			_, _ = io.WriteString(w, outputGuardTestEvent(map[string]any{
				"type": "response.failed",
				"response": map[string]any{
					"status": "failed",
					"error": map[string]any{
						"status_code": 503,
						"code":        "server_error",
						"message":     "upstream overloaded",
					},
				},
			}))
			return
		}
		_, _ = io.WriteString(w, outputGuardTestDelta("msg_guard_budget", secondDelta))
		_, _ = io.WriteString(w, outputGuardTestEvent(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_guard_budget",
				"status": "completed",
				"output": []any{
					map[string]any{
						"type": "message",
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "output_text", "text": firstDelta + secondDelta},
						},
					},
				},
			},
		}))
	}))
	t.Cleanup(upstream.Close)
	enableOutputGuardHTTPTestResin(t, upstream.URL)

	handler := NewHandler(newOutputGuardHTTPTestStore(t, 1), nil, &config.Config{AllowAnonymousV1: true}, nil)
	c, recorder := newOutputGuardResponsesTestContext(t, `{"model":"gpt-5.5","stream":false,"input":"test"}`)

	handler.Responses(c)

	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("expected the retryable first attempt plus one guarded attempt (2 upstream calls), got %d; body=%s", got, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected status 502 once the inherited budget trips, got %d; body=%s", recorder.Code, outputGuardTestBodySummary(recorder.Body.String()))
	}
	if code := decodeOutputGuardErrorCode(t, recorder.Body.Bytes()); code != outputguard.TriggeredCode {
		t.Fatalf("expected error code %q, got %q; body=%s", outputguard.TriggeredCode, code, outputGuardTestBodySummary(recorder.Body.String()))
	}
}
