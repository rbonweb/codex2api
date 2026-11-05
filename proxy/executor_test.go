package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

type maxChunkReader struct {
	reader io.Reader
	size   int
}

func (r *maxChunkReader) Read(p []byte) (int, error) {
	if len(p) > r.size {
		p = p[:r.size]
	}
	return r.reader.Read(p)
}

func TestReadSSEStream_MergesMultilineData(t *testing.T) {
	input := strings.NewReader("data: {\"type\":\"response.output_text.delta\",\n" +
		"data: \"delta\":\"hello\"}\n\n" +
		"data: [DONE]\n\n")

	var events []string
	err := ReadSSEStream(input, func(data []byte) bool {
		events = append(events, string(data))
		return true
	})
	if err != nil {
		t.Fatalf("ReadSSEStream returned error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	want := "{\"type\":\"response.output_text.delta\",\n\"delta\":\"hello\"}"
	if events[0] != want {
		t.Fatalf("unexpected merged event: got %q want %q", events[0], want)
	}
}

func TestReadSSEStreamWithEventPreservesEventName(t *testing.T) {
	input := strings.NewReader("event: error\n" +
		"data: {\"error\":{\"status_code\":403,\"code\":\"forbidden\"}}\n\n")

	var gotEvent string
	var gotData string
	err := ReadSSEStreamWithEvent(input, func(event string, data []byte) bool {
		gotEvent = event
		gotData = string(data)
		return true
	})
	if err != nil {
		t.Fatalf("ReadSSEStreamWithEvent returned error: %v", err)
	}
	if gotEvent != "error" {
		t.Fatalf("event = %q, want error", gotEvent)
	}
	if gotData != `{"error":{"status_code":403,"code":"forbidden"}}` {
		t.Fatalf("data = %q", gotData)
	}
}

func TestReadSSEStreamPreservesEventsAcrossReadBoundaries(t *testing.T) {
	const eventCount = 2048

	var input strings.Builder
	for i := 0; i < eventCount; i++ {
		input.WriteString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
	}
	input.WriteString("data: [DONE]\n\n")

	got := 0
	reader := &maxChunkReader{reader: strings.NewReader(input.String()), size: 37}
	err := ReadSSEStream(reader, func(data []byte) bool {
		if string(data) != `{"type":"response.output_text.delta","delta":"hello"}` {
			t.Fatalf("unexpected event %d: %q", got, data)
		}
		got++
		return true
	})
	if err != nil {
		t.Fatalf("ReadSSEStream returned error: %v", err)
	}
	if got != eventCount {
		t.Fatalf("event count = %d, want %d", got, eventCount)
	}
}

func TestClassifyStreamOutcome(t *testing.T) {
	tests := []struct {
		name         string
		ctxErr       error
		readErr      error
		writeErr     error
		gotTerminal  bool
		wantStatus   int
		wantKind     string
		wantPenalize bool
	}{
		{
			name:        "terminal success",
			gotTerminal: true,
			wantStatus:  200,
		},
		{
			name:         "client canceled",
			ctxErr:       context.Canceled,
			wantStatus:   logStatusClientClosed,
			wantPenalize: false,
		},
		{
			name:         "upstream timeout",
			readErr:      errors.New("read timeout"),
			wantStatus:   logStatusUpstreamStreamBreak,
			wantKind:     "timeout",
			wantPenalize: true,
		},
		{
			name:         "websocket message too big",
			readErr:      errors.New("websocket read error: websocket: close 1009 (message too big)"),
			wantStatus:   logStatusUpstreamStreamBreak,
			wantKind:     upstreamErrorKindMessageTooBig,
			wantPenalize: false,
		},
		{
			name:         "upstream early eof",
			wantStatus:   logStatusUpstreamStreamBreak,
			wantKind:     "transport",
			wantPenalize: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome := classifyStreamOutcome(tc.ctxErr, tc.readErr, tc.writeErr, tc.gotTerminal)
			if outcome.logStatusCode != tc.wantStatus {
				t.Fatalf("status mismatch: got %d want %d", outcome.logStatusCode, tc.wantStatus)
			}
			if outcome.failureKind != tc.wantKind {
				t.Fatalf("failure kind mismatch: got %q want %q", outcome.failureKind, tc.wantKind)
			}
			if outcome.penalize != tc.wantPenalize {
				t.Fatalf("penalize mismatch: got %v want %v", outcome.penalize, tc.wantPenalize)
			}
		})
	}
}

func TestShouldFallbackWebsocketMessageTooBigToHTTP(t *testing.T) {
	outcome := streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    upstreamErrorKindMessageTooBig,
		failureMessage: "upstream stream read failed: websocket read error: websocket: close 1009 (message too big)",
		penalize:       false,
	}

	if !shouldFallbackWebsocketMessageTooBigToHTTP(outcome, true, false, nil, nil) {
		t.Fatal("expected websocket message-too-big before first downstream bytes to fall back to HTTP")
	}
	if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, false, false, nil, nil) {
		t.Fatal("HTTP upstream should not fall back again")
	}
	if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, true, true, nil, nil) {
		t.Fatal("should not fall back after downstream body has been written")
	}
	if shouldFallbackWebsocketMessageTooBigToHTTP(outcome, true, false, context.Canceled, nil) {
		t.Fatal("should not fall back after downstream context is canceled")
	}
}

func TestWebsocketMessageTooBigSource(t *testing.T) {
	readLimitErr := errors.New("websocket read error: websocket: read limit exceeded")
	if got := classifyTransportFailure(readLimitErr); got != upstreamErrorKindMessageTooBig {
		t.Fatalf("local read-limit failure kind = %q, want %q", got, upstreamErrorKindMessageTooBig)
	}
	outcome := classifyStreamOutcome(nil, readLimitErr, nil, false)
	if outcome.failureKind != upstreamErrorKindMessageTooBig || outcome.penalize || outcome.verifyAccountAuth {
		t.Fatalf("local read-limit outcome = %+v, want message-too-big without penalty or auth verification", outcome)
	}
	if got := websocketMessageTooBigSource(outcome.failureMessage); got != "local_read_limit" {
		t.Fatalf("local read-limit source = %q", got)
	}
	if got := websocketMessageTooBigSource("websocket read error: websocket: close 1009 (message too big)"); got != "peer_close" {
		t.Fatalf("peer close source = %q", got)
	}
}

func TestClassifyResponseFailedOutcome(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"An error occurred while processing your request. Please include the request ID req-123."}}}`)

	outcome := classifyResponseFailedOutcome(payload)

	if outcome.logStatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusInternalServerError)
	}
	if outcome.failureKind != "server" {
		t.Fatalf("failure kind = %q, want server", outcome.failureKind)
	}
	if !outcome.penalize {
		t.Fatal("response.failed server error should be penalized")
	}
	if !strings.Contains(outcome.failureMessage, "server_error") || !strings.Contains(outcome.failureMessage, "req-123") {
		t.Fatalf("failure message = %q, want upstream code and request id", outcome.failureMessage)
	}
}

func TestClassifyResponseFailedOutcomeInvalidRequest(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"error":{"code":"invalid_value","type":"invalid_request_error","message":"Invalid input"}}}`)

	outcome := classifyResponseFailedOutcome(payload)

	if outcome.logStatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusBadRequest)
	}
	if outcome.failureKind != "client" {
		t.Fatalf("failure kind = %q, want client", outcome.failureKind)
	}
	if outcome.penalize {
		t.Fatal("client-side response.failed should not penalize account")
	}
}

