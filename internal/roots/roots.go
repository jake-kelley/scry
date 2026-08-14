// Package roots normalizes and de-duplicates a set of user-configured search
// roots. It resolves each path to an absolute, symlink-free form and then
// collapses containment so that no kept root is nested inside another —
// indexing a root and one of its own descendants would otherwise double up
// every file beneath the descendant.
package roots

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// FoldCase controls whether path comparisons (in Contains and during
// Normalize's de-duplication step) are case-insensitive. It defaults to true
// on Windows and macOS, whose default filesystems (NTFS, APFS) are both
// case-insensitive, and false elsewhere. Tests may override it to exercise
// both behaviours regardless of the host OS.
var FoldCase = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// Warning describes a non-fatal issue found while normalizing a root.
type Warning struct {
	Path   string
	Reason string
}

// Contains reports whether parent and child refer to the same path, or child
// is a path beneath parent. Comparison is done on path-component boundaries:
// Contains("/a/b", "/a/bc") is false even though "/a/bc" has the string
// "/a/b" as a prefix. Case folding follows FoldCase.
func Contains(parent, child string) bool {
	p := filepath.Clean(parent)
	c := filepath.Clean(child)
	if FoldCase {
		p = strings.ToLower(p)
		c = strings.ToLower(c)
	}
	if p == c {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(p, sep) {
		p += sep
	}
	return strings.HasPrefix(c, p)
}

// entry is a candidate root partway through normalization, carrying the
// path it will be kept or reported under.
type entry struct {
	path string
}

// foldKey returns the string used to compare two paths for equality,
// respecting FoldCase.
func foldKey(path string) string {
	if FoldCase {
		return strings.ToLower(path)
	}
	return path
}

// Normalize cleans, resolves, and de-duplicates paths, then collapses
// containment so that no kept root contains another. kept is returned in
// stable sorted order. warnings reports, for every path that needed special
// handling, what happened and why.
func Normalize(paths []string) (kept []string, warnings []Warning) {
	var candidates []entry

	for _, raw := range paths {
		cleaned := filepath.Clean(raw)

		abs := cleaned
		if a, err := filepath.Abs(cleaned); err == nil {
			abs = a
		} else {
			warnings = append(warnings, Warning{
				Path:   cleaned,
				Reason: fmt.Sprintf("could not make path absolute: %v", err),
			})
		}

		resolved := abs
		existed := true
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			resolved = r
		} else {
			existed = false
			if os.IsNotExist(err) {
				warnings = append(warnings, Warning{
					Path:   abs,
					Reason: "path does not exist",
				})
			} else {
				warnings = append(warnings, Warning{
					Path:   abs,
					Reason: fmt.Sprintf("could not resolve symlinks: %v", err),
				})
			}
			resolved = abs
		}

		if existed {
			info, err := os.Stat(resolved)
			if err != nil {
				// Raced out of existence between EvalSymlinks and Stat;
				// keep it, same as any other missing path.
				warnings = append(warnings, Warning{
					Path:   resolved,
					Reason: "path does not exist",
				})
			} else if !info.IsDir() {
				warnings = append(warnings, Warning{
					Path:   resolved,
					Reason: "not a directory",
				})
				continue
			}
		}

		candidates = append(candidates, entry{path: resolved})
	}

	// Drop exact duplicates, keeping the first-seen form.
	seen := make(map[string]string) // foldKey -> kept path
	var deduped []entry
	for _, c := range candidates {
		key := foldKey(c.path)
		if orig, ok := seen[key]; ok {
			warnings = append(warnings, Warning{
				Path:   c.path,
				Reason: fmt.Sprintf("duplicate of already-configured root %s", orig),
			})
			continue
		}
		seen[key] = c.path
		deduped = append(deduped, c)
	}

	// Collapse containment: shallower paths are strictly shorter strings
	// than any path they contain, so processing in ascending length order
	// guarantees a parent is already in kept by the time its descendants
	// are considered, however deeply nested.
	sort.SliceStable(deduped, func(i, j int) bool {
		if len(deduped[i].path) != len(deduped[j].path) {
			return len(deduped[i].path) < len(deduped[j].path)
		}
		return deduped[i].path < deduped[j].path
	})

	var final []entry
	for _, c := range deduped {
		absorbedBy := ""
		for _, k := range final {
			if Contains(k.path, c.path) {
				absorbedBy = k.path
				break
			}
		}
		if absorbedBy != "" {
			warnings = append(warnings, Warning{
				Path:   c.path,
				Reason: fmt.Sprintf("contained within existing root %s", absorbedBy),
			})
			continue
		}
		final = append(final, c)
	}

	kept = make([]string, len(final))
	for i, c := range final {
		kept[i] = c.path
	}
	sort.Strings(kept)

	return kept, warnings
}
