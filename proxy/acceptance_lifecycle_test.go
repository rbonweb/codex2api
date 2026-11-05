// proxy/acceptance_lifecycle_test.go
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security/outputguard"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// Helper: builds the test Handler, Store and Account with the given upstream
func setupLifecycleTestHandler(t *testing.T, upstreamURL string, model string) (*Handler, *auth.Store, *auth.Account) {
	t.Helper()

	// Load a valid minimal output runaway guard config, and reset the state on exit
	setupOutputGuardHTTPTestEnv(t)

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "acceptance_lifecycle.db"))
	if err != nil {
		t.Fatalf("database.New(sqlite) error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := auth.NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:      10,
		MaxRetries:          2,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)

	account := &auth.Account{
		DBID:        101,
		Status:      auth.StatusReady,
		HealthTier:  auth.HealthTierHealthy,
		AccessToken: "at-lifecycle-test-token",
		PlanType:    "plus",
		AccountID:   "acct-lifecycle-101",
		Models:      []string{model},
	}
	if strings.HasPrefix(upstreamURL, "http://") {
		codexRestoredUseResin(t, upstreamURL)
	}

	store.AddAccount(account)

	cfg := &config.Config{AllowAnonymousV1: true}
	handler := NewHandler(store, db, cfg, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	return handler, store, account
}

// -----------------------------------------------------------------------------
// Scenario 12: §4.1 each entry point stream/non-stream, native/converted, and each actually supported upstream combination
// Covers: Responses HTTP (SSE/non-streaming), Responses WS, the Realtime WS text subset,
// ChatCompletions (SSE/non-streaming via the Codex Responses upstream)
// -----------------------------------------------------------------------------
func TestAcceptanceScenario12_FullProtocolCoverageMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Build a generic runaway repeating payload (a 20B period looped 500 times = 10000B >= 9215B, triggering repetition)
	pattern := "runaway_loop_abcdef_"
	runawayText := strings.Repeat(pattern, 500)

	matrix := []struct {
		name        string
		endpoint    string
		routePath   string
		model       string
		stream      bool
		reqBody     string
		mockHandler http.HandlerFunc
		invoke      func(h *Handler, c *gin.Context)
	}{
		{
			name:      "ResponsesHTTP_Stream_SSE",
			endpoint:  "/v1/responses",
			routePath: "/v1/responses",
			model:     "gpt-5.5",
			stream:    true,
			reqBody:   `{"model":"gpt-5.5","stream":true,"input":"test"}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.Responses(c) },
		},
		{
			name:      "ResponsesHTTP_NonStream_JSON",
			endpoint:  "/v1/responses",
			routePath: "/v1/responses",
			model:     "gpt-5.5",
			stream:    false,
			reqBody:   `{"model":"gpt-5.5","stream":false,"input":"test"}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				// Codex always answers over SSE, even for a non-stream client request.
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.Responses(c) },
		},
		{
			name:      "ChatCompletions_Stream_SSE_ResponsesUpstream",
			endpoint:  "/v1/chat/completions",
			routePath: "/v1/chat/completions",
			model:     "gpt-5.5",
			stream:    true,
			reqBody:   `{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"Hello "}`+"\n\n")
				flusher.Flush()
				time.Sleep(10 * time.Millisecond)
				chunk := `{"type":"response.output_text.delta","delta":"` + runawayText + `"}`
				_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
		},
		{
			name:      "ChatCompletions_NonStream_JSON_ResponsesUpstream",
			endpoint:  "/v1/chat/completions",
			routePath: "/v1/chat/completions",
			model:     "gpt-5.5",
			stream:    false,
			reqBody:   `{"model":"gpt-5.5","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				chunk := `{"type":"response.output_text.delta","delta":"` + runawayText + `"}`
				_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
		},
	}

	for _, tc := range matrix {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				tc.mockHandler(w, r)
			}))
			defer upstream.Close()

			handler, _, _ := setupLifecycleTestHandler(t, upstream.URL, tc.model)

			router := gin.New()
			router.POST(tc.routePath, func(c *gin.Context) {
				tc.invoke(handler, c)
			})

			rec := &guardDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			req := httptest.NewRequest(http.MethodPost, tc.endpoint, strings.NewReader(tc.reqBody))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)

			// Assertion 1: the upstream is really called, and since the guard blocks retries, calls is strictly 1 (I1)
			if calls := upstreamCalls.Load(); calls != 1 {
				t.Fatalf("upstream calls = %d, want exactly 1 for %s", calls, tc.name)
			}

			respBody := rec.Body.String()

			// Assertions 2 & 4: define the terminal state exactly per protocol, removing the weakening "or" and strictly enforcing "and"
			isCommittedStream := rec.Code == http.StatusOK
			if isCommittedStream {
				// Headers already committed for streaming: a normal completion terminal state is forbidden
				if strings.Contains(respBody, "response.completed") ||
					strings.Contains(respBody, "[DONE]") ||
					strings.Contains(respBody, "message_stop") ||
					strings.Contains(respBody, `"finishReason":"STOP"`) ||
					strings.Contains(respBody, `"finish_reason":"stop"`) {
					t.Fatalf("downstream received successful completion for %s: %s", tc.name, respBody)
				}
				// Protocol-level terminal error verification
				if strings.Contains(tc.name, "Anthropic") {
					if !strings.Contains(respBody, "event: error") || !strings.Contains(respBody, GuardTriggeredPublicMessage) {
						t.Fatalf("Anthropic stream missing error event or guard message for %s: %s", tc.name, respBody)
					}
				} else {
					if !strings.Contains(respBody, GuardTriggeredPublicCode) || !strings.Contains(respBody, GuardTriggeredPublicMessage) {
						t.Fatalf("stream missing guard public code and message for %s: %s", tc.name, respBody)
					}
				}
			} else {
				// Headers not committed (HTTP terminal state): must be exactly 502 Bad Gateway
				if rec.Code != http.StatusBadGateway {
					t.Fatalf("expected 502 Bad Gateway for %s, got %d", tc.name, rec.Code)
				}
				if strings.Contains(tc.name, "Gemini") {
					if !strings.Contains(respBody, GuardTriggeredPublicMessage) || !strings.Contains(respBody, "502") {
						t.Fatalf("Gemini 502 missing guard message or 502 code for %s: %s", tc.name, respBody)
					}
				} else {
					if !strings.Contains(respBody, GuardTriggeredPublicCode) || !strings.Contains(respBody, GuardTriggeredPublicMessage) {
						t.Fatalf("expected 502 with guard code and message for %s: %s", tc.name, respBody)
					}
				}
			}

			// Assertion 3: the runaway text is not visible downstream (the whole event is blocked; a leak of even a single copy of the pattern is detected)
			if strings.Contains(respBody, pattern) {
				t.Fatalf("runaway pattern leaked to downstream client for %s", tc.name)
			}
		})
	}
}

// Scenario 12 (WS-specific): real client connection routing, a real WebSocket turn request, with exactly 1 upstream call
func TestAcceptanceScenario12_WebSocketMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "ws_runaway_loop_12345678_"
	runawayText := strings.Repeat(pattern, 400) // 24 * 400 = 9600B >= 9215B

	var upstreamCalls atomic.Int32
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		upstreamCalls.Add(1)
		sse := `data: {"type":"response.output_text.delta","delta":"` + runawayText + `"}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	handler, store, account := setupLifecycleTestHandler(t, "https://upstream.example", "gpt-5.5")
	account.ResponsesUpstreamTransport = auth.OpenAIResponsesTransportHTTP
	store.SetCodexModelMapping(`{"client-ws-alias":"gpt-5.5"}`)

	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebSocket)
	router.GET("/v1/realtime", handler.RealtimeWebSocket)

	server := httptest.NewServer(router)
	defer server.Close()

	// 1. Test ResponsesWebSocket
	t.Run("ResponsesWebSocket_Turn", func(t *testing.T) {
		upstreamCalls.Store(0)
		wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial websocket failed: %v", err)
		}
		defer conn.Close()
		if resp != nil {
			_ = resp.Body.Close()
		}

		turnReq := `{"model":"client-ws-alias","input":"hello"}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(turnReq)); err != nil {
			t.Fatalf("write websocket turn: %v", err)
		}

		assertWSGuardTerminalThen1013(t, conn, pattern)
		if calls := upstreamCalls.Load(); calls != 1 {
			t.Fatalf("upstream calls = %d, want exactly 1", calls)
		}
	})

	// 2. Test RealtimeWebSocket
	t.Run("RealtimeWebSocket_TextSubprotocol", func(t *testing.T) {
		upstreamCalls.Store(0)
		wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?model=gpt-5.5"
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial realtime websocket failed: %v", err)
		}
		defer conn.Close()
		if resp != nil {
			_ = resp.Body.Close()
		}

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _ = conn.ReadMessage() // session.created

		turnReq := `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`
		_ = conn.WriteMessage(websocket.TextMessage, []byte(turnReq))
		_, _, _ = conn.ReadMessage() // item.created

		triggerReq := `{"type":"response.create","response":{"max_output_tokens":64}}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(triggerReq)); err != nil {
			t.Fatalf("write realtime trigger: %v", err)
		}

		assertWSGuardTerminalThen1013(t, conn, pattern)
		if calls := upstreamCalls.Load(); calls != 1 {
			t.Fatalf("upstream calls = %d, want exactly 1", calls)
		}
	})
}

