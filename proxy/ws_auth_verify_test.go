package proxy

import (
	"errors"
	"fmt"
	"testing"
)

// TestClassifyStreamOutcomeFlagsWSCloseForAuthVerify verifies that a WS upstream read stream failure
// (such as close 1008 policy violation) sets verifyAccountAuth, which is used to verify account auth asynchronously;
// a plain transport failure, a downstream disconnect, or a normal termination does not set it.
func TestClassifyStreamOutcomeFlagsWSCloseForAuthVerify(t *testing.T) {
	wsErr := fmt.Errorf("websocket read error: %w", errors.New("websocket: close 1008 (policy violation)"))
	if out := classifyStreamOutcome(nil, wsErr, nil, false); !out.verifyAccountAuth {
		t.Fatalf("WS close should set verifyAccountAuth, got %+v", out)
	}
	messageTooBig := errors.New("websocket read error: websocket: close 1009 (message too big)")
	if out := classifyStreamOutcome(nil, messageTooBig, nil, false); out.verifyAccountAuth || out.penalize {
		t.Fatalf("WS close 1009 should not trigger the auth probe or an account penalty, got %+v", out)
	}

	if out := classifyStreamOutcome(nil, errors.New("connection reset by peer"), nil, false); out.verifyAccountAuth {
		t.Fatal("a plain transport failure should not trigger auth verification")
	}

	if out := classifyStreamOutcome(nil, nil, nil, true); out.verifyAccountAuth {
		t.Fatal("a normal termination should not trigger auth verification")
	}

	if out := classifyStreamOutcome(errors.New("context canceled"), nil, nil, false); out.verifyAccountAuth {
		t.Fatal("a downstream disconnect should not trigger auth verification")
	}
}

func TestIsWebsocketUpstreamClose(t *testing.T) {
	if !isWebsocketUpstreamClose(fmt.Errorf("websocket read error: %w", errors.New("websocket: close 1008 (policy violation)"))) {
		t.Fatal("should recognize a WS upstream read failure")
	}
	if isWebsocketUpstreamClose(nil) {
		t.Fatal("nil should not be recognized as a WS close")
	}
	if isWebsocketUpstreamClose(errors.New("some http transport error")) {
		t.Fatal("a non-WS error should not be recognized")
	}
}
