package proxy

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/security/promptfilter"
)

const discardRaceEvent = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"runaway repetition\"}\n\n"

// concurrentSafeBuffer is a sink dedicated to the concurrency regression. streamFlushWriter runs downstream writes on
// outside its internal state lock (a slow client must not block DiscardBuffered), so the test fixture itself
// must be thread-safe, so a data race in the fixture is not mistaken for a production code problem.
type concurrentSafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *concurrentSafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *concurrentSafeBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *concurrentSafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The guard's discard callback runs outside the lock inside HandleTripped's once, and it shares the same streamFlushWriter
// concurrently with the keepalive/forward path that holds downstreamMu. The regression requires: no panic (no longer concurrently
// read or write bytes.Buffer/the scanner), the discard state takes effect only once, and after discard completes no entry point
// writes bytes to the sink anymore.
func TestStreamFlushWriterDiscardBufferedConcurrent(t *testing.T) {
	sink := &concurrentSafeBuffer{}
	w := &streamFlushWriter{writer: sink, policy: StreamFlushPolicyCoalesce, interval: time.Hour, lastFlush: time.Now()}
	event := []byte(discardRaceEvent)
	// The buffer first holds leftover bytes already judged runaway: discard must drop them completely.
	if err := w.WriteBytes(event); err != nil {
		t.Fatalf("prefill WriteBytes: %v", err)
	}
	if w.buffer.Len() == 0 {
		t.Fatalf("expected prefill to stay buffered")
	}

	const (
		writers    = 50
		iterations = 200
	)
	start := make(chan struct{})
	midway := make(chan struct{})
	var midwayOnce sync.Once
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				_ = w.Flush()
				switch {
				case j%3 == 0:
					_ = w.WriteBytes(event)
				case j%7 == 0:
					_ = w.WriteSSEData(event)
				case j%11 == 0:
					_ = w.WriteString(discardRaceEvent)
				}
				if j%13 == 0 {
					_ = w.Finalize()
				}
				if j == 10 {
					// Ensures discard happens while the write goroutine is still running.
					midwayOnce.Do(func() { close(midway) })
				}
			}
		}()
	}
	discarded := make(chan struct{})
	go func() {
		defer close(discarded)
		<-start
		<-midway
		w.DiscardBuffered()
	}()
	close(start)
	wg.Wait()
	<-discarded

	if !w.aborted.Load() {
		t.Fatalf("expected aborted after DiscardBuffered")
	}
	if w.buffer.Len() != 0 || w.scanCarry != nil || w.outputScanner != nil {
		t.Fatalf("expected buffer/scanCarry/outputScanner cleared")
	}

	// After discard, every write entry point must short-circuit: it must not write any bytes to the sink.
	before := sink.Len()
	for i := 0; i < 100; i++ {
		if err := w.Flush(); err != nil {
			t.Fatalf("Flush after discard: %v", err)
		}
		if err := w.Finalize(); err != nil {
			t.Fatalf("Finalize after discard: %v", err)
		}
		if err := w.WriteBytes(event); err != nil {
			t.Fatalf("WriteBytes after discard: %v", err)
		}
		if err := w.WriteString(discardRaceEvent); err != nil {
			t.Fatalf("WriteString after discard: %v", err)
		}
		if err := w.WriteSSEData(event); err != nil {
			t.Fatalf("WriteSSEData after discard: %v", err)
		}
		if err := w.WriteSSEComment(": keepalive\n\n"); err != nil {
			t.Fatalf("WriteSSEComment after discard: %v", err)
		}
	}
	if got := sink.Len(); got != before {
		t.Fatalf("bytes still written downstream after discard: before=%d after=%d", before, got)
	}
	if w.buffer.Len() != 0 {
		t.Fatalf("buffer was written again after discard: %q", w.buffer.String())
	}

	// Repeated or concurrent calls to DiscardBuffered must be idempotent: no panic, the state does not roll back, and no new bytes.
	var repeat sync.WaitGroup
	for i := 0; i < writers; i++ {
		repeat.Add(1)
		go func() {
			defer repeat.Done()
			w.DiscardBuffered()
		}()
	}
	repeat.Wait()
	if !w.aborted.Load() || w.buffer.Len() != 0 || w.scanCarry != nil || w.outputScanner != nil {
		t.Fatalf("repeated discard must stay idempotent")
	}
	if got := sink.Len(); got != before {
		t.Fatalf("repeated discard wrote bytes: before=%d after=%d", before, got)
	}
}

// blockingWriteSink holds the first downstream write inside Write, to verify that DiscardBuffered is not
// held up by a slow or blocked downstream write (the constraint that guard termination orchestration is not delayed by the downstream write lock).
type blockingWriteSink struct {
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
	mu        sync.Mutex
	buf       bytes.Buffer
}

