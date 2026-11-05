package proxy

import (
	"testing"

	"github.com/gin-gonic/gin"
)

// End to end: a local high-confidence severe violation must record one strike on the NewAPI user before it is sent upstream,
// triggering the CYB accrual and automatic ban on the NewAPI side, while repeated probes in the same session are caught by the session lock
// and no further strikes accrue.
func TestLocalSevereViolationEmitsStrikeThenLockDeduplicates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newPromptConversationLockTestHandler(t)
	identity := newAPIIdentity{UserID: "77", ClientIP: "203.0.113.9"}
	fingerprint := "0123456789abcdef0123456789abcdef"

	// First: a terminal severe violation sent directly by the current user. It should be blocked locally and record one strike.
	blatantBody := promptRequestBody(t, blatantIntentBlockedByLocalRegex)
	first := signedBoundNewAPIPolicyContext(t, "local-strike-first", identity, blatantBody, 101, "gateway-a", "gateway-a-secret", fingerprint)
	setIngressRequestBodyIfAbsent(first, blatantBody)
	if blocked := handler.inspectPromptFilterOpenAI(first, blatantBody, "/v1/responses", "gpt-5.5"); !blocked {
		t.Fatal("anchor severe violation was not blocked locally, test precondition not met")
	}
	firstMeta := policyDecisionMetadataFromHeaders(first.Writer.Header())
	if !firstMeta.StrikeEligible {
		t.Fatalf("first local severe violation did not record a strike: %+v", firstMeta)
	}
	if firstMeta.ReasonCode == promptConversationLockedReasonCode {
		t.Fatal("first violation should not appear as a session lock block")
	}

	// Second: the same session probes again (even with a variant that bypasses the regex). It should be blocked by the session lock,
	// and must never accrue a strike again, otherwise one violation would push the user straight to the ban threshold.
	evasiveBody := promptRequestBody(t, evasiveVariantThatDefeatsLocalRegex)
	second := signedBoundNewAPIPolicyContext(t, "local-strike-repeat", identity, evasiveBody, 101, "gateway-a", "gateway-a-secret", fingerprint)
	setIngressRequestBodyIfAbsent(second, evasiveBody)
	if blocked := handler.inspectPromptFilterOpenAI(second, evasiveBody, "/v1/responses", "gpt-5.5"); !blocked {
		t.Fatal("repeated probe in a locked session was let through upstream")
	}
	secondMeta := policyDecisionMetadataFromHeaders(second.Writer.Header())
	if secondMeta.ReasonCode != promptConversationLockedReasonCode {
		t.Fatalf("repeated probe should appear as a session lock block, actual reason=%q", secondMeta.ReasonCode)
	}
	if secondMeta.StrikeEligible {
		t.Fatal("repeated block in a locked session accrued a strike again")
	}
}

// With LocalSevereStrikeEnabled turned off, local severe violations are still blocked, but no longer record a strike—
// Blocking (safety) and banning (irreversible) are decoupled, and the operator controls each one independently.
func TestLocalSevereStrikeCanBeDisabledWhileStillBlocking(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newPromptConversationLockTestHandler(t)
	cfg := handler.store.GetPromptFilterConfig()
	cfg.Advanced.Enforcement.LocalSevereStrikeEnabled = false
	handler.store.SetPromptFilterConfig(cfg)

	body := promptRequestBody(t, blatantIntentBlockedByLocalRegex)
	c := signedBoundNewAPIPolicyContext(t, "local-strike-disabled", newAPIIdentity{UserID: "77", ClientIP: "203.0.113.9"}, body, 101, "gateway-a", "gateway-a-secret", "0123456789abcdef0123456789abcdef")
	setIngressRequestBodyIfAbsent(c, body)
	if blocked := handler.inspectPromptFilterOpenAI(c, body, "/v1/responses", "gpt-5.5"); !blocked {
		t.Fatal("turning off strike accrual should not affect the block itself")
	}
	meta := policyDecisionMetadataFromHeaders(c.Writer.Header())
	if meta.StrikeEligible {
		t.Fatalf("strike still recorded after LocalSevereStrikeEnabled was turned off: %+v", meta)
	}
}
