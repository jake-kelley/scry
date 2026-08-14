// Package query implements fuzzy search across one or more index.Shard
// values, per "everything-macos-design.md" §5.
package query

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"scry/internal/fuzzy"
	"scry/internal/index"
)

// defaultLimit is used when the caller passes limit <= 0, and is also the
// hard cap applied regardless of what the caller asks for: "nobody reads
// result 900" (design doc §5).
const defaultLimit = 200

// Result is one ranked search hit.
type Result struct {
	Path  string
	Name  string
	Score int
	IsDir bool
	Size  int64
	MTime int64 // unix nanos
}

// Search runs q against every online shard in shards in parallel and
// returns up to limit ranked results (limit <= 0 or limit > 200 is
// clamped to 200).
//
// Each shard is matched in two passes, per §5: fuzzy.Filter walks the
// shard's name arena to cheaply reject non-candidates, then fuzzy.Score
// runs only on survivors. fuzzy.Score's own contract stops at the score
// and match positions; it deliberately leaves ties to the caller, so
// Search breaks them here, in descending priority:
//
//  1. Score (higher first) — the fuzzy match quality itself.
//  2. Shorter name — an exact short name beats a long one containing the
//     same match.
//  3. More recent MTime — a file touched recently is more likely to be
//     the one being searched for.
//  4. Shallower path (fewer path separators) — a top-level file beats one
//     buried several directories down.
//  5. Path, lexically — a final deterministic tiebreak so result order
//     never depends on goroutine scheduling.
func Search(shards []*index.Shard, q string, limit int) []Result {
	limit = clampLimit(limit)

	query := fuzzy.Normalize(q)

	var wg sync.WaitGroup
	perShard := make([][]Result, len(shards))
	for i, s := range shards {
		if s == nil || !s.Online() {
			continue
		}
		wg.Add(1)
		go func(i int, s *index.Shard) {
			defer wg.Done()
			perShard[i] = searchShard(s, query)
		}(i, s)
	}
	wg.Wait()

	var merged []Result
	for _, rs := range perShard {
		merged = append(merged, rs...)
	}

	sort.Slice(merged, func(i, j int) bool {
		return less(merged[i], merged[j])
	})

	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// clampLimit applies Search's default-and-cap rule to a caller-supplied
// limit.
func clampLimit(limit int) int {
	if limit <= 0 || limit > defaultLimit {
		return defaultLimit
	}
	return limit
}

// searchShard runs the two-pass match against one shard's name arena:
// Filter over every NUL-separated name to reject cheaply, then Score on
// survivors.
func searchShard(s *index.Shard, query []byte) []Result {
	arena := s.Arena()

	var out []Result
	start := 0
	for start < len(arena) {
		end := start
		for end < len(arena) && arena[end] != 0 {
			end++
		}
		name := arena[start:end]
		start = end + 1

		if len(name) == 0 {
			continue // the shard's own root entry has an empty arena name
		}
		if !fuzzy.Filter(query, name) {
			continue
		}
		match, ok := fuzzy.Score(query, name)
		if !ok {
			continue
		}

		id, ok := s.EntryAt(uint32(end - len(name)))
		if !ok {
			continue // stale offset: entry was tombstoned/reused since Arena() was taken
		}
		entry, ok := s.Get(id)
		if !ok {
			continue
		}

		out = append(out, Result{
			Path:  s.Path(id),
			Name:  entry.Name,
			Score: match.Score,
			IsDir: entry.IsDir,
			Size:  entry.Size,
			MTime: entry.MTime,
		})
	}
	return out
}

// less implements the tiebreak ordering documented on Search.
func less(a, b Result) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if len(a.Name) != len(b.Name) {
		return len(a.Name) < len(b.Name)
	}
	if a.MTime != b.MTime {
		return a.MTime > b.MTime
	}
	da, db := depth(a.Path), depth(b.Path)
	if da != db {
		return da < db
	}
	return a.Path < b.Path
}

// depth counts path separators, used as a cheap proxy for how deeply
// nested a path is.
func depth(path string) int {
	return strings.Count(filepath.ToSlash(path), "/")
}
