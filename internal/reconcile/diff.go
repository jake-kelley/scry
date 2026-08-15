package reconcile

import "scry/internal/index"

// Diff summarizes what changed between two shards for the same root: the
// counts of entries present in the new crawl but not the old (Added),
// present in the old but not the new (Removed), and present in both but
// different (Changed). It says nothing about which entries — reconcile
// passes run over whole home directories, so a path-by-path report is not
// something anyone wants in a log line; the counts are.
type Diff struct {
	Added   int
	Removed int
	Changed int
}

// Empty reports whether the diff found no differences at all: the case
// that lets a reconcile pass skip the snapshot write and the live shard
// swap entirely.
func (d Diff) Empty() bool {
	return d.Added == 0 && d.Removed == 0 && d.Changed == 0
}

// entrySnapshot is the subset of index.Entry the "changed" comparison
// looks at.
type entrySnapshot struct {
	isDir bool
	size  int64
	mtime int64
}

// diffShards compares old and new, which must be shards of the same root,
// and reports what changed between them.
//
// "Changed" is decided by size and mtime (unix nanos, exactly as the
// crawler recorded them) plus the file/directory bit: an entry present at
// the same relative path on both sides is unchanged only if all three
// match, changed otherwise. This is the same signal a stat(2) gives
// FSEvents-driven updates elsewhere in this codebase (see
// internal/watcher's applyPathEvent) — it costs nothing beyond what the
// crawl already collected, since neither side needs to be re-read.
//
// What it can miss: a file rewritten in place with the same size, whose
// mtime lands within the filesystem's mtime resolution of the original
// (one second on some setups), is indistinguishable from an untouched
// file and will not be reported. Content hashing would catch that, at the
// cost of reading every file on every reconcile pass — a cost this
// package's doc comment already rejects for the crawl itself. A directory
// entry's own size is always 0 and its own mtime frequently does not move
// when a child is added, removed, or renamed underneath it, so a
// directory is never reported "changed" for that reason alone; the
// added/removed/changed entries for its children are what surface a
// change there.
func diffShards(old, new *index.Shard) Diff {
	oldEntries := snapshotEntries(old)
	newEntries := snapshotEntries(new)

	var d Diff
	for rel, ne := range newEntries {
		oe, existed := oldEntries[rel]
		switch {
		case !existed:
			d.Added++
		case oe != ne:
			d.Changed++
		}
	}
	for rel := range oldEntries {
		if _, ok := newEntries[rel]; !ok {
			d.Removed++
		}
	}
	return d
}

// snapshotEntries walks every live entry in s and returns a map keyed by
// path relative to s's root (e.g. "sub/file.txt"), built from parent/child
// links rather than s.Path, so it never depends on how s's root string
// happens to be formatted. The root entry itself is not included — it has
// no name and reconcile has nothing meaningful to say about it changing.
func snapshotEntries(s *index.Shard) map[string]entrySnapshot {
	m := make(map[string]entrySnapshot)
	var walk func(id uint32, rel string)
	walk = func(id uint32, rel string) {
		for _, childID := range s.Children(id) {
			e, ok := s.Get(childID)
			if !ok {
				continue
			}
			childRel := e.Name
			if rel != "" {
				childRel = rel + "/" + e.Name
			}
			m[childRel] = entrySnapshot{isDir: e.IsDir, size: e.Size, mtime: e.MTime}
			if e.IsDir {
				walk(childID, childRel)
			}
		}
	}
	walk(s.RootID(), "")
	return m
}
