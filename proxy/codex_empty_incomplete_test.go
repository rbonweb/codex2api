package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestEmptyIncompleteTracker_Detection(t *testing.T) {
	trueEmpty := []byte(`{"type":"response.incomplete","response":{"id":"r1","output":[],"usage":{"output_tokens":0}}}`)
	parsedEmpty := gjson.ParseBytes(trueEmpty)

	t.Run("true empty incomplete", func(t *testing.T) {
		tracker := &emptyIncompleteTracker{}
		if !tracker.IsEmptyIncomplete("response.incomplete", parsedEmpty) {
			t.Fatal("expected empty incomplete to be detected")
		}
	})
	t.Run("non-empty delta observed", func(t *testing.T) {
		tracker := &emptyIncompleteTracker{}
		tracker.Observe("response.output_text.delta", gjson.Parse(`{"type":"response.output_text.delta","delta":"Hi"}`))
		if tracker.IsEmptyIncomplete("response.incomplete", parsedEmpty) {
			t.Fatal("text delta must suppress detection")
		}
	})
	t.Run("whitespace delta does not count", func(t *testing.T) {
		tracker := &emptyIncompleteTracker{}
		tracker.Observe("response.output_text.delta", gjson.Parse(`{"type":"response.output_text.delta","delta":"   "}`))
		tracker.Observe("response.reasoning_summary_text.delta", gjson.Parse(`{"delta":""}`))
		if !tracker.IsEmptyIncomplete("response.incomplete", parsedEmpty) {
			t.Fatal("empty/whitespace deltas must not count as output")
		}
	})
	t.Run("function call arguments delta counts", func(t *testing.T) {
		tracker := &emptyIncompleteTracker{}
		tracker.Observe("response.function_call_arguments.delta", gjson.Parse(`{"delta":"{\"a\":1}"}`))
		if tracker.IsEmptyIncomplete("response.incomplete", parsedEmpty) {
			t.Fatal("tool argument delta must suppress detection")
		}
	})
	t.Run("output item done counts", func(t *testing.T) {
		tracker := &emptyIncompleteTracker{}
		tracker.Observe("response.output_item.done", gjson.Parse(`{"item":{"type":"reasoning"}}`))
		if tracker.IsEmptyIncomplete("response.incomplete", parsedEmpty) {
			t.Fatal("completed output item must suppress detection")
		}
	})
	t.Run("positive output tokens", func(t *testing.T) {
		tracker := &emptyIncompleteTracker{}
		parsed := gjson.Parse(`{"type":"response.incomplete","response":{"output":[],"usage":{"output_tokens":5}}}`)
		if tracker.IsEmptyIncomplete("response.incomplete", parsed) {
			t.Fatal("output_tokens > 0 must not be treated as empty")
		}
	})
	t.Run("float zero-ish tokens rejected", func(t *testing.T) {
		tracker := &emptyIncompleteTracker{}
		parsed := gjson.Parse(`{"type":"response.incomplete","response":{"output":[],"usage":{"output_tokens":0.5}}}`)
		if tracker.IsEmptyIncomplete("response.incomplete", parsed) {
			t.Fatal("0.5 must not be treated as zero")
		}
	})
	t.Run("missing or null tokens rejected", func(t *testing.T) {
		tracker := &emptyIncompleteTracker{}
		for _, raw := range []string{
			`{"type":"response.incomplete","response":{"output":[],"usage":{}}}`,
			`{"type":"response.incomplete","response":{"output":[],"usage":{"output_tokens":null}}}`,
			`{"type":"response.incomplete","response":{"output":[]}}`,
		} {
			if tracker.IsEmptyIncomplete("response.incomplete", gjson.Parse(raw)) {
				t.Fatalf("payload without explicit numeric zero must not match: %s", raw)
			}
		}
	})
	t.Run("non-empty response.output rejected", func(t *testing.T) {
		tracker := &emptyIncompleteTracker{}
		parsed := gjson.Parse(`{"type":"response.incomplete","response":{"output":[{"type":"message"}],"usage":{"output_tokens":0}}}`)
		if tracker.IsEmptyIncomplete("response.incomplete", parsed) {
			t.Fatal("non-empty output must not be treated as empty")
		}
	})
	t.Run("response.completed never matches", func(t *testing.T) {
		tracker := &emptyIncompleteTracker{}
		parsed := gjson.Parse(`{"type":"response.completed","response":{"output":[],"usage":{"output_tokens":0}}}`)
		if tracker.IsEmptyIncomplete("response.completed", parsed) {
			t.Fatal("only response.incomplete is eligible")
		}
	})
}

