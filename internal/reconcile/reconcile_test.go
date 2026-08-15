package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"scry/internal/crawler"
	"scry/internal/index"
	"scry/internal/snapshot"
)

func withTempCacheHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
}

func TestReconcileCarriesLastEIDForward(t *testing.T) {
	withTempCacheHome(t)

	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "sub"))
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	old := index.New(root)
	old.SetLastEID(999)
	if err := snapshot.Save(old); err != nil {
		t.Fatalf("Save: %v", err)
	}

	shard, stats, err := Reconcile(root, crawler.Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Entries == 0 {
		t.Fatalf("Reconcile produced no entries for a root with files in it")
	}
	if shard.LastEID() != 999 {
		t.Fatalf("LastEID() = %d, want 999 (carried forward from prior snapshot)", shard.LastEID())
	}
}

func TestReconcileNoExistingSnapshotStartsAtZero(t *testing.T) {
	withTempCacheHome(t)

	root := t.TempDir()

	shard, _, err := Reconcile(root, crawler.Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if shard.LastEID() != 0 {
		t.Fatalf("LastEID() = %d, want 0 (no prior snapshot to carry forward)", shard.LastEID())
	}
}

func TestSchedulerRunsAllRootsAndRespectsCancellation(t *testing.T) {
	withTempCacheHome(t)

	rootA := t.TempDir()
	rootB := t.TempDir()

	results := make(chan Result, 4)

	sc := NewScheduler(
		[]RootSpec{{Path: rootA}, {Path: rootB}},
		50*time.Millisecond,
		func(r Result) { results <- r },
	)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		sc.Run(ctx)
		close(done)
	}()

	seen := map[string]bool{}
	timeout := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case r := <-results:
			seen[r.Root] = true
		case <-timeout:
			t.Fatal("timed out waiting for both roots to be reconciled")
		}
	}

	<-done // Run must return once ctx is cancelled.
}

// TestSchedulerNoIntervalNeverFiresOnItsOwn covers recrawl_interval =
// "off": the timer must be absent entirely, not merely long. The negative
// interval also has to survive NewScheduler, which turns *zero* into the
// 24h default — mixing those two up would silently re-enable the recrawl.
func TestSchedulerNoIntervalNeverFiresOnItsOwn(t *testing.T) {
	withTempCacheHome(t)

	root := t.TempDir()
	results := make(chan Result, 4)

	sc := NewScheduler([]RootSpec{{Path: root}}, NoInterval, func(r Result) { results <- r })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		sc.Run(ctx)
		close(done)
	}()

	select {
	case r := <-results:
		t.Fatalf("a pass ran for %q with the periodic recrawl off", r.Root)
	case <-time.After(300 * time.Millisecond):
	}

	// An explicit rebuild must still work: off means no timer, not a
	// scheduler that has stopped listening.
	sc.WakeFromSleep()
	select {
	case r := <-results:
		if r.Root != root {
			t.Fatalf("Result.Root = %q, want %q", r.Root, root)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WakeFromSleep did not trigger a pass with the periodic recrawl off")
	}

	// And a second wake still works — the reset path must not block or
	// panic on the drain of a timer that does not exist.
	sc.WakeFromSleep()
	select {
	case <-results:
	case <-time.After(2 * time.Second):
		t.Fatal("second WakeFromSleep produced no pass")
	}

	cancel()
	<-done
}

