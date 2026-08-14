// Package reconcile implements the recrawl safety net described in
// "everything-macos-design.md" §7 item 3 and §8 item 3: a full, from-scratch
// crawl of one root on a low-priority timer, bounding FSEvents drift to
// hours rather than forever. It is deliberately dumb — a full swap, not an
// incremental diff — because that diff (§10 build order step 4) is out of
// scope here and a full crawl is cheap at the size of one root.
package reconcile

import (
	"context"
	"runtime"
	"time"

	"scry/internal/crawler"
	"scry/internal/index"
	"scry/internal/snapshot"
)

// DefaultInterval is how often each root is reconciled when the caller does
// not specify one: once a day, per §8 item 3.
const DefaultInterval = 24 * time.Hour

// Reconcile crawls root fresh and returns the resulting shard. If a
// snapshot already exists for root, the new shard's lastEID is carried
// forward from it, so a full-swap reconcile does not reset FSEvents
// tracking back to zero. A missing or stale existing snapshot is not an
// error here — Reconcile's job is to produce a fresh shard regardless.
func Reconcile(root string, opts crawler.Options) (*index.Shard, crawler.Stats, error) {
	var lastEID uint64
	if old, err := snapshot.Load(root); err == nil {
		lastEID = old.LastEID()
	}

	shard, stats, err := crawler.Crawl(root, opts)
	if shard != nil {
		shard.SetLastEID(lastEID)
	}
	return shard, stats, err
}

// RootSpec pairs a root path with the crawler options it should be
// reconciled with.
type RootSpec struct {
	Path string
	Opts crawler.Options
}

// Result is what a completed reconciliation pass over one root produced,
// delivered to a Scheduler's callback.
type Result struct {
	Root  string
	Shard *index.Shard
	Stats crawler.Stats
	Err   error
}

// Scheduler runs Reconcile against a fixed list of roots on a repeating
// interval, one root at a time — never two concurrently, so a resident
// daemon's background recrawl never competes with itself for disk and CPU.
// "Low priority" here means exactly that sequencing plus yielding between
// roots (runtime.Gosched), not an OS-level thread priority; Go has no
// portable way to ask for one.
type Scheduler struct {
	roots    []RootSpec
	interval time.Duration
	onResult func(Result)

	// wake lets an external trigger (see WakeFromSleep) force an
	// out-of-band pass without waiting for the interval timer.
	wake chan struct{}
}

// NewScheduler builds a Scheduler over roots, reconciling all of them once
// per interval (DefaultInterval if interval <= 0). onResult is called once
// per root, after each Reconcile completes, from the goroutine Run is
// called on — the caller is expected to save the snapshot and update its
// in-memory shard from there.
func NewScheduler(roots []RootSpec, interval time.Duration, onResult func(Result)) *Scheduler {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Scheduler{
		roots:    roots,
		interval: interval,
		onResult: onResult,
		wake:     make(chan struct{}, 1),
	}
}

// Run blocks, reconciling every root once per interval (or immediately on a
// WakeFromSleep trigger), until ctx is cancelled. It never runs two roots'
// reconciliations concurrently.
func (sc *Scheduler) Run(ctx context.Context) {
	timer := time.NewTimer(sc.interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			sc.passOnce(ctx)
			timer.Reset(sc.interval)
		case <-sc.wake:
			sc.passOnce(ctx)
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(sc.interval)
		}
	}
}

// passOnce reconciles every configured root, in order, one at a time,
// stopping early if ctx is cancelled mid-pass.
func (sc *Scheduler) passOnce(ctx context.Context) {
	for _, r := range sc.roots {
		if ctx.Err() != nil {
			return
		}
		shard, stats, err := Reconcile(r.Path, r.Opts)
		if sc.onResult != nil {
			sc.onResult(Result{Root: r.Path, Shard: shard, Stats: stats, Err: err})
		}
		runtime.Gosched()
	}
}

// WakeFromSleep requests an immediate reconciliation pass, as if the
// interval timer had just fired, without waiting for the rest of the
// current interval. It is non-blocking: a pending trigger is not doubled up
// if called again before Run picks it up.
//
// This is the integration point for the wake-from-sleep triggering the
// design doc calls out (§8 item 3) as darwin-specific: detecting the actual
// wake event means an IOKit/IOPMLib power-notification callback via cgo,
// which is out of scope for this phase. A darwin-only file elsewhere in the
// build is expected to call this method when it observes a wake event;
// nothing here needs to change to support that.
func (sc *Scheduler) WakeFromSleep() {
	select {
	case sc.wake <- struct{}{}:
	default:
	}
}
