// Package watcher turns FSEvents notifications into shard mutations, as
// described in "everything-macos-design.md" §6. It depends only on
// internal/fsevents' Stream interface, not the darwin cgo implementation
// directly, so its logic — routing, idempotence, exclude rules, rescans,
// offline handling — runs and is tested on every platform, including
// Windows, against a fake event source. Only the real event source is
// darwin-only.
package watcher

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"scry/internal/crawler"
	"scry/internal/fsevents"
	"scry/internal/index"
	"scry/internal/roots"
)

// DefaultFlushInterval is how often dirty shards (ones that changed since
// their last snapshot write) are flushed to disk, absent an explicit
// Config.FlushInterval. It trades a little durability window for not
// fsyncing a full shard on every single filesystem event.
const DefaultFlushInterval = 5 * time.Second

// Root binds one configured root to the crawler options and offline
// policy the watcher should apply to events under it. Callers (cmd/scry)
// build this the same way they build crawler.Options for a full crawl, so
// exclude rules never drift between the two paths.
type Root struct {
	Path          string
	Opts          crawler.Options
	OfflinePolicy string // "keep" (default) or "drop"; see §6 "Offline roots"
}

func (r Root) dropOnOffline() bool {
	return strings.EqualFold(r.OfflinePolicy, "drop")
}

// Config configures a Watcher.
type Config struct {
	Roots  []Root
	Source fsevents.Stream

	// GetShard returns the *index.Shard currently resident for root, or
	// nil if root is no longer configured. SetShard installs a freshly
	// recrawled shard for root (a full-swap, the same shape
	// internal/reconcile uses). Persist saves shard to disk; it is only
	// ever called with a shard already obtained from GetShard.
	//
	// All three are expected to be safe for concurrent use with whatever
	// else reads or replaces the caller's shard list (see
	// cmd/scry/daemon.go's daemonState), since GetShard/SetShard run from
	// the watcher's own goroutine while queries run from others.
	GetShard func(root string) *index.Shard
	SetShard func(root string, shard *index.Shard)
	Persist  func(shard *index.Shard)

	// FlushInterval overrides DefaultFlushInterval. <= 0 uses the default.
	FlushInterval time.Duration

	// Logf receives one line per notable watcher action (rescans,
	// recrawls, offline/online transitions, wrap fallback). nil discards.
	Logf func(format string, args ...interface{})
}

// Watcher applies FSEvents notifications to a set of resident shards.
type Watcher struct {
	roots []Root

	getShard func(root string) *index.Shard
	setShard func(root string, shard *index.Shard)
	persist  func(shard *index.Shard)

	source fsevents.Stream
	logf   func(format string, args ...interface{})

	flushInterval time.Duration

	mu    sync.Mutex
	dirty map[string]bool
}

// New builds a Watcher from cfg. It does not start consuming events —
// call Run for that.
func New(cfg Config) *Watcher {
	interval := cfg.FlushInterval
	if interval <= 0 {
		interval = DefaultFlushInterval
	}
	return &Watcher{
		roots:         append([]Root(nil), cfg.Roots...),
		getShard:      cfg.GetShard,
		setShard:      cfg.SetShard,
		persist:       cfg.Persist,
		source:        cfg.Source,
		logf:          cfg.Logf,
		flushInterval: interval,
	}
}

// Run consumes events from the configured Source until ctx is cancelled or
// the source's event channel closes on its own. On return it flushes every
// dirty shard and stops the source, so a clean shutdown never loses a
// lastEID advance that happened since the last periodic flush.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	events := w.source.Events()

	for {
		select {
		case <-ctx.Done():
			w.flushAll()
			w.source.Stop()
			return

		case ev, ok := <-events:
			if !ok {
				w.flushAll()
				return
			}
			w.apply(ev)

		case <-ticker.C:
			w.flushDirty()
		}
	}
}

func (w *Watcher) log(format string, args ...interface{}) {
	if w.logf != nil {
		w.logf(format, args...)
	}
}

