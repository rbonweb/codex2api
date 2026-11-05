package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const pendingFirstTokenFlushBytes = 1024 * 1024

var (
	sseDataPrefix = []byte("data: ")
	sseDataSuffix = []byte("\n\n")
)

// streamFlushWriter handles SSE stream buffer merging and flush transport.
//
// Concurrency contract: the write methods of one instance (Flush / WriteBytes / WriteString / WriteSSEData / Finalize, etc.)
// must not be called concurrently across goroutines; the caller must serialize them with an external mutex (for example downstreamMu in the handler).
// serialization protects them. Internal mu and atomic aborted are used only to coordinate the state cleanup of output-runaway discard (DiscardBuffered)
// against concurrent writes and flushes, so that DiscardBuffered does not wait on slow downstream I/O.
type streamFlushWriter struct {
	writer        io.Writer
	flusher       http.Flusher
	policy        string
	interval      time.Duration
	lastFlush     time.Time
	buffer        bytes.Buffer
	outputScanner *promptfilter.OutputScanner
	// scanCarry holds the tail that the scanner has released but that does not yet fill a complete SSE event. The scanner, by byte count minus the safety window,
	// means the cut point can fall in the middle of an event; if half an event were written to the underlying writer, any bytes written straight to the ResponseWriter that bypass this
	// writer (request-level keepalive comments, the thinking heartbeat) would be inserted into the
	// JSON (issue #696). Here the underlying writes are aligned to event boundaries, ensuring the underlying stream always stops
	// between two events.
	scanCarry    []byte
	writtenBytes atomic.Int64
	diag         *streamPhaseDiagnostics
	// aborted marks that the buffer has been forcibly discarded by the output runaway guard. Once set, Flush/Finalize must
	// silently short-circuit, and must never write leftover bytes back to the downstream or release the scanner safety window.
	// Use atomic.Bool rather than a plain field: DiscardBuffered must set it before any lock contention
	// (first make new incoming writes short-circuit, then clean the buffer), and reads in Flush/WriteBytes/
	// Finalize are naturally free of data races. CAS guarantees that under concurrent triggering only the first call actually performs
	// the cleanup.
	aborted atomic.Bool
	// mu protects the reads, writes and cleanup of buffer / scanCarry / outputScanner / lastFlush.
	// The keepalive goroutine (issue #458) and the forward goroutine concurrently share the same writer and call Flush/WriteBytes;
	// the guard's discard callback runs inside HandleTripped's once,
	// outside the lock; without this lock the bytes.Buffer would be read and written concurrently (data race, corrupted output,
	// and at worst a panic).
	//
	// Downstream writes (w.writer.Write / flusher.Flush) are always done outside the lock: a slow client may keep the
	// write blocked for a long time; calling it under the lock would make DiscardBuffered wait on downstream I/O, breaking the
	// guard termination orchestration's constraint of not being delayed by the downstream write lock, and could also re-enter user callbacks.
	mu sync.Mutex
}

// Small buffers are reused across token events; an exceptional large event
// must not keep its allocation alive for the rest of a long stream.
// Caller must hold w.mu.
func (w *streamFlushWriter) resetBuffer() {
	if w.buffer.Cap() > 256<<10 {
		w.buffer = bytes.Buffer{}
	} else {
		w.buffer.Reset()
	}
}

// takeBufferedLocked takes a snapshot of the current content of the merged buffer and resets the buffer. The returned slice, after unlocking,
// can still be safely written downstream: resetBuffer only reuses w.buffer's own backing array, so the snapshot is independent.
// Caller must hold w.mu.
func (w *streamFlushWriter) takeBufferedLocked() []byte {
	if w.buffer.Len() == 0 {
		return nil
	}
	data := append([]byte(nil), w.buffer.Bytes()...)
	w.resetBuffer()
	return data
}

// flushDueLocked reports whether the merged buffer has reached its flush time point. Caller must hold w.mu.
func (w *streamFlushWriter) flushDueLocked() bool {
	return w.lastFlush.IsZero() || time.Since(w.lastFlush) >= w.interval
}