func TestSchedulerWakeFromSleepTriggersImmediatePass(t *testing.T) {
	withTempCacheHome(t)

	root := t.TempDir()
	results := make(chan Result, 4)

	sc := NewScheduler(
		[]RootSpec{{Path: root}},
		time.Hour, // long enough that only WakeFromSleep can produce a result in this test's window
		func(r Result) { results <- r },
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sc.Run(ctx)

	sc.WakeFromSleep()

	select {
	case r := <-results:
		if r.Root != root {
			t.Fatalf("Result.Root = %q, want %q", r.Root, root)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WakeFromSleep did not trigger a reconciliation pass")
	}
}

func TestReconcileOneNoChangeYieldsEmptyDiff(t *testing.T) {
	withTempCacheHome(t)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	spec := RootSpec{Path: root}

	first := reconcileOne(spec)
	if first.Err != nil {
		t.Fatalf("first pass: %v", first.Err)
	}
	if first.Diff.Empty() {
		t.Fatalf("first pass diff = %+v, want non-empty (nothing existed before)", first.Diff)
	}
	if err := snapshot.Save(first.Shard); err != nil {
		t.Fatalf("Save: %v", err)
	}

	second := reconcileOne(spec)
	if second.Err != nil {
		t.Fatalf("second pass: %v", second.Err)
	}
	if !second.Diff.Empty() {
		t.Fatalf("second pass diff = %+v, want empty (nothing changed on disk)", second.Diff)
	}
}

// TestReconcileNoSnapshotWriteOnNoChangePass proves the mechanism
// cmd/scry/daemon.go's onResult gates its snapshot.Save call on:
// Result.Diff.Empty() must be true, and only true, when the tree really
// did not change, so a caller that skips saving on it never skips a save
// that mattered and never leaves a stale-but-different snapshot on disk.
func TestReconcileNoSnapshotWriteOnNoChangePass(t *testing.T) {
	withTempCacheHome(t)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	spec := RootSpec{Path: root}

	// Mirror what cmd/scry/daemon.go's onResult does: save only when the
	// diff is non-empty.
	save := func(res Result) bool {
		if res.Err != nil || res.Diff.Empty() {
			return false
		}
		if err := snapshot.Save(res.Shard); err != nil {
			t.Fatalf("Save: %v", err)
		}
		return true
	}

	first := reconcileOne(spec)
	if !save(first) {
		t.Fatalf("first pass: expected a save (no prior snapshot existed)")
	}

	path, err := snapshot.PathFor(root)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat snapshot after first save: %v", err)
	}

	second := reconcileOne(spec)
	if save(second) {
		t.Fatalf("second pass: expected no save (tree is unchanged)")
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat snapshot after second (no-op) pass: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("snapshot file changed on a no-change pass: before=%v/%d after=%v/%d",
			before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
}

func TestReconcileOneAppliesRealChanges(t *testing.T) {
	withTempCacheHome(t)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	spec := RootSpec{Path: root}

	first := reconcileOne(spec)
	if first.Err != nil {
		t.Fatalf("first pass: %v", first.Err)
	}
	if err := snapshot.Save(first.Shard); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("y"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	second := reconcileOne(spec)
	if second.Err != nil {
		t.Fatalf("second pass: %v", second.Err)
	}
	if second.Diff.Added != 1 || second.Diff.Removed != 0 || second.Diff.Changed != 0 {
		t.Fatalf("second pass diff = %+v, want {Added:1 Removed:0 Changed:0}", second.Diff)
	}
}

// TestReconcileGuardsAgainstEmptyFailedCrawl covers the data-loss case the
// design brief calls out explicitly: a root that goes offline (or a
// permission error that truncates the walk down to nothing) must not be
// mistaken for a root that was genuinely emptied out. crawler.Crawl
// reports this the same way for both a vanished root and a totally
// unreadable one — zero entries, at least one error — so this test
// exercises it by deleting the root's directory out from under the same
// path string that was previously snapshotted with real entries in it.
func TestReconcileGuardsAgainstEmptyFailedCrawl(t *testing.T) {
	withTempCacheHome(t)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	first := reconcileOne(RootSpec{Path: root})
	if first.Err != nil {
		t.Fatalf("first pass: %v", first.Err)
	}
	if first.Diff.Added == 0 {
		t.Fatalf("first pass diff = %+v, want at least one addition", first.Diff)
	}
	if err := snapshot.Save(first.Shard); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Simulate the root going offline / becoming unreadable: the
	// directory is gone, but the daemon still reconciles the same
	// configured path.
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	second := reconcileOne(RootSpec{Path: root})
	if second.Err == nil {
		t.Fatalf("second pass: want an error guarding against a mass-deletion diff, got nil (Diff=%+v)", second.Diff)
	}
	if !second.Diff.Empty() {
		t.Fatalf("second pass: Diff = %+v, want zero value (not computed) when the guard trips", second.Diff)
	}

	// The stored snapshot must be exactly what it was before: proof the
	// guard, applied the way onResult applies it, never gets a chance to
	// wipe out the last good index.
	restored, err := snapshot.Load(root)
	if err != nil {
		t.Fatalf("Load after guarded pass: %v", err)
	}
	if restored.CountIndexed() == 0 {
		t.Fatalf("snapshot has 0 entries after a guarded pass; the guard did not protect it")
	}
}

// TestReconcileDistinguishesGenuineEmptyFromFailedCrawl is the other half
// of the guard: a root that really was emptied out (no crawl errors) must
// still produce a real removal diff, not be swallowed by the same guard
// that protects against a failed crawl. If it were, deleting everything
// under a watched root for real would leave a phantom, never-updated
// index behind forever.
func TestReconcileDistinguishesGenuineEmptyFromFailedCrawl(t *testing.T) {
	withTempCacheHome(t)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	first := reconcileOne(RootSpec{Path: root})
	if first.Err != nil {
		t.Fatalf("first pass: %v", first.Err)
	}
	if err := snapshot.Save(first.Shard); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Genuinely empty the root, rather than removing it: the directory
	// still exists and is fully readable, it just has nothing in it.
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	second := reconcileOne(RootSpec{Path: root})
	if second.Err != nil {
		t.Fatalf("second pass: unexpected error for a genuinely empty root: %v", second.Err)
	}
	if second.Diff.Removed != 1 {
		t.Fatalf("second pass diff = %+v, want Removed:1 for a genuinely emptied root", second.Diff)
	}
}