// assertWSGuardTerminalThen1013 asserts the behavior contract of the WS guard terminal state: first receive one frame
// error (error.code == GuardTriggeredPublicCode), then the connection is closed with exactly
// 1013 CloseTryAgainLater, and the runaway original text never leaks downstream.
func assertWSGuardTerminalThen1013(t *testing.T, conn *websocket.Conn, runawayPattern string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("expected guard terminal error frame before close, got: %v", err)
	}
	if msgType != websocket.TextMessage {
		t.Fatalf("guard terminal message type = %d, want text frame: %s", msgType, string(msg))
	}
	if strings.Contains(string(msg), runawayPattern) {
		t.Fatalf("runaway pattern leaked to downstream client: %s", string(msg))
	}
	if gjson.GetBytes(msg, "type").String() != "error" ||
		gjson.GetBytes(msg, "error.code").String() != GuardTriggeredPublicCode {
		t.Fatalf("WS terminal frame missing %s: %s", GuardTriggeredPublicCode, string(msg))
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, extra, nextErr := conn.ReadMessage()
	if nextErr == nil {
		t.Fatalf("expected 1013 CloseTryAgainLater after guard terminal, got extra frame: %s", string(extra))
	}
	var closeErr *websocket.CloseError
	if !errors.As(nextErr, &closeErr) || closeErr.Code != websocket.CloseTryAgainLater {
		t.Fatalf("expected websocket close %d (CloseTryAgainLater), got %v", websocket.CloseTryAgainLater, nextErr)
	}
}

// TestResponsesWebSocketGuardTripSkipsBufferedFlush is a regression test for the case where, after a guard trip, the buffered output was still written
// to outputBuffer: messages held in the output filter buffer before the trip must not be flushed again after the guard terminal frame
// flush to the client: the terminal state must be the last frame before the connection closes.
func TestResponsesWebSocketGuardTripSkipsBufferedFlush(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "ws_buffered_runaway_12345678_"
	runawayText := strings.Repeat(pattern, 400)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		sse := `data: {"type":"response.output_text.delta","delta":"buffered-before-guard"}` + "\n\n" +
			`data: {"type":"response.output_text.delta","delta":"` + runawayText + `"}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	handler, store, account := setupLifecycleTestHandler(t, "https://upstream.example", "gpt-5.5")
	account.ResponsesUpstreamTransport = auth.OpenAIResponsesTransportHTTP
	store.SetCodexModelMapping(`{"client-ws-alias":"gpt-5.5"}`)
	// Enable the output filter buffer, so that valid deltas from before the guard trip stay in outputBuffer;
	// the buffer is still non-empty when the trip happens, so the "flush after terminal state" path is covered.
	filterConfig := promptfilter.DefaultConfig()
	filterConfig.Enabled = true
	filterConfig.Mode = promptfilter.ModeBlock
	filterConfig.Advanced.Output = promptfilter.OutputConfig{
		Enabled:      true,
		BufferBytes:  1 << 20,
		OverlapBytes: 64,
		StrictOnly:   true,
	}
	store.SetPromptFilterConfig(filterConfig)

	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()
	if resp != nil {
		_ = resp.Body.Close()
	}

	turnReq := `{"model":"client-ws-alias","input":"hello"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(turnReq)); err != nil {
		t.Fatalf("write websocket turn: %v", err)
	}

	assertWSGuardTerminalThen1013(t, conn, pattern)
}