// apply routes one event to the shard that owns it and mutates that shard,
// per §6. Every branch here must be idempotent: FSEvents ids are
// host-global, and resuming a combined stream from the oldest shard's
// lastEID deliberately replays events other shards already applied.
func (w *Watcher) apply(ev fsevents.Event) {
	if ev.Flags.Has(fsevents.FlagEventIdsWrapped) {
		// The stored id is meaningless from here on; the only correct
		// response is to recrawl everything and start over.
		w.log("watcher: FSEvents id wrapped; recrawling all %d root(s)", len(w.roots))
		for _, r := range w.roots {
			w.recrawlRoot(r, 0)
		}
		return
	}
	if ev.Flags.Has(fsevents.FlagHistoryDone) {
		w.log("watcher: history replay complete; now live")
		return
	}
	if ev.Path == "" {
		return
	}

	r, ok := w.ownerRoot(ev.Path)
	if !ok {
		return // outside every configured root
	}
	shard := w.getShard(r.Path)
	if shard == nil {
		return // root was removed from config since this event was queued
	}

	switch {
	case ev.Flags.Has(fsevents.FlagUnmount):
		w.setOffline(r, shard)
		shard.SetLastEID(ev.ID)
		w.markDirty(r.Path)

	case ev.Flags.Has(fsevents.FlagMount):
		w.recrawlRoot(r, ev.ID)

	case ev.Flags.Has(fsevents.FlagRootChanged):
		w.handleRootChanged(r, ev.ID)

	case ev.Flags.Has(fsevents.FlagMustScanSubDirs):
		if err := rescanSubtree(shard, r, ev.Path); err != nil {
			w.log("watcher: rescan %s: %v", ev.Path, err)
		}
		shard.SetLastEID(ev.ID)
		w.markDirty(r.Path)

	default:
		if err := applyPathEvent(shard, r, ev.Path); err != nil {
			// Not fatal, and deliberately not a removal: the entry stays
			// as it was until something can actually read the path. Worth
			// logging every time, because the usual cause is a revoked TCC
			// grant, and silence there is what let an index lose two
			// thirds of its entries unnoticed.
			w.log("watcher: %s: cannot read (%v); leaving the index entry as-is", ev.Path, err)
		}
		shard.SetLastEID(ev.ID)
		w.markDirty(r.Path)
	}
}

// ownerRoot finds the configured Root that contains path. Roots are
// collapsed at configuration time (internal/roots.Normalize) so no kept
// root is ever nested inside another, meaning at most one can match.
func (w *Watcher) ownerRoot(path string) (Root, bool) {
	for _, r := range w.roots {
		if roots.Contains(r.Path, path) {
			return r, true
		}
	}
	return Root{}, false
}

// setOffline marks shard offline and, if r's offline_policy is "drop",
// empties it — never deletes the on-disk snapshot, per §6 "Offline roots".
func (w *Watcher) setOffline(r Root, shard *index.Shard) {
	shard.SetOnline(false)
	if r.dropOnOffline() {
		for _, id := range shard.Children(shard.RootID()) {
			shard.Remove(id)
		}
	}
	w.log("watcher: root %s offline (policy=%s)", r.Path, offlinePolicyLabel(r))
}

func offlinePolicyLabel(r Root) string {
	if r.dropOnOffline() {
		return "drop"
	}
	return "keep"
}

// handleRootChanged reacts to a RootChanged event that arrived without an
// accompanying Mount/Unmount flag: something changed along the path to the
// root itself (renamed, replaced, or the volume went away without an
// explicit unmount notification). Verify by stat: gone means offline,
// still there means reconcile with a recrawl.
//
// Only an error that means the root is really absent counts as gone (see
// statMeansGone). A root that is merely unreadable — a revoked TCC grant is
// the common case — must not be marked offline, because offline_policy =
// "drop" empties the shard, which would discard the whole index over a
// permission change rather than a missing disk.
func (w *Watcher) handleRootChanged(r Root, eid uint64) {
	_, err := os.Stat(r.Path)
	switch {
	case err == nil:
		w.recrawlRoot(r, eid)
	case statMeansGone(err):
		if shard := w.getShard(r.Path); shard != nil {
			w.setOffline(r, shard)
			shard.SetLastEID(eid)
			w.markDirty(r.Path)
		}
	default:
		w.log("watcher: root %s: cannot read (%v); not marking it offline", r.Path, err)
	}
}

