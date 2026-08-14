// Package snapshot persists index.Shard values to disk as described in
// "everything-macos-design.md" §4 and §7 item 2: one file per shard, at
// <cache dir>/shards/<hash of root>.idx, written atomically so a crash
// mid-write never clobbers a good snapshot with a partial one.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"scry/internal/config"
	"scry/internal/index"
)

// hashPrefixLen is the number of hex characters (from a sha256 digest) used
// to name a shard file. 16 hex chars is 64 bits of the digest — comfortably
// collision-free for the number of roots any one machine will ever have,
// while keeping filenames short.
const hashPrefixLen = 16

// fileExt is the extension used for shard snapshot files.
const fileExt = ".idx"

// Dir returns the directory shard snapshots live in, creating it if
// necessary: <config.CacheDir()>/shards.
func Dir() (string, error) {
	cacheDir, err := config.CacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cacheDir, "shards")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("snapshot: dir: %w", err)
	}
	return dir, nil
}

// PathFor returns the snapshot file path for root: <Dir()>/<hash>.idx. The
// hash is stable across runs and, on filesystems where paths are
// case-insensitive (Windows, macOS), case-folded first so the same root
// always maps to the same file regardless of how it was typed.
func PathFor(root string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hashName(root)+fileExt), nil
}

// hashName returns the filename stem (no extension) for root.
func hashName(root string) string {
	key := filepath.Clean(root)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		key = strings.ToLower(key)
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:hashPrefixLen]
}

// Save writes s to its snapshot file atomically: a temp file is written in
// the same directory as the final path, fsynced, renamed over the final
// path, and then the directory itself is fsynced so the rename is durable
// too. A crash at any point before the rename leaves the existing snapshot
// (if any) untouched; a crash after the rename leaves the new one intact.
// Nothing in between is ever observable from outside this function.
func Save(s *index.Shard) error {
	path, err := PathFor(s.Root())
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".snapshot-*.tmp")
	if err != nil {
		return fmt.Errorf("snapshot: save %s: %w", path, err)
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	if _, err := s.WriteTo(tmp); err != nil {
		return fmt.Errorf("snapshot: save %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("snapshot: save %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("snapshot: save %s: %w", path, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("snapshot: save %s: %w", path, err)
	}
	success = true

	// Best effort: fsync the directory so the rename itself survives a
	// crash. Not all platforms support fsync-ing a directory handle (most
	// notably Windows); the write above is already durable and atomic
	// without it, so a failure here is not fatal to the on-disk guarantee
	// this function makes, just to how quickly the rename becomes durable
	// on platforms that support it.
	syncDir(dir)

	return nil
}

// syncDir attempts to fsync a directory so a preceding rename inside it is
// durable, ignoring any error: unsupported on some platforms (Windows), and
// never something the caller should treat as a save failure.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// Load reads root's snapshot file and reconstructs its Shard. A missing
// file, a corrupt file, or one written by a different format version all
// return an error wrapping index.ErrStale — the caller's correct response
// is to recrawl this one root, not to treat the whole app as broken.
func Load(root string) (*index.Shard, error) {
	path, err := PathFor(root)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("snapshot: load %s: %w", path, index.ErrStale)
		}
		return nil, fmt.Errorf("snapshot: load %s: %w", path, err)
	}
	defer f.Close()

	shard, err := index.ReadShard(f)
	if err != nil {
		return nil, fmt.Errorf("snapshot: load %s: %w", path, err)
	}
	return shard, nil
}

// Remove deletes root's snapshot file, if any. Removing a snapshot that
// does not exist is not an error.
func Remove(root string) error {
	path, err := PathFor(root)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("snapshot: remove %s: %w", path, err)
	}
	return nil
}

// List returns the roots that currently have a snapshot file, by reading
// each file's header. Order is unspecified but stable for a given
// directory contents (sorted by filename).
func List() ([]string, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("snapshot: list: %w", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != fileExt {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var roots []string
	for _, name := range names {
		s, err := loadFile(filepath.Join(dir, name))
		if err != nil {
			// A corrupt or unreadable snapshot file costs its own root,
			// never the rest of List's results.
			continue
		}
		roots = append(roots, s.Root())
	}
	return roots, nil
}

// loadFile reads and parses a shard snapshot at an exact path, used by List
// which discovers files by name rather than by hashing a known root.
func loadFile(path string) (*index.Shard, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return index.ReadShard(f)
}
