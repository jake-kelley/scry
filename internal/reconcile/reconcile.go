// Package reconcile implements the recrawl safety net described in
// "everything-macos-design.md" §7 item 3 and §8 item 3: a full, from-scratch
// crawl of one root on a low-priority timer, bounding FSEvents drift to
// hours rather than forever.
//
// The crawl itself is still a full walk, not an incremental one — a diff
// cannot avoid stat-ing everything to learn what changed, so there is no
// way to make the walk itself cheaper this way, and this package does not
// claim to. What §10 build order step 4 adds on top of the full-swap
// design this package started with (see diff.go) is: skip the snapshot
// write and the live shard swap when the walk found nothing different,
// and make what drifted visible via a logged summary and Result.Diff,
// instead of silently discarding it the way a blind swap always did.
package reconcile

import (
	"context"
	"fmt"
	"os"
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
	_, _, shard, stats, err := doReconcile(root, opts)
	return shard, stats, err
}

// doReconcile is Reconcile's implementation, additionally returning the
// old (snapshot-loaded) shard and whether one existed, so passOnce can
// diff against it without loading the snapshot a second time.
func doReconcile(root string, opts crawler.Options) (old *index.Shard, hadOld bool, shard *index.Shard, stats crawler.Stats, err error) {
	if o, lerr := snapshot.Load(root); lerr == nil {
		old, hadOld = o, true
	}

	var lastEID uint64
	if hadOld {
		lastEID = old.LastEID()
	}

	shard, stats, err = crawler.Crawl(root, opts)
	if shard != nil {
		shard.SetLastEID(lastEID)
	}
	return old, hadOld, shard, stats, err
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

	// Diff is what changed between the shard that was on disk before this
	// pass and the one this pass just crawled. When Err is non-nil, or
	// when this pass refused to trust its own crawl (see the guard in
	// passOnce), Diff is the zero value and must not be read as "nothing
	// changed" — it means "not computed".
	Diff Diff

	Err error
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
		if sc.onResult != nil {
			sc.onResult(reconcileOne(r))
		}
		runtime.Gosched()
	}
}

// reconcileOne runs one root's reconciliation pass and produces the Result
// a Scheduler delivers to onResult: the fresh shard, the diff against
// whatever was on disk before this pass, and a one-line log summary.
//
// The guard below is the one place this change can lose data. A crawl
// that comes back with zero entries but a nonzero error count — root
// unmounted mid-walk, or the root path itself unreadable — looks
// identical, entry-count-wise, to a root that has genuinely been emptied
// out. Diffing that against a shard that used to have thousands of
// entries in it would produce a "removed everything" diff and, once
// applied, a permanently empty index for a root that is probably still
// fine. crawler.Stats.Errors plus the pre-crawl entry count is what
// distinguishes the two: a genuinely empty crawl has no errors. When the
// guard trips, the pass is reported as a failure (Result.Err set) instead
// of a diff, exactly like any other Reconcile error — the existing
// caller-side handling of Err already refuses to save or swap in that
// case, so no separate skip path is needed for it.
//
// A second guard covers the partial version of the same accident, which
// is the more likely one in practice: a permission change or an unmount
// that takes out one large subtree rather than the whole root. That crawl
// succeeds, reports errors, and yields a diff removing a large fraction of
// the index. The pairing is what makes it safe to refuse — a user really
// can delete half their files between two passes, but doing so does not
// make the crawler report errors, so "lost a lot of entries" plus "hit
// errors while walking" is a shape no legitimate change produces.
// Deliberately conservative: it needs Errors > 0, so an error-free crawl
// is always trusted no matter how much it removes.
func reconcileOne(r RootSpec) Result {
	old, hadOld, shard, stats, err := doReconcile(r.Path, r.Opts)
	if err != nil {
		logPass(r.Path, stats, Diff{}, err)
		return Result{Root: r.Path, Shard: shard, Stats: stats, Err: err}
	}

	oldCount := 0
	if hadOld {
		oldCount = old.CountIndexed()
	}
	if stats.Entries == 0 && stats.Errors > 0 && oldCount > 0 {
		err := fmt.Errorf("reconcile: crawl produced zero entries but reported %d error(s); refusing to treat %d previously-indexed entries as removed (root likely offline or unreadable)", stats.Errors, oldCount)
		logPass(r.Path, stats, Diff{}, err)
		return Result{Root: r.Path, Shard: shard, Stats: stats, Err: err}
	}

	if !hadOld {
		old = index.New(shard.Root())
	}
	diff := diffShards(old, shard)
	if err := guardTruncatedDiff(stats.Errors, oldCount, diff.Removed); err != nil {
		logPass(r.Path, stats, Diff{}, err)
		return Result{Root: r.Path, Shard: shard, Stats: stats, Err: err}
	}
	logPass(r.Path, stats, diff, nil)
	return Result{Root: r.Path, Shard: shard, Stats: stats, Diff: diff}
}

// guardTruncatedDiff returns a non-nil error when a diff is too destructive
// to have come from a crawl that went wrong. Split out from reconcileOne so
// the rule itself is testable without having to make a subtree unreadable,
// which is not portable across the platforms this repo is developed and
// shipped on.
//
// The threshold is half the previous index. Both conditions are required:
// with no crawl errors the diff is always trusted, however large, because a
// user emptying a directory is a legitimate thing that produces no errors.
func guardTruncatedDiff(crawlErrors, oldCount, removed int) error {
	if crawlErrors <= 0 || oldCount <= 0 || removed <= oldCount/2 {
		return nil
	}
	return fmt.Errorf("reconcile: crawl reported %d error(s) and would remove %d of %d indexed entries; "+
		"refusing a diff that large from a crawl that did not complete cleanly "+
		"(subtree likely unreadable or unmounted)", crawlErrors, removed, oldCount)
}

// logPass writes the one-line-per-pass summary this change adds: what the
// crawl found, and what changed relative to what was already on disk.
// Written straight to stderr rather than threaded through a Logf field on
// Scheduler, so turning this on needs no change to any caller's wiring.
func logPass(root string, stats crawler.Stats, diff Diff, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: %s: FAILED: %v (%d entries, %d errors, %s)\n",
			root, err, stats.Entries, stats.Errors, stats.Duration)
		return
	}
	fmt.Fprintf(os.Stderr, "reconcile: %s: %d entries (%d errors, %s) — diff: +%d -%d ~%d\n",
		root, stats.Entries, stats.Errors, stats.Duration, diff.Added, diff.Removed, diff.Changed)
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