// recrawlRoot does a full, from-scratch crawl of r (the same operation
// internal/reconcile's timer runs), swaps in the result, marks it online,
// and persists it immediately. eid, if non-zero, becomes the new shard's
// lastEID; 0 means "carry the previous shard's lastEID forward" (used by
// the EventIdsWrapped fallback, where the triggering event's own id is not
// a meaningful position to resume from).
func (w *Watcher) recrawlRoot(r Root, eid uint64) {
	shard, stats, err := crawler.Crawl(r.Path, r.Opts)
	if shard == nil {
		w.log("watcher: recrawl %s failed: %v", r.Path, err)
		return
	}
	shard.SetOnline(true)
	if eid != 0 {
		shard.SetLastEID(eid)
	} else if old := w.getShard(r.Path); old != nil {
		shard.SetLastEID(old.LastEID())
	}

	if w.setShard != nil {
		w.setShard(r.Path, shard)
	}
	if w.persist != nil {
		w.persist(shard)
	}
	w.log("watcher: recrawled %s: %d entries, %d errors, %s", r.Path, stats.Entries, stats.Errors, stats.Duration)
}

func (w *Watcher) markDirty(root string) {
	w.mu.Lock()
	if w.dirty == nil {
		w.dirty = make(map[string]bool)
	}
	w.dirty[root] = true
	w.mu.Unlock()
}

func (w *Watcher) flushDirty() {
	w.mu.Lock()
	dirty := w.dirty
	w.dirty = nil
	w.mu.Unlock()

	for root := range dirty {
		w.flushOne(root)
	}
}

func (w *Watcher) flushAll() {
	w.mu.Lock()
	w.dirty = nil
	w.mu.Unlock()

	for _, r := range w.roots {
		w.flushOne(r.Path)
	}
}

func (w *Watcher) flushOne(root string) {
	if w.persist == nil || w.getShard == nil {
		return
	}
	if shard := w.getShard(root); shard != nil {
		w.persist(shard)
	}
}

// relParts splits path into components relative to root. It reports false
// if path is not root itself or beneath it.
func relParts(root, path string) ([]string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, false
	}
	rel = filepath.Clean(rel)
	if rel == "." {
		return nil, true // path == root
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, false
	}
	return strings.Split(rel, string(filepath.Separator)), true
}

// pathExcluded reports whether path, relative to root, is skipped by r's
// exclude rules — checked against every path component, not just the
// final one, so a file three levels inside a newly created node_modules
// is excluded because node_modules is, exactly as a full crawl would skip
// it (§6: "a newly created node_modules must not get indexed").
func pathExcluded(root string, path string, opts crawler.Options) bool {
	// Anchored excludes are checked against the whole path, which already
	// covers everything underneath them — no need to walk components.
	if crawler.MatchesExcludePath(path, opts.ExcludePaths) {
		return true
	}
	parts, ok := relParts(root, path)
	if !ok {
		return true
	}
	for _, part := range parts {
		if !opts.Hidden && strings.HasPrefix(part, ".") {
			return true
		}
		if crawler.MatchesExclude(part, opts.Excludes, opts.Globs) {
			return true
		}
	}
	return false
}