func TestRewriteEmptyIncompleteTerminal_SynthesizesRetryableFailure(t *testing.T) {
	tracker := &emptyIncompleteTracker{}
	original := []byte(`{"type":"response.incomplete","sequence_number":7,"response":{"id":"resp_1","model":"gpt-5.5","service_tier":"default","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":12,"output_tokens":0,"total_tokens":12}}}`)
	eventType, data, parsed := rewriteEmptyIncompleteTerminal(tracker, "response.incomplete", original, gjson.ParseBytes(original))
	if eventType != "response.failed" {
		t.Fatalf("event type = %q, want response.failed", eventType)
	}
	if got := parsed.Get("type").String(); got != "response.failed" {
		t.Fatalf("payload type = %q", got)
	}
	if got := parsed.Get("response.status").String(); got != "failed" {
		t.Fatalf("response.status = %q", got)
	}
	if parsed.Get("response.incomplete_details").Exists() {
		t.Fatal("incomplete_details must be dropped from the synthesized failure")
	}
	if got := parsed.Get("response.error.code").String(); got != codexEmptyIncompleteErrorCode {
		t.Fatalf("error.code = %q", got)
	}
	if got := parsed.Get("response.id").String(); got != "resp_1" {
		t.Fatal("response.id must be preserved for logging")
	}
	if got := parsed.Get("response.usage.input_tokens").Int(); got != 12 {
		t.Fatal("usage must be preserved for billing")
	}
	if !isResponsesTerminalEvent(eventType) || isResponsesSuccessTerminalEvent(eventType) {
		t.Fatal("rewritten event must be a failure terminal")
	}

	outcome := classifyResponseFailedOutcome(data)
	if outcome.logStatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", outcome.logStatusCode)
	}
	if !outcome.penalize {
		t.Fatal("outcome must remain retryable (penalize drives transparent retry)")
	}
	if !outcome.requestScoped {
		t.Fatal("outcome must be request scoped so the account is not penalized")
	}
	if outcome.capacityShed {
		t.Fatal("must not be classified as capacity shed")
	}
	if outcome.failureKind != codexEmptyIncompleteFailureKind {
		t.Fatalf("failureKind = %q", outcome.failureKind)
	}
	if !strings.Contains(outcome.failureMessage, "0 output tokens") {
		t.Fatalf("failureMessage = %q", outcome.failureMessage)
	}
	if !responseFailedRetryable(data) {
		t.Fatal("WS ingress relies on responseFailedRetryable for pre-content rotation")
	}
}

func TestRewriteEmptyIncompleteTerminal_PassThroughWhenNotEmpty(t *testing.T) {
	tracker := &emptyIncompleteTracker{}
	tracker.Observe("response.output_text.delta", gjson.Parse(`{"delta":"partial"}`))
	original := []byte(`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"output_tokens":0}}}`)
	eventType, data, _ := rewriteEmptyIncompleteTerminal(tracker, "response.incomplete", original, gjson.ParseBytes(original))
	if eventType != "response.incomplete" {
		t.Fatalf("event type = %q, want untouched response.incomplete", eventType)
	}
	if string(data) != string(original) {
		t.Fatal("payload must be returned untouched")
	}
}

func TestEmptyIncompleteNonStreamBody(t *testing.T) {
	body := []byte(`{"id":"resp_2","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":3,"output_tokens":0}}`)
	if !isEmptyIncompleteResponseBody(body) {
		t.Fatal("expected non-stream empty incomplete to be detected")
	}
	rewritten := synthesizeEmptyIncompleteFailureBody(body)
	if got := gjson.GetBytes(rewritten, "status").String(); got != "failed" {
		t.Fatalf("status = %q", got)
	}
	if gjson.GetBytes(rewritten, "incomplete_details").Exists() {
		t.Fatal("incomplete_details must be dropped")
	}
	failure, failed := responsesNonStreamFailure(rewritten)
	if !failed {
		t.Fatal("rewritten body must be recognized as a non-stream failure")
	}
	if failure.logStatusCode != http.StatusBadGateway || !failure.requestScoped {
		t.Fatalf("unexpected outcome: status=%d requestScoped=%v", failure.logStatusCode, failure.requestScoped)
	}

	completed := []byte(`{"id":"resp_3","object":"response","status":"completed","output":[],"usage":{"output_tokens":0}}`)
	if isEmptyIncompleteResponseBody(completed) {
		t.Fatal("completed responses are never rewritten")
	}
	truncated := []byte(`{"id":"resp_4","object":"response","status":"incomplete","output":[{"type":"message"}],"usage":{"output_tokens":0}}`)
	if isEmptyIncompleteResponseBody(truncated) {
		t.Fatal("incomplete with output must stay a normal truncation")
	}
}

func TestReportStreamOutcomeFailure_SkipsRequestScoped(t *testing.T) {
	// 空 incomplete 与容量降载一样不进账号健康度:用真实 store 与账号断言
	// 连击与健康档位不变,并以同样的非请求维度故障做对照。
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	store.AddAccount(account)
	h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	failureStreak := func() int {
		account.Mu().RLock()
		defer account.Mu().RUnlock()
		return account.FailureStreak
	}
	tierBefore := account.GetHealthTier()

	scoped := streamOutcome{logStatusCode: http.StatusBadGateway, failureKind: codexEmptyIncompleteFailureKind, penalize: true, requestScoped: true}
	h.reportStreamOutcomeFailure(account, scoped, 0)
	if got := failureStreak(); got != 0 {
		t.Fatalf("request-scoped failure must not touch FailureStreak, got %d", got)
	}
	if got := account.GetHealthTier(); got != tierBefore {
		t.Fatalf("request-scoped failure must not change health tier: %s -> %s", tierBefore, got)
	}

	plain := streamOutcome{logStatusCode: http.StatusBadGateway, failureKind: "server", penalize: true}
	h.reportStreamOutcomeFailure(account, plain, 0)
	if got := failureStreak(); got != 1 {
		t.Fatalf("control: ordinary 5xx must be reported, FailureStreak = %d", got)
	}
}