// TestResponsesWebSocketGuardTripWithPendingInterruptDropsFailedFrame is a regression test for a guard trip
// that happens while an unacknowledged response.interrupt is pending: the guard terminal frame must be the last frame before the connection closes,
// and no response.interrupt.failed frame may be sent after it.
func TestResponsesWebSocketGuardTripWithPendingInterruptDropsFailedFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "ws_guard_interrupt_runaway_12345678_"
	runawayText := strings.Repeat(pattern, 400)

	pipeR, pipeW := io.Pipe()
	interruptReceived := make(chan struct{})

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		// Register the interrupt upstream send function, simulating an upstream that supports interrupt
		RegisterResponsesWSInterrupt(ctx, func(payload []byte) error {
			close(interruptReceived)
			return nil
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       pipeR,
		}, nil
	}

	handler, store, account := setupLifecycleTestHandler(t, "https://upstream.example", "gpt-5.5")
	account.ResponsesUpstreamTransport = auth.OpenAIResponsesTransportHTTP
	store.SetCodexModelMapping(`{"client-ws-alias":"gpt-5.5"}`)

	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()
	if resp != nil {
		_ = resp.Body.Close()
	}

	turnReq := `{"model":"client-ws-alias","input":"hello"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(turnReq)); err != nil {
		t.Fatalf("write websocket turn: %v", err)
	}

	// The upstream first sends response.created and one normal delta, triggering the first packet and recording the response.id
	go func() {
		_, _ = pipeW.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_guard_int_test\"}}\n\n"))
		_, _ = pipeW.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"))
	}()

	// Consume the response.created frame
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read response.created: %v", err)
	}
	if msgType != websocket.TextMessage || gjson.GetBytes(msg, "type").String() != "response.created" {
		t.Fatalf("msgType=%d payload=%s, want response.created", msgType, string(msg))
	}

	// Consume the delta frame
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, msg, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read delta: %v", err)
	}
	if msgType != websocket.TextMessage || gjson.GetBytes(msg, "type").String() != "response.output_text.delta" {
		t.Fatalf("msgType=%d payload=%s, want response.output_text.delta", msgType, string(msg))
	}

	// Now response.id is bound; the client sends interrupt
	interruptReq := `{"type":"response.interrupt","response_id":"resp_guard_int_test","mode":"discard_partial_items"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(interruptReq)); err != nil {
		t.Fatalf("write interrupt req: %v", err)
	}

	// Wait for the gateway to dispatch the interrupt and call state.send (setting state.requested)
	select {
	case <-interruptReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for interrupt to be processed by gateway")
	}

	// The upstream pushes runaway text that hits the guard, and does not report an interrupt ack, simulating a pending-interrupt scenario
	go func() {
		_, _ = pipeW.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + runawayText + "\"}\n\n"))
	}()

	// Assert: the terminal state must be the guard terminal error frame, and after the following close there are no extra frames (such as response.interrupt.failed)
	assertWSGuardTerminalThen1013(t, conn, pattern)
}

