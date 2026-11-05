package admin

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestImportProgressPusherStopsOnWriteFailure locks in this behaviour: when the downstream disconnects (the write returns broken pipe)
// the push goroutine stops at once. Before the fix it kept writing to the dead connection at every interval, emitting one line each time
// "failed to write SSE event: broken pipe", until the whole import finishes.
func TestImportProgressPusherStopsOnWriteFailure(t *testing.T) {
	var sends int64
	done := make(chan struct{})
	defer close(done)

	stopped := runImportProgressPusher(
		context.Background(), done, time.Millisecond,
		func() importEvent { return importEvent{Type: "progress"} },
		func(importEvent) bool {
			atomic.AddInt64(&sends, 1)
			return false // downstream has disconnected
		},
	)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("push goroutine did not exit after the write failed")
	}

	// Wait a few more intervals to confirm that it stopped writing.
	got := atomic.LoadInt64(&sends)
	time.Sleep(50 * time.Millisecond)
	if now := atomic.LoadInt64(&sends); now != got {
		t.Fatalf("still writing after exit: %d -> %d", got, now)
	}
	if got != 1 {
		t.Fatalf("wrote %d more times after the write failed, expected to stop after 1", got)
	}
}

// TestImportProgressPusherStopsOnClientDisconnect checks that when the client disconnects (the request ctx is cancelled)
// it also stops, without waiting for a write failure.
func TestImportProgressPusherStopsOnClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer close(done)

	stopped := runImportProgressPusher(
		ctx, done, time.Hour, // the interval is deliberately large: only ctx cancellation can make it exit
		func() importEvent { return importEvent{Type: "progress"} },
		func(importEvent) bool { return true },
	)
	cancel()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("push goroutine did not exit after the client disconnected")
	}
}

// TestImportProgressPusherStopSignalGatesFinalWrite checks that after the import ends, the caller can wait until the push
// goroutine has really exited before writing the complete event; writing to the same ResponseWriter from both sides at once would interleave the events,
// so the frontend would not receive a complete event and the progress bar would stick at its last percentage.
func TestImportProgressPusherStopSignalGatesFinalWrite(t *testing.T) {
	var writing int64
	var overlapped int64
	done := make(chan struct{})

	stopped := runImportProgressPusher(
		context.Background(), done, time.Millisecond,
		func() importEvent { return importEvent{Type: "progress"} },
		func(importEvent) bool {
			if atomic.AddInt64(&writing, 1) != 1 {
				atomic.AddInt64(&overlapped, 1)
			}
			time.Sleep(2 * time.Millisecond) // simulate one slow write
			atomic.AddInt64(&writing, -1)
			return true
		},
	)

	time.Sleep(20 * time.Millisecond)
	close(done)
	<-stopped

	// Final write: the push goroutine has already exited, so the writes cannot overlap.
	if atomic.AddInt64(&writing, 1) != 1 {
		t.Fatal("final event and progress events wrote to the same ResponseWriter concurrently")
	}
	atomic.AddInt64(&writing, -1)
	if overlapped != 0 {
		t.Fatalf("the push goroutine had %d overlapping writes internally", overlapped)
	}
}
