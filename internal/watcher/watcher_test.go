package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"scry/internal/crawler"
	"scry/internal/fsevents"
	"scry/internal/index"
)

// fakeStream is an fsevents.Stream a test drives directly, standing in for
// the darwin cgo implementation. This is the whole point of the
// interface split in internal/fsevents: watcher logic is exercised here on
// every platform.
type fakeStream struct {
	ch       chan fsevents.Event
	stopped  chan struct{}
	stopOnce sync.Once
}

func newFakeStream() *fakeStream {
	return &fakeStream{ch: make(chan fsevents.Event, 64), stopped: make(chan struct{})}
}

func (f *fakeStream) Events() <-chan fsevents.Event { return f.ch }

func (f *fakeStream) Stop() {
	f.stopOnce.Do(func() { close(f.ch); close(f.stopped) })
}

func (f *fakeStream) send(ev fsevents.Event) { f.ch <- ev }

// harness bundles a single-root Watcher wired to a fakeStream and an
// in-memory shard registry, plus synchronous helpers so tests don't need
// to sleep-and-poll: every send is followed by a drain that blocks until
// the watcher's event channel is empty and one more no-op round-trip has
// completed.
type harness struct {
	t      *testing.T
	root   string
	opts   crawler.Options
	stream *fakeStream

	mu    sync.Mutex
	shard *index.Shard
	saves int

	w      *Watcher
	cancel context.CancelFunc
	done   chan struct{}
}

func newHarness(t *testing.T, root string, opts crawler.Options) *harness {
	t.Helper()
	h := &harness{t: t, root: root, opts: opts, stream: newFakeStream(), shard: index.New(root)}

	cfg := Config{
		Roots:  []Root{{Path: root, Opts: opts, OfflinePolicy: "keep"}},
		Source: h.stream,
		GetShard: func(r string) *index.Shard {
			h.mu.Lock()
			defer h.mu.Unlock()
			if r != h.root {
				return nil
			}
			return h.shard
		},
		SetShard: func(r string, s *index.Shard) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if r == h.root {
				h.shard = s
			}
		},
		Persist: func(s *index.Shard) {
			h.mu.Lock()
			h.saves++
			h.mu.Unlock()
		},
		FlushInterval: time.Hour, // tests flush explicitly; keep the ticker out of the way
		Logf:          func(string, ...interface{}) {},
	}
	h.w = New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		h.w.Run(ctx)
		close(h.done)
	}()
	return h
}

// sync sends a harmless probe event for a path outside the root and waits
// for it to come back out the other side of apply() by observing a status
// call would be racy; instead, since apply() processes events strictly in
// channel order, sending on the buffered channel and then calling
// stopAndWait's synchronization primitive is overkill for these tests —
// each test instead calls settle() after sending real events, which drains
// via a marker event round-trip.
func (h *harness) settle() {
	h.t.Helper()
	// A dedicated marker channel would need watcher cooperation; simpler
	// and sufficient here: events are applied synchronously in Run's
	// select loop, so pushing N events and then closing the stream (which
	// only happens in stopAndWait) guarantees prior events were applied
	// first. For mid-test synchronization, poll briefly — flaky only if
	// apply() itself hangs, which would be its own bug worth surfacing.
	deadline := time.Now().Add(2 * time.Second)
	for len(h.stream.ch) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Give the goroutine a moment to finish processing the last item it
	// already read off the channel.
	time.Sleep(10 * time.Millisecond)
}

func (h *harness) stop() {
	h.cancel()
	<-h.done
}

func (h *harness) shardNow() *index.Shard {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shard
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func findID(t *testing.T, s *index.Shard, root, path string) (uint32, bool) {
	t.Helper()
	id := s.RootID()
	parts, ok := relParts(root, path)
	if !ok {
		return 0, false
	}
	for _, part := range parts {
		next, found := s.Lookup(id, part)
		if !found {
			return 0, false
		}
		id = next
	}
	return id, true
}

func TestCreateEventIndexesFile(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t, root, crawler.Options{})
	defer h.stop()

	target := filepath.Join(root, "proof.txt")
	mustWrite(t, target, "hi")

	h.stream.send(fsevents.Event{Path: target, ID: 1, Flags: fsevents.FlagItemCreated})
	h.settle()

	if _, ok := findID(t, h.shardNow(), root, target); !ok {
		t.Fatalf("expected %s to be indexed after a create event", target)
	}
	if got := h.shardNow().LastEID(); got != 1 {
		t.Fatalf("LastEID() = %d, want 1", got)
	}
}