func TestClassifyResponseFailedOutcomeUsageLimit(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"free","resets_in_seconds":3600}}}`)

	outcome := classifyResponseFailedOutcome(payload)

	if outcome.logStatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusTooManyRequests)
	}
	if outcome.failureKind != "usage_limit" {
		t.Fatalf("failure kind = %q, want usage_limit", outcome.failureKind)
	}
	if !outcome.penalize {
		t.Fatal("usage_limit response.failed should penalize account")
	}
	if !IsUsageLimitReachedError(payload) {
		t.Fatal("nested response.failed usage_limit_reached should be detected")
	}
}

// issue #310: context_length_exceeded is a deterministic client error (retrying on another account always fails),
// must not be classified as 500, which would trigger a transparent retry and penalize account health.
func TestClassifyResponseFailedOutcomeContextLengthExceeded(t *testing.T) {
	// Real upstream shape: code is at response.error.code, with no explicit status_code
	payload := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}}`)

	outcome := classifyResponseFailedOutcome(payload)

	if outcome.logStatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusBadRequest)
	}
	if outcome.failureKind != "client" {
		t.Fatalf("failure kind = %q, want client", outcome.failureKind)
	}
	if outcome.penalize {
		t.Fatal("context_length_exceeded must not penalize the account")
	}
	if shouldTransparentRetryStream(outcome, 0, 2, false, nil, nil) {
		t.Fatal("context_length_exceeded must not trigger transparent account-rotation retry")
	}
}

// The relay upstream returns the context overflow as code:null / type:"upstream_error", and only the message reveals the real
// cause. Matching only code/type falls through to the default 500, which retries through every account in the pool and penalizes each healthy account.
func TestClassifyResponseFailedOutcomeContextLengthExceededMessageOnly(t *testing.T) {
	for name, payload := range map[string][]byte{
		"nested null code": []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":null,"type":"upstream_error","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}}`),
		"top-level error":  []byte(`{"type":"error","error":{"type":"upstream_error","message":"Your input exceeds the context window of this model."}}`),
		"status details":   []byte(`{"type":"response.failed","response":{"status":"failed","status_details":{"error":{"message":"maximum context length exceeded"}}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			outcome := classifyResponseFailedOutcome(payload)
			if outcome.logStatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", outcome.logStatusCode, http.StatusBadRequest)
			}
			if outcome.penalize {
				t.Fatal("context window overflow must not penalize the account")
			}
			if shouldTransparentRetryStream(outcome, 0, 2, false, nil, nil) {
				t.Fatal("context window overflow must not trigger transparent account-rotation retry")
			}
		})
	}
}

// Echoed request content must not change the verdict: only the fixed error field is authoritative.
func TestClassifyResponseFailedOutcomeIgnoresEchoedContextWindowText(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","type":"upstream_error","message":"boom"},"echo":"my prompt explains that input exceeds the context window"}}`)

	if got := classifyResponseFailedOutcome(payload).logStatusCode; got != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", got, http.StatusInternalServerError)
	}
}

func TestClassifyResponseFailedOutcomeDeterministicClientErrors(t *testing.T) {
	for _, code := range []string{"context_window_exceeded", "string_above_max_length", "model_not_found", "unsupported_parameter"} {
		payload := []byte(`{"type":"response.failed","response":{"error":{"code":"` + code + `","message":"boom"}}}`)
		outcome := classifyResponseFailedOutcome(payload)
		if outcome.logStatusCode != http.StatusBadRequest {
			t.Errorf("code %s: status = %d, want %d", code, outcome.logStatusCode, http.StatusBadRequest)
		}
		if outcome.penalize {
			t.Errorf("code %s: deterministic client error must not penalize", code)
		}
	}
}

func TestClassifyResponseFailedOutcomeAnthropicAuthAndPermissionErrors(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		want int
	}{
		{typ: "authentication_error", want: http.StatusUnauthorized},
		{typ: "invalid_token", want: http.StatusUnauthorized},
		{typ: "permission_error", want: http.StatusForbidden},
	} {
		payload := []byte(`{"type":"error","error":{"type":"` + tc.typ + `","message":"failure"}}`)
		if got := classifyResponseFailedOutcome(payload).logStatusCode; got != tc.want {
			t.Errorf("error type %s: status = %d, want %d", tc.typ, got, tc.want)
		}
	}
}

