package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestUsageLimitedPoolMessagesTransientCarriesRetryAfter(t *testing.T) {
	got := usageLimitedPoolMessages(auth.UsageLimitedCandidateSummary{Found: true, TransientOnly: true, RetryAfter: 14*time.Second + 200*time.Millisecond})
	if got.RetryAfterSeconds != 15 {
		t.Fatalf("RetryAfterSeconds = %d, want 15 (ceil)", got.RetryAfterSeconds)
	}
	if !strings.Contains(got.Chinese, "15 seconds") || !strings.Contains(got.English, "15 seconds") {
		t.Fatalf("messages = %#v, want the wait time in both texts", got)
	}
	if strings.Contains(got.Chinese, "usage window") {
		t.Fatal("transient throttle must not be described as an exhausted usage window")
	}

	quota := usageLimitedPoolMessages(auth.UsageLimitedCandidateSummary{Found: true})
	if quota.RetryAfterSeconds != 0 || quota.Chinese != usageWindowExhaustedMessageZH {
		t.Fatalf("quota message = %#v, want usage-window text without Retry-After", quota)
	}
}
