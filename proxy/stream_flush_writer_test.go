package proxy

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestWriteSSECommentDrainsBufferFirstAndBypassesScanner(t *testing.T) {
	var out bytes.Buffer
	w := &streamFlushWriter{writer: &out, policy: StreamFlushPolicyCoalesce, interval: time.Hour}
	// A zero lastFlush value would flush the first write immediately; setting it to the current time lets the coalesce buffer really hold the event.
	w.lastFlush = time.Now()

	// The coalesce buffer holds one complete event (the interval is very long, so it never flushes on its own).
	if err := w.WriteSSEData([]byte(`{"type":"response.output_text.delta"}`)); err != nil {
		t.Fatalf("WriteSSEData: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("events should not reach the underlying writer immediately under the coalesce strategy: %q", out.String())
	}

	if err := w.WriteSSEComment(continueKeepaliveComment); err != nil {
		t.Fatalf("WriteSSEComment: %v", err)
	}
	got := out.String()
	idxEvent := strings.Index(got, `data: {"type":"response.output_text.delta"}`)
	idxComment := strings.Index(got, ": keepalive\n\n")
	if idxEvent < 0 || idxComment < 0 {
		t.Fatalf("output is missing an event or comment: %q", got)
	}
	if idxComment < idxEvent {
		t.Fatalf("comment landed before the buffered event, so the event order was broken: %q", got)
	}
}

func TestWriteSSECommentNilAndEmptySafe(t *testing.T) {
	var w *streamFlushWriter
	if err := w.WriteSSEComment(": keepalive\n\n"); err != nil {
		t.Fatalf("nil writer: %v", err)
	}
	var out bytes.Buffer
	w2 := &streamFlushWriter{writer: &out}
	if err := w2.WriteSSEComment(""); err != nil {
		t.Fatalf("empty comment: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("an empty comment should not write any bytes: %q", out.String())
	}
}