func TestShouldRecyclePooledClient(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "connection shutting down",
			err:  errors.New("http2: client connection is shutting down"),
			want: true,
		},
		{
			name: "connection reset",
			err:  errors.New("read tcp: connection reset by peer"),
			want: true,
		},
		{
			name: "broken pipe",
			err:  errors.New("write: broken pipe"),
			want: true,
		},
		{
			name: "plain timeout",
			err:  errors.New("read timeout"),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRecyclePooledClient(tc.err); got != tc.want {
				t.Fatalf("shouldRecyclePooledClient() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldTransparentRetryStream(t *testing.T) {
	retryable := streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failureMessage: "upstream failed before first byte",
		penalize:       true,
	}

	if !shouldTransparentRetryStream(retryable, 0, 2, false, nil, nil) {
		t.Fatal("expected early upstream failure to be transparently retried")
	}
	if shouldTransparentRetryStream(retryable, 2, 2, false, nil, nil) {
		t.Fatal("expected retry to stop at maxRetries")
	}
	if shouldTransparentRetryStream(retryable, 0, 2, true, nil, nil) {
		t.Fatal("expected retry to stop after downstream already received bytes")
	}
	if shouldTransparentRetryStream(retryable, 0, 2, false, context.Canceled, nil) {
		t.Fatal("expected retry to stop when downstream context is canceled")
	}
}

func TestApplyCodexRequestHeadersUsesSessionIDWithoutConversationID(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	acc := &auth.Account{
		DBID:      42,
		AccountID: "acct-42",
	}
	cfg := &DeviceProfileConfig{
		UserAgent:              "codex_cli_rs/0.120.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464",
		PackageVersion:         "0.120.0",
		RuntimeVersion:         "0.120.0",
		OS:                     "MacOS",
		Arch:                   "arm64",
		StabilizeDeviceProfile: true,
	}
	downstreamHeaders := http.Header{
		"Originator": []string{"custom-originator"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "cache-key-1", "api-key-1", cfg, downstreamHeaders)

	if got := req.Header.Get("Authorization"); got != "Bearer token-123" {
		t.Fatalf("Authorization = %q", got)
	}
	// Real clients send session-id / thread-id (hyphenated), not the underscore form, and do not send
	// Conversation_id. In a single-threaded session, thread-id and session-id have the same value.
	if got := req.Header.Get("Session-Id"); got != "cache-key-1" {
		t.Fatalf("Session-Id = %q", got)
	}
	if got := req.Header.Get("Thread-Id"); got != "cache-key-1" {
		t.Fatalf("Thread-Id = %q, want same as session", got)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "cache-key-1" {
		t.Fatalf("X-Client-Request-Id = %q, want equal to thread id", got)
	}
	if got := req.Header.Get("Session_id"); got != "" {
		t.Fatalf("Session_id = %q, want empty (the underscore form is not a real shape)", got)
	}
	if got := req.Header.Get("Conversation_id"); got != "" {
		t.Fatalf("Conversation_id = %q, want empty", got)
	}
	if got := req.Header.Get("User-Agent"); got != cfg.UserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("Version"); got != "0.120.0" {
		t.Fatalf("Version = %q", got)
	}
	if got := req.Header.Get("Originator"); got != Originator {
		t.Fatalf("Originator = %q, want fallback %q", got, Originator)
	}
	if got := req.Header.Get("Chatgpt-Account-Id"); got != "acct-42" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	for _, name := range []string{"X-Stainless-Package-Version", "X-Stainless-Runtime-Version", "X-Stainless-Os", "X-Stainless-Arch"} {
		if got := req.Header.Get(name); got != "" {
			t.Fatalf("%s = %q, want empty", name, got)
		}
	}
}

func TestApplyCodexRequestHeadersAppliesAccountCustomHeadersLast(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	acc := &auth.Account{
		DBID:      42,
		AccountID: "acct-default",
		CustomHeaders: map[string]string{
			"Authorization":      "Bearer upstream-override",
			"Chatgpt-Account-Id": "acct-override",
			"X-Custom-Header":    "custom-value",
		},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "cache-key-1", "api-key-1", nil, http.Header{})

	if got := req.Header.Get("Authorization"); got != "Bearer upstream-override" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("Chatgpt-Account-Id"); got != "acct-override" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
	if got := req.Header.Get("X-Custom-Header"); got != "custom-value" {
		t.Fatalf("X-Custom-Header = %q", got)
	}
}

// TestApplyCodexRequestHeadersUsesProjectAttestation verifies that attestation is generated only by this project.
func TestApplyCodexRequestHeadersUsesProjectAttestation(t *testing.T) {
	acc := &auth.Account{DBID: 42, AccountID: "acct-42", CustomHeaders: map[string]string{
		"X-Oai-Attestation": "v1.account-override",
	}}

	withToken, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyCodexRequestHeaders(withToken, acc, "token-123", "cache-key-1", "api-key-1", nil, http.Header{
		"User-Agent":        {"codex-tui/0.164.0 (Windows 11; x86_64) unknown"},
		"X-Oai-Attestation": []string{"v1.real-devicecheck-token"},
	})
	if got := withToken.Header.Get("X-Oai-Attestation"); !strings.HasPrefix(got, `{"v":1,"s":0,"t":"v1.`) {
		t.Fatalf("X-Oai-Attestation = %q, want project-generated envelope", got)
	}

	// The Linux outbound identity clears all external attestations.
	without, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyCodexRequestHeaders(without, acc, "token-123", "cache-key-1", "api-key-1", nil, http.Header{
		"User-Agent":        {"codex-tui/0.164.0 (Linux Ubuntu 24.04; x86_64) unknown"},
		"X-Oai-Attestation": {"v1.real-devicecheck-token"},
	})
	if got := without.Header.Get("X-Oai-Attestation"); got != "" {
		t.Fatalf("X-Oai-Attestation = %q, want empty for Linux", got)
	}
}

func TestApplyCodexRequestHeadersForwardsResponsesLiteOnlyWhenPresent(t *testing.T) {
	const headerName = codexResponsesLiteHeader
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	downstreamHeaders := make(http.Header)
	downstreamHeaders.Set(headerName, "true")

	withLite, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyCodexRequestHeaders(withLite, acc, "token-123", "cache-key-1", "api-key-1", nil, downstreamHeaders)
	if got := withLite.Header.Get(headerName); got != "true" {
		t.Fatalf("%s = %q, want true", headerName, got)
	}

	withoutLite, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyCodexRequestHeaders(withoutLite, acc, "token-123", "cache-key-1", "api-key-1", nil, http.Header{})
	if got := withoutLite.Header.Get(headerName); got != "" {
		t.Fatalf("%s = %q, want empty when downstream omitted the signal", headerName, got)
	}
}

func TestCodexResponsesLiteRequestedRequiresExplicitTrue(t *testing.T) {
	headerTrue := make(http.Header)
	headerTrue.Set(codexResponsesLiteHeader, " TRUE ")
	headerFalse := make(http.Header)
	headerFalse.Set(codexResponsesLiteHeader, "false")

	tests := []struct {
		name    string
		body    []byte
		headers http.Header
		want    bool
	}{
		{name: "http header", body: []byte(`{"model":"gpt-5.4"}`), headers: headerTrue, want: true},
		{name: "websocket metadata", body: []byte(`{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`), want: true},
		{name: "false values", body: []byte(`{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"false"}}`), headers: headerFalse, want: false},
		{name: "model name alone", body: []byte(`{"model":"gpt-5.6-sol"}`), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexResponsesLiteRequested(tt.body, tt.headers); got != tt.want {
				t.Fatalf("codexResponsesLiteRequested() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestPrepareCodexResponsesLiteTransportBridgesRequestScopedSignal(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[{"type":"function_call","name":"run","namespace":"code_tools","arguments":"{}"}],
		"client_metadata":{"other":"kept","ws_request_header_x_openai_internal_codex_responses_lite":"true"}
	}`)
	headers := make(http.Header)
	headers.Set(codexResponsesLiteHeader, "true")

	wsBody, wsHeaders := prepareCodexResponsesLiteTransport(body, headers, true, true)
	if got := gjson.GetBytes(wsBody, codexResponsesLiteWSMetadataPath).String(); got != "true" {
		t.Fatalf("WS Lite metadata = %q, want true; body=%s", got, wsBody)
	}
	if got := wsHeaders.Get(codexResponsesLiteHeader); got != "" {
		t.Fatalf("WS handshake Lite header = %q, want empty", got)
	}

	httpBody, httpHeaders := prepareCodexResponsesLiteTransport(body, headers, false, true)
	if got := httpHeaders.Get(codexResponsesLiteHeader); got != "true" {
		t.Fatalf("HTTP Lite header = %q, want true", got)
	}
	if marker := gjson.GetBytes(httpBody, codexResponsesLiteWSMetadataPath); marker.Exists() {
		t.Fatalf("HTTP body retained WS-only Lite metadata: %s", httpBody)
	}
	if got := gjson.GetBytes(httpBody, "client_metadata.other").String(); got != "kept" {
		t.Fatalf("unrelated client metadata = %q, want kept; body=%s", got, httpBody)
	}
	if got := gjson.GetBytes(httpBody, "input.0.namespace").String(); got != "code_tools" {
		t.Fatalf("tool namespace = %q, want code_tools; body=%s", got, httpBody)
	}
	if got := headers.Get(codexResponsesLiteHeader); got != "true" {
		t.Fatalf("caller headers were mutated: Lite header = %q", got)
	}

	nextWSBody, nextWSHeaders := prepareCodexResponsesLiteTransport([]byte(`{"model":"gpt-5.6-sol"}`), nil, true, false)
	if marker := gjson.GetBytes(nextWSBody, codexResponsesLiteWSMetadataPath); marker.Exists() {
		t.Fatalf("non-Lite request inherited pooled WS metadata: %s", nextWSBody)
	}
	if got := nextWSHeaders.Get(codexResponsesLiteHeader); got != "" {
		t.Fatalf("non-Lite request inherited pooled WS header: %q", got)
	}

	nonLiteHeaders := make(http.Header)
	nonLiteHeaders.Set(codexResponsesLiteHeader, "false")
	nonLiteBody := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"other":"kept","ws_request_header_x_openai_internal_codex_responses_lite":"false"}}`)
	nonLiteBody, nonLiteHeaders = prepareCodexResponsesLiteTransport(nonLiteBody, nonLiteHeaders, false, false)
	if got := nonLiteHeaders.Get(codexResponsesLiteHeader); got != "" {
		t.Fatalf("false Lite header was forwarded as %q", got)
	}
	if marker := gjson.GetBytes(nonLiteBody, codexResponsesLiteWSMetadataPath); marker.Exists() {
		t.Fatalf("HTTP body retained false WS-only Lite metadata: %s", nonLiteBody)
	}

	// After the model gate strips the signal (enabled=false), the WS path must clear the in-body markers that came from downstream,
	// otherwise non-lite models still carry the markers to the WS upstream and trigger a 400.
	gatedWSBody := []byte(`{"model":"gpt-5.5","client_metadata":{"other":"kept","ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`)
	gatedWSHeaders := make(http.Header)
	gatedWSHeaders.Set(codexResponsesLiteHeader, "true")
	gatedWSBody, gatedWSHeaders = prepareCodexResponsesLiteTransport(gatedWSBody, gatedWSHeaders, true, false)
	if marker := gjson.GetBytes(gatedWSBody, codexResponsesLiteWSMetadataPath); marker.Exists() {
		t.Fatalf("WS body retained Lite metadata after gate disabled it: %s", gatedWSBody)
	}
	if got := gjson.GetBytes(gatedWSBody, "client_metadata.other").String(); got != "kept" {
		t.Fatalf("unrelated client metadata = %q, want kept; body=%s", got, gatedWSBody)
	}
	if got := gatedWSHeaders.Get(codexResponsesLiteHeader); got != "" {
		t.Fatalf("WS handshake Lite header = %q, want empty after gate disabled it", got)
	}
}

func TestNormalizeCodexResponsesLiteBodyEnforcesUpstreamConstraints(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"parallel_tool_calls":true,
		"reasoning":{"effort":"low"},
		"tools":[{"type":"function","name":"run"},{"type":"image_generation","model":"gpt-image-2"}],
		"instructions":"Base prompt.\n\n` + "<codex2api-codex-image-generation>\\nbridge text\\n</codex2api-codex-image-generation>" + `"
	}`)

	wsBody := normalizeCodexResponsesLiteBody(body, false)
	if got := gjson.GetBytes(wsBody, "parallel_tool_calls").Bool(); got {
		t.Fatalf("WS parallel_tool_calls = %v, want false", got)
	}
	if got := gjson.GetBytes(wsBody, "reasoning.context").String(); got != "all_turns" {
		t.Fatalf("WS reasoning.context = %q, want all_turns", got)
	}
	if got := gjson.GetBytes(wsBody, "reasoning.effort").String(); got != "low" {
		t.Fatalf("WS reasoning.effort = %q, want low (preserved)", got)
	}
	if got := gjson.GetBytes(wsBody, "tools.#").Int(); got != 2 {
		t.Fatalf("WS tools count = %d, want 2 (image tool kept on WS)", got)
	}

	httpBody := normalizeCodexResponsesLiteBody(body, true)
	if got := gjson.GetBytes(httpBody, "tools.#").Int(); got != 1 {
		t.Fatalf("HTTP tools count = %d, want 1; body=%s", got, httpBody)
	}
	if got := gjson.GetBytes(httpBody, "tools.0.type").String(); got != "function" {
		t.Fatalf("HTTP surviving tool type = %q, want function", got)
	}
	if instructions := gjson.GetBytes(httpBody, "instructions").String(); instructions != "Base prompt." {
		t.Fatalf("HTTP instructions = %q, want bridge text removed", instructions)
	}
}

func TestNormalizeCodexResponsesLiteBodyStripsImageOnlyToolSet(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"tools":[{"type":"image_generation","model":"gpt-image-2"}],
		"tool_choice":{"type":"image_generation"},
		"instructions":"` + "<codex2api-codex-image-generation>\\nbridge text\\n</codex2api-codex-image-generation>" + `"
	}`)

	httpBody := normalizeCodexResponsesLiteBody(body, true)
	if tools := gjson.GetBytes(httpBody, "tools"); tools.Exists() {
		t.Fatalf("HTTP tools should be removed entirely, got %s", tools.Raw)
	}
	if choice := gjson.GetBytes(httpBody, "tool_choice"); choice.Exists() {
		t.Fatalf("HTTP tool_choice should be removed, got %s", choice.Raw)
	}
	if instructions := gjson.GetBytes(httpBody, "instructions").String(); instructions != "" {
		t.Fatalf("HTTP instructions = %q, want empty after bridge removal", instructions)
	}
}

func TestExecuteRequestWebsocketSendsCompactionTriggerLast(t *testing.T) {
	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })

	var seenBody []byte
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		seenBody = append([]byte(nil), requestBody...)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_test"}`)),
		}, nil
	}

	body := []byte(`{"model":"gpt-5.6-sol","input":[
		{"type":"compaction_trigger"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"}
	]}`)
	resp, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1, AccessToken: "token"}, body, "session-1", "", "sk-local", nil, http.Header{}, true)
	if err != nil {
		t.Fatalf("ExecuteRequest() error = %v", err)
	}
	resp.Body.Close()

	input := gjson.GetBytes(seenBody, "input").Array()
	if len(input) != 2 || input[1].Get("type").String() != "compaction_trigger" {
		t.Fatalf("final websocket body must end with compaction_trigger: %s", seenBody)
	}
}

