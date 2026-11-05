package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// The real sequence of upstream capacity shed is "event: error → event: response.failed". Retryable
// error frames do not count as client output: once written, wroteAnyBody is set, and the following response.failed
// can no longer reach the silent account-switch branch before the first packet. Non-retryable types keep being forwarded immediately, unchanged.
func TestIsRetryableUpstreamErrorFrame(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		payload   string
		want      bool
	}{
		{"capacity shed server_is_overloaded", "error", `{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`, true},
		{"capacity shed slow_down", "error", `{"type":"error","error":{"code":"slow_down","message":"slow down"}}`, true},
		{"rate limit rate_limit_exceeded", "error", `{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"limited"}}`, true},
		{"non-retryable content_policy forwarded as-is", "error", `{"type":"error","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"blocked"}}`, false},
		{"non-retryable invalid_request forwarded as-is", "error", `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_request","message":"bad"}}`, false},
		{"response.failed is not covered by this check", "response.failed", `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`, false},
		{"content events are not covered by this check", "response.output_text.delta", `{"type":"response.output_text.delta","delta":"hi"}`, false},
		{"empty payload", "error", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isRetryableUpstreamErrorFrame(tc.eventType, []byte(tc.payload))
			if got != tc.want {
				t.Errorf("isRetryableUpstreamErrorFrame(%q, %s) = %v, want %v", tc.eventType, tc.payload, got, tc.want)
			}
		})
	}
}