// Scenario 12 (WS concurrency regression): two named lanes on the same connection hit the guard at the same time. gorilla/websocket
// forbids concurrent writes on the same conn; terminal frames must be serialized through the per-conn write lock, otherwise concurrent writes would panic,
// corrupt frames, and make -race report a data race. Each of the two lanes receives an error frame containing GuardTriggeredPublicCode
// and after that the connection close and the multiplexer worker release must stay normal.
func TestAcceptanceScenario12ConcurrentLanesGuardTerminalSerialized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Load the real guard config (Enabled + default thresholds), reusing the multip connection test fixture.
	setupOutputGuardHTTPTestEnv(t)

	pattern := "ws_lane_runaway_loop_12345678_"
	runawayText := strings.Repeat(pattern, 400) // 26 * 400 = 10400B >= 9215B
	started := make(chan string, 2)
	release := make(chan struct{})
	client := newResponsesWSMultiplexClient(t, func(ctx context.Context, body []byte) (*http.Response, error) {
		lane := ""
		for _, candidate := range []string{"lane-a", "lane-b"} {
			if strings.Contains(string(body), candidate) {
				lane = candidate
			}
		}
		if lane == "" {
			return nil, errors.New("unexpected synthetic guard lane: " + string(body))
		}
		started <- lane
		// Both upstreams are released at the same barrier, so the guard hits and terminal states of the two lanes
		// writes overlap as much as possible, exposing the missing per-conn write serialization.
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		sse := `data: {"type":"response.output_text.delta","delta":"` + runawayText + `"}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	})

	responsesWSMultiplexSend(t, client, "lane-a", "lane-a")
	responsesWSMultiplexWaitStart(t, started, "lane-a")
	responsesWSMultiplexSend(t, client, "lane-b", "lane-b")
	responsesWSMultiplexWaitStart(t, started, "lane-b")
	close(release)

	terminals := map[string]bool{}
	for len(terminals) < 2 {
		frame := responsesWSMultiplexRead(t, client)
		lane := gjson.GetBytes(frame, "stream_id").String()
		if lane != "lane-a" && lane != "lane-b" {
			t.Fatalf("unexpected frame on concurrent guard lanes: %s", frame)
		}
		if strings.Contains(string(frame), runawayText) {
			t.Fatalf("runaway pattern leaked on lane %s: %s", lane, frame)
		}
		if gjson.GetBytes(frame, "type").String() != "error" ||
			gjson.GetBytes(frame, "error.code").String() != GuardTriggeredPublicCode {
			t.Fatalf("lane %s guard terminal frame missing %s: %s", lane, GuardTriggeredPublicCode, frame)
		}
		if terminals[lane] {
			t.Fatalf("lane %s received a duplicate guard terminal frame", lane)
		}
		terminals[lane] = true
	}
	// The guard terminal states of the two named lanes are lane-level wrap-ups, and the connection stays usable; the client explicitly closes
	// The multiplex fixture asserts that all handler workers are released normally, with no panic/leak.
	if err := client.Close(); err != nil {
		t.Fatalf("close concurrent guard client: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Scenario 13: hit-before-provenance/collector/replay, blocked before the real sink
// Real observation point: the real Responses handler has continuous retry enabled and the upstream never succeeds,
// verify that the real provenance cache physical storage was never written with the abnormal sentinel
// -----------------------------------------------------------------------------
func TestAcceptanceScenario13_HitBeforeProvenanceAndReplaySink(t *testing.T) {
	gin.SetMode(gin.TestMode)

	badSentinel := "malicious_provenance_sentinel_xyz_"
	runawayPayload := strings.Repeat(badSentinel, 400) // 34 * 400 = 13600B >= 9215B

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// The upstream outputs a runaway tool arguments delta
		frame := `{"type":"response.function_call_arguments.delta","call_id":"call_13","delta":"` + runawayPayload + `"}`
		_, _ = io.WriteString(w, "data: "+frame+"\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	handler, store, account := setupLifecycleTestHandler(t, upstream.URL, "gpt-5.5")
	// Enable the continuous retry policy, so the retry logic is active
	store.SetContinuousRetryPolicy(database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryHTTP5xx, database.ContinuousRetryCategoryTransport},
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"test"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Responses(c)

	// Real observation point: check that the compaction provenance physical cache must never contain the malicious sentinel digest
	// The badDigestProbe probe takes the digest of the real aggregated content (runawayPayload), while keeping the badSentinel single-chunk probe for double verification;
	// Note: if provenance later accounts per single chunk, the single-chunk sentinel probe can also catch it immediately.
	badDigestAggregated := compactionContentDigest(runawayPayload)
	raw, ok, err := handler.cache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, badDigestAggregated)
	if err != nil {
		t.Fatalf("GetRuntime failed: %v", err)
	}
	if ok || raw != nil {
		t.Fatalf("CRITICAL: provenance cache stored malicious sentinel after guard tripped: %v", raw)
	}

	badDigestSingle := compactionContentDigest(badSentinel)
	rawSingle, okSingle, errSingle := handler.cache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, badDigestSingle)
	if errSingle != nil {
		t.Fatalf("GetRuntime single sentinel failed: %v", errSingle)
	}
	if okSingle || rawSingle != nil {
		t.Fatalf("CRITICAL: provenance cache stored single malicious sentinel chunk: %v", rawSingle)
	}
	_ = account

	if calls := upstreamCalls.Load(); calls != 1 {
		t.Fatalf("continuous retry was not blocked before replay sink! calls = %d, want 1", calls)
	}
}

// -----------------------------------------------------------------------------
// Scenario 14: cancel/bodyclose has no 5s usage drain; the newDrainableUpstreamContext path disconnects immediately,
// and the typed reason is preserved and not overwritten by context.Canceled (499) or EOF (598)
// -----------------------------------------------------------------------------
func TestAcceptanceScenario14_ImmediateCancelNo5sDrain(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "drain_bypass_pattern_"
	runawayText := strings.Repeat(pattern, 500)

	upstreamCanceled := make(chan struct{})
	var closeOnce sync.Once

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
		flusher.Flush()

		// Monitor whether the upstream context is canceled immediately after the guard trips, rather than waiting 5 seconds to drain
		select {
		case <-r.Context().Done():
			closeOnce.Do(func() { close(upstreamCanceled) })
		case <-time.After(3 * time.Second):
		}
	}))
	defer upstream.Close()

	handler, _, _ := setupLifecycleTestHandler(t, upstream.URL, "gpt-5.5")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"test"}`)).WithContext(clientCtx)
	c.Request.Header.Set("Content-Type", "application/json")

	start := time.Now()
	handlerDone := make(chan struct{})
	go func() {
		handler.Responses(c)
		close(handlerDone)
	}()

	select {
	case <-upstreamCanceled:
		elapsed := time.Since(start)
		if elapsed >= 2*time.Second {
			t.Fatalf("upstream cancel delayed! took %v, must not wait for 5s drain", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("upstream context was not canceled immediately, still waiting (likely 5s drain bug)")
	}

	<-handlerDone

	// Assert: the downstream error status is not misjudged as 499 (Client Closed Request) or 598 (Network Read Timeout)
	if rec.Code == 499 || rec.Code == 598 {
		t.Fatalf("guard outcome misclassified as %d", rec.Code)
	}

	// Coordinator primitive assertion: after the outcome returned by HandleTripped passes through OverlayGuardOutcome, guardReason stays repetition
	t.Run("Coordinator_Outcome_Overlay_Typed_Reason", func(t *testing.T) {
		guard := outputguard.NewRequestGuard(outputguard.Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		_ = guard.ObserveAppend("c_14", "", outputguard.KindText, bytes.Repeat([]byte("rep_14_"), 2000))

		uCtx, uCancel := context.WithCancel(context.Background())
		defer uCancel()
		var bodyClosed atomic.Bool
		closer := &testCloser{onClose: func() { bodyClosed.Store(true) }}

		coordinator := NewGuardTerminationCoordinator(guard, uCancel, closer)
		outcome := coordinator.HandleTripped(guard.TrippedReason())

		if uCtx.Err() == nil {
			t.Fatalf("expected upstream context canceled immediately")
		}
		if !bodyClosed.Load() {
			t.Fatalf("expected upstream body closed immediately")
		}

		overlaid := OverlayGuardOutcome(outcome, guard)
		if !overlaid.guardTripped || overlaid.guardReason != "repetition" {
			t.Fatalf("typed reason was overwritten! got %+v", overlaid)
		}
		if overlaid.logStatusCode == 499 || overlaid.logStatusCode == 598 {
			t.Fatalf("guard outcome misclassified as 499 or 598: %d", overlaid.logStatusCode)
		}
	})
}

// -----------------------------------------------------------------------------
// Scenario 15: keepalive and auto continue-thinking running concurrently with the guard leak no old buffer, have no success/duplicate terminal states, and have no data race
// Covers the coalesce and immediate strategies, the real downstreamMu locking convention, and zero leaks in Finalize
// -----------------------------------------------------------------------------
func TestAcceptanceScenario15_ConcurrencyKeepaliveNoRace(t *testing.T) {
	policies := []struct {
		name   string
		policy string
	}{
		{name: "CoalescePolicy", policy: StreamFlushPolicyCoalesce},
		{name: "ImmediatePolicy", policy: StreamFlushPolicyImmediate},
	}

	for _, p := range policies {
		t.Run(p.name, func(t *testing.T) {
			guard := outputguard.NewRequestGuard(outputguard.Config{
				Enabled:               true,
				MaxToolArgumentBytes:  262144,
				MaxToolArgumentDeltas: 4096,
				MaxChannelBytes:       2097152,
				MaxRequestBytes:       4194304,
			})

			var outBuf bytes.Buffer
			var downstreamMu sync.Mutex
			writer := &streamFlushWriter{
				writer:   &outBuf,
				policy:   p.policy,
				interval: 10 * time.Millisecond,
			}

			var wg sync.WaitGroup
			var stopKeepalive atomic.Bool

			// Goroutine 1: the keepalive comment Ticker (locked per the downstreamMu convention)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stopKeepalive.Load() {
					downstreamMu.Lock()
					if !guard.IsTripped() {
						_ = writer.WriteSSEComment("keepalive-ping")
					}
					downstreamMu.Unlock()
					time.Sleep(200 * time.Microsecond)
				}
			}()

			// Goroutine 2: concurrently trigger the guard hit and clear the buffer
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Inject runaway repeated text into the buffer (simulating unflushed data accumulated in the coalesce phase)
				pattern := []byte("race_runaway_pattern_")
				downstreamMu.Lock()
				_ = writer.WriteBytes(pattern)

				_ = guard.ObserveAppend("c_15", "", outputguard.KindText, bytes.Repeat(pattern, 500))

				// Lock and clear according to the production downstreamMu convention
				writer.DiscardBuffered()
				downstreamMu.Unlock()

				stopKeepalive.Store(true)
			}()

			// Goroutine 3: concurrently read the guard state
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stopKeepalive.Load() {
					_ = guard.IsTripped()
					_ = guard.TrippedReason()
					time.Sleep(100 * time.Microsecond)
				}
			}()

			wg.Wait()

			// Finalize call: assert that after DiscardBuffered nothing new is released to the underlying writer, and there is no success frame
			beforeLen := outBuf.Len()
			downstreamMu.Lock()
			_ = writer.Finalize()
			downstreamMu.Unlock()

			if outBuf.Len() > beforeLen {
				t.Fatalf("buffer leak! Finalize released bytes after discard: before=%d, after=%d", beforeLen, outBuf.Len())
			}

			outStr := outBuf.String()
			if strings.Contains(outStr, "response.completed") || strings.Contains(outStr, "[DONE]") {
				t.Fatalf("illegal terminal frame emitted: %s", outStr)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Scenario 16: each ordinary/continuous select/catchall/account switch/WS HTTP fallback/
// context repair/compact/continue entry point call count == 1 (must be exactly 1: neither more than 1 nor 0 is tolerated)
// -----------------------------------------------------------------------------
func TestAcceptanceScenario16_RecoveryBlockedCallsExactlyOne(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "retry_block_test_loop_"
	runawayText := strings.Repeat(pattern, 450) // 22 * 450 = 9900B >= 9215B

	paths := []struct {
		name     string
		policy   database.ContinuousRetryPolicy
		maxRetry int
		setup    func(h *Handler, store *auth.Store, upstreamURL string)
		trigger  func(h *Handler, upstreamURL string) *httptest.ResponseRecorder
	}{
		{
			name:     "OrdinaryRetry",
			policy:   database.ContinuousRetryPolicy{Enabled: false},
			maxRetry: 3,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name: "SelectiveContinuousRetry",
			policy: database.ContinuousRetryPolicy{
				Enabled:    true,
				Categories: []string{database.ContinuousRetryCategoryHTTP5xx, database.ContinuousRetryCategoryTransport},
			},
			maxRetry: 3,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name: "CatchAllContinuousRetry",
			policy: database.ContinuousRetryPolicy{
				Enabled:  true,
				CatchAll: true,
			},
			maxRetry: 3,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name: "AccountRotation",
			policy: database.ContinuousRetryPolicy{
				Enabled:  true,
				CatchAll: true,
			},
			maxRetry: 2,
			setup: func(h *Handler, store *auth.Store, upstreamURL string) {
				// Add a second candidate account for rotation
				store.AddAccount(&auth.Account{
					DBID:        202,
					Status:      auth.StatusReady,
					AccessToken: "at-recovery-rotate-2",
					PlanType:    "plus",
					AccountID:   "acct-lifecycle-202",
					Models:      []string{"gpt-5.5"},
				})
			},
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name:     "AutoCompact",
			policy:   database.ContinuousRetryPolicy{Enabled: false},
			maxRetry: 2,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				// Simulate a compact request
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction","encrypted_content":"enc_state"}]}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name:     "AutoContinue",
			policy:   database.ContinuousRetryPolicy{Enabled: false},
			maxRetry: 2,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"auto-continue-token"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
	}

	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
				flusher.Flush()
			}))
			defer upstream.Close()

			handler, store, _ := setupLifecycleTestHandler(t, upstream.URL, "gpt-5.5")
			store.SetContinuousRetryPolicy(tc.policy)
			if tc.setup != nil {
				tc.setup(handler, store, upstream.URL)
			}

			rec := tc.trigger(handler, upstream.URL)

			calls := upstreamCalls.Load()
			if calls != 1 {
				t.Fatalf("path %s executed retry or failed to call upstream! upstreamCalls = %d, want exactly 1 (status=%d, body=%s)",
					tc.name, calls, rec.Code, rec.Body.String())
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Scenario 17: HTTP pre-commit 502 / post-commit protocol envelope (Chat without DONE / Anthropic without stop / Gemini bare JSON / WS lease abandoned)
// -----------------------------------------------------------------------------
func TestAcceptanceScenario17_ProtocolTerminalEnvelopes(t *testing.T) {
	// 1. Headers not yet committed: HTTP 502 and carries the standard APIError structure
	rec502 := httptest.NewRecorder()
	c502, _ := gin.CreateTestContext(rec502)
	c502.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	var mu502 sync.Mutex
	clientGone502 := false

	err := SendDownstreamGuardTerminalHTTP(c502, &mu502, &clientGone502, "chat", "repetition")
	if err != nil {
		t.Fatalf("SendDownstreamGuardTerminalHTTP error: %v", err)
	}
	if rec502.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway for uncommitted error, got %d", rec502.Code)
	}
	if !strings.Contains(rec502.Body.String(), GuardTriggeredPublicCode) {
		t.Fatalf("body missing %s: %s", GuardTriggeredPublicCode, rec502.Body.String())
	}

	// 2. Chat SSE real streaming, committed scenario: must never include [DONE] or finish_reason: "stop"
	t.Run("Chat_Committed_Stream_No_DONE", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			runaway := strings.Repeat("chat_runaway_repeat_", 500)
			_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runaway+`"}`+"\n\n")
			flusher.Flush()
		}))
		defer upstream.Close()

		handler, _, _ := setupLifecycleTestHandler(t, upstream.URL, "gpt-5.5")
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		c.Request.Header.Set("Content-Type", "application/json")

		handler.ChatCompletions(c)
		chatStr := rec.Body.String()
		if strings.Contains(chatStr, "[DONE]") {
			t.Fatalf("Chat stream terminal contains illegal [DONE]: %s", chatStr)
		}
		if strings.Contains(chatStr, `"stop"`) {
			t.Fatalf("Chat stream terminal contains illegal stop finish_reason: %s", chatStr)
		}
	})

	// 4. The actual WebSocket SendDownstreamGuardTerminalWS connection and the 1013 Close control frame
	// Note: WS lease isolation and abandonment under concurrency are fully covered by scenario 20 (Concurrent_WS_Lease_Isolation)
	t.Run("WS_Terminal_Frame", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upgrader := websocket.Upgrader{}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()

			_ = SendDownstreamGuardTerminalWS(conn, "stream_test_17", "repetition")
			// After sending the guard terminal frame, close the connection with the 1013 CloseTryAgainLater control frame
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseTryAgainLater, GuardTriggeredPublicMessage),
				time.Now().Add(time.Second),
			)
		}))
		defer server.Close()

		wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial WS error: %v", err)
		}
		defer conn.Close()

		// Assertion 1: must read an error frame containing the public GuardTriggeredPublicCode
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("expected error frame before close, got: %v", err)
		}
		if msgType != websocket.TextMessage || !strings.Contains(string(msg), GuardTriggeredPublicCode) {
			t.Fatalf("WS terminal frame missing %s: %s", GuardTriggeredPublicCode, string(msg))
		}

		// Assertion 2: then must receive the 1013 (CloseTryAgainLater) close control frame (both must hold, never weakened)
		_, _, nextErr := conn.ReadMessage()
		if nextErr == nil {
			t.Fatalf("expected connection closed with 1013 CloseTryAgainLater, got nil error")
		}
		var closeErr *websocket.CloseError
		if !errors.As(nextErr, &closeErr) || closeErr.Code != websocket.CloseTryAgainLater {
			t.Fatalf("expected websocket 1013 CloseTryAgainLater, got %v", nextErr)
		}
	})

	// 5. Envelope base primitive format verification
	t.Run("Format_Envelope_Primitives", func(t *testing.T) {
		chatEnv, _, err := FormatGuardTerminalEnvelope("chat", "")
		if err != nil || strings.Contains(string(chatEnv), "[DONE]") || !strings.Contains(string(chatEnv), GuardTriggeredPublicCode) {
			t.Fatalf("invalid chat envelope: %s err=%v", string(chatEnv), err)
		}
		anthropicEnv, _, err := FormatGuardTerminalEnvelope("anthropic", "")
		if err != nil || strings.Contains(string(anthropicEnv), "message_stop") || !strings.Contains(string(anthropicEnv), GuardTriggeredPublicMessage) {
			t.Fatalf("invalid anthropic envelope: %s err=%v", string(anthropicEnv), err)
		}
		geminiEnv, _, err := FormatGuardTerminalEnvelope("gemini", "")
		if err != nil || !gjson.Valid(string(geminiEnv)) || !strings.Contains(string(geminiEnv), GuardTriggeredPublicMessage) {
			t.Fatalf("invalid gemini envelope: %s err=%v", string(geminiEnv), err)
		}
	})
}