func TestApplyCodexRequestHeadersUsesMinimalFallbackByDefault(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	acc := &auth.Account{
		DBID:      42,
		AccountID: "acct-42",
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, http.Header{})

	if got := req.Header.Get("User-Agent"); got != defaultCodexCLIUserAgent {
		t.Fatalf("User-Agent = %q, want minimal Codex CLI %q", got, defaultCodexCLIUserAgent)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, latestCodexCLIVersion)
	}
}

func TestApplyCodexRequestHeadersUsesCustomGeneratedUserAgentConfig(t *testing.T) {
	prev := CurrentRuntimeSettings()
	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"client_name":"codex-tui","client_version":"0.142.0-alpha.10","os_name":"Mac OS","os_version":"13.7.8","arch":"arm64","terminal":"xterm-256color"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:     ClientCompatModeForce,
		CodexUserAgentConfig: normalized,
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	applyCodexRequestHeaders(req, &auth.Account{DBID: 42}, "token-123", "", "api-key-1", nil, http.Header{})

	wantUA := "codex-tui/0.142.0-alpha.10 (Mac OS 13.7.8; arm64) xterm-256color (codex-tui; 0.142.0-alpha.10)"
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
	}
	if got := req.Header.Get("Version"); got != "0.142.0-alpha.10" {
		t.Fatalf("Version = %q, want 0.142.0-alpha.10", got)
	}
}