// isCapacityShedPayload recognizes capacity shed codes in response.failed / error frames, accepting both nesting shapes,
// server_error / rate_limit and the like do not count as capacity shed.
func TestIsCapacityShedPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"bare error.code overloaded", `{"error":{"code":"server_is_overloaded"}}`, true},
		{"bare error.code slow_down", `{"error":{"code":"SLOW_DOWN"}}`, true},
		{"nested response.error.code", `{"response":{"error":{"code":"server_is_overloaded"}}}`, true},
		{"status_details.error.code", `{"response":{"status_details":{"error":{"code":"slow_down"}}}}`, true},
		{"server_error is not a capacity shed", `{"error":{"code":"server_error"}}`, false},
		{"rate_limit is not a capacity shed", `{"error":{"code":"rate_limit_exceeded"}}`, false},
		{"empty payload", ``, false},
		{"non-JSON", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCapacityShedPayload([]byte(tc.payload)); got != tc.want {
				t.Errorf("isCapacityShedPayload(%s) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}

// Capacity shed events in classify keep penalize=true (reusing the pre-first-packet frame buffering / transparent retry mechanism),
// and also set capacityShed=true (so the reporting side skips the account failure streak). An environment variable can revert everything to the old behavior.
func TestClassifyResponseFailedOutcomeCapacityShed(t *testing.T) {
	shed := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","message":"overloaded"}}}`)

	got := classifyResponseFailedOutcome(shed)
	if !got.capacityShed {
		t.Fatalf("capacityShed = false, want true for shed payload")
	}
	if !got.penalize {
		t.Fatalf("penalize = false, want true (frame buffering and transparent retry depend on it)")
	}

	// A plain 500 is not capacity shed.
	server := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"internal_error","status_code":500}}}`)
	if classifyResponseFailedOutcome(server).capacityShed {
		t.Fatalf("capacityShed = true for generic 500, want false")
	}

	// Escape hatch: revert everything to the old behavior.
	t.Setenv("CODEX_DISABLE_CAPACITY_SHED_HANDLING", "1")
	if classifyResponseFailedOutcome(shed).capacityShed {
		t.Fatalf("capacityShed = true when disabled by env, want false")
	}
	if !classifyResponseFailedOutcome(shed).penalize {
		t.Fatalf("penalize = false when disabled, want true (the old behavior still retries on another account)")
	}
}

// A capacity shed first backs off and retries the same account maxCapacityShedSameAccountRetries times (keeping affinity), then switches account when exhausted;
// non-shed failures never keep affinity. Budgets are counted per account (map semantics).
func TestCapacityShedRetainsAffinity(t *testing.T) {
	shed := streamOutcome{capacityShed: true, penalize: true}
	for i := 0; i < maxCapacityShedSameAccountRetries; i++ {
		if !capacityShedRetainsAffinity(shed, i) {
			t.Fatalf("retriesSoFar=%d: want retain affinity", i)
		}
	}
	if capacityShedRetainsAffinity(shed, maxCapacityShedSameAccountRetries) {
		t.Fatalf("retriesSoFar=%d: want switch (budget exhausted)", maxCapacityShedSameAccountRetries)
	}
	nonShed := streamOutcome{capacityShed: false, penalize: true}
	if capacityShedRetainsAffinity(nonShed, 0) {
		t.Fatalf("non-shed outcome should never retain affinity")
	}

	// Per-account budgets: after account A is used up and switches to account B, B starts from 0 and still has its own backoff budget.
	retries := map[int64]int{}
	const accountA, accountB = int64(11), int64(22)
	for capacityShedRetainsAffinity(shed, retries[accountA]) {
		retries[accountA]++
	}
	if retries[accountA] != maxCapacityShedSameAccountRetries {
		t.Fatalf("accountA budget = %d, want %d", retries[accountA], maxCapacityShedSameAccountRetries)
	}
	if !capacityShedRetainsAffinity(shed, retries[accountB]) {
		t.Fatalf("accountB should have its own fresh budget, not inherit A's exhaustion")
	}
}

// After the budget is exhausted the account must be soft-excluded: shed does not penalize health, and merely unbinding affinity is not enough to switch accounts, so only soft exclusion can
// let scheduling pick a sibling account; after the pool has been tried, ResetSoft clears it so it is never parked permanently.
func TestCapacityShedExhaustionSoftExcludes(t *testing.T) {
	ex := newRetryAccountExclusions()
	ex.MarkSoft(11)
	if sel := ex.ForSelection(); !sel[11] {
		t.Fatalf("MarkSoft(11) should exclude account 11 from selection")
	}
	if !ex.ResetSoft() {
		t.Fatalf("ResetSoft should report it cleared a soft entry")
	}
	if sel := ex.ForSelection(); sel[11] {
		t.Fatalf("ResetSoft should clear soft exclusion of 11")
	}
	// Soft exclusion must not override hard exclusion.
	ex.MarkHard(33)
	ex.MarkSoft(33)
	ex.ResetSoft()
	if sel := ex.ForSelection(); !sel[33] {
		t.Fatalf("hard exclusion of 33 must survive ResetSoft")
	}
}

// Only the capacity shed codes (server_is_overloaded/slow_down) are rewritten to server_error, which clients can retry,
// while other error codes (especially rate_limit_exceeded, whose original code clients rely on to parse the retry delay) must be kept as-is;
// error messages are never changed.
func TestSanitizeCapacityShedEventForClient(t *testing.T) {
	cases := []struct {
		name        string
		eventType   string
		payload     string
		wantContain string
		wantAbsent  string
	}{
		{
			name:        "failed event nested code rewrite",
			eventType:   "response.failed",
			payload:     `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`,
			wantContain: `"code":"server_error"`,
			wantAbsent:  "server_is_overloaded",
		},
		{
			name:        "error frame code rewritten and message kept",
			eventType:   "error",
			payload:     `{"type":"error","error":{"code":"slow_down","message":"slow down"}}`,
			wantContain: `"message":"slow down"`,
			wantAbsent:  `"code":"slow_down"`,
		},
		{
			name:        "rate_limit not rewritten",
			eventType:   "response.failed",
			payload:     `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"try again in 3s"}}}`,
			wantContain: `"code":"rate_limit_exceeded"`,
		},
		{
			name:        "plain server_error not rewritten",
			eventType:   "error",
			payload:     `{"type":"error","error":{"code":"server_error","message":"boom"}}`,
			wantContain: `"code":"server_error"`,
		},
		{
			name:        "non-error events untouched",
			eventType:   "response.completed",
			payload:     `{"type":"response.completed","response":{"id":"resp_1"}}`,
			wantContain: `"id":"resp_1"`,
		},
		{
			name:        "non-JSON untouched",
			eventType:   "error",
			payload:     `not-json`,
			wantContain: `not-json`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(sanitizeCapacityShedEventForClient(tc.eventType, []byte(tc.payload)))
			if !strings.Contains(out, tc.wantContain) {
				t.Errorf("sanitize(%q) = %q, want contains %q", tc.payload, out, tc.wantContain)
			}
			if tc.wantAbsent != "" && strings.Contains(out, tc.wantAbsent) {
				t.Errorf("sanitize(%q) = %q, want NOT contains %q", tc.payload, out, tc.wantAbsent)
			}
		})
	}
}

// capacityShedEpilogue replicates how the Responses streaming path handles error/response.failed frames:
// the suppress → intercept → defer/write ordering (same shape as the three SSE forwarding loops in handler.go), and returns whether
// it hit the silent account-switch branch before the first packet.
func capacityShedEpilogue(t *testing.T, c *gin.Context, events [][]byte, attempt, maxRetries int) bool {
	t.Helper()
	c.Header("Content-Type", "text/event-stream")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Error("c.Writer does not implement http.Flusher")
		return false
	}
	streamWriter := newStreamFlushWriter(c.Writer, flusher)
	var pending bytes.Buffer
	ttftRecorded := false
	wroteAnyBody := false
	gotTerminal := false
	abortedForHTTPError := false
	var terminalFailurePayload []byte

	for _, data := range events {
		eventType := gjson.GetBytes(data, "type").String()
		if !ttftRecorded && isFirstTokenPayload(data) {
			ttftRecorded = true
		}
		if eventType == "response.completed" || eventType == "response.failed" {
			gotTerminal = true
		}
		if eventType == "response.failed" {
			terminalFailurePayload = append([]byte(nil), data...)
		}
		if shouldSuppressRetryableResponseFailedBeforeFirstToken(eventType, terminalFailurePayload, ttftRecorded, wroteAnyBody, attempt, maxRetries, nil, nil) {
			pending.Reset()
			return true
		}
		if shouldReturnHTTPErrorForResponseFailed(eventType, ttftRecorded, wroteAnyBody, false) {
			pending.Reset()
			abortedForHTTPError = true
			break
		}
		shouldDefer := !ttftRecorded && !gotTerminal &&
			(isPreContentLifecycleEvent(eventType) || isRetryableUpstreamErrorFrame(eventType, data))
		wrote, err := writeDeferredSSEData(streamWriter, &pending, sanitizeCapacityShedEventForClient(eventType, data), shouldDefer)
		if err != nil {
			t.Errorf("writeDeferredSSEData: %v", err)
			return false
		}
		if wrote {
			wroteAnyBody = true
		}
	}
	if wroteAnyBody {
		_ = streamWriter.Flush()
	}
	if abortedForHTTPError && !wroteAnyBody {
		outcome := classifyResponseFailedOutcome(terminalFailurePayload)
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(outcome.logStatusCode, gin.H{
			"error": gin.H{"message": outcome.failureMessage, "type": "upstream_error"},
		})
	}
	return false
}

// Regression case (real upstream shed sequence): created → in_progress → error frame → response.failed.
func TestStreamCapacityShedWireBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)

	shedError := []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"sequence_number":2}`)
	shedFailed := []byte(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}},"sequence_number":3}`)
	preamble := [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_1"},"sequence_number":0}`),
		[]byte(`{"type":"response.in_progress","response":{"id":"resp_1"},"sequence_number":1}`),
	}

	t.Run("shed before the first packet with retry budget left → silent account switch, zero bytes sent", func(t *testing.T) {
		var suppressed bool
		router := gin.New()
		router.GET("/stream", func(c *gin.Context) {
			suppressed = capacityShedEpilogue(t, c, append(append([][]byte{}, preamble...), shedError, shedFailed), 0, 3)
		})
		srv := httptest.NewServer(router)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/stream")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if !suppressed {
			t.Error("expected the silent account-switch branch before the first packet")
		}
		if len(body) != 0 {
			t.Errorf("expected zero bytes sent, got %q", body)
		}
	})

	t.Run("shed before the first packet with retries exhausted → real 5xx JSON instead of a 200 stream", func(t *testing.T) {
		router := gin.New()
		router.GET("/stream", func(c *gin.Context) {
			capacityShedEpilogue(t, c, append(append([][]byte{}, preamble...), shedError, shedFailed), 3, 3)
		})
		srv := httptest.NewServer(router)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/stream")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500 (body=%q)", resp.StatusCode, body)
		}
		if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("Content-Type = %q, want application/json", resp.Header.Get("Content-Type"))
		}
	})

	t.Run("shed mid-stream → passed through, but the shed code rewritten to a retryable server_error", func(t *testing.T) {
		events := append(append([][]byte{}, preamble...),
			[]byte(`{"type":"response.output_text.delta","delta":"partial"}`),
			shedError, shedFailed)
		router := gin.New()
		router.GET("/stream", func(c *gin.Context) {
			capacityShedEpilogue(t, c, events, 0, 3)
		})
		srv := httptest.NewServer(router)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/stream")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		body := string(raw)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if !strings.Contains(body, "partial") {
			t.Errorf("body should include the content already sent, got %q", body)
		}
		if !strings.Contains(body, "response.failed") {
			t.Errorf("body should include the terminal event, got %q", body)
		}
		if !strings.Contains(body, `"code":"server_error"`) {
			t.Errorf("shed code should be rewritten to server_error, got %q", body)
		}
		if strings.Contains(body, "server_is_overloaded") {
			t.Errorf("server_is_overloaded should not remain, got %q", body)
		}
		if !strings.Contains(body, "Our servers are currently overloaded") {
			t.Errorf("error message should be kept as-is, got %q", body)
		}
	})
}