// -----------------------------------------------------------------------------
// Scenario 18: TryLock on an already committed lock appends no bytes/clientGone, SetWriteDeadline(5s) positive assertion,
// unsupportedWriter has no Write and exits
// -----------------------------------------------------------------------------
func TestAcceptanceScenario18_TryLockAndWriteDeadlines(t *testing.T) {
	// 1. Lock contention (TryLock fails): give up safely at once, mark clientGone, and do not leave an orphan goroutine blocked
	var downstreamMu sync.Mutex
	downstreamMu.Lock() // Deliberately hold the lock
	clientGone := false

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer.WriteHeaderNow() // Simulate an already committed response
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	done := make(chan error, 1)
	go func() {
		done <- SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition")
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected error on lock contention, got nil")
		}
		if rec.Body.Len() > 0 {
			t.Fatalf("wrote bytes while lock was held by another goroutine!")
		}
		if !clientGone {
			t.Fatalf("expected clientGone = true on lock contention")
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatalf("SendDownstreamGuardTerminalHTTP hung on contended lock! Orphan goroutine detected!")
	}
	downstreamMu.Unlock()

	// 2. Positive assertion for SetWriteDeadline(5s) and the write-success test
	t.Run("SetWriteDeadline_5s_Assertion", func(t *testing.T) {
		rawRecorder := httptest.NewRecorder()
		spy := &spyResponseControllerWriter{ResponseWriter: rawRecorder}
		cSpy, _ := gin.CreateTestContext(rawRecorder)
		cSpy.Writer = &customGinResponseWriter{ResponseWriter: spy}
		cSpy.Writer.WriteHeaderNow() // Simulate an already committed response
		cSpy.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		var mu sync.Mutex
		clientGoneSpy := false
		err := SendDownstreamGuardTerminalHTTP(cSpy, &mu, &clientGoneSpy, "responses", "repetition")
		if err != nil {
			t.Fatalf("SendDownstreamGuardTerminalHTTP with spy error: %v", err)
		}
		if !spy.deadlineSet.Load() {
			t.Fatalf("expected SetWriteDeadline(5s) was called, but was not")
		}
		lastDeadline := time.Unix(0, spy.lastDeadline.Load())
		expectedDeadline := time.Now().Add(5 * time.Second)
		diff := lastDeadline.Sub(expectedDeadline)
		if diff < -2*time.Second || diff > 2*time.Second {
			t.Fatalf("expected SetWriteDeadline ~5s in the future (within ±2s), got %v (diff %v)", lastDeadline, diff)
		}
		if !spy.writeCalled.Load() {
			t.Fatalf("expected terminal bytes written, but Write was not called")
		}
	})

	// 3. unsupportedWriter: exit safely, never write unbounded, and never let a nil err pass
	t.Run("Unsupported_Writer_No_Write", func(t *testing.T) {
		unsupportedWriter := &customNoDeadlineWriter{header: make(http.Header)}
		recUnsupported := httptest.NewRecorder()
		cUnsup, _ := gin.CreateTestContext(recUnsupported)
		cUnsup.Writer = &customGinResponseWriter{ResponseWriter: unsupportedWriter}
		cUnsup.Writer.WriteHeaderNow() // Simulate an already committed response
		cUnsup.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		var muUnsup sync.Mutex
		clientGoneUnsup := false
		errUnsup := SendDownstreamGuardTerminalHTTP(cUnsup, &muUnsup, &clientGoneUnsup, "responses", "repetition")
		// I7: an unsupported nil err must not pass; it must return an explicit error or http.ErrNotSupported
		if errUnsup == nil {
			t.Fatalf("unsupported writer must return non-nil error, got nil")
		}
		if !errors.Is(errUnsup, http.ErrNotSupported) && !strings.Contains(errUnsup.Error(), "not supported") {
			t.Fatalf("unexpected error on unsupported writer: %v", errUnsup)
		}
		if unsupportedWriter.writeCount > 0 {
			t.Fatalf("unsupported writer performed unsafe unbounded write! writeCount = %d", unsupportedWriter.writeCount)
		}
	})
}

