package auth

import (
	"sync"
	"time"
)

// Timer is an injectable timer abstraction whose semantics match the commonly used subset of time.Timer.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Clock is an injectable clock abstraction: the production path uses RealClock, the test path uses FakeClock.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
	Since(t time.Time) time.Duration
	Until(t time.Time) time.Duration
	NewTimer(d time.Duration) Timer
}

// RealClock is the real clock implementation based on the time package.
type RealClock struct{}

// Now returns the current local time of the real clock.
func (RealClock) Now() time.Time { return time.Now() }

// Sleep suspends the current goroutine for the given duration.
func (RealClock) Sleep(d time.Duration) { time.Sleep(d) }

// Since returns the time elapsed since the given moment.
func (RealClock) Since(t time.Time) time.Duration { return time.Since(t) }

// Until returns the time remaining from now until the target time.
func (RealClock) Until(t time.Time) time.Duration { return time.Until(t) }

// NewTimer creates and returns a Timer adapter instance based on the real clock.
func (RealClock) NewTimer(d time.Duration) Timer { return &realTimer{Timer: time.NewTimer(d)} }

type realTimer struct {
	*time.Timer
}

// C returns the expiry channel of the real timer.
func (r *realTimer) C() <-chan time.Time {
	return r.Timer.C
}

// DefaultClock is the global default clock; tests can replace it with FakeClock.
var DefaultClock Clock = RealClock{}

// fakeTimer is the timer implementation inside FakeClock, protected by FakeClock.mu.
type fakeTimer struct {
	clk      *FakeClock
	ch       chan time.Time
	deadline time.Time
	active   bool
}

// C returns the fire-event channel of the fake timer.
func (ft *fakeTimer) C() <-chan time.Time {
	return ft.ch
}

// Stop stops the fake timer and reports whether it was active before.
func (ft *fakeTimer) Stop() bool {
	ft.clk.mu.Lock()
	defer ft.clk.mu.Unlock()
	wasActive := ft.active
	ft.active = false
	return wasActive
}

// Reset resets the fake timer's expiry time and reactivates it.
func (ft *fakeTimer) Reset(d time.Duration) bool {
	ft.clk.mu.Lock()
	defer ft.clk.mu.Unlock()
	wasActive := ft.active
	ft.deadline = ft.clk.now.Add(d)
	ft.active = true
	return wasActive
}

// FakeClock is a deterministic test clock: time only advances explicitly through Advance.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

// NewFakeClock creates a fake clock starting at initial.
func NewFakeClock(initial time.Time) *FakeClock {
	return &FakeClock{now: initial}
}

// Now returns the current simulated time recorded by the fake clock.
func (fc *FakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

// Sleep steps the fake clock forward by the given time through Advance.
func (fc *FakeClock) Sleep(d time.Duration) {
	fc.Advance(d)
}

// Since computes the duration between the target time and the fake clock's current time.
func (fc *FakeClock) Since(t time.Time) time.Duration {
	return fc.Now().Sub(t)
}

// Until computes the distance from the fake clock's current time to the target time.
func (fc *FakeClock) Until(t time.Time) time.Duration {
	return t.Sub(fc.Now())
}

// NewTimer registers a new simulated timer on the fake clock.
func (fc *FakeClock) NewTimer(d time.Duration) Timer {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	ft := &fakeTimer{
		clk:      fc,
		ch:       make(chan time.Time, 1),
		deadline: fc.now.Add(d),
		active:   true,
	}
	fc.timers = append(fc.timers, ft)
	return ft
}

// Advance moves the fake clock forward by d and wakes all active timers whose deadline has been reached.
func (fc *FakeClock) Advance(d time.Duration) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.now = fc.now.Add(d)
	for _, ft := range fc.timers {
		if ft.active && !fc.now.Before(ft.deadline) {
			ft.active = false
			select {
			case ft.ch <- ft.deadline:
			default:
			}
		}
	}
}
