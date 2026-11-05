package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// When a streaming request receives response.failed before the first token, SSE forwarding should be aborted and the real
// HTTP error code returned outside the loop, rather than wrapping the failure as 200 + [DONE] (the latter is misjudged by upstream relays/billers as a
// successful stream and billed on estimated input tokens). See shouldReturnHTTPErrorForResponseFailed.
func TestShouldReturnHTTPErrorForResponseFailed(t *testing.T) {
	cases := []struct {
		name         string
		eventType    string
		ttftRecorded bool
		wroteAnyBody bool
		clientGone   bool
		want         bool
	}{
		{"response.failed before the first token should return an error code", "response.failed", false, false, false, true},
		{"response.completed is not intercepted", "response.completed", false, false, false, false},
		{"no interception once the first output token has been produced (keep the streaming wrap-up)", "response.failed", true, false, false, false},
		{"no interception once the body has been written downstream (200 already sent)", "response.failed", false, true, false, false},
		{"no interception when the client has disconnected (keep reading upstream for usage)", "response.failed", false, false, true, false},
		{"ordinary content events are not intercepted", "response.output_text.delta", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldReturnHTTPErrorForResponseFailed(tc.eventType, tc.ttftRecorded, tc.wroteAnyBody, tc.clientGone)
			if got != tc.want {
				t.Errorf("shouldReturnHTTPErrorForResponseFailed(%q, ttft=%v, wrote=%v, gone=%v) = %v, want %v",
					tc.eventType, tc.ttftRecorded, tc.wroteAnyBody, tc.clientGone, got, tc.want)
			}
		})
	}
}

// streamEpilogue reproduces the epilogue timing of the Responses/ChatCompletions streaming path:
// event by event through the defer/intercept logic → flush the epilogue only when a body has been written → on intercept, c.JSON outside the loop.
// Verified over a real HTTP connection, because the fatal point is at the gin layer: flusher.Flush first calls WriteHeaderNow
// to commit 200; flushing early with zero writes would make the subsequent c.JSON(4xx) status code never reach downstream.
func streamEpilogue(t *testing.T, c *gin.Context, events [][]byte) {
	t.Helper()
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Error("c.Writer does not implement http.Flusher")
		return
	}
	streamWriter := newStreamFlushWriter(c.Writer, flusher)
	var pending bytes.Buffer
	ttftRecorded := false
	wroteAnyBody := false
	abortedForHTTPError := false
	gotTerminal := false

	for _, data := range events {
		eventType := gjson.GetBytes(data, "type").String()
		if eventType == "response.output_text.delta" {
			ttftRecorded = true
		}
		if eventType == "response.completed" || eventType == "response.failed" {
			gotTerminal = true
		}
		if shouldReturnHTTPErrorForResponseFailed(eventType, ttftRecorded, wroteAnyBody, false) {
			pending.Reset()
			abortedForHTTPError = true
			break
		}
		shouldDefer := !ttftRecorded && !gotTerminal && isPreContentLifecycleEvent(eventType)
		wrote, err := writeDeferredSSEData(streamWriter, &pending, data, shouldDefer)
		if err != nil {
			t.Errorf("writeDeferredSSEData: %v", err)
			return
		}
		if wrote {
			wroteAnyBody = true
		}
	}
	if wroteAnyBody {
		_ = streamWriter.Flush()
	}
	if abortedForHTTPError && !wroteAnyBody {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "context_length_exceeded", "type": "upstream_error"},
		})
	}
}

func TestStreamFirstTokenFailureWireBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)

	failedEvent := []byte(`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"input too long"}}}`)
	cases := []struct {
		name         string
		events       [][]byte
		wantStatus   int
		wantJSONType bool
		wantBodyHas  string
	}{
		{
			name: "response.failed before the first token → downstream receives a real 4xx JSON",
			events: [][]byte{
				[]byte(`{"type":"response.created"}`),
				[]byte(`{"type":"response.in_progress"}`),
				failedEvent,
			},
			wantStatus:   http.StatusBadRequest,
			wantJSONType: true,
			wantBodyHas:  "context_length_exceeded",
		},
		{
			name: "first output token already produced → keep the 200 SSE streaming wrap-up",
			events: [][]byte{
				[]byte(`{"type":"response.created"}`),
				[]byte(`{"type":"response.output_text.delta","delta":"hi"}`),
				failedEvent,
			},
			wantStatus:   http.StatusOK,
			wantJSONType: false,
			wantBodyHas:  "response.output_text.delta",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/stream", func(c *gin.Context) {
				streamEpilogue(t, c, tc.events)
			})
			srv := httptest.NewServer(router)
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/stream")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%q)", resp.StatusCode, tc.wantStatus, body)
			}
			ct := resp.Header.Get("Content-Type")
			if tc.wantJSONType && !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if !tc.wantJSONType && !strings.Contains(ct, "text/event-stream") {
				t.Errorf("Content-Type = %q, want text/event-stream", ct)
			}
			if !strings.Contains(string(body), tc.wantBodyHas) {
				t.Errorf("body = %q, want contains %q", body, tc.wantBodyHas)
			}
		})
	}
}

// The equivalent fix on the inbound WS path: a non-retryable response.failed before the first token does not pass the raw failure frame through,
// it instead sends an error frame plus a non-normal close code by error category (aligned with the SSE path returning a real 4xx).
func TestResponsesWSCloseCodeForStatus(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{http.StatusTooManyRequests, websocket.CloseTryAgainLater},
		{http.StatusBadRequest, websocket.ClosePolicyViolation},
		{http.StatusUnauthorized, websocket.ClosePolicyViolation},
		{http.StatusForbidden, websocket.ClosePolicyViolation},
		{http.StatusPaymentRequired, websocket.ClosePolicyViolation},
		{http.StatusInternalServerError, websocket.CloseInternalServerErr},
		{http.StatusBadGateway, websocket.CloseInternalServerErr},
		{http.StatusOK, websocket.CloseInternalServerErr},
	}
	for _, tc := range cases {
		if got := responsesWSCloseCodeForStatus(tc.status); got != tc.want {
			t.Errorf("responsesWSCloseCodeForStatus(%d) = %d, want %d", tc.status, got, tc.want)
		}
	}
}

// End to end: when the inbound WS receives a non-retryable response.failed before the first token (context_length_exceeded),
// the client should receive a structured error frame plus a ClosePolicyViolation close, rather than the raw failure frame plus
// a normal wrap-up (the latter would be counted by downstream relays/billers as one successful session). Deterministic client errors do not retry on another account.
func TestResponsesWebSocketNonRetryableFailureReturnsErrorClose(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.CodexWSSilentRetry = true
	nextSettings.CodexWSHideErrors = false
	nextSettings.CodexWSSilentRetries = 2
	ApplyRuntimeSettings(nextSettings)

	attemptCh := make(chan int64, 4)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		attemptCh <- account.ID()
		sse := `data: {"type":"response.created"}` + "\n\n" +
			`data: {"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"input too long"}}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.5"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.5","input":"hello"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, first, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read error frame: %v", err)
	}
	if eventType := gjson.GetBytes(first, "type").String(); eventType != "error" {
		t.Fatalf("first event type = %q, want \"error\" (the raw response.failed frame must not be passed through) body=%s", eventType, first)
	}
	if !strings.Contains(string(first), "input too long") {
		t.Fatalf("error frame should carry upstream message when hiding disabled: %s", first)
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("expected close %d (policy violation for deterministic 4xx), got err=%v", websocket.ClosePolicyViolation, err)
	}

	select {
	case <-attemptCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first attempt")
	}
	select {
	case got := <-attemptCh:
		t.Fatalf("unexpected retry on account %d for non-retryable failure", got)
	case <-time.After(100 * time.Millisecond):
	}
}
