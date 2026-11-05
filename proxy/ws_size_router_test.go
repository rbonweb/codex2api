package proxy

import (
	"testing"
	"time"
)

func TestWebsocketSizeRouterLearnsAndRoutes(t *testing.T) {
	var r websocketSizeRouter

	if r.PreferHTTP(10 * 1024 * 1024) {
		t.Fatal("without any learned sample, requests should not be rerouted to HTTP")
	}

	r.RecordMessageTooBig(400 * 1024)
	if !r.PreferHTTP(400 * 1024) {
		t.Fatal("a request reaching the known failing size should be rerouted to HTTP")
	}
	if !r.PreferHTTP(390 * 1024) {
		t.Fatal("a request within the margin (95%) should be rerouted to HTTP")
	}
	if r.PreferHTTP(300 * 1024) {
		t.Fatal("a request clearly below the threshold should keep using WS")
	}

	// A smaller failing sample tightens the threshold; a larger one does not loosen it
	r.RecordMessageTooBig(350 * 1024)
	if !r.PreferHTTP(340 * 1024) {
		t.Fatal("a smaller failing sample should tighten the threshold")
	}
	r.RecordMessageTooBig(900 * 1024)
	if r.minTooBig != 350*1024 {
		t.Fatalf("a larger failing sample should not loosen the threshold, minTooBig=%d", r.minTooBig)
	}
}

func TestWebsocketSizeRouterIgnoresTinySamples(t *testing.T) {
	var r websocketSizeRouter
	r.RecordMessageTooBig(wsSizeRouterMinSample - 1)
	if r.PreferHTTP(10 * 1024 * 1024) {
		t.Fatal("a 1009 below the sample floor should not take part in learning")
	}
}

func TestWebsocketSizeRouterExpiresLearnedThreshold(t *testing.T) {
	var r websocketSizeRouter
	r.RecordMessageTooBig(400 * 1024)
	r.learnedAt = time.Now().Add(-wsSizeRouterTTL - time.Minute)
	if r.PreferHTTP(500 * 1024) {
		t.Fatal("an expired learned result should no longer apply")
	}
	if r.minTooBig != 0 {
		t.Fatal("learned state should be cleared after expiry")
	}
}

func TestWebsocketSizeRouterEnvEscapeHatch(t *testing.T) {
	t.Setenv("CODEX_WS_SIZE_ROUTER", "off")
	var r websocketSizeRouter
	r.RecordMessageTooBig(400 * 1024)
	if r.PreferHTTP(500 * 1024) {
		t.Fatal("with CODEX_WS_SIZE_ROUTER=off the old behavior should be kept")
	}
}