func TestApplyCodexRequestHeadersGeneratedDesktopClientSendsMatchingOriginator(t *testing.T) {
	// issue #653: when forcing the ChatGPT desktop client to be emulated, Originator must follow the generated UA prefix,
	// instead of still sending codex-tui; real clients always use the same identifier for both.
	prev := CurrentRuntimeSettings()
	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"client_name":"Codex Desktop","client_version":"0.153.3","os_name":"Windows","os_version":"10.0.26100","arch":"x86_64","terminal":"unknown"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:     ClientCompatModeForce,
		CodexUserAgentConfig: normalized,
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"codex-tui/0.153.3 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.153.3)"},
		"Originator": []string{"codex-tui"},
	}

	applyCodexRequestHeaders(req, &auth.Account{DBID: 42}, "token-123", "", "api-key-1", nil, downstreamHeaders)

	wantUA := "Codex Desktop/0.153.3 (Windows 10.0.26100; x86_64) unknown (Codex Desktop; 26.930.51102)"
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
	}
	if got := req.Header.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %q, want Codex Desktop to match generated User-Agent", got)
	}
	if got := req.Header.Get("Version"); got != "0.153.3" {
		t.Fatalf("Version = %q, want 0.153.3", got)
	}
}

func TestApplyCodexRequestHeadersRawUserAgentOriginatorFollowsOfficialPrefix(t *testing.T) {
	cases := []struct {
		name           string
		rawUserAgent   string
		wantOriginator string
	}{
		{"desktop raw override", "Codex Desktop/0.153.3 (Mac OS 26.4.0; arm64) dumb (codex_exec; 0.153.3)", "Codex Desktop"},
		{"unofficial raw override keeps default", "my-router", Originator},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := CurrentRuntimeSettings()
			normalized, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"` + tc.rawUserAgent + `"}`)
			if err != nil {
				t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
			}
			ApplyRuntimeSettings(RuntimeSettings{
				ClientCompatMode:     ClientCompatModeForce,
				CodexUserAgentConfig: normalized,
			})
			t.Cleanup(func() { ApplyRuntimeSettings(prev) })

			req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
			if err != nil {
				t.Fatalf("http.NewRequest() error = %v", err)
			}
			downstreamHeaders := http.Header{"Originator": []string{"codex-tui"}}

			applyCodexRequestHeaders(req, &auth.Account{DBID: 42}, "token-123", "", "api-key-1", nil, downstreamHeaders)

			if got := req.Header.Get("User-Agent"); got != tc.rawUserAgent {
				t.Fatalf("User-Agent = %q, want %q", got, tc.rawUserAgent)
			}
			if got := req.Header.Get("Originator"); got != tc.wantOriginator {
				t.Fatalf("Originator = %q, want %q", got, tc.wantOriginator)
			}
		})
	}
}

func TestApplyCodexRequestHeadersRawUserAgentWithoutVersionOmitsVersionHeader(t *testing.T) {
	prev := CurrentRuntimeSettings()
	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"my-router"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:     ClientCompatModeForce,
		CodexUserAgentConfig: normalized,
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	applyCodexRequestHeaders(req, &auth.Account{DBID: 42}, "token-123", "", "api-key-1", nil, http.Header{})

	if got := req.Header.Get("User-Agent"); got != "my-router" {
		t.Fatalf("User-Agent = %q, want raw override", got)
	}
	if got := req.Header.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
}

func TestApplyCodexRequestHeadersRetainsManualVersionBelowAutoMinimum(t *testing.T) {
	prev := CurrentRuntimeSettings()
	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"client_name":"codex-tui","client_version":"0.142.0","os_name":"Linux","os_version":"Unknown","arch":"x86_64","terminal":"xterm-256color"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:     ClientCompatModeAuto,
		CodexMinCLIVersion:   "0.150.0",
		CodexUserAgentConfig: normalized,
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"codex-tui/0.100.0 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.100.0)"},
		"Originator": []string{"codex-tui"},
	}

	applyCodexRequestHeaders(req, &auth.Account{DBID: 42}, "token-123", "", "api-key-1", nil, downstreamHeaders)

	wantUA := "codex-tui/0.142.0 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.142.0)"
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
	}
	if got := req.Header.Get("Version"); got != "0.142.0" {
		t.Fatalf("Version = %q, want 0.142.0", got)
	}
}

func TestApplyCodexRequestHeadersRepairsBlankStabilizedProfileUserAgent(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}

	cfg := &DeviceProfileConfig{StabilizeDeviceProfile: true}
	applyCodexRequestHeaders(req, &auth.Account{DBID: 42}, "token-123", "", "api-key-1", cfg, http.Header{})

	if got := req.Header.Get("User-Agent"); got != defaultCodexCLIUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, defaultCodexCLIUserAgent)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, latestCodexCLIVersion)
	}
}

func TestApplyCodexRequestHeadersPreservesOfficialClientHeaders(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModePreserve})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	downstreamHeaders := http.Header{
		"User-Agent":            []string{"codex_vscode/1.2.3"},
		"Originator":            []string{"codex_vscode"},
		"Version":               []string{"1.2.3"},
		"X-Codex-Turn-State":    []string{"turn-state"},
		"X-Codex-Turn-Metadata": []string{"turn-metadata"},
		"X-Client-Request-Id":   []string{"req-123"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "cache-key-1", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != "codex_vscode/1.2.3" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("Originator"); got != "codex_vscode" {
		t.Fatalf("Originator = %q", got)
	}
	if got := req.Header.Get("Version"); got != "1.2.3" {
		t.Fatalf("Version = %q", got)
	}
	for _, name := range []string{"X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Client-Request-Id"} {
		if got := req.Header.Get(name); got != downstreamHeaders.Get(name) {
			t.Fatalf("%s = %q, want %q", name, got, downstreamHeaders.Get(name))
		}
	}
}

func TestApplyCodexRequestHeadersAutoDerivesVersionFromDesktopUserAgent(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:   ClientCompatModeAuto,
		CodexMinCLIVersion: "0.153.3",
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	desktopUserAgent := "Codex Desktop/0.153.3 (Mac OS 26.4.0; arm64) dumb (codex_exec; 0.153.3)"
	downstreamHeaders := http.Header{
		"User-Agent": []string{desktopUserAgent},
		"Originator": []string{"Codex Desktop"},
	}

	applyCodexRequestHeaders(req, &auth.Account{DBID: 42}, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != desktopUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, desktopUserAgent)
	}
	if got := req.Header.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %q, want Codex Desktop", got)
	}
	if got := req.Header.Get("Version"); got != "0.153.3" {
		t.Fatalf("Version = %q, want 0.153.3 derived from desktop User-Agent", got)
	}
}

func TestApplyCodexRequestHeadersAutoUpgradesOldDesktopClient(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:   ClientCompatModeAuto,
		CodexMinCLIVersion: "0.153.3",
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"Codex Desktop/0.152.0 (Mac OS 26.4.0; arm64) dumb (codex_exec; 0.152.0)"},
		"Originator": []string{"Codex Desktop"},
	}

	applyCodexRequestHeaders(req, &auth.Account{DBID: 42}, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got == downstreamHeaders.Get("User-Agent") {
		t.Fatalf("User-Agent preserved old desktop UA %q", got)
	}
	if got := req.Header.Get("Originator"); got != Originator {
		t.Fatalf("Originator = %q, want generated client originator %q", got, Originator)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want generated client version %s", got, latestCodexCLIVersion)
	}
}

func TestApplyCodexRequestHeadersAutoUpgradesOldCodexCLI(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:   ClientCompatModeAuto,
		CodexMinCLIVersion: "0.118.0",
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"codex_cli_rs/0.117.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464"},
		"Originator": []string{Originator},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got == downstreamHeaders.Get("User-Agent") {
		t.Fatalf("User-Agent preserved old CLI UA %q", got)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, latestCodexCLIVersion)
	}
}

func TestApplyCodexRequestHeadersAutoDoesNotUpgradeEmbeddedCodexToken(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{
		ClientCompatMode:   ClientCompatModeAuto,
		CodexMinCLIVersion: "0.118.0",
	})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	spoofedUA := "Mozilla/5.0 codex_cli_rs/0.117.0"
	downstreamHeaders := http.Header{
		"User-Agent": []string{spoofedUA},
		"Originator": []string{"random-client"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != spoofedUA {
		t.Fatalf("User-Agent = %q, want legacy-preserved spoofed UA %q", got, spoofedUA)
	}
	if got := req.Header.Get("Version"); got != "0.117.0" {
		t.Fatalf("Version = %q, want parsed legacy version 0.117.0", got)
	}
}

func TestApplyCodexRequestHeadersFallsBackForNonOfficialClient(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"curl/8.0"},
		"Originator": []string{"random-client"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != defaultCodexCLIUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, defaultCodexCLIUserAgent)
	}
	if got := req.Header.Get("Originator"); got != Originator {
		t.Fatalf("Originator = %q, want %q", got, Originator)
	}
	if got := req.Header.Get("Version"); got != latestCodexCLIVersion {
		t.Fatalf("Version = %q, want %q", got, latestCodexCLIVersion)
	}
}

func TestApplyCodexRequestHeadersPreservesOpenCodeClient(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModePreserve})
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}
	downstreamHeaders := http.Header{
		"User-Agent": []string{"opencode/0.5.0"},
		"Originator": []string{"opencode"},
	}

	applyCodexRequestHeaders(req, acc, "token-123", "", "api-key-1", nil, downstreamHeaders)

	if got := req.Header.Get("User-Agent"); got != "opencode/0.5.0" {
		t.Fatalf("User-Agent = %q, want %q", got, "opencode/0.5.0")
	}
	if got := req.Header.Get("Originator"); got != "opencode" {
		t.Fatalf("Originator = %q, want %q", got, "opencode")
	}
}

func TestCodexTransportModeDefaultsToStandard(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "")
	if _, ok := newCodexTransport("").(*http.Transport); !ok {
		t.Fatalf("newCodexTransport default = %T, want *http.Transport", newCodexTransport(""))
	}
}

func TestCodexTransportModeCanUseUTLSChrome(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "utls_chrome")
	if _, ok := newCodexTransport("").(*utlsRoundTripper); !ok {
		t.Fatalf("newCodexTransport utls_chrome = %T, want *utlsRoundTripper", newCodexTransport(""))
	}
}

func TestClientPoolKeyIncludesTransportMode(t *testing.T) {
	acc := &auth.Account{DBID: 42}
	standard := clientPoolKey(acc, "http://proxy", codexTransportModeStandard)
	utlsChrome := clientPoolKey(acc, "http://proxy", codexTransportModeUTLSChrome)
	if standard == utlsChrome {
		t.Fatalf("clientPoolKey should include transport mode, got %q", standard)
	}
}

func TestIsolateCodexSessionIDUsesAPIKeyScope(t *testing.T) {
	raw := "session-1"
	if got := IsolateCodexSessionID(0, raw); got != raw {
		t.Fatalf("IsolateCodexSessionID without api key = %q, want %q", got, raw)
	}
	first := IsolateCodexSessionID(1, raw)
	second := IsolateCodexSessionID(2, raw)
	if first == raw || second == raw || first == second {
		t.Fatalf("expected distinct isolated session ids, got first=%q second=%q raw=%q", first, second, raw)
	}
}

func TestResolveSessionIDPrefersContinuityHeaders(t *testing.T) {
	headers := http.Header{
		"Session_id":      []string{"session-from-header"},
		"Conversation_id": []string{"conversation-from-header"},
		"Authorization":   []string{"Bearer sk-test-123"},
	}

	if got := ResolveSessionID(headers, []byte(`{"prompt_cache_key":"body-key"}`)); got != "session-from-header" {
		t.Fatalf("ResolveSessionID() = %q, want %q", got, "session-from-header")
	}

	headers.Del("Session_id")
	if got := ResolveSessionID(headers, []byte(`{"prompt_cache_key":"body-key"}`)); got != "conversation-from-header" {
		t.Fatalf("ResolveSessionID() = %q, want %q", got, "conversation-from-header")
	}

	headers.Del("Conversation_id")
	headers.Set("Idempotency-Key", "idempotency-key-1")
	if got := ResolveSessionID(headers, []byte(`{"prompt_cache_key":"body-key"}`)); got != "idempotency-key-1" {
		t.Fatalf("ResolveSessionID() = %q, want %q", got, "idempotency-key-1")
	}
}

func TestResolveSessionIDUsesLocalAffinityHeader(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"same prompt"}]}`)
	baseHeaders := http.Header{"Authorization": []string{"Bearer shared-key"}}
	headersA := baseHeaders.Clone()
	headersA.Set("X-Codex2API-Affinity-Key", "user-a")
	headersB := headersA.Clone()
	headersB.Set("X-Codex2API-Affinity-Key", "user-b")

	identityWithoutAffinity := resolveRequestSessionIdentity(baseHeaders, body)
	identityA := resolveRequestSessionIdentity(headersA, body)
	identityB := resolveRequestSessionIdentity(headersB, body)
	if identityWithoutAffinity.hasDownstreamAffinity {
		t.Fatal("request without the dedicated affinity header was marked as having one")
	}
	if identityWithoutAffinity.hasRequestFingerprint {
		t.Fatal("plain request without a Codex fingerprint was marked as fingerprinted")
	}
	if !identityA.hasDownstreamAffinity || !identityB.hasDownstreamAffinity {
		t.Fatal("request with the dedicated affinity header was not marked as having one")
	}
	if !identityA.hasRequestFingerprint || !identityB.hasRequestFingerprint {
		t.Fatal("dedicated affinity header must count as a routing fingerprint")
	}
	idA := identityA.affinityID
	idARepeat := ResolveSessionID(headersA, body)
	idB := identityB.affinityID
	if idA == "" || idA != idARepeat {
		t.Fatalf("affinity id must be non-empty and stable: first=%q repeat=%q", idA, idARepeat)
	}
	if idA == idB {
		t.Fatalf("different affinity headers produced the same id %q", idA)
	}
	if idA == "user-a" || idB == "user-b" {
		t.Fatal("raw downstream affinity identifiers must not be retained")
	}
	if explicit := ResolveExplicitSessionID(headersA, body); explicit != "" {
		t.Fatalf("local affinity header leaked into upstream explicit session id: %q", explicit)
	}
	if identityA.upstreamSeed != identityWithoutAffinity.upstreamSeed || identityB.upstreamSeed != identityWithoutAffinity.upstreamSeed {
		t.Fatalf("local affinity changed upstream seed: without=%q a=%q b=%q", identityWithoutAffinity.upstreamSeed, identityA.upstreamSeed, identityB.upstreamSeed)
	}
	if identityA.explicitUpstreamID != "" || identityB.explicitUpstreamID != "" {
		t.Fatalf("local affinity became an explicit upstream id: a=%q b=%q", identityA.explicitUpstreamID, identityB.explicitUpstreamID)
	}
}