// streamPhaseDiagnostics collects the break-scene evidence for the stream phase.
//
// An upstream RST_STREAM (such as "INTERNAL_ERROR; received from peer") has two mutually exclusive causes,
// which cannot be told apart from the error text alone afterwards (issue #491):
//   - downstream backpressure: SSE is written back to the downstream synchronously inside the callback that reads upstream, so if the client reads slowly it stalls
//     body.Read, the HTTP/2 flow-control window is no longer replenished, and the upstream edge may reset the stream because of it.
//     then writeBlockedNanos is noticeably large.
//   - upstream itself: the backend was reclaimed or stayed quiet during long inference, and the edge resets directly. Then downstream write blocking
//     is close to 0, while the gap since the upstream's last frame is large.
//
// Both counters are written to the usage log along with the break information, so operators can read them on the error detail page without a new table.
type streamPhaseDiagnostics struct {
	writeBlockedNanos   atomic.Int64
	lastUpstreamFrameAt atomic.Int64 // UnixNano
	upstreamFrames      atomic.Int64
}

func newStreamPhaseDiagnostics() *streamPhaseDiagnostics {
	return &streamPhaseDiagnostics{}
}

// annotateStreamBreakDiagnostics adds break-scene evidence only to stream-break failures. A client-initiated disconnect
// (499) and an explicit upstream response.failed each have their own clear cause, so these two numbers are not needed.
func annotateStreamBreakDiagnostics(outcome streamOutcome, diag *streamPhaseDiagnostics) streamOutcome {
	if outcome.logStatusCode != logStatusUpstreamStreamBreak {
		return outcome
	}
	summary := diag.summary()
	if summary == "" {
		return outcome
	}
	outcome.failureMessage = strings.TrimSpace(strings.TrimSpace(outcome.failureMessage) + " " + summary)
	return outcome
}

// markUpstreamFrame records a mark each time an upstream SSE event arrives.
func (d *streamPhaseDiagnostics) markUpstreamFrame() {
	if d == nil {
		return
	}
	d.lastUpstreamFrameAt.Store(time.Now().UnixNano())
	d.upstreamFrames.Add(1)
}

func (d *streamPhaseDiagnostics) addWriteBlocked(start time.Time) {
	if d == nil || start.IsZero() {
		return
	}
	d.writeBlockedNanos.Add(int64(time.Since(start)))
}

// summary produces the human-readable evidence; it returns an empty string when there are no upstream frames at all (not even the response headers were read,
// unrelated to backpressure, and adding it would only be noise).
func (d *streamPhaseDiagnostics) summary() string {
	if d == nil {
		return ""
	}
	frames := d.upstreamFrames.Load()
	if frames == 0 {
		return ""
	}
	blockedMs := time.Duration(d.writeBlockedNanos.Load()).Milliseconds()
	sinceLastMs := int64(-1)
	if last := d.lastUpstreamFrameAt.Load(); last > 0 {
		sinceLastMs = time.Since(time.Unix(0, last)).Milliseconds()
	}
	if sinceLastMs < 0 {
		return fmt.Sprintf("[upstream frames %d, downstream write blocked %dms]", frames, blockedMs)
	}
	return fmt.Sprintf("[upstream frames %d, downstream write blocked %dms, %dms since the last upstream frame]", frames, blockedMs, sinceLastMs)
}

func (h *Handler) newStreamFlushWriter(c *gin.Context, writer io.Writer, flusher http.Flusher) *streamFlushWriter {
	w := newStreamFlushWriter(writer, flusher)
	if h != nil && h.store != nil {
		cfg := h.promptFilterConfigForRequest(c)
		if cfg.Enabled && cfg.Advanced.Output.Enabled {
			w.outputScanner = promptfilter.NewOutputScannerFromNormalizedConfig(cfg)
		}
	}
	return w
}

// newAttemptStreamFlushWriter keeps a continuously retried attempt free of
// local output-policy side effects until the upstream terminal is known. The
// winning attempt is scanned exactly once by commitStreamAttempt below.
func (h *Handler) newAttemptStreamFlushWriter(c *gin.Context, attempt *continuousRetryStreamAttempt, writer io.Writer, flusher http.Flusher) *streamFlushWriter {
	if attempt != nil {
		return newStreamFlushWriter(attempt.writerOr(writer), attempt.flusherOr(flusher))
	}
	return h.newStreamFlushWriter(c, writer, flusher)
}

type streamFlushWriterAdapter struct {
	writer *streamFlushWriter
}

func (a streamFlushWriterAdapter) Write(data []byte) (int, error) {
	if a.writer == nil {
		return 0, io.ErrClosedPipe
	}
	if err := a.writer.WriteBytes(data); err != nil {
		return 0, err
	}
	return len(data), nil
}

