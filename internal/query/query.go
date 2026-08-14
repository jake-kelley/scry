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
	"scry/internal/qsyntax"
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

// SearchString parses s with qsyntax.Parse and runs Search against the
// result. It is a convenience wrapper for callers — the CLI, the socket
// handler — that only ever have a raw query string; anything that already
// has a qsyntax.Query (e.g. because it wants to reuse a parse across
// keystrokes) should call Search directly.
func SearchString(shards []*index.Shard, s string, limit int) ([]Result, error) {
	q, err := qsyntax.Parse(s)
	if err != nil {
		return nil, err
	}
	return Search(shards, q, limit), nil
}

// Search runs q against every online shard in shards in parallel and
// returns up to limit ranked results (limit <= 0 or limit > 200 is
// clamped to 200).
//
// Matching happens in two stages, per §5 and per qsyntax's own
// deterministic/fuzzy split (see internal/qsyntax's package doc):
//
//  1. Deterministic filters — root:, literal, glob, ext, path:, and negated
//     fuzzy terms — are evaluated first, because they are cheap and shrink
//     the candidate set before any scoring runs. root: filters out whole
//     shards before their arenas are ever scanned.
//  2. Fuzzy scoring runs only on survivors. Every positive fuzzy term in q
//     (q.FuzzyTerms()) must match (they are ANDed); a candidate's score is
//     the sum of each term's fuzzy.Score. A query with no fuzzy terms at
//     all (e.g. bare "ext:go") still returns every filter-matched entry,
//     each with score 0, in the tiebreaker order below.
//
// fuzzy.Score's own contract stops at the score and match positions; it
// deliberately leaves ties to the caller, so Search breaks them here, in
// descending priority:
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
func Search(shards []*index.Shard, q qsyntax.Query, limit int) []Result {
	limit = clampLimit(limit)

	fuzzyTerms := q.FuzzyTerms()
	fuzzyQueries := make([][]byte, len(fuzzyTerms))
	for i, t := range fuzzyTerms {
		fuzzyQueries[i] = fuzzy.Normalize(t)
	}

	rootSubstr, hasRootFilter := q.RootFilter()
	hasPathFilter := hasPathTerm(q)

	var wg sync.WaitGroup
	perShard := make([][]Result, len(shards))
	for i, s := range shards {
		if s == nil || !s.Online() {
			continue
		}
		if hasRootFilter && !strings.Contains(strings.ToLower(filepath.ToSlash(s.Root())), rootSubstr) {
			continue
		}
		wg.Add(1)
		go func(i int, s *index.Shard) {
			defer wg.Done()
			perShard[i] = searchShard(s, q, fuzzyQueries, hasPathFilter)
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

// hasPathTerm reports whether q has any path: term, positive or negated —
// Search only needs to reconstruct a candidate's full path (an allocation
// per candidate) when there is one to check it against.
func hasPathTerm(q qsyntax.Query) bool {
	for _, t := range q.Terms {
		if t.Kind == qsyntax.KindPath {
			return true
		}
	}
	return false
}

// clampLimit applies Search's default-and-cap rule to a caller-supplied
// limit.
func clampLimit(limit int) int {
	if limit <= 0 || limit > defaultLimit {
		return defaultLimit
	}
	return limit
}

// searchShard runs the filter-then-score match against one shard's name
// arena: deterministic filters over every NUL-separated name reject cheaply
// (and, for a positive root: match, we're already here), then fuzzy
// scoring runs on survivors.
func searchShard(s *index.Shard, q qsyntax.Query, fuzzyQueries [][]byte, hasPathFilter bool) []Result {
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

		// The arena already holds the lowercased name, so this string
		// conversion is exactly what qsyntax's Match* methods want — no
		// double lowering of anything but the ASCII case-fold itself,
		// which they do unconditionally regardless of input case.
		nameStr := string(name)
		if !q.MatchName(nameStr) {
			continue
		}

		var id uint32
		var path string
		if hasPathFilter {
			var ok bool
			id, ok = s.EntryAt(uint32(end - len(name)))
			if !ok {
				continue // stale offset: entry was tombstoned/reused since Arena() was taken
			}
			path = s.Path(id)
			if !q.MatchPath(path) {
				continue
			}
		}

		score := 0
		matched := true
		for _, fq := range fuzzyQueries {
			if !fuzzy.Filter(fq, name) {
				matched = false
				break
			}
			m, ok := fuzzy.Score(fq, name)
			if !ok {
				matched = false
				break
			}
			score += m.Score
		}
		if !matched {
			continue
		}

		if path == "" {
			var ok bool
			id, ok = s.EntryAt(uint32(end - len(name)))
			if !ok {
				continue
			}
			path = s.Path(id)
		}

		entry, ok := s.Get(id)
		if !ok {
			continue
		}

		out = append(out, Result{
			Path:  path,
			Name:  entry.Name,
			Score: score,
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
