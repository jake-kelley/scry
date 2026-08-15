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

// NoInterval, passed to NewScheduler as the interval, turns the periodic
// timer off entirely: the Scheduler still exists and still reconciles on
// demand via WakeFromSleep, but nothing ever fires on its own. It is for a
// caller that trusts the FSEvents watcher to be the only update mechanism
// and wants the recrawl reserved for an explicit "rebuild index".
//
// Any negative duration means this. Zero still means DefaultInterval, so
// the "unset" path that predates this constant is unchanged.
const NoInterval = -1 * time.Nanosecond

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
// per interval (DefaultInterval if interval is zero, never if it is
// negative — see NoInterval). onResult is called once per root, after each
// Reconcile completes, from the goroutine Run is called on — the caller is
// expected to save the snapshot and update its in-memory shard from there.
func NewScheduler(roots []RootSpec, interval time.Duration, onResult func(Result)) *Scheduler {
	if interval == 0 {
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
// reconciliations concurrently. With a negative interval there is no timer
// at all and only WakeFromSleep drives a pass.
//
// Note that the timer is restarted after a pass finishes, not at a fixed
// rate: the period observed from outside is the interval plus however long
// the crawl took. That is deliberate for a backstop — two passes should
// never overlap or queue up behind each other — but it does mean the
// interval is a gap, not a rate.
func (sc *Scheduler) Run(ctx context.Context) {
	var timerC <-chan time.Time
	var timer *time.Timer
	if sc.interval > 0 {
		timer = time.NewTimer(sc.interval)
		defer timer.Stop()
		timerC = timer.C
	}

	// reset restarts the timer, if there is one, for another full interval.
	// The Stop/drain dance is only correct for a timer that has not already
	// fired, so the two call sites below differ in whether they drain.
	reset := func(drain bool) {
		if timer == nil {
			return
		}
		if drain && !timer.Stop() {
			<-timer.C
		}
		timer.Reset(sc.interval)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-timerC:
			sc.passOnce(ctx)
			reset(false)
		case <-sc.wake:
			sc.passOnce(ctx)
			reset(true)
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
// This is the fallback half of the wake-from-sleep integration the design
// doc calls out (§8 item 3): internal/power detects the actual IOKit wake
// event, and cmd/scry/daemon.go's power.Coordinator calls this method only
// when the cheap path — restarting the FSEvents stream from each shard's
// saved lastEID — could not work, and only when recrawl_interval has not
// been turned off. A wake that resyncs cleanly never reaches here at all.
func (sc *Scheduler) WakeFromSleep() {
	select {
	case sc.wake <- struct{}{}:
	default:
	}
}
