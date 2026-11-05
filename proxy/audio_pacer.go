package proxy

import (
	"context"
	"errors"
	"sync"
	"time"
)

// audioFrameSink receives one Opus frame per pacer tick together with the duration it represents.
type audioFrameSink func(frame []byte, duration time.Duration) error

const (
	// audioFrameInterval is the real-time length of one Opus frame.
	audioFrameInterval = 20 * time.Millisecond
	// audioPacerMaxFrames caps the queue at 60 seconds of audio.
	audioPacerMaxFrames = 3000
)

var (
	errAudioPacerFull    = errors.New("audio pacer queue is full")
	errAudioPacerStopped = errors.New("audio pacer is stopped")
)

// audioPacer writes one Opus frame per tick to a sink, using silence when nothing is queued.
type audioPacer struct {
	sink        audioFrameSink
	interval    time.Duration
	maxBuffered int

	mu      sync.Mutex
	queue   [][]byte
	drained chan struct{} // closed when the queue becomes empty; nil when nothing waits
	started bool
	stopped bool
	err     error

	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
}

func newAudioPacer(sink audioFrameSink, interval time.Duration, maxBuffered int) *audioPacer {
	if interval <= 0 {
		interval = audioFrameInterval
	}
	if maxBuffered <= 0 {
		maxBuffered = audioPacerMaxFrames
	}
	return &audioPacer{
		sink:        sink,
		interval:    interval,
		maxBuffered: maxBuffered,
		stopCh:      make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// Start launches the pacing goroutine. A second call, or a call after Stop, does nothing.
func (p *audioPacer) Start(ctx context.Context) {
	p.mu.Lock()
	if p.started || p.stopped {
		p.mu.Unlock()
		return
	}
	p.started = true
	p.mu.Unlock()
	go p.loop(ctx)
}

func (p *audioPacer) loop(ctx context.Context) {
	defer close(p.done)
	defer p.finish()
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-ticker.C:
		}
		// A tick and a stop can be ready together; select picks at random, so check again before writing.
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		default:
		}
		if err := p.sink(p.next(), audioFrameInterval); err != nil {
			p.mu.Lock()
			p.err = err
			p.mu.Unlock()
			return
		}
	}
}

// next pops the next queued frame, or returns silence when the queue is empty.
func (p *audioPacer) next() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return opusSilenceFrame
	}
	frame := p.queue[0]
	p.queue[0] = nil
	p.queue = p.queue[1:]
	if len(p.queue) == 0 {
		p.queue = nil
		p.releaseDrainLocked()
	}
	return frame
}

// finish marks the pacer as stopped when the loop exits and wakes drain waiters.
func (p *audioPacer) finish() {
	p.mu.Lock()
	p.stopped = true
	p.releaseDrainLocked()
	p.mu.Unlock()
}

func (p *audioPacer) releaseDrainLocked() {
	if p.drained != nil {
		close(p.drained)
		p.drained = nil
	}
}

// Enqueue copies and queues frames. It queues nothing and returns errAudioPacerFull when the cap would be exceeded.
func (p *audioPacer) Enqueue(frames [][]byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		if p.err != nil {
			return p.err
		}
		return errAudioPacerStopped
	}
	if len(frames) == 0 {
		return nil
	}
	if len(p.queue)+len(frames) > p.maxBuffered {
		return errAudioPacerFull
	}
	for _, f := range frames {
		p.queue = append(p.queue, append([]byte(nil), f...))
	}
	return nil
}

// Clear drops all queued frames and wakes drain waiters.
func (p *audioPacer) Clear() {
	p.mu.Lock()
	p.queue = nil
	p.releaseDrainLocked()
	p.mu.Unlock()
}

// Buffered returns the number of queued frames.
func (p *audioPacer) Buffered() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

// Err returns the sink error that stopped the loop, if any.
func (p *audioPacer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// WaitDrained blocks until the queue is empty, the pacer stops, or ctx ends.
// It wakes when the loop pops the last frame, which is just before that frame's sink write returns, so the final frame may still be in flight.
// Frames enqueued after the wake-up but before the waiter runs again keep it waiting until they drain too.
func (p *audioPacer) WaitDrained(ctx context.Context) error {
	for {
		p.mu.Lock()
		if len(p.queue) == 0 {
			p.mu.Unlock()
			return nil
		}
		if p.err != nil {
			err := p.err
			p.mu.Unlock()
			return err
		}
		if p.stopped {
			p.mu.Unlock()
			return errAudioPacerStopped
		}
		if p.drained == nil {
			p.drained = make(chan struct{})
		}
		ch := p.drained
		p.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Stop ends the loop and waits for the goroutine to exit. It is idempotent.
func (p *audioPacer) Stop() {
	p.mu.Lock()
	started := p.started
	p.stopped = true
	p.releaseDrainLocked()
	p.mu.Unlock()
	p.stopOnce.Do(func() { close(p.stopCh) })
	if started {
		<-p.done
	}
}