// commitStreamAttempt applies the request's output policy only after the
// attempt has reached a successful protocol terminal. A filter or downstream
// write failure is local and must never trigger another upstream request.
func (h *Handler) commitStreamAttempt(c *gin.Context, attempt *continuousRetryStreamAttempt) error {
	if attempt == nil {
		return nil
	}
	if attempt.closed || attempt.replay == nil {
		return errContinuousRetryReplayClosed
	}
	flusher, _ := c.Writer.(http.Flusher)
	filtered := h.newStreamFlushWriter(c, c.Writer, flusher)
	if err := attempt.replay.CommitTo(streamFlushWriterAdapter{writer: filtered}, nil); err != nil {
		return err
	}
	return filtered.Finalize()
}

// scanOutput feeds the bytes to be output into the output scanner and returns only the part that can be landed immediately.
// It reads and writes outputScanner and scanCarry, so the caller must hold w.mu.
func (w *streamFlushWriter) scanOutput(data []byte) ([]byte, error) {
	if w == nil || w.outputScanner == nil {
		return data, nil
	}
	released, err := w.outputScanner.Push(data)
	if err != nil {
		// When the scanner decides to block, it discards its own safety window; a tail not yet landed also cannot go out.
		w.scanCarry = nil
		return nil, err
	}
	return w.alignToEventBoundary(released), nil
}

// alignToEventBoundary concatenates the bytes released by the scanner with the previously staged tail, and returns only the part up to the last
// event delimiter (blank line); the remaining half event stays staged. If there is no complete event, it returns
// nil, and the caller treats it as if the scanner is still holding back. Holding back a bit more only delays output; it never releases bytes
// the scanner has not yet seen, so the safety window semantics are unchanged.
func (w *streamFlushWriter) alignToEventBoundary(released []byte) []byte {
	if len(released) == 0 && len(w.scanCarry) == 0 {
		return nil
	}
	w.scanCarry = append(w.scanCarry, released...)
	idx := bytes.LastIndex(w.scanCarry, sseDataSuffix)
	if idx < 0 {
		return nil
	}
	cut := idx + len(sseDataSuffix)
	out := append([]byte(nil), w.scanCarry[:cut]...)
	rest := w.scanCarry[cut:]
	if len(rest) == 0 {
		w.scanCarry = w.scanCarry[:0]
	} else {
		w.scanCarry = append(w.scanCarry[:0], rest...)
	}
	return out
}

func newStreamFlushWriter(writer io.Writer, flusher http.Flusher) *streamFlushWriter {
	settings := CurrentRuntimeSettings()
	return &streamFlushWriter{
		writer:   writer,
		flusher:  flusher,
		policy:   settings.StreamFlushPolicy,
		interval: currentStreamFlushInterval(),
	}
}

func appendSSEData(buf *bytes.Buffer, data []byte) {
	if buf == nil {
		return
	}
	buf.Write(sseDataPrefix)
	buf.Write(data)
	buf.Write(sseDataSuffix)
}

func writeDeferredSSEData(streamWriter *streamFlushWriter, pending *bytes.Buffer, data []byte, shouldDefer bool) (bool, error) {
	if streamWriter == nil {
		return false, nil
	}
	if shouldDefer {
		appendSSEData(pending, data)
		if pending != nil && pending.Len() <= pendingFirstTokenFlushBytes {
			return false, nil
		}
	}
	if pending != nil && pending.Len() > 0 {
		if !shouldDefer {
			appendSSEData(pending, data)
		}
		before := streamWriter.deliveredBytes()
		if err := streamWriter.WriteBytes(pending.Bytes()); err != nil {
			return false, err
		}
		pending.Reset()
		return streamWriter.deliveredBytes() > before, nil
	}
	if shouldDefer {
		return false, nil
	}
	before := streamWriter.deliveredBytes()
	if err := streamWriter.WriteSSEData(data); err != nil {
		return false, err
	}
	return streamWriter.deliveredBytes() > before, nil
}

func (w *streamFlushWriter) deliveredBytes() int64 {
	if w == nil {
		return 0
	}
	return w.writtenBytes.Load()
}

func (w *streamFlushWriter) writeUnderlying(data []byte) error {
	if w == nil || w.writer == nil || len(data) == 0 {
		return nil
	}
	blockStart := w.diagnosticsClock()
	written, err := w.writer.Write(data)
	w.diag.addWriteBlocked(blockStart)
	if written > 0 {
		w.writtenBytes.Add(int64(written))
	}
	return err
}

func (w *streamFlushWriter) writeUnderlyingString(data string) error {
	if w == nil || w.writer == nil || data == "" {
		return nil
	}
	blockStart := w.diagnosticsClock()
	written, err := io.WriteString(w.writer, data)
	w.diag.addWriteBlocked(blockStart)
	if written > 0 {
		w.writtenBytes.Add(int64(written))
	}
	return err
}

