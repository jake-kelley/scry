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