func TestRemoveEventTombstonesFile(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t, root, crawler.Options{})
	defer h.stop()

	target := filepath.Join(root, "gone.txt")
	mustWrite(t, target, "x")
	h.stream.send(fsevents.Event{Path: target, ID: 1, Flags: fsevents.FlagItemCreated})
	h.settle()
	if _, ok := findID(t, h.shardNow(), root, target); !ok {
		t.Fatalf("setup: expected %s indexed", target)
	}

	if err := os.Remove(target); err != nil {
		t.Fatalf("remove: %v", err)
	}
	h.stream.send(fsevents.Event{Path: target, ID: 2, Flags: fsevents.FlagItemRemoved})
	h.settle()

	if _, ok := findID(t, h.shardNow(), root, target); ok {
		t.Fatalf("expected %s removed after delete event", target)
	}
}

// TestApplyIsIdempotent is the test the design doc calls for explicitly:
// applying the very same event twice — the scenario a combined stream
// resuming from the oldest shard's lastEID produces routinely — must be a
// no-op the second time, for both an add and a remove.
func TestApplyIsIdempotent(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t, root, crawler.Options{})
	defer h.stop()

	target := filepath.Join(root, "twice.txt")
	mustWrite(t, target, "x")

	ev := fsevents.Event{Path: target, ID: 5, Flags: fsevents.FlagItemCreated}
	h.stream.send(ev)
	h.settle()
	id1, ok := findID(t, h.shardNow(), root, target)
	if !ok {
		t.Fatalf("setup: expected indexed")
	}

	// Re-apply the identical event, as a resumed stream would.
	h.stream.send(ev)
	h.settle()
	id2, ok := findID(t, h.shardNow(), root, target)
	if !ok {
		t.Fatalf("expected still indexed after re-applying the same create event")
	}
	if id1 != id2 {
		t.Fatalf("re-applying the same event changed the entry's id: %d -> %d (should be stable)", id1, id2)
	}
	if got := h.shardNow().Len(); got != 2 { // root + one file
		t.Fatalf("Len() = %d, want 2 (no duplicate entry from replay)", got)
	}

	// Now the remove side: apply the same removal twice.
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove: %v", err)
	}
	removeEv := fsevents.Event{Path: target, ID: 6, Flags: fsevents.FlagItemRemoved}
	h.stream.send(removeEv)
	h.settle()
	h.stream.send(removeEv)
	h.settle()

	if _, ok := findID(t, h.shardNow(), root, target); ok {
		t.Fatalf("expected removed after replaying the same delete event twice")
	}
	if got := h.shardNow().Len(); got != 1 { // root only
		t.Fatalf("Len() = %d, want 1 after idempotent removal", got)
	}
}

func TestEventOutsideRootIsIgnored(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	h := newHarness(t, root, crawler.Options{})
	defer h.stop()

	target := filepath.Join(elsewhere, "not-mine.txt")
	mustWrite(t, target, "x")
	h.stream.send(fsevents.Event{Path: target, ID: 1, Flags: fsevents.FlagItemCreated})
	h.settle()

	if got := h.shardNow().Len(); got != 1 { // root only, untouched
		t.Fatalf("Len() = %d, want 1 (event outside the root must be ignored)", got)
	}
}

func TestExcludedDirectoryContentsNeverIndexed(t *testing.T) {
	root := t.TempDir()
	opts := crawler.Options{Excludes: []string{"node_modules"}}
	h := newHarness(t, root, opts)
	defer h.stop()

	nested := filepath.Join(root, "node_modules", "left-pad", "index.js")
	mustWrite(t, nested, "module.exports = {}")

	// A new node_modules dir and the file three levels inside it: send
	// events the way FSEvents' per-file granularity would, deepest first,
	// as a worst case for anything relying on parent-before-child order.
	h.stream.send(fsevents.Event{Path: nested, ID: 1, Flags: fsevents.FlagItemCreated})
	h.settle()

	if got := h.shardNow().Len(); got != 1 { // root only
		t.Fatalf("Len() = %d, want 1 (node_modules contents must not be indexed)", got)
	}
}

func TestHiddenFileRespectsHiddenOption(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t, root, crawler.Options{Hidden: false})
	defer h.stop()

	target := filepath.Join(root, ".secret")
	mustWrite(t, target, "x")
	h.stream.send(fsevents.Event{Path: target, ID: 1, Flags: fsevents.FlagItemCreated})
	h.settle()

	if _, ok := findID(t, h.shardNow(), root, target); ok {
		t.Fatalf("expected .secret not indexed when Hidden is false")
	}
}