// diagnosticsClock takes a timestamp only when a diagnostic probe is attached; with none attached it has zero overhead.
func (w *streamFlushWriter) diagnosticsClock() time.Time {
	if w == nil || w.diag == nil {
		return time.Time{}
	}
	return time.Now()
}

// WriteString writes a string into the stream.
// Contract: the methods of one instance such as Flush/WriteBytes/WriteString must not be called concurrently across goroutines.
func (w *streamFlushWriter) WriteString(data string) error {
	if w == nil || w.writer == nil {
		return nil
	}
	if w.aborted.Load() {
		return nil
	}
	w.mu.Lock()
	if w.aborted.Load() {
		// After the entry check, DiscardBuffered has already been set: bytes must never be written back into the buffer, otherwise
		// the buffer after cleanup would be refilled, and "the buffer is fully discarded" would not hold.
		w.mu.Unlock()
		return nil
	}
	filtered, err := w.scanOutput([]byte(data))
	coalesce := w.policy == StreamFlushPolicyCoalesce
	due := false
	if err == nil && len(filtered) > 0 && coalesce {
		_, _ = w.buffer.WriteString(string(filtered))
		due = w.flushDueLocked()
	}
	w.mu.Unlock()
	if err != nil || len(filtered) == 0 {
		return err
	}
	if !coalesce {
		if w.aborted.Load() {
			return nil
		}
		if err := w.writeUnderlyingString(string(filtered)); err != nil {
			return err
		}
		w.flushTransport()
		return nil
	}
	if due {
		return w.Flush()
	}
	return nil
}

// WriteBytes writes a byte slice into the stream.
// Contract: the methods of one instance such as Flush/WriteBytes/WriteString must not be called concurrently across goroutines.
func (w *streamFlushWriter) WriteBytes(data []byte) error {
	if w == nil || w.writer == nil || len(data) == 0 {
		return nil
	}
	if w.aborted.Load() {
		return nil
	}
	w.mu.Lock()
	if w.aborted.Load() {
		w.mu.Unlock()
		return nil
	}
	filtered, err := w.scanOutput(data)
	coalesce := w.policy == StreamFlushPolicyCoalesce
	due := false
	if err == nil && len(filtered) > 0 && coalesce {
		_, _ = w.buffer.Write(filtered)
		due = w.flushDueLocked()
	}
	w.mu.Unlock()
	if err != nil || len(filtered) == 0 {
		return err
	}
	if !coalesce {
		if w.aborted.Load() {
			return nil
		}
		if err := w.writeUnderlying(filtered); err != nil {
			return err
		}
		w.flushTransport()
		return nil
	}
	if due {
		return w.Flush()
	}
	return nil
}

// WriteSSEData writes one SSE data frame into the stream (with the "data: " prefix and "\n\n" suffix).
// Contract: the methods of one instance such as Flush/WriteBytes/WriteSSEData must not be called concurrently across goroutines.
func (w *streamFlushWriter) WriteSSEData(data []byte) error {
	if w == nil || w.writer == nil {
		return nil
	}
	if w.aborted.Load() {
		return nil
	}
	framed := make([]byte, 0, len(sseDataPrefix)+len(data)+len(sseDataSuffix))
	framed = append(framed, sseDataPrefix...)
	framed = append(framed, data...)
	framed = append(framed, sseDataSuffix...)
	w.mu.Lock()
	if w.aborted.Load() {
		w.mu.Unlock()
		return nil
	}
	filtered, err := w.scanOutput(framed)
	coalesce := w.policy == StreamFlushPolicyCoalesce
	due := false
	if err == nil && len(filtered) > 0 && coalesce {
		w.buffer.Write(filtered)
		due = w.flushDueLocked()
	}
	w.mu.Unlock()
	if err != nil || len(filtered) == 0 {
		return err
	}
	if !coalesce {
		if w.aborted.Load() {
			return nil
		}
		if err := w.writeUnderlying(filtered); err != nil {
			return err
		}
		w.flushTransport()
		return nil
	}
	if due {
		return w.Flush()
	}
	return nil
}