func TestResolveSessionIdentityTreatsBlankAffinityHeaderAsMissing(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer shared-key"}}
	headers.Set("X-Codex2API-Affinity-Key", "   ")
	identity := resolveRequestSessionIdentity(headers, []byte(`{"model":"gpt-5.4","input":"hello"}`))
	if identity.hasDownstreamAffinity {
		t.Fatal("blank affinity header must be treated as missing")
	}
}

func TestResolveSessionIdentityRecognizesCodexEngineFingerprint(t *testing.T) {
	headers := http.Header{
		"Authorization":      []string{"Bearer shared-key"},
		"X-Codex-Turn-State": []string{"turn-state"},
	}
	identity := resolveRequestSessionIdentity(headers, []byte(`{"model":"gpt-5.4","input":"hello"}`))
	if identity.hasDownstreamAffinity {
		t.Fatal("Codex engine fingerprint must not be treated as the dedicated affinity header")
	}
	if !identity.hasRequestFingerprint {
		t.Fatal("Codex engine fingerprint was not recognized for group routing")
	}
}

func TestLocalAffinityDoesNotAffectPerAPIKeyHTTPUpstreamSessionID(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.RequestIsolationMode = RequestIsolationModePerAPIKey
	ApplyRuntimeSettings(nextSettings)

	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"same prompt"}]}`)
	baseHeaders := http.Header{"Authorization": []string{"Bearer shared-key"}}
	affinityHeaders := baseHeaders.Clone()
	affinityHeaders.Set("X-Codex2API-Affinity-Key", "user-a")

	withoutAffinity := resolveRequestSessionIdentity(baseHeaders, body)
	withAffinity := resolveRequestSessionIdentity(affinityHeaders, body)
	withoutUpstreamID := resolveUpstreamSessionID(7, withoutAffinity.upstreamSeed, withoutAffinity.explicitUpstreamID, false)
	withUpstreamID := resolveUpstreamSessionID(7, withAffinity.upstreamSeed, withAffinity.explicitUpstreamID, false)
	if withoutUpstreamID == "" || withUpstreamID != withoutUpstreamID {
		t.Fatalf("per-api-key HTTP upstream id changed with local affinity: without=%q with=%q", withoutUpstreamID, withUpstreamID)
	}
	if withUpstreamID == withAffinity.affinityID {
		t.Fatalf("local affinity id leaked as upstream id %q", withUpstreamID)
	}
}

func TestLocalAffinityPreservesExplicitAndAPIKeyUpstreamSeeds(t *testing.T) {
	t.Run("explicit session", func(t *testing.T) {
		headers := http.Header{
			"Authorization": []string{"Bearer shared-key"},
			"Session_id":    []string{"explicit-session"},
		}
		headers.Set("X-Codex2API-Affinity-Key", "user-a")
		identity := resolveRequestSessionIdentity(headers, []byte(`{"model":"gpt-5.4"}`))
		if identity.upstreamSeed != "explicit-session" || identity.explicitUpstreamID != "explicit-session" {
			t.Fatalf("explicit upstream identity changed: seed=%q explicit=%q", identity.upstreamSeed, identity.explicitUpstreamID)
		}
		if identity.affinityID == identity.upstreamSeed {
			t.Fatalf("local affinity did not remain separate from explicit upstream seed %q", identity.upstreamSeed)
		}
	})

	t.Run("api key fallback", func(t *testing.T) {
		headers := http.Header{"Authorization": []string{"Bearer shared-key"}}
		headers.Set("X-Codex2API-Affinity-Key", "user-a")
		identity := resolveRequestSessionIdentity(headers, []byte(`{}`))
		// It shares the seed and derivation with deterministicPromptCacheKey: if the two compute different values, the same
		// API key would get two unrelated upstream identities on the HTTP and WS paths.
		wantSeed := DeriveStableSessionUUIDv7("codex2api:prompt-cache:shared-key")
		if identity.upstreamSeed != wantSeed || identity.explicitUpstreamID != "" {
			t.Fatalf("API-key upstream fallback changed: seed=%q explicit=%q want=%q", identity.upstreamSeed, identity.explicitUpstreamID, wantSeed)
		}
		if identity.affinityID == identity.upstreamSeed {
			t.Fatalf("local affinity did not remain separate from API-key upstream seed %q", identity.upstreamSeed)
		}
	})
}

func TestResolveExplicitSessionIDDoesNotUseAPIKeyFallback(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer sk-test-123"}}

	if got := ResolveExplicitSessionID(headers, []byte(`{}`)); got != "" {
		t.Fatalf("ResolveExplicitSessionID() = %q, want empty", got)
	}
	if got := ResolveSessionID(headers, []byte(`{}`)); got == "" {
		t.Fatal("ResolveSessionID() should still generate API-key fallback")
	}
}

func TestExecuteRequestExplicitFalseBypassesForcedWebsocket(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	ApplyRuntimeSettings(nextSettings)

	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })
	wsCalled := false
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		wsCalled = true
		return nil, errors.New("websocket should not be used")
	}

	_, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1}, []byte(`{"model":"gpt-5.4"}`), "", "", "sk-local", nil, http.Header{}, false)
	if err == nil {
		t.Fatal("ExecuteRequest() error = nil, want missing account error after bypassing websocket")
	}
	if wsCalled {
		t.Fatal("WebsocketExecuteFunc was called despite explicit useWebsocket=false")
	}
}

func TestExecuteRequestForcedWebsocketUsesStatelessSessionWhenMissing(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	// per-api-key mode: restores the v2 behavior and asserts that prompt_cache_key is deterministic across requests (regression guard for 8ea79aa).
	nextSettings.RequestIsolationMode = RequestIsolationModePerAPIKey
	ApplyRuntimeSettings(nextSettings)

	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })
	var gotSessionIDs []string
	var gotCacheKeys []string
	var gotPoolKeys []string
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		gotSessionIDs = append(gotSessionIDs, sessionID)
		gotCacheKeys = append(gotCacheKeys, gjson.GetBytes(requestBody, "prompt_cache_key").String())
		gotPoolKeys = append(gotPoolKeys, poolRouteKey)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_test"}`)),
		}, nil
	}

	for i := 0; i < 2; i++ {
		resp, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1, AccessToken: "token"}, []byte(`{"model":"gpt-5.4"}`), "", "", "sk-local", nil, http.Header{})
		if err != nil {
			t.Fatalf("ExecuteRequest() error = %v", err)
		}
		resp.Body.Close()
	}
	for _, sessionID := range gotSessionIDs {
		if !strings.HasPrefix(sessionID, "stateless-") {
			t.Fatalf("sessionID = %q, want stateless-*", sessionID)
		}
	}
	if gotSessionIDs[0] == gotSessionIDs[1] {
		t.Fatalf("stateless sessionIDs should differ per request, both = %q", gotSessionIDs[0])
	}
	// In per-api-key mode the prompt cache key must be deterministic: two requests match, and it does not equal the one-off connection ID.
	if gotCacheKeys[0] == "" || gotCacheKeys[0] != gotCacheKeys[1] {
		t.Fatalf("prompt_cache_key = %q / %q, want identical deterministic key", gotCacheKeys[0], gotCacheKeys[1])
	}
	if strings.HasPrefix(gotCacheKeys[0], "stateless-") {
		t.Fatalf("prompt_cache_key = %q, must not be a stateless connection ID", gotCacheKeys[0])
	}
	// per-api-key mode does not split the connection pool key (the cache key is both the upstream identity and the baseKey).
	if gotPoolKeys[0] != "" {
		t.Fatalf("per-api-key mode poolRouteKey = %q, want empty", gotPoolKeys[0])
	}
}

