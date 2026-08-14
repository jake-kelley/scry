// Package fuzzy implements a two-pass fuzzy matcher for filenames.
//
// This runs on every keystroke against up to ~250k names, so it is split
// into a cheap filter pass and an expensive scoring pass:
//
//  1. Filter is a tight, allocation-free byte loop that answers only "do the
//     query's characters appear in name, in order?" It is expected to reject
//     95%+ of candidates and must stay allocation-free — that is the
//     difference between a 2ms and a 40ms keystroke over a large index.
//  2. Score runs only on Filter survivors (typically a few thousand). It
//     computes the best possible alignment and its match positions, used
//     both for ranking and for highlighting matched characters in the UI.
//
// # Case handling
//
// Filter and Score both require ALREADY-LOWERCASED input. Normalize is the
// only function that lowercases; call it once per keystroke on the query,
// and lowercase names once when they are indexed (not per query). Passing
// mixed-case bytes to Filter or Score does not error — it silently returns
// no match (Filter) or false (Score), because byte-for-byte comparison
// fails on differing case. This is deliberate: hot-path functions must not
// pay for a case check or a lowercasing allocation on every candidate.
package fuzzy

import "unicode"

// Normalize lowercases query for use with Filter and Score. Call it once per
// keystroke on the user's query string. It allocates; callers on other hot
// paths (e.g. lowercasing indexed names) should do their own once-per-name
// lowercasing rather than calling this on every query.
func Normalize(query string) []byte {
	out := make([]byte, 0, len(query))
	for _, r := range query {
		out = append(out, string(unicode.ToLower(r))...)
	}
	return out
}

// Filter reports whether every byte of query appears in name, in order,
// as a subsequence. It does not score or locate the match — that is
// Score's job. query and name must already be lowercased (see Normalize);
// this function performs no case folding.
//
// Filter is allocation-free and safe for concurrent use (it holds no
// state). An empty query always matches.
func Filter(query, name []byte) bool {
	if len(query) == 0 {
		return true
	}
	if len(query) > len(name) {
		return false
	}
	qi := 0
	for ni := 0; ni < len(name) && qi < len(query); ni++ {
		if name[ni] == query[qi] {
			qi++
		}
	}
	return qi == len(query)
}
