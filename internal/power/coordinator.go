package power

import (
	"context"
	"sync"
	"time"
)

// DefaultDebounce is the minimum gap between two wake-triggered resyncs.
// A Mac can emit several power notifications around one physical wake
// (display wake, lid open, kIOMessageSystemHasPoweredOn), and treating
// each as a separate trigger would run the resync path — and, worse,
// could escalate to a full reconcile — more than once for what is really
// one event. Per §8 item 3.
const DefaultDebounce = 30 * time.Second

// Coordinator turns wake notifications into the behaviour §8 item 3
// requires: try the cheap repair first, always, and only fall back to a
// full reconcile pass when that cheap path could not work — and never
// fall back at all if the operator has turned the periodic recrawl off.
//
// Resync is expected to be the cheap path: restarting the FSEvents stream
// from each shard's saved lastEID (cmd/scry/daemon.go's startWatcher,
// reused rather than reinvented) rather than crawling anything. Fallback
// is the expensive path — a full reconcile pass, i.e.
// (*reconcile.Scheduler).WakeFromSleep — and is only called when Resync
// returns a non-nil error and RecrawlEnabled reports the periodic recrawl
// is not turned off.
//
// A Coordinator is safe for concurrent use; OnWake may be called from
// multiple goroutines (e.g. more than one Notifier feeding the same
// Coordinator) without racing the debounce window.
type Coordinator struct {
	Resync         func() error
	Fallback       func()
	RecrawlEnabled func() bool

	// Debounce overrides DefaultDebounce. <= 0 uses the default.
	Debounce time.Duration

	// Logf receives one line per notable decision (resync, resync
	// failure, escalation, debounce skip, recrawl-off skip). nil
	// discards.
	Logf func(format string, args ...interface{})

	// Now is injectable for tests; nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	lastRun time.Time
	hasRun  bool
}

func (c *Coordinator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Coordinator) log(format string, args ...interface{}) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Coordinator) debounce() time.Duration {
	if c.Debounce <= 0 {
		return DefaultDebounce
	}
	return c.Debounce
}

// OnWake handles one wake notification: debounce, then the cheap resync,
// then — only if the resync could not work, and only if the operator has
// not turned the periodic recrawl off — the full-reconcile fallback.
//
// It is exported directly (rather than reachable only through Run) so the
// decision logic can be table tested without a goroutine or a real
// Notifier — see coordinator_test.go, which is the deliverable for this
// package; nothing here touches IOKit.
func (c *Coordinator) OnWake() {
	now := c.now()

	c.mu.Lock()
	if c.hasRun && now.Sub(c.lastRun) < c.debounce() {
		since := now.Sub(c.lastRun)
		c.mu.Unlock()
		c.log("power: wake ignored, %s since the last resync (debounce is %s)", since, c.debounce())
		return
	}
	c.lastRun = now
	c.hasRun = true
	c.mu.Unlock()

	c.log("power: system woke; resyncing the FSEvents stream from the saved position")

	var err error
	if c.Resync != nil {
		err = c.Resync()
	}
	if err == nil {
		return
	}

	c.log("power: resync failed, the saved position could not be resumed: %v", err)

	if c.RecrawlEnabled != nil && !c.RecrawlEnabled() {
		c.log("power: recrawl_interval is off; not falling back to a full reconcile — " +
			"the index may have drifted while the resync was down, and \"Rebuild index\" will repair it")
		return
	}

	c.log("power: falling back to a full reconcile pass")
	if c.Fallback != nil {
		c.Fallback()
	}
}

// Run consumes n.Events() and calls OnWake for each signal, until ctx is
// cancelled or n's channel closes on its own. It stops n before returning
// on ctx cancellation, mirroring internal/watcher.Watcher.Run's shutdown
// shape.
func (c *Coordinator) Run(ctx context.Context, n Notifier) {
	events := n.Events()
	for {
		select {
		case <-ctx.Done():
			n.Stop()
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			c.OnWake()
		}
	}
}
