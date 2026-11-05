package wsrelay

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"testing"
)

// TestLogCompressionNegotiationDedupe verifies that the negotiation result log is deduplicated by result:
// The same result is reported only once. After each of the two results has been reported once, nothing more is printed, so mixed links do not flood the log.
func TestLogCompressionNegotiationDedupe(t *testing.T) {
	wsCompressionSeen.Store(0)
	t.Cleanup(func() { wsCompressionSeen.Store(0) })

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	negotiated := &http.Response{Header: http.Header{
		"Sec-Websocket-Extensions": {"permessage-deflate; server_no_context_takeover; client_no_context_takeover"},
	}}
	plain := &http.Response{Header: http.Header{}}

	logCompressionNegotiation(nil, 1)
	if buf.Len() != 0 {
		t.Fatalf("nil resp should not produce a log: %q", buf.String())
	}

	logCompressionNegotiation(negotiated, 1)
	if got := buf.String(); !strings.Contains(got, "negotiated permessage-deflate") {
		t.Fatalf("the first successful negotiation should log, got %q", got)
	}
	buf.Reset()

	logCompressionNegotiation(negotiated, 2)
	if buf.Len() != 0 {
		t.Fatalf("a repeated result should not log again: %q", buf.String())
	}

	logCompressionNegotiation(plain, 3)
	if got := buf.String(); !strings.Contains(got, "did not negotiate permessage-deflate") {
		t.Fatalf("the first non-negotiation should also log once, got %q", got)
	}
	buf.Reset()

	logCompressionNegotiation(plain, 4)
	logCompressionNegotiation(negotiated, 5)
	if buf.Len() != 0 {
		t.Fatalf("after each of the two results has been reported once, output should be completely silent: %q", buf.String())
	}
}