// WriteSSEComment writes an SSE comment (such as ": keepalive\n\n") and flushes to the transport at once.
// Comments are not model output and do not go through the scanner: first drain the merged buffer (it holds only complete events), then write directly to the underlying writer.
// When output filtering is on, scanOutput has already aligned the underlying writes to event boundaries (see scanCarry),
// so the underlying stream never stops in the middle of an event and a direct write is safe; if it went through the scanner, the comment would be held back along with the safety window
// until the next terminal frame, and keepalive would be completely ineffective when upstream is silent for a long time.
func (w *streamFlushWriter) WriteSSEComment(comment string) error {
	if w == nil || w.writer == nil || comment == "" {
		return nil
	}
	if w.aborted.Load() {
		return nil
	}
	w.mu.Lock()
	buffered := w.takeBufferedLocked()
	w.mu.Unlock()
	if w.aborted.Load() {
		return nil
	}
	if err := w.writeUnderlying(buffered); err != nil {
		return err
	}
	if w.aborted.Load() {
		return nil
	}
	if err := w.writeUnderlyingString(comment); err != nil {
		return err
	}
	w.flushTransport()
	return nil
}

// DiscardBuffered fully discards the local buffer: it clears the merged buffer, discards the scanner's staged tail and detaches the output
// scanner, while setting aborted. After the guard trips, calling it once guarantees that later Flush/Finalize
// no longer output any data.
//
// Concurrency contract: aborted is set first with CAS, so that under multiple triggers (guard decision, keepalive / forward
// race) only the first call performs cleanup; a CAS failure means a discard has already completed, so return directly.
// The cleanup takes only the internal state lock and does not touch the downstream write lock, so a slow client cannot hold it up.
func (w *streamFlushWriter) DiscardBuffered() {
	if w == nil {
		return
	}
	if !w.aborted.CompareAndSwap(false, true) {
		return
	}
	w.mu.Lock()
	w.resetBuffer()
	w.scanCarry = nil
	w.outputScanner = nil
	w.mu.Unlock()
}

// Flush writes out the current merged buffer and the safety window already released by the output scanner.
// Contract: the methods of one instance such as Flush/WriteBytes must not be called concurrently across goroutines.
func (w *streamFlushWriter) Flush() error {
	if w == nil {
		return nil
	}
	if w.aborted.Load() {
		return nil
	}
	// Under the lock, only snapshot and clean up the buffer / safety window; the downstream write is done outside the lock.
	w.mu.Lock()
	buffered := w.takeBufferedLocked()
	var released []byte
	var scanErr error
	if w.outputScanner != nil {
		released, scanErr = w.outputScanner.Flush()
	}
	w.mu.Unlock()
	if w.aborted.Load() {
		// A buffer snapshot already judged runaway is discarded in place and is not released downstream.
		return nil
	}
	if err := w.writeUnderlying(buffered); err != nil {
		return err
	}
	if scanErr != nil {
		return scanErr
	}
	if w.aborted.Load() {
		return nil
	}
	if err := w.writeUnderlying(released); err != nil {
		return err
	}
	w.flushTransport()
	return nil
}

// Finalize releases the retained safety window at a real semantic end-of-stream.
// A transport Flush must not call this because an unsafe phrase may span chunks.
func (w *streamFlushWriter) Finalize() error {
	if w == nil || w.aborted.Load() {
		return nil
	}
	// Phase 1: only take the merged buffer snapshot; write the downstream outside the lock.
	w.mu.Lock()
	buffered := w.takeBufferedLocked()
	w.mu.Unlock()
	if w.aborted.Load() {
		return nil
	}
	if err := w.writeUnderlying(buffered); err != nil {
		return err
	}
	// Phase 2: release the scanner safety window and the staged half event. Still do not write downstream while holding the lock.
	w.mu.Lock()
	if w.aborted.Load() {
		w.mu.Unlock()
		return nil
	}
	var released, carry []byte
	var scanErr error
	if w.outputScanner != nil {
		released, scanErr = w.outputScanner.Finalize()
		if scanErr != nil {
			// When the scanner decides to block, discard the staged half event together with it.
			w.scanCarry = nil
		} else {
			carry = w.scanCarry
			w.scanCarry = nil
		}
	}
	w.mu.Unlock()
	if scanErr != nil {
		return scanErr
	}
	if w.aborted.Load() {
		return nil
	}
	// When the stream truly ends: first complete the staged half event, then write the safety window the scanner releases.
	if err := w.writeUnderlying(carry); err != nil {
		return err
	}
	if err := w.writeUnderlying(released); err != nil {
		return err
	}
	w.flushTransport()
	return nil
}

func (w *streamFlushWriter) flushTransport() {
	if w == nil || w.flusher == nil {
		return
	}
	w.flusher.Flush()
	w.mu.Lock()
	w.lastFlush = time.Now()
	w.mu.Unlock()
}