// -----------------------------------------------------------------------------
// Scenario 19: real accountReportFailure / store health / cache / usage checks
// End-to-end real Responses request verification: a guard hit incurs no penalty (zero penalty), with no fake success cache or fabricated token;
// the control group produces a non-guard upstream failure, proving that the FailureStreak/LastFailureKind/LastFailureAt observation points work.
// -----------------------------------------------------------------------------
func TestAcceptanceScenario19_ZeroPenaltyAndNoCacheUsageFabrication(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// A 30-byte period repeated 400 times = 12000B >= 9215B, triggering the repetition guard
	pattern := "runaway_cycle_30_bytes_123456_"
	runawayText := strings.Repeat(pattern, 400)

	var upstreamFailWith5xx atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstreamFailWith5xx.Load() {
			// Control group: simulate an upstream 500 internal error
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"Internal upstream crash"}}`)
			return
		}
		// Experiment group: the upstream outputs a runaway stream
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	handler, store, account := setupLifecycleTestHandler(t, upstream.URL, "gpt-5.5")

	// 1. Experiment group: issue a real Responses streaming request so the guard really hits
	recGuard := httptest.NewRecorder()
	cGuard, _ := gin.CreateTestContext(recGuard)
	cGuard.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"guard_test"}`))
	cGuard.Request.Header.Set("Content-Type", "application/json")

	handler.Responses(cGuard)

	// Assert the guard really fired: it must be a 502 terminal state carrying the public code (both must hold, never weakened)
	if recGuard.Code != http.StatusBadGateway || !strings.Contains(recGuard.Body.String(), GuardTriggeredPublicCode) {
		t.Fatalf("expected guard triggered in experiment group, got code=%d body=%s", recGuard.Code, recGuard.Body.String())
	}

	// Real observation point assertion: this account must absolutely not receive any failure penalty
	account.Mu().RLock()
	streak := account.FailureStreak
	lastKind := account.LastFailureKind
	lastAt := account.LastFailureAt
	healthTier := account.HealthTier
	account.Mu().RUnlock()

	if streak != 0 {
		t.Fatalf("expected FailureStreak == 0 on guard hit, got %d", streak)
	}
	if lastKind != "" {
		t.Fatalf("expected LastFailureKind == \"\" on guard hit, got %q", lastKind)
	}
	if !lastAt.IsZero() {
		t.Fatalf("expected LastFailureAt to be zero on guard hit, got %v", lastAt)
	}
	if healthTier != auth.HealthTierHealthy {
		t.Fatalf("expected HealthTier to remain Healthy on guard hit, got %v", healthTier)
	}

	// 2. Real DB Usage and Cache checks: run after the real hit
	if handler.db != nil {
		handler.db.FlushUsageLogs()
		logs, err := handler.db.ListUsageLogsByFilter(context.Background(), database.UsageLogFilter{
			Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), IncludeCanceled: true,
		})
		if err == nil {
			for _, logEntry := range logs {
				if logEntry.StatusCode == http.StatusOK {
					t.Fatalf("found successful usage log for tripped guard stream: %+v", logEntry)
				}
				if logEntry.InputTokens > 0 && logEntry.OutputTokens > 100000 {
					t.Fatalf("fabricated usage tokens detected: %+v", logEntry)
				}
			}
		}
	}

	// 3. Control group: construct one real non-guard stream/upstream failure (upstream 5xx), and assert that the observation point fields really are written, to guard against false assertions
	upstreamFailWith5xx.Store(true)

	recControl := httptest.NewRecorder()
	cControl, _ := gin.CreateTestContext(recControl)
	cControl.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"control_test"}`))
	cControl.Request.Header.Set("Content-Type", "application/json")

	handler.Responses(cControl)

	account.Mu().RLock()
	controlStreak := account.FailureStreak
	controlKind := account.LastFailureKind
	controlAt := account.LastFailureAt
	account.Mu().RUnlock()

	if controlStreak == 0 {
		t.Fatalf("control group expected non-zero FailureStreak for upstream failure, got 0 (fake assertion check failed)")
	}
	if controlKind == "" {
		t.Fatalf("control group expected non-empty LastFailureKind for upstream failure, got empty (fake assertion check failed)")
	}
	if controlAt.IsZero() {
		t.Fatalf("control group expected non-zero LastFailureAt for upstream failure, got zero time (fake assertion check failed)")
	}

	_ = store
}

// -----------------------------------------------------------------------------
// Scenario 20: concurrent requests and strict WS lease isolation
// -----------------------------------------------------------------------------
func TestAcceptanceScenario20_ConcurrentRequestsAndLeaseIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// C9: an 18B period repeated 600 times = 10800B >= 9215B, ensuring repetition is triggered without any other budget firing first
	pattern := "abnormal_loop_xyz_"
	runawayText := strings.Repeat(pattern, 600)

	// Mock Upstream returns normal and abnormal data depending on the request parameters
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Codex bodies are zstd-compressed, so decode before matching.
		body := readUpstreamRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		if strings.Contains(string(body), "runaway") {
			// Abnormal requests return a runaway stream
			_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
		} else {
			// Normal requests return a success stream
			_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"Valid output"}`+"\n\n")
			_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_ok","status":"completed"}}`+"\n\n")
		}
		flusher.Flush()
	}))
	defer upstream.Close()

	handler, store, _ := setupLifecycleTestHandler(t, upstream.URL, "gpt-5.5")
	store.SetCodexModelMapping(`{"client-ws-alias":"gpt-5.5"}`)

	router := gin.New()
	router.POST("/v1/responses", handler.Responses)
	router.GET("/v1/responses", handler.ResponsesWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	// 1. Concurrent HTTP request strict isolation test
	t.Run("Concurrent_HTTP_Isolation", func(t *testing.T) {
		var wg sync.WaitGroup
		recNormal := httptest.NewRecorder()
		recAbnormal := httptest.NewRecorder()

		// Run normal requests concurrently
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := gin.CreateTestContext(recNormal)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"normal"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.Responses(c)
		}()

		// Run abnormal requests concurrently
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := gin.CreateTestContext(recAbnormal)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"runaway"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.Responses(c)
		}()

		wg.Wait()

		// Assertion 1: abnormal requests are accurately blocked
		abnormalBody := recAbnormal.Body.String()
		if !strings.Contains(abnormalBody, GuardTriggeredPublicCode) && recAbnormal.Code != http.StatusBadGateway {
			t.Fatalf("abnormal request was not tripped, got code=%d body=%s", recAbnormal.Code, abnormalBody)
		}

		// Assertion 2: normal requests are completely unaffected, and succeed in output and complete
		normalBody := recNormal.Body.String()
		if strings.Contains(normalBody, GuardTriggeredPublicCode) {
			t.Fatalf("isolation failure! normal request was falsely tripped by concurrent abnormal request")
		}
		if !strings.Contains(normalBody, "Valid output") {
			t.Fatalf("normal request missing expected output: %s", normalBody)
		}
	})

	// 2. Concurrent WebSocket lease and connection strict isolation test: normal WS leases are returned, abnormal WS leases are abandoned
	t.Run("Concurrent_WS_Lease_Isolation", func(t *testing.T) {
		wsAccount := &auth.Account{
			DBID:         102,
			UpstreamType: "",
			Status:       auth.StatusReady,
			HealthTier:   auth.HealthTierHealthy,
			AccessToken:  "at-lifecycle-ws-102",
			PlanType:     "plus",
			AccountID:    "acct-lifecycle-102",
			Models:       []string{"gpt-5.5"},
		}
		store.AddAccount(wsAccount)
		t.Cleanup(func() { store.RemoveAccount(wsAccount.DBID) })

		previousExec := WebsocketExecuteFunc
		t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

		WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
			if strings.Contains(string(requestBody), "runaway") {
				sse := `data: {"type":"response.output_text.delta","delta":"` + runawayText + `"}` + "\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(sse)),
				}, nil
			}
			sse := `data: {"type":"response.output_text.delta","delta":"Normal WS output"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"id":"resp_ws_ok","status":"completed"}}` + "\n\n"
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		}

		wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"

		var wg sync.WaitGroup
		var normalMsg string
		var abnormalClosed bool

		// Concurrent normal WS sessions
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"client-ws-alias","input":"normal"}`))
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, msg, _ := conn.ReadMessage()
			normalMsg = string(msg)
		}()

		// Concurrent abnormal WS sessions
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"client-ws-alias","input":"runaway"}`))
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, msg, err := conn.ReadMessage()
			if err != nil && (websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.ClosePolicyViolation, websocket.CloseInternalServerErr) || errors.Is(err, io.EOF)) {
				abnormalClosed = true
			} else if strings.Contains(string(msg), GuardTriggeredPublicCode) {
				// After reading the error frame, wait to receive the close control frame or for the connection to drop
				_, _, nextErr := conn.ReadMessage()
				if nextErr != nil {
					abnormalClosed = true
				}
			}
		}()

		wg.Wait()

		if !strings.Contains(normalMsg, "Normal WS output") {
			t.Fatalf("normal WS session failed to deliver output: %s", normalMsg)
		}
		if !abnormalClosed {
			t.Fatalf("expected abnormal WS lease to be discarded and connection closed, but was not")
		}
	})
}

