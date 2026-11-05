package proxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDrainableUpstreamContextSurvivesAfterClientCancel(t *testing.T) {
	clientCtx, cancelClient := context.WithCancel(context.Background())

	upstreamCtx, cancelUpstream := newDrainableUpstreamContext(clientCtx, 50*time.Millisecond)
	defer cancelUpstream()

	cancelClient()

	// Upstream should still be alive at the moment the client cancels, leaving time for the subsequent SSE drain.
	select {
	case <-upstreamCtx.Done():
		t.Fatal("upstream ctx canceled too early after client cancel")
	case <-time.After(10 * time.Millisecond):
	}

	// Once the drain timeout is reached, upstream should be cancelled.
	select {
	case <-upstreamCtx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("upstream ctx not canceled after drain timeout")
	}

	if !errors.Is(upstreamCtx.Err(), context.Canceled) {
		t.Fatalf("upstream ctx err = %v, want context.Canceled", upstreamCtx.Err())
	}
}

func TestDrainableUpstreamContextManualCancelStopsTimer(t *testing.T) {
	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()

	upstreamCtx, cancelUpstream := newDrainableUpstreamContext(clientCtx, time.Hour)

	cancelUpstream()
	select {
	case <-upstreamCtx.Done():
	case <-time.After(50 * time.Millisecond):
		t.Fatal("manual cancel did not propagate")
	}
}

func TestDrainableUpstreamContextStaysAliveWhileClientAlive(t *testing.T) {
	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()

	upstreamCtx, cancelUpstream := newDrainableUpstreamContext(clientCtx, 10*time.Millisecond)
	defer cancelUpstream()

	select {
	case <-upstreamCtx.Done():
		t.Fatal("upstream ctx canceled while client still alive")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestDrainableUpstreamContextPreservesRequestValues(t *testing.T) {
	type contextKey struct{}
	clientCtx := context.WithValue(context.Background(), contextKey{}, "audit-value")

	upstreamCtx, cancelUpstream := newDrainableUpstreamContext(clientCtx, time.Second)
	defer cancelUpstream()

	if got := upstreamCtx.Value(contextKey{}); got != "audit-value" {
		t.Fatalf("upstream context value = %v, want audit-value", got)
	}
}

func TestDrainableUpstreamContextCancelsImmediatelyForContinuousRetryDeadline(t *testing.T) {
	clientCtx, cancelClient := context.WithCancelCause(context.Background())
	upstreamCtx, cancelUpstream := newDrainableUpstreamContext(clientCtx, time.Hour)
	defer cancelUpstream()

	cancelClient(errContinuousRetryDeadlineExceeded)
	select {
	case <-upstreamCtx.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("continuous retry deadline inherited the client drain delay")
	}
}