// TestExecuteRequestForcedWebsocketIsolatedMode verifies the default isolated mode: when there is no explicit session,
// the upstream prompt_cache_key is unique per request (isolated), but the pool route key (poolRouteKey) is stable per API key
// so that the 8-slot pool reuse and protection against handshake rate limiting are kept.
func TestExecuteRequestForcedWebsocketIsolatedMode(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	nextSettings.RequestIsolationMode = RequestIsolationModeIsolated
	ApplyRuntimeSettings(nextSettings)

	previousWS := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousWS })
	var gotCacheKeys []string
	var gotPoolKeys []string
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		gotCacheKeys = append(gotCacheKeys, gjson.GetBytes(requestBody, "prompt_cache_key").String())
		gotPoolKeys = append(gotPoolKeys, poolRouteKey)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_test"}`)),
		}, nil
	}

	for i := 0; i < 2; i++ {
		resp, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1, AccessToken: "token"}, []byte(`{"model":"gpt-5.4"}`), "", "", "sk-local", nil, http.Header{})
		if err != nil {
			t.Fatalf("ExecuteRequest() error = %v", err)
		}
		resp.Body.Close()
	}
	// Upstream identity isolation: the two prompt_cache_key values differ, and both are non-empty and not a stateless connection ID.
	if gotCacheKeys[0] == "" || gotCacheKeys[0] == gotCacheKeys[1] {
		t.Fatalf("isolated prompt_cache_key = %q / %q, want distinct per request", gotCacheKeys[0], gotCacheKeys[1])
	}
	if strings.HasPrefix(gotCacheKeys[0], "stateless-") {
		t.Fatalf("prompt_cache_key = %q, must not be a stateless connection ID", gotCacheKeys[0])
	}
	// Connection pool key is stable: the two poolRouteKey values match and are non-empty, derived from the API key (guards against handshake storms).
	if gotPoolKeys[0] == "" || gotPoolKeys[0] != gotPoolKeys[1] {
		t.Fatalf("isolated poolRouteKey = %q / %q, want identical stable key", gotPoolKeys[0], gotPoolKeys[1])
	}
	// The pool key must not equal the per-request unique upstream identity key, or the slot pool breaks.
	if gotPoolKeys[0] == gotCacheKeys[0] {
		t.Fatalf("poolRouteKey must not equal per-request upstream cache key")
	}
}

