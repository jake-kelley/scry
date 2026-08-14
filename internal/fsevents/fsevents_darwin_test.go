//go:build darwin

package fsevents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitForEventMentioning drains s until an event whose Path contains want
// arrives, or the deadline passes. It returns the matching event.
//
// Events are matched on a path substring rather than equality because
// FSEvents reports the volume's real path: a directory handed out as
// /var/folders/... is reported as /private/var/folders/..., and coalescing
// can report a parent directory rather than the file itself.
func waitForEventMentioning(t *testing.T, s Stream, want string, timeout time.Duration) Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("event channel closed before an event mentioning %q arrived", want)
			}
			if strings.Contains(ev.Path, want) {
				return ev
			}
		case <-deadline:
			t.Fatalf("no event mentioning %q within %s", want, timeout)
		}
	}
}

// TestStreamDeliversRealFileEvent is the only test that exercises the cgo
// layer against the actual kernel: everything else in this package tests
// pure-Go helpers, and internal/watcher tests its logic against a fake
// stream. Without this, a broken CFRunLoop callback or a botched
// C-string-to-Go conversion would pass the whole suite and only show up
// when a human ran the daemon.
func TestStreamDeliversRealFileEvent(t *testing.T) {
	// FSEvents reports the fully resolved path, and on darwin t.TempDir()
	// hands back a /var symlink into /private/var.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	s, err := NewStream(Config{
		Paths:      []string{dir},
		Latency:    10 * time.Millisecond,
		FileEvents: true,
		WatchRoot:  true,
		NoDefer:    true,
	})
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	defer s.Stop()

	// The stream is scheduled on its run loop asynchronously, so a file
	// created immediately can land before the watch is live. Write in a
	// loop until an event shows up rather than sleeping a fixed guess.
	const name = "fsevents-probe.txt"
	target := filepath.Join(dir, name)
	stopWriting := make(chan struct{})
	defer close(stopWriting)
	go func() {
		for {
			select {
			case <-stopWriting:
				return
			default:
			}
			os.WriteFile(target, []byte("x"), 0o600)
			time.Sleep(100 * time.Millisecond)
		}
	}()

	ev := waitForEventMentioning(t, s, name, 30*time.Second)
	if ev.ID == 0 {
		t.Error("event ID is 0; callers persist this as a resume point, so it must be real")
	}
	if !ev.Flags.Has(FlagItemIsFile) {
		t.Errorf("Flags = %#x, want FlagItemIsFile set (FileEvents was requested)", ev.Flags)
	}
}

// TestLatestEventIDIsMonotonic pins the resume-point source. A LatestEventID
// that returned 0 or went backwards would make a daemon restart either
// replay the entire journal or silently skip changes.
func TestLatestEventIDIsMonotonic(t *testing.T) {
	first, err := LatestEventID()
	if err != nil {
		t.Fatalf("LatestEventID: %v", err)
	}
	if first == 0 {
		t.Fatal("LatestEventID = 0 on a running system, want a real host-global id")
	}

	// Generate some filesystem activity, then confirm the counter has not
	// gone backwards. It is host-global, so it may or may not advance from
	// this test's writes alone -- only regression is a defect.
	dir := t.TempDir()
	for i := range 5 {
		if err := os.WriteFile(filepath.Join(dir, string(rune('a'+i))), []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	time.Sleep(200 * time.Millisecond)

	second, err := LatestEventID()
	if err != nil {
		t.Fatalf("LatestEventID: %v", err)
	}
	if second < first {
		t.Errorf("LatestEventID went backwards: %d then %d", first, second)
	}
}
