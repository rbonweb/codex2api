package proxy

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/codex2api/security/promptfilter"
)

func newOutputScannerForTest(t *testing.T, bufferBytes int) *promptfilter.OutputScanner {
	t.Helper()
	cfg := promptfilter.DefaultConfig()
	cfg.Enabled = true
	cfg.Advanced.Output.Enabled = true
	cfg.Advanced.Output.BufferBytes = bufferBytes
	cfg.Advanced.Output.OverlapBytes = 64
	scanner := promptfilter.NewOutputScanner(cfg)
	if scanner == nil {
		t.Fatal("expected output scanner")
	}
	return scanner
}

func issue696Frames(n int) [][]byte {
	frames := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		frames = append(frames, []byte(fmt.Sprintf(
			`{"type":"response.output_text.delta","sequence_number":%d,"delta":"%s","safety_buffering":false}`,
			1300+i, strings.Repeat("x", 120+i*3))))
	}
	return frames
}

// issue #696: when the output scanner deducts the safety window byte by byte, the underlying stream once stopped in the middle of an event, bypassing the writer
// to write to ResponseWriter directly, a request-level keepalive comment got inserted into the middle of JSON. The writer now aligns the underlying write
// to event boundaries; a directly written comment must occupy its own line, and all event bytes must arrive unchanged and in order.
func TestStreamFlushWriterOutputScannerKeepsUnderlyingAtEventBoundary(t *testing.T) {
	var sink bytes.Buffer
	w := &streamFlushWriter{writer: &sink, policy: StreamFlushPolicyImmediate, outputScanner: newOutputScannerForTest(t, 512)}
	frames := issue696Frames(40)
	var expected bytes.Buffer
	for _, frame := range frames {
		if err := w.WriteSSEData(frame); err != nil {
			t.Fatal(err)
		}
		appendSSEData(&expected, frame)
		if tail := sink.Bytes(); len(tail) > 0 && !bytes.HasSuffix(tail, sseDataSuffix) {
			t.Fatalf("underlying stream stopped mid-event after frame %s: %q", frame[:40], tail[len(tail)-40:])
		}
	}
	if sink.Len() == 0 {
		t.Fatal("scanner released nothing; test needs more frames than the safety window")
	}
	// Simulates a request-level keepalive writing directly to the underlying writer, bypassing the writer.
	sink.WriteString(downstreamSSEKeepaliveComment)
	if err := w.Finalize(); err != nil {
		t.Fatal(err)
	}
	out := sink.String()
	idx := strings.Index(out, ": keepalive")
	if idx < 0 {
		t.Fatal("keepalive missing")
	}
	if idx > 0 && !strings.HasSuffix(out[:idx], "\n\n") {
		t.Fatalf("keepalive landed inside an event: %q", out[max(0, idx-60):idx+14])
	}
	if got := strings.Replace(out, downstreamSSEKeepaliveComment, "", 1); got != expected.String() {
		t.Fatalf("event bytes changed or reordered\n got: %q\nwant: %q", got[:min(len(got), 200)], expected.String()[:200])
	}
}

// A comment is not model output; even with output scanning on, it must reach the underlying writer immediately, or keepalive stops working during upstream silence.
func TestStreamFlushWriterSSECommentBypassesOutputScanner(t *testing.T) {
	var sink bytes.Buffer
	w := &streamFlushWriter{writer: &sink, policy: StreamFlushPolicyImmediate, outputScanner: newOutputScannerForTest(t, 4096)}
	if err := w.WriteSSEData([]byte(`{"type":"response.output_text.delta","delta":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	if sink.Len() != 0 {
		t.Fatalf("scanner should still hold the first event, got %q", sink.String())
	}
	if err := w.WriteSSEComment(downstreamSSEKeepaliveComment); err != nil {
		t.Fatal(err)
	}
	if sink.String() != downstreamSSEKeepaliveComment {
		t.Fatalf("comment not delivered immediately: %q", sink.String())
	}
	if err := w.Finalize(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(sink.String(), `"delta":"hi"}`+"\n\n") {
		t.Fatalf("held event not released on finalize: %q", sink.String())
	}
}

// Under the coalesced flush strategy, boundary alignment must also hold: the coalesced buffer only ever holds complete events.
func TestStreamFlushWriterOutputScannerCoalesceKeepsWholeEvents(t *testing.T) {
	var sink bytes.Buffer
	w := &streamFlushWriter{writer: &sink, policy: StreamFlushPolicyCoalesce, interval: 0, outputScanner: newOutputScannerForTest(t, 512)}
	for _, frame := range issue696Frames(30) {
		if err := w.WriteSSEData(frame); err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if tail := sink.Bytes(); len(tail) > 0 && !bytes.HasSuffix(tail, sseDataSuffix) {
			t.Fatalf("underlying stream stopped mid-event: %q", tail[len(tail)-40:])
		}
	}
}
