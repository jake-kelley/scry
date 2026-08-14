package watcher

import (
	"io/fs"
	"path/filepath"
	"strings"

	"scry/internal/crawler"
	"scry/internal/index"
)

// rescanSubtree implements §6's MustScanSubDirs handling: re-enumerate the
// subtree rooted at dirPath on disk, and diff it against shard's existing
// children maps, exactly as the design doc frames it — "this is the only
// genuinely subtle correctness surface in the project."
//
// Every directory the walk visits (dirPath itself and every live
// descendant directory) gets its post-walk child set compared against
// shard.Children: anything shard still has that the walk didn't see gets
// removed (recursively, via Shard.Remove); anything new gets upserted.
// Both directions are idempotent, so re-running a rescan (e.g. from a
// duplicate event, or one replayed after a restart) converges to the same
// state rather than accumulating drift.
func rescanSubtree(shard *index.Shard, r Root, dirPath string) error {
	dirID, ok := ensurePath(shard, r.Path, dirPath)
	if !ok {
		// dirPath itself is gone or unreachable: whatever the shard has
		// under it is stale, and Remove tombstones the whole subtree.
		removeIfPresent(shard, r.Path, dirPath)
		return nil
	}

	// seen[parentID] holds the set of child ids the fresh walk found
	// under parentID. A directory that turns out to have no surviving
	// children still gets an entry (possibly empty) so its diff below
	// still runs and clears out anything stale.
	seen := map[uint32]map[uint32]bool{dirID: {}}
	dirIDs := map[string]uint32{dirPath: dirID}

	err := filepath.WalkDir(dirPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p == dirPath {
			return nil
		}

		name := d.Name()
		hidden := strings.HasPrefix(name, ".")
		if !r.Opts.Hidden && hidden {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if crawler.MatchesExclude(name, r.Opts.Excludes, r.Opts.Globs) ||
			crawler.MatchesExcludePath(p, r.Opts.ExcludePaths) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		parentID, ok := dirIDs[filepath.Dir(p)]
		if !ok {
			// Should not happen given WalkDir's traversal order; skip
			// defensively rather than mis-parent the entry.
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		info, ierr := d.Info()
		if ierr != nil {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		var flags index.Flags
		if hidden {
			flags |= index.FlagHidden
		}
		if d.Type()&fs.ModeSymlink != 0 {
			flags |= index.FlagSymlink
		}

		var id uint32
		if d.IsDir() {
			flags |= index.FlagDir
			id = shard.Upsert(parentID, name, flags, 0, info.ModTime().UnixNano())
			dirIDs[p] = id
			if seen[id] == nil {
				seen[id] = map[uint32]bool{}
			}
		} else {
			id = shard.Upsert(parentID, name, flags, info.Size(), info.ModTime().UnixNano())
		}

		if seen[parentID] == nil {
			seen[parentID] = map[uint32]bool{}
		}
		seen[parentID][id] = true
		return nil
	})
	if err != nil {
		return err
	}

	for parentID, keep := range seen {
		for _, childID := range shard.Children(parentID) {
			if !keep[childID] {
				shard.Remove(childID)
			}
		}
	}
	return nil
}
