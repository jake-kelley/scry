package power

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fixedClock lets a test move OnWake's notion of "now" forward without
// sleeping.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

func TestOnWakeResyncSucceedsNeverEscalates(t *testing.T) {
	var resyncCalls, fallbackCalls int
	c := &Coordinator{
		Resync:         func() error { resyncCalls++; return nil },
		Fallback:       func() { fallbackCalls++ },
		RecrawlEnabled: func() bool { return true },
	}
	c.OnWake()

	if resyncCalls != 1 {
		t.Errorf("resyncCalls = %d, want 1", resyncCalls)
	}
	if fallbackCalls != 0 {
		t.Errorf("fallbackCalls = %d, want 0 — a successful resync must never escalate", fallbackCalls)
	}
}

func TestOnWakeResyncFailsRecrawlEnabledEscalates(t *testing.T) {
	var fallbackCalls int
	c := &Coordinator{
		Resync:         func() error { return errors.New("stream restart failed") },
		Fallback:       func() { fallbackCalls++ },
		RecrawlEnabled: func() bool { return true },
	}
	c.OnWake()

	if fallbackCalls != 1 {
		t.Errorf("fallbackCalls = %d, want 1 — a failed resync with recrawl enabled must escalate", fallbackCalls)
	}
}

func TestOnWakeResyncFailsRecrawlOffNeverEscalates(t *testing.T) {
	var fallbackCalls int
	c := &Coordinator{
		Resync:         func() error { return errors.New("stream restart failed") },
		Fallback:       func() { fallbackCalls++ },
		RecrawlEnabled: func() bool { return false },
	}
	c.OnWake()

	if fallbackCalls != 0 {
		t.Errorf("fallbackCalls = %d, want 0 — recrawl_interval = off must never be overridden by a wake", fallbackCalls)
	}
}

func TestOnWakeNilRecrawlEnabledEscalates(t *testing.T) {
	// A Coordinator built without RecrawlEnabled set (should not happen
	// from cmd/scry, but guards the zero value) must not silently refuse
	// to escalate — nil means "unknown", not "off".
	var fallbackCalls int
	c := &Coordinator{
		Resync:   func() error { return errors.New("boom") },
		Fallback: func() { fallbackCalls++ },
	}
	c.OnWake()

	if fallbackCalls != 1 {
		t.Errorf("fallbackCalls = %d, want 1", fallbackCalls)
	}
}

func TestOnWakeDebounceSuppressesSecondWakeWithinWindow(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1000, 0)}
	var resyncCalls int
	c := &Coordinator{
		Resync:         func() error { resyncCalls++; return nil },
		RecrawlEnabled: func() bool { return true },
		Debounce:       30 * time.Second,
		Now:            clock.now,
	}

	c.OnWake()
	clock.t = clock.t.Add(10 * time.Second) // inside the 30s window
	c.OnWake()

	if resyncCalls != 1 {
		t.Errorf("resyncCalls = %d, want 1 — second wake inside the debounce window must be ignored", resyncCalls)
	}
}

func TestOnWakeDebounceAllowsWakeAfterWindow(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1000, 0)}
	var resyncCalls int
	c := &Coordinator{
		Resync:         func() error { resyncCalls++; return nil },
		RecrawlEnabled: func() bool { return true },
		Debounce:       30 * time.Second,
		Now:            clock.now,
	}

	c.OnWake()
	clock.t = clock.t.Add(30 * time.Second) // exactly at the window edge
	c.OnWake()
	clock.t = clock.t.Add(31 * time.Second)
	c.OnWake()

	if resyncCalls != 3 {
		t.Errorf("resyncCalls = %d, want 3 — wakes at/after the debounce window must all run", resyncCalls)
	}
}

func TestOnWakeDebounceDefaultsWhenUnset(t *testing.T) {
	clock := &fixedClock{t: time.Unix(1000, 0)}
	var resyncCalls int
	c := &Coordinator{
		Resync:         func() error { resyncCalls++; return nil },
		RecrawlEnabled: func() bool { return true },
		Now:            clock.now,
		// Debounce left at zero: DefaultDebounce (30s) should apply.
	}

	c.OnWake()
	clock.t = clock.t.Add(29 * time.Second)
	c.OnWake()

	if resyncCalls != 1 {
		t.Errorf("resyncCalls = %d, want 1 — zero Debounce must fall back to DefaultDebounce", resyncCalls)
	}
}

// fakeNotifier is a Notifier a test fully controls, standing in for the
// darwin IOKit implementation the same way internal/fsevents' tests use a
// fake Stream. Stop and isStopped are called from different goroutines in
// TestRunCallsOnWakePerEvent (Run's own goroutine calls Stop; the test
// goroutine polls isStopped), so stopped needs a lock.
type fakeNotifier struct {
	ch chan struct{}

	mu      sync.Mutex
	stopped bool
}

func newFakeNotifier() *fakeNotifier { return &fakeNotifier{ch: make(chan struct{}, 4)} }

func (f *fakeNotifier) Events() <-chan struct{} { return f.ch }
func (f *fakeNotifier) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.stopped {
		f.stopped = true
		close(f.ch)
	}
}

func (f *fakeNotifier) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func TestRunCallsOnWakePerEvent(t *testing.T) {
	n := newFakeNotifier()
	resyncDone := make(chan struct{}, 4)
	c := &Coordinator{
		Resync:         func() error { resyncDone <- struct{}{}; return nil },
		RecrawlEnabled: func() bool { return true },
	}

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx, n)

	n.ch <- struct{}{}
	select {
	case <-resyncDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not call OnWake for a signalled event")
	}

	cancel()
	// n.Stop() runs from Run's own goroutine on ctx.Done(); give it a
	// moment before asserting.
	deadline := time.Now().Add(2 * time.Second)
	for !n.isStopped() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !n.isStopped() {
		t.Error("Run did not Stop the notifier on ctx cancellation")
	}
}

func TestRunReturnsWhenNotifierChannelCloses(t *testing.T) {
	n := newFakeNotifier()
	c := &Coordinator{RecrawlEnabled: func() bool { return true }}

	done := make(chan struct{})
	go func() {
		c.Run(context.Background(), n)
		close(done)
	}()

	close(n.ch)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return when the notifier's channel closed")
	}
}