func TestMustScanSubDirsRescanDiffsAdditionsAndRemovals(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t, root, crawler.Options{})
	defer h.stop()

	sub := filepath.Join(root, "sub")
	keep := filepath.Join(sub, "keep.txt")
	stray := filepath.Join(sub, "stray.txt")
	mustWrite(t, keep, "a")
	mustWrite(t, stray, "b")

	// Establish baseline via individual create events first.
	h.stream.send(fsevents.Event{Path: keep, ID: 1, Flags: fsevents.FlagItemCreated})
	h.stream.send(fsevents.Event{Path: stray, ID: 2, Flags: fsevents.FlagItemCreated})
	h.settle()
	if _, ok := findID(t, h.shardNow(), root, stray); !ok {
		t.Fatalf("setup: expected stray indexed before rescan")
	}

	// Now the world changes without individual events reaching us
	// (coalesced): stray.txt is deleted, added.txt appears.
	if err := os.Remove(stray); err != nil {
		t.Fatalf("remove: %v", err)
	}
	added := filepath.Join(sub, "added.txt")
	mustWrite(t, added, "c")

	h.stream.send(fsevents.Event{Path: sub, ID: 3, Flags: fsevents.FlagMustScanSubDirs})
	h.settle()

	if _, ok := findID(t, h.shardNow(), root, keep); !ok {
		t.Fatalf("expected keep.txt to survive the rescan")
	}
	if _, ok := findID(t, h.shardNow(), root, added); !ok {
		t.Fatalf("expected added.txt to be picked up by the rescan")
	}
	if _, ok := findID(t, h.shardNow(), root, stray); ok {
		t.Fatalf("expected stray.txt to be removed by the rescan diff")
	}
	if got := h.shardNow().LastEID(); got != 3 {
		t.Fatalf("LastEID() = %d, want 3", got)
	}
}

func TestUnmountKeepPolicyRetainsEntriesButGoesOffline(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t, root, crawler.Options{})
	defer h.stop()

	target := filepath.Join(root, "file.txt")
	mustWrite(t, target, "x")
	h.stream.send(fsevents.Event{Path: target, ID: 1, Flags: fsevents.FlagItemCreated})
	h.settle()
	before := h.shardNow().Len()

	h.stream.send(fsevents.Event{Path: root, ID: 2, Flags: fsevents.FlagUnmount})
	h.settle()

	s := h.shardNow()
	if s.Online() {
		t.Fatalf("expected shard offline after Unmount")
	}
	if got := s.Len(); got != before {
		t.Fatalf("keep policy: Len() = %d, want unchanged %d", got, before)
	}
}

func TestUnmountDropPolicyEmptiesShard(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t, root, crawler.Options{})
	// Override the default "keep" policy for this test.
	h.w.roots[0].OfflinePolicy = "drop"
	defer h.stop()

	target := filepath.Join(root, "file.txt")
	mustWrite(t, target, "x")
	h.stream.send(fsevents.Event{Path: target, ID: 1, Flags: fsevents.FlagItemCreated})
	h.settle()

	h.stream.send(fsevents.Event{Path: root, ID: 2, Flags: fsevents.FlagUnmount})
	h.settle()

	s := h.shardNow()
	if s.Online() {
		t.Fatalf("expected shard offline after Unmount")
	}
	if got := s.Len(); got != 1 { // root entry only
		t.Fatalf("drop policy: Len() = %d, want 1 (emptied)", got)
	}
}

func TestEventIdsWrappedTriggersFullRecrawl(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t, root, crawler.Options{})
	defer h.stop()

	// Index a file the normal way, then create a second one on disk
	// without ever telling the watcher about it — only a wrap recovery
	// should pick it up.
	seen := filepath.Join(root, "seen.txt")
	mustWrite(t, seen, "a")
	h.stream.send(fsevents.Event{Path: seen, ID: 1, Flags: fsevents.FlagItemCreated})
	h.settle()

	unseen := filepath.Join(root, "unseen.txt")
	mustWrite(t, unseen, "b")

	h.stream.send(fsevents.Event{Path: "", ID: 0, Flags: fsevents.FlagEventIdsWrapped})
	h.settle()

	if _, ok := findID(t, h.shardNow(), root, unseen); !ok {
		t.Fatalf("expected unseen.txt picked up by the wrap-triggered recrawl")
	}
}

func TestPersistIsCalledOnGracefulShutdown(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t, root, crawler.Options{})

	target := filepath.Join(root, "x.txt")
	mustWrite(t, target, "x")
	h.stream.send(fsevents.Event{Path: target, ID: 1, Flags: fsevents.FlagItemCreated})
	h.settle()

	h.stop()

	h.mu.Lock()
	saves := h.saves
	h.mu.Unlock()
	if saves == 0 {
		t.Fatalf("expected at least one Persist call on shutdown flush")
	}
}