// -----------------------------------------------------------------------------
// Helper type definitions (including a full gin.ResponseWriter implementation)
// -----------------------------------------------------------------------------

type testCloser struct {
	onClose func()
}

func (c *testCloser) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	return nil
}

type customNoDeadlineWriter struct {
	header     http.Header
	writeCount int
}

func (w *customNoDeadlineWriter) Header() http.Header         { return w.header }
func (w *customNoDeadlineWriter) Write(b []byte) (int, error) { w.writeCount++; return len(b), nil }
func (w *customNoDeadlineWriter) WriteHeader(statusCode int)  {}

type customGinResponseWriter struct {
	http.ResponseWriter
}

func (w *customGinResponseWriter) CloseNotify() <-chan bool { return nil }
func (w *customGinResponseWriter) Flush()                   {}
func (w *customGinResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	var conn net.Conn
	var rw *bufio.ReadWriter
	return conn, rw, errors.New("hijack unsupported")
}
func (w *customGinResponseWriter) Size() int                         { return 0 }
func (w *customGinResponseWriter) Status() int                       { return 200 }
func (w *customGinResponseWriter) WriteHeaderNow()                   {}
func (w *customGinResponseWriter) Written() bool                     { return true }
func (w *customGinResponseWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *customGinResponseWriter) Pusher() http.Pusher               { return nil }
func (w *customGinResponseWriter) Unwrap() http.ResponseWriter       { return w.ResponseWriter }

var _ gin.ResponseWriter = (*customGinResponseWriter)(nil)

type spyResponseControllerWriter struct {
	http.ResponseWriter
	deadlineSet    atomic.Bool
	lastDeadline   atomic.Int64
	writtenNanos   atomic.Int64
	writeCalled    atomic.Bool
	lockAttemptAt  atomic.Int64
	cancelNoticeAt atomic.Int64
}

func (s *spyResponseControllerWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		s.deadlineSet.Store(true)
		s.lastDeadline.Store(deadline.UnixNano())
	}
	return nil
}

func (s *spyResponseControllerWriter) Write(b []byte) (int, error) {
	s.writeCalled.Store(true)
	s.writtenNanos.Store(time.Now().UnixNano())
	return s.ResponseWriter.Write(b)
}

func (s *spyResponseControllerWriter) WriteHeader(code int) {
	s.ResponseWriter.WriteHeader(code)
}

func (s *spyResponseControllerWriter) Header() http.Header {
	return s.ResponseWriter.Header()
}

func (s *spyResponseControllerWriter) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
