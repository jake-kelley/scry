// Package crawler builds an index.Shard for one root by walking its
// directory tree, as described in "everything-macos-design.md" §10, step 1:
// a naive filepath.WalkDir crawler is the deliberate starting point.
// getattrlistbulk-style bulk enumeration is a later, measured optimization,
// not something to reach for here.
package crawler

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"scry/internal/index"
)

// Options controls what a Crawl skips and how it treats symlinks and
// dotfiles.
type Options struct {
	Excludes       []string // directory/file base names to skip entirely
	Globs          []string // filepath.Match patterns (against base name) to skip
	FollowSymlinks bool     // if false (default), symlinks are indexed as leaves, never descended into
	Hidden         bool     // if false (default), dotfiles and dot-directories are skipped
}

// Stats summarizes one Crawl call.
type Stats struct {
	Entries  int // files and directories added to the shard (root not counted)
	Dirs     int // subset of Entries that are directories
	Skipped  int // entries excluded by name, glob, or hidden rules
	Errors   int // individual entries or directories that could not be read
	Duration time.Duration
}

// Crawl walks root and returns a freshly populated index.Shard plus stats
// about the walk. A permission error on an individual entry increments
// Stats.Errors and the walk continues — on macOS a TCC denial looks exactly
// like this, and a crawl must never abort because one directory was
// unreadable.
func Crawl(root string, opts Options) (*index.Shard, Stats, error) {
	start := time.Now()

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, Stats{}, err
	}

	shard := index.New(absRoot)
	var stats Stats

	// dirIDs maps a directory's path (as passed to WalkDir) to its shard
	// id, so a child entry can look up its parent. WalkDir visits a
	// directory before any of its children, so a parent is always present
	// by the time a child is walked.
	dirIDs := map[string]uint32{absRoot: shard.RootID()}

	// visited guards against symlink cycles when FollowSymlinks is set,
	// keyed by the canonical (fully resolved) path of a followed
	// directory symlink's target.
	visited := map[string]bool{absRoot: true}

	var doWalk func(walkRoot string) error
	doWalk = func(walkRoot string) error {
		return filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				stats.Errors++
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}

			if path == walkRoot {
				// The root of this (sub-)walk is already represented in
				// the shard: either the shard's own root entry, or the
				// directory id installed for a followed symlink before
				// recursing into it.
				return nil
			}

			name := d.Name()
			isDir := d.IsDir()
			hidden := strings.HasPrefix(name, ".")

			if !opts.Hidden && hidden {
				stats.Skipped++
				if isDir {
					return fs.SkipDir
				}
				return nil
			}
			if matchesExclude(name, opts.Excludes, opts.Globs) {
				stats.Skipped++
				if isDir {
					return fs.SkipDir
				}
				return nil
			}

			parentID, ok := dirIDs[filepath.Dir(path)]
			if !ok {
				// Should not happen given WalkDir's traversal order; skip
				// defensively rather than mis-parent the entry.
				stats.Errors++
				if isDir {
					return fs.SkipDir
				}
				return nil
			}

			info, ierr := d.Info()
			if ierr != nil {
				stats.Errors++
				if isDir {
					return fs.SkipDir
				}
				return nil
			}

			var flags index.Flags
			if hidden {
				flags |= index.FlagHidden
			}
			isSymlink := d.Type()&fs.ModeSymlink != 0
			if isSymlink {
				flags |= index.FlagSymlink
			}

			switch {
			case isSymlink:
				// filepath.WalkDir never treats a symlink dirent as a
				// directory, so by default it is simply indexed as a
				// leaf and never descended into. Only when
				// FollowSymlinks is set do we resolve it and, if it
				// points at a directory, recurse into that target
				// ourselves.
				if opts.FollowSymlinks {
					if target, terr := filepath.EvalSymlinks(path); terr == nil {
						if tinfo, serr := os.Stat(target); serr == nil && tinfo.IsDir() {
							id := shard.Upsert(parentID, name, flags|index.FlagDir, 0, tinfo.ModTime().UnixNano())
							stats.Entries++
							stats.Dirs++
							if visited[target] {
								stats.Skipped++
								return nil
							}
							visited[target] = true
							dirIDs[target] = id
							if werr := doWalk(target); werr != nil {
								stats.Errors++
							}
							return nil
						}
					}
				}
				shard.Upsert(parentID, name, flags, info.Size(), info.ModTime().UnixNano())
				stats.Entries++
				return nil

			case isDir:
				flags |= index.FlagDir
				id := shard.Upsert(parentID, name, flags, 0, info.ModTime().UnixNano())
				dirIDs[path] = id
				stats.Entries++
				stats.Dirs++
				return nil

			default:
				shard.Upsert(parentID, name, flags, info.Size(), info.ModTime().UnixNano())
				stats.Entries++
				return nil
			}
		})
	}

	if werr := doWalk(absRoot); werr != nil {
		stats.Duration = time.Since(start)
		return shard, stats, werr
	}

	stats.Duration = time.Since(start)
	return shard, stats, nil
}

// matchesExclude reports whether name should be skipped: an exact match
// against excludes, or a filepath.Match hit against any of globs.
func matchesExclude(name string, excludes, globs []string) bool {
	for _, e := range excludes {
		if name == e {
			return true
		}
	}
	for _, g := range globs {
		if ok, err := filepath.Match(g, name); err == nil && ok {
			return true
		}
	}
	return false
}
