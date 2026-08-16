package watcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"scry/internal/crawler"
	"scry/internal/index"
)

// TestStatMeansGone pins the rule that cost a real index: only an error
// that actually means "not there" may be treated as a deletion. A macOS TCC
// revocation (which happens whenever the app's signing identity changes)
// makes ~/Desktop, ~/Documents and ~/Downloads lstat with EPERM, and the
// FSEvents history replay on every daemon start walks straight into them.
func TestStatMeansGone(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"ErrNotExist means gone", fs.ErrNotExist, true},
		{"a wrapped ErrNotExist means gone", fmt.Errorf("lstat: %w", fs.ErrNotExist), true},
		{"a real PathError from a missing file means gone",
			&fs.PathError{Op: "lstat", Path: "/nope", Err: syscall.ENOENT}, true},

		{"ErrPermission does NOT mean gone", fs.ErrPermission, false},
		{"a TCC-shaped EPERM does NOT mean gone",
			&fs.PathError{Op: "lstat", Path: "/Users/example/Desktop/a.txt", Err: syscall.EPERM}, false},
		{"EACCES does NOT mean gone",
			&fs.PathError{Op: "lstat", Path: "/Users/example/Documents", Err: syscall.EACCES}, false},
		{"an unclassified error does NOT mean gone", errors.New("i/o error"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := statMeansGone(tc.err); got != tc.want {
				t.Errorf("statMeansGone(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestApplyPathEventKeepsEntryWhenUnreadable is the behavioural half: an
// lstat failure that is not a "gone" error must leave the shard untouched
// and report the error, rather than tombstoning the entry.
//
// Producing a genuine EPERM portably is not possible (root ignores mode
// bits, and Windows has no equivalent), so this drives applyPathEvent
// against a path whose parent is a regular file. That is ENOTDIR on Unix
// and ERROR_PATH_NOT_FOUND on Windows — the point being only that it is an
// error the old code would have deleted on, whatever it maps to. If it does
// map to ErrNotExist on this platform, deletion is correct and the test
// says so instead of failing for the wrong reason.
func TestApplyPathEventKeepsEntryWhenUnreadable(t *testing.T) {
	root := t.TempDir()
	notADir := filepath.Join(root, "file.txt")
	mustWrite(t, notADir, "x")
	victim := filepath.Join(notADir, "child.txt")

	_, statErr := os.Lstat(victim)
	if statErr == nil {
		t.Fatalf("setup: expected lstat %s to fail", victim)
	}
	if statMeansGone(statErr) {
		t.Skipf("on this platform lstat under a file reports %v, which correctly means gone", statErr)
	}

	// Index the victim path by hand, as a prior crawl would have.
	shard := index.New(root)
	parent := shard.Upsert(shard.RootID(), "file.txt", index.FlagDir, 0, 0)
	shard.Upsert(parent, "child.txt", 0, 1, 0)
	before := shard.Len()

	r := Root{Path: root, Opts: crawler.Options{}}
	err := applyPathEvent(shard, r, victim)

	if err == nil {
		t.Fatalf("applyPathEvent returned nil for an unreadable path; the caller needs the error to log it")
	}
	if _, found := shard.Lookup(parent, "child.txt"); !found {
		t.Errorf("entry was removed because the path could not be read — this is the bug that "+
			"took a 43k-entry index down to 14k (lstat said %v, which is not evidence of deletion)", statErr)
	}
	if got := shard.Len(); got != before {
		t.Errorf("shard.Len() = %d, want %d (unchanged)", got, before)
	}
}

// TestApplyPathEventStillRemovesWhenActuallyGone guards the other
// direction: making the permission case safe must not stop real deletions
// from being applied.
func TestApplyPathEventStillRemovesWhenActuallyGone(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(root, "deleted.txt")

	shard := index.New(root)
	shard.Upsert(shard.RootID(), "deleted.txt", 0, 1, 0)

	r := Root{Path: root, Opts: crawler.Options{}}
	if err := applyPathEvent(shard, r, gone); err != nil {
		t.Fatalf("applyPathEvent on a genuinely missing path returned %v, want nil", err)
	}
	if _, found := shard.Lookup(shard.RootID(), "deleted.txt"); found {
		t.Errorf("a genuinely deleted file stayed indexed")
	}
}
