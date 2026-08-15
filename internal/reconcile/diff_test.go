package reconcile

import (
	"testing"

	"scry/internal/index"
)

// build constructs a shard for root with the given entries, each a
// slash-separated relative path under a single top-level directory
// ("dir") plus its own top-level files, so tests can express a small tree
// concisely. size and mtime are per-entry; every entry not a directory
// gets FlagDir cleared.
type fileSpec struct {
	path  string
	isDir bool
	size  int64
	mtime int64
}

func buildShard(t *testing.T, root string, specs []fileSpec) *index.Shard {
	t.Helper()
	s := index.New(root)
	dirIDs := map[string]uint32{"": s.RootID()}
	for _, spec := range specs {
		parts := splitPath(spec.path)
		parentKey := ""
		for i, part := range parts {
			key := part
			if parentKey != "" {
				key = parentKey + "/" + part
			}
			last := i == len(parts)-1
			flags := index.Flags(0)
			size, mtime := int64(0), int64(0)
			if last && spec.isDir {
				flags |= index.FlagDir
			} else if !last {
				flags |= index.FlagDir
			}
			if last {
				size, mtime = spec.size, spec.mtime
			}
			if _, ok := dirIDs[key]; ok {
				parentKey = key
				continue
			}
			parentID, ok := dirIDs[parentKey]
			if !ok {
				t.Fatalf("no parent for %q", key)
			}
			id := s.Upsert(parentID, part, flags, size, mtime)
			if !last || spec.isDir {
				dirIDs[key] = id
			}
			parentKey = key
		}
	}
	return s
}

func splitPath(p string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			parts = append(parts, p[start:i])
			start = i + 1
		}
	}
	parts = append(parts, p[start:])
	return parts
}

func TestDiffShardsNoChange(t *testing.T) {
	root := "/root"
	specs := []fileSpec{
		{path: "a.txt", size: 10, mtime: 100},
		{path: "dir", isDir: true},
		{path: "dir/b.txt", size: 20, mtime: 200},
	}
	old := buildShard(t, root, specs)
	new := buildShard(t, root, specs)

	d := diffShards(old, new)
	if !d.Empty() {
		t.Fatalf("diffShards() = %+v, want empty for identical trees", d)
	}
}

func TestDiffShardsAdditions(t *testing.T) {
	old := buildShard(t, "/root", []fileSpec{
		{path: "a.txt", size: 10, mtime: 100},
	})
	new := buildShard(t, "/root", []fileSpec{
		{path: "a.txt", size: 10, mtime: 100},
		{path: "b.txt", size: 5, mtime: 50},
		{path: "dir", isDir: true},
		{path: "dir/c.txt", size: 1, mtime: 1},
	})

	d := diffShards(old, new)
	if d.Added != 3 || d.Removed != 0 || d.Changed != 0 {
		t.Fatalf("diffShards() = %+v, want {Added:3 Removed:0 Changed:0}", d)
	}
}

func TestDiffShardsRemovals(t *testing.T) {
	old := buildShard(t, "/root", []fileSpec{
		{path: "a.txt", size: 10, mtime: 100},
		{path: "dir", isDir: true},
		{path: "dir/b.txt", size: 20, mtime: 200},
	})
	new := buildShard(t, "/root", []fileSpec{
		{path: "a.txt", size: 10, mtime: 100},
	})

	d := diffShards(old, new)
	if d.Added != 0 || d.Removed != 2 || d.Changed != 0 {
		t.Fatalf("diffShards() = %+v, want {Added:0 Removed:2 Changed:0}", d)
	}
}

func TestDiffShardsModifications(t *testing.T) {
	old := buildShard(t, "/root", []fileSpec{
		{path: "a.txt", size: 10, mtime: 100},
		{path: "b.txt", size: 20, mtime: 200},
	})
	new := buildShard(t, "/root", []fileSpec{
		{path: "a.txt", size: 999, mtime: 100}, // size changed
		{path: "b.txt", size: 20, mtime: 999},  // mtime changed
	})

	d := diffShards(old, new)
	if d.Added != 0 || d.Removed != 0 || d.Changed != 2 {
		t.Fatalf("diffShards() = %+v, want {Added:0 Removed:0 Changed:2}", d)
	}
}

func TestDiffShardsEmptyToPopulated(t *testing.T) {
	old := buildShard(t, "/root", nil)
	new := buildShard(t, "/root", []fileSpec{
		{path: "a.txt", size: 10, mtime: 100},
		{path: "dir", isDir: true},
	})

	d := diffShards(old, new)
	if d.Added != 2 || d.Removed != 0 || d.Changed != 0 {
		t.Fatalf("diffShards() = %+v, want {Added:2 Removed:0 Changed:0}", d)
	}
	if d.Empty() {
		t.Fatalf("diffShards() reported Empty() for a tree that went from nothing to two entries")
	}
}

func TestDiffShardsPopulatedToGenuinelyEmpty(t *testing.T) {
	// This is the diff-level behavior: a shard with nothing errored
	// genuinely has nothing in it, and that must show up as removals,
	// not be silently ignored. The crawl-failure case that must NOT
	// look like this is exercised at the reconcileOne level in
	// reconcile_test.go, since diffShards has no visibility into
	// crawler.Stats at all — that guard lives one level up.
	old := buildShard(t, "/root", []fileSpec{
		{path: "a.txt", size: 10, mtime: 100},
		{path: "b.txt", size: 20, mtime: 200},
	})
	new := buildShard(t, "/root", nil)

	d := diffShards(old, new)
	if d.Added != 0 || d.Removed != 2 || d.Changed != 0 {
		t.Fatalf("diffShards() = %+v, want {Added:0 Removed:2 Changed:0}", d)
	}
}