// removeIfPresent tombstones the entry at path in shard, if it is
// currently indexed. Walking down via Shard.Lookup rather than creating
// anything makes this side idempotent by construction: an already-absent
// path (whether never indexed, or removed by an earlier application of
// this same event) is simply a no-op.
func removeIfPresent(shard *index.Shard, root, path string) {
	parts, ok := relParts(root, path)
	if !ok || len(parts) == 0 {
		return
	}
	id := shard.RootID()
	for _, part := range parts {
		next, found := shard.Lookup(id, part)
		if !found {
			return
		}
		id = next
	}
	shard.Remove(id)
}

// ensurePath walks path's components under root, idempotently upserting
// any that are missing (mkdir -p semantics, restricted to shard state —
// nothing on disk is created), and returns the id path itself now has.
// Each component that must be created is lstat'd individually, so a path
// that raced out of existence partway through simply stops early: that
// is reported as !ok and the caller treats it the same as "nothing to
// do", which is what remove-if-present would have produced for the same
// race anyway.
func ensurePath(shard *index.Shard, root, path string) (uint32, bool) {
	parts, ok := relParts(root, path)
	if !ok {
		return 0, false
	}
	id := shard.RootID()
	if len(parts) == 0 {
		return id, true // path == root
	}

	cur := root
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		isLast := i == len(parts)-1

		if existing, found := shard.Lookup(id, part); found && !isLast {
			// An already-indexed intermediate directory: trust it rather
			// than re-stat every ancestor on every single event.
			id = existing
			continue
		}

		info, err := os.Lstat(cur)
		if err != nil {
			return 0, false
		}

		var flags index.Flags
		if strings.HasPrefix(part, ".") {
			flags |= index.FlagHidden
		}
		var size int64
		if info.Mode()&os.ModeSymlink != 0 {
			flags |= index.FlagSymlink
			size = info.Size()
		} else if info.IsDir() {
			flags |= index.FlagDir
		} else {
			size = info.Size()
		}

		id = shard.Upsert(id, part, flags, size, info.ModTime().UnixNano())
	}
	return id, true
}

// applyPathEvent synchronizes shard's view of path to its current on-disk
// reality: present and not excluded means indexed (idempotently upserted
// all the way down), absent or excluded means removed if present. This
// deliberately ignores the event's own Created/Removed/Renamed subtype
// flags — see the package doc for why: FSEvents does not promise
// per-event fidelity, but a fresh lstat is authoritative about existence,
// and synchronizing to it is idempotent no matter how many duplicate or
// out-of-order events name the same path.
//
// "Authoritative" only covers lstat errors that actually mean the path is
// gone, which is why statMeansGone exists. A returned error means the path
// could not be classified and the index was left exactly as it was; the
// caller logs it.
func applyPathEvent(shard *index.Shard, r Root, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if statMeansGone(err) {
			removeIfPresent(shard, r.Path, path)
			return nil
		}
		return err
	}
	if pathExcluded(r.Path, path, r.Opts) {
		removeIfPresent(shard, r.Path, path)
		return nil
	}
	_ = info // ensurePath re-lstats the final component; kept simple over micro-optimizing away one syscall.
	ensurePath(shard, r.Path, path)
	return nil
}

// statMeansGone reports whether an lstat/stat error is evidence that the
// path is really absent, as opposed to merely unreadable right now.
//
// Treating every stat failure as "deleted" cost a real index. On macOS,
// TCC revokes a grant whenever the app's code signing identity changes —
// renaming the bundle ID does it, and so does an ad-hoc rebuild. Paths
// under ~/Desktop, ~/Documents and ~/Downloads then lstat with EPERM, not
// ENOENT. The FSEvents history replay that runs on every daemon start and
// every wake from sleep walks straight into them, and the old code deleted
// each one from the shard, then persisted the result: a 43,000-entry index
// dropped to 14,000 with no error logged anywhere, because nothing here
// treated a permission failure as different from a deletion.
//
// A stale entry for a file that is genuinely gone is trivially repaired by
// the next event or the next recrawl. A deleted entry for a file that is
// still there is invisible — the file simply stops being findable. Keep
// the entry whenever there is doubt.
func statMeansGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