// TestResolveUpstreamSessionID covers each branch of the upstream identity key derivation.
func TestResolveUpstreamSessionID(t *testing.T) {
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })

	setMode := func(mode string) {
		next := previousSettings
		next.RequestIsolationMode = mode
		ApplyRuntimeSettings(next)
	}

	// 1. WS with no explicit session: always "" (handed to the stateless path), regardless of the isolation mode.
	setMode(RequestIsolationModeIsolated)
	if got := resolveUpstreamSessionID(7, "sess-abc", "", true); got != "" {
		t.Fatalf("WS no-explicit isolated: got %q, want empty", got)
	}
	setMode(RequestIsolationModePerAPIKey)
	if got := resolveUpstreamSessionID(7, "sess-abc", "", true); got != "" {
		t.Fatalf("WS no-explicit per-api-key: got %q, want empty", got)
	}

	// 2. HTTP with no explicit session + isolated: unique per request (the two values differ and are both non-empty).
	setMode(RequestIsolationModeIsolated)
	a := resolveUpstreamSessionID(7, "sess-abc", "", false)
	b := resolveUpstreamSessionID(7, "sess-abc", "", false)
	if a == "" || a == b {
		t.Fatalf("HTTP isolated: got %q / %q, want distinct non-empty", a, b)
	}

	// 3. HTTP with no explicit session + per-api-key: deterministic (the two values match and equal IsolateCodexSessionID).
	setMode(RequestIsolationModePerAPIKey)
	c := resolveUpstreamSessionID(7, "sess-abc", "", false)
	d := resolveUpstreamSessionID(7, "sess-abc", "", false)
	if c == "" || c != d {
		t.Fatalf("HTTP per-api-key: got %q / %q, want identical deterministic", c, d)
	}
	if want := IsolateCodexSessionID(7, "sess-abc"); c != want {
		t.Fatalf("HTTP per-api-key: got %q, want %q", c, want)
	}

	// 4. Explicit session: both modes use the deterministic IsolateCodexSessionID (a session the user declared explicitly is not isolated).
	for _, mode := range []string{RequestIsolationModeIsolated, RequestIsolationModePerAPIKey} {
		setMode(mode)
		got := resolveUpstreamSessionID(7, "sess-xyz", "sess-xyz", false)
		if want := IsolateCodexSessionID(7, "sess-xyz"); got != want {
			t.Fatalf("explicit session mode=%s: got %q, want %q", mode, got, want)
		}
	}
}

// The HTTP outbound path must strip the top-level type of the WS event envelope as a fallback, even when the upper-layer prepare is bypassed
// (the 1009 degrade of native WS ingress / image generation forcing HTTP / Agent Identity forcing HTTP
// all of them send an envelope body that reaches this point); nested types are not affected (issue #548).
func TestExecuteRequestHTTPStripsTopLevelEnvelopeType(t *testing.T) {
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	bodyCh := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readUpstreamRequestBody(r)
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_test"}`))
	}))
	t.Cleanup(upstream.Close)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	raw := []byte(`{"type":"response.create","model":"gpt-5.4","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	resp, err := ExecuteRequest(context.Background(), &auth.Account{DBID: 1, AccessToken: "token"}, raw, "", "", "sk-local", nil, http.Header{}, false)
	if err != nil {
		t.Fatalf("ExecuteRequest() error = %v", err)
	}
	resp.Body.Close()

	select {
	case got := <-bodyCh:
		if gjson.GetBytes(got, "type").Exists() {
			t.Fatalf("top-level type should be stripped before HTTP upstream: %s", got)
		}
		if it := gjson.GetBytes(got, "input.0.type").String(); it != "message" {
			t.Fatalf("nested input type = %q, want message; body=%s", it, got)
		}
		if ct := gjson.GetBytes(got, "input.0.content.0.type").String(); ct != "input_text" {
			t.Fatalf("nested content type = %q, want input_text; body=%s", ct, got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}
}
