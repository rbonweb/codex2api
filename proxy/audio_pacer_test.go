package proxy

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

type pacerOut struct {
	frame []byte
	d     time.Duration
}

func newTestPacer(t *testing.T, maxBuffered int, sinkErr func(n int) error) (*audioPacer, chan pacerOut) {
	t.Helper()
	out := make(chan pacerOut, 4096)
	n := 0
	p := newAudioPacer(func(frame []byte, d time.Duration) error {
		n++
		if sinkErr != nil {
			if err := sinkErr(n); err != nil {
				return err
			}
		}
		out <- pacerOut{append([]byte(nil), frame...), d}
		return nil
	}, time.Millisecond, maxBuffered)
	t.Cleanup(p.Stop)
	return p, out
}

func recvFrame(t *testing.T, out chan pacerOut) pacerOut {
	t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a frame")
		return pacerOut{}
	}
}

func TestAudioPacerDefaults(t *testing.T) {
	p := newAudioPacer(nil, 0, -1)
	if p.interval != audioFrameInterval || p.maxBuffered != audioPacerMaxFrames {
		t.Fatalf("defaults not applied: %v %d", p.interval, p.maxBuffered)
	}
}

func TestAudioPacerOrderThenSilence(t *testing.T) {
	p, out := newTestPacer(t, 0, nil)
	frames := [][]byte{{1}, {2}, {3}}
	if err := p.Enqueue(frames); err != nil {
		t.Fatal(err)
	}
	frames[0][0] = 9 // the pacer must have copied
	p.Start(context.Background())
	for i, want := range []byte{1, 2, 3} {
		o := recvFrame(t, out)
		if len(o.frame) != 1 || o.frame[0] != want {
			t.Fatalf("frame %d = %v, want %d", i, o.frame, want)
		}
		if o.d != audioFrameInterval {
			t.Fatalf("duration = %v", o.d)
		}
	}
	if o := recvFrame(t, out); !bytes.Equal(o.frame, audioPacerSilence) {
		t.Fatalf("expected silence, got %v", o.frame)
	}
}

func TestAudioPacerClearDropsPending(t *testing.T) {
	p, out := newTestPacer(t, 0, nil)
	if err := p.Enqueue([][]byte{{1}, {2}}); err != nil {
		t.Fatal(err)
	}
	p.Clear()
	if p.Buffered() != 0 {
		t.Fatalf("buffered = %d", p.Buffered())
	}
	p.Start(context.Background())
	if o := recvFrame(t, out); !bytes.Equal(o.frame, audioPacerSilence) {
		t.Fatalf("expected silence after Clear, got %v", o.frame)
	}
}

func TestAudioPacerCap(t *testing.T) {
	p, _ := newTestPacer(t, 2, nil)
	if err := p.Enqueue([][]byte{{1}, {2}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Enqueue([][]byte{{3}}); !errors.Is(err, errAudioPacerFull) {
		t.Fatalf("err = %v", err)
	}
	if p.Buffered() != 2 {
		t.Fatalf("buffered = %d", p.Buffered())
	}
	p.Clear()
	if err := p.Enqueue([][]byte{{1}, {2}, {3}}); !errors.Is(err, errAudioPacerFull) {
		t.Fatalf("oversized batch err = %v", err)
	}
	if p.Buffered() != 0 {
		t.Fatalf("a rejected batch must queue nothing, buffered = %d", p.Buffered())
	}
}

func TestAudioPacerWaitDrained(t *testing.T) {
	p, out := newTestPacer(t, 0, nil)
	if err := p.WaitDrained(context.Background()); err != nil {
		t.Fatalf("empty queue: %v", err)
	}
	if err := p.Enqueue([][]byte{{1}, {2}, {3}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.WaitDrained(context.Background()) }()
	p.Start(context.Background())
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitDrained: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitDrained did not return")
	}
	if p.Buffered() != 0 {
		t.Fatalf("buffered = %d", p.Buffered())
	}
	_ = out
}

func TestAudioPacerWaitDrainedCancelled(t *testing.T) {
	p, _ := newTestPacer(t, 0, nil) // never started, so the queue never drains
	if err := p.Enqueue([][]byte{{1}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.WaitDrained(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestAudioPacerWaitDrainedReturnsOnStop(t *testing.T) {
	p, _ := newTestPacer(t, 0, nil)
	if err := p.Enqueue([][]byte{{1}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.WaitDrained(context.Background()) }()
	p.Stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when the pacer stops with frames queued")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitDrained did not return after Stop")
	}
}

func TestAudioPacerSinkErrorStopsLoop(t *testing.T) {
	boom := errors.New("boom")
	p, out := newTestPacer(t, 0, func(n int) error {
		if n == 3 {
			return boom
		}
		return nil
	})
	p.Start(context.Background())
	recvFrame(t, out)
	recvFrame(t, out)
	<-p.done
	if !errors.Is(p.Err(), boom) {
		t.Fatalf("Err = %v", p.Err())
	}
	if err := p.Enqueue([][]byte{{1}}); !errors.Is(err, boom) {
		t.Fatalf("Enqueue after sink error = %v", err)
	}
	p.Stop()
}

func TestAudioPacerContextCancelStopsLoop(t *testing.T) {
	p, _ := newTestPacer(t, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	cancel()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop on context cancel")
	}
	if err := p.Enqueue([][]byte{{1}}); err == nil {
		t.Fatal("Enqueue after stop must fail")
	}
}

func TestAudioPacerStopIdempotentAndStartAfterStop(t *testing.T) {
	p, out := newTestPacer(t, 0, nil)
	p.Start(context.Background())
	p.Start(context.Background()) // second Start is a no-op
	recvFrame(t, out)
	p.Stop()
	p.Stop()
	if err := p.Enqueue([][]byte{{1}}); err == nil {
		t.Fatal("Enqueue after Stop must fail")
	}
	p.Start(context.Background()) // must not panic or restart
	p.Stop()

	never, _ := newTestPacer(t, 0, nil)
	never.Stop() // Stop without Start must not block
	never.Stop()
}