func newBlockingWriteSink() *blockingWriteSink {
	return &blockingWriteSink{entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *blockingWriteSink) Write(p []byte) (int, error) {
	s.enterOnce.Do(func() { close(s.entered) })
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *blockingWriteSink) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

func TestStreamFlushWriterDiscardBufferedNotBlockedBySlowDownstream(t *testing.T) {
	sink := newBlockingWriteSink()
	w := &streamFlushWriter{writer: sink, policy: StreamFlushPolicyCoalesce, interval: time.Hour, lastFlush: time.Now()}
	event := []byte(discardRaceEvent)
	if err := w.WriteBytes(event); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	if w.buffer.Len() == 0 {
		t.Fatalf("expected prefill to stay buffered")
	}

	flushDone := make(chan error, 1)
	go func() { flushDone <- w.Flush() }()
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("Flush did not enter the downstream write")
	}

	// At this point the downstream write is still blocked inside the sink; discard must return immediately instead of waiting for downstream I/O.
	discardDone := make(chan struct{})
	go func() {
		defer close(discardDone)
		w.DiscardBuffered()
	}()
	select {
	case <-discardDone:
	case <-time.After(5 * time.Second):
		close(sink.release)
		t.Fatalf("DiscardBuffered was blocked by a slow downstream write")
	}
	if !w.aborted.Load() || w.buffer.Len() != 0 || w.scanCarry != nil || w.outputScanner != nil {
		t.Fatalf("discard did not finish cleanup: aborted=%t buffer=%d carry=%v scanner=%v", w.aborted.Load(), w.buffer.Len(), w.scanCarry != nil, w.outputScanner != nil)
	}

	close(sink.release)
	if err := <-flushDone; err != nil {
		t.Fatalf("Flush returned error: %v", err)
	}

	// A write that has already entered Write cannot be taken back; but new entry points after discard must never write again.
	before := sink.Len()
	for i := 0; i < 20; i++ {
		if err := w.Flush(); err != nil {
			t.Fatalf("Flush after discard: %v", err)
		}
		if err := w.Finalize(); err != nil {
			t.Fatalf("Finalize after discard: %v", err)
		}
		if err := w.WriteBytes(event); err != nil {
			t.Fatalf("WriteBytes after discard: %v", err)
		}
		if err := w.WriteSSEData(event); err != nil {
			t.Fatalf("WriteSSEData after discard: %v", err)
		}
		if err := w.WriteString(discardRaceEvent); err != nil {
			t.Fatalf("WriteString after discard: %v", err)
		}
	}
	if got := sink.Len(); got != before {
		t.Fatalf("bytes still written downstream after discard: before=%d after=%d", before, got)
	}
}

// TestStreamFlushWriterDiscardBufferedConcurrentWithScanner is the concurrency regression variant for when an outputScanner is attached.
// DiscardBuffered interleaves concurrently with Flush/WriteSSEData/WriteBytes/Finalize,
// and verifies no panic under -race, that outputScanner cleanup is safe, and zero new bytes after discard.
func TestStreamFlushWriterDiscardBufferedConcurrentWithScanner(t *testing.T) {
	sink := &concurrentSafeBuffer{}
	cfg := promptfilter.DefaultConfig()
	cfg.Enabled = true
	cfg.Advanced.Output.Enabled = true
	cfg.Advanced.Output.BufferBytes = 256
	cfg.Advanced.Output.OverlapBytes = 64
	scanner := promptfilter.NewOutputScanner(cfg)
	if scanner == nil {
		t.Fatal("expected non-nil output scanner")
	}

	w := &streamFlushWriter{
		writer:        sink,
		policy:        StreamFlushPolicyCoalesce,
		interval:      time.Hour,
		lastFlush:     time.Now(),
		outputScanner: scanner,
	}
	event := []byte(discardRaceEvent)
	if err := w.WriteBytes(event); err != nil {
		t.Fatalf("prefill WriteBytes: %v", err)
	}

	const (
		writers    = 50
		iterations = 200
	)
	start := make(chan struct{})
	midway := make(chan struct{})
	var midwayOnce sync.Once
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				_ = w.Flush()
				switch {
				case j%2 == 0:
					_ = w.WriteSSEData(event)
				case j%3 == 0:
					_ = w.WriteBytes(event)
				case j%5 == 0:
					_ = w.WriteString(discardRaceEvent)
				}
				if j%7 == 0 {
					_ = w.Finalize()
				}
				if j == 10 {
					midwayOnce.Do(func() { close(midway) })
				}
			}
		}()
	}
	discarded := make(chan struct{})
	go func() {
		defer close(discarded)
		<-start
		<-midway
		w.DiscardBuffered()
	}()
	close(start)
	wg.Wait()
	<-discarded

	if !w.aborted.Load() {
		t.Fatalf("expected aborted after DiscardBuffered")
	}
	if w.buffer.Len() != 0 || w.scanCarry != nil || w.outputScanner != nil {
		t.Fatalf("expected buffer/scanCarry/outputScanner cleared")
	}

	// After discard, every write entry point must short-circuit: it must not write any bytes to the sink.
	before := sink.Len()
	for i := 0; i < 100; i++ {
		if err := w.Flush(); err != nil {
			t.Fatalf("Flush after discard: %v", err)
		}
		if err := w.Finalize(); err != nil {
			t.Fatalf("Finalize after discard: %v", err)
		}
		if err := w.WriteBytes(event); err != nil {
			t.Fatalf("WriteBytes after discard: %v", err)
		}
		if err := w.WriteString(discardRaceEvent); err != nil {
			t.Fatalf("WriteString after discard: %v", err)
		}
		if err := w.WriteSSEData(event); err != nil {
			t.Fatalf("WriteSSEData after discard: %v", err)
		}
		if err := w.WriteSSEComment(": keepalive\n\n"); err != nil {
			t.Fatalf("WriteSSEComment after discard: %v", err)
		}
	}
	if got := sink.Len(); got != before {
		t.Fatalf("bytes still written downstream after discard: before=%d after=%d", before, got)
	}
	if w.buffer.Len() != 0 {
		t.Fatalf("buffer was written again after discard: %q", w.buffer.String())
	}
}
