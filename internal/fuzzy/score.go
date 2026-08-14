package fuzzy

import (
	"unicode"
	"unicode/utf8"
)

// Match is the result of a successful Score call.
type Match struct {
	Score     int
	Positions []int // byte offsets into name that matched, ascending
}

// Scoring weights. These are deliberately simple integer constants; the
// design doc calls out that the *relative* ordering they produce is the
// contract under test, not their absolute values, so these are expected to
// get tuned later without breaking callers.
//
//   - matchBase: flat credit for every matched character. This is the
//     "floor" every other bonus/penalty is added to or subtracted from.
//   - boundaryBonus: extra credit when a matched character starts a "word"
//     — see isBoundary. This is what makes a query like "qr24" prefer
//     matching the R that starts a new word segment over a nearer, but
//     boundary-less, R.
//   - consecutiveBonus: extra credit when a matched character immediately
//     follows the previous matched character (a contiguous run), on top of
//     matchBase and any boundary bonus for that character.
//   - gapCost: per-skipped-character penalty for the distance between one
//     matched character and the next (or between the start of name and the
//     first matched character). Contiguous runs pay nothing here.
//   - exactSubstringBonus: awarded once, on top of everything else, when
//     the entire query matched as one contiguous run in name (i.e. name
//     literally contains query as a substring).
//   - prefixBonus: awarded once when the match's first character is at
//     position 0 of name — a match at the very start of the name beats an
//     otherwise-identical match in the middle.
const (
	matchBase           = 10
	boundaryBonus       = 15
	consecutiveBonus    = 8
	gapCost             = 1
	exactSubstringBonus = 60
	prefixBonus         = 25
)

// Hard caps on the work a single Score call will do, so one pathological
// candidate (an absurdly long name, or one built from a handful of
// characters repeated thousands of times) cannot stall a query. Real
// filenames are nowhere near these sizes; when a name exceeds
// maxNameRunes, scoring runs only against its first maxNameRunes runes —
// a documented, deliberately degraded result rather than unbounded work.
// A query longer than maxQueryRunes is rejected outright: nobody types a
// 256-character search.
const (
	maxNameRunes  = 4096
	maxQueryRunes = 256
)

// negInf is a sentinel for "no valid alignment ends here" in the DP below.
// It is far enough from zero that adding any combination of the bonuses/
// penalties above cannot make it look like a real score, but stays well
// inside the range of int on both 32- and 64-bit builds.
const negInf = -1 << 30

// Score computes the best possible alignment of query against name and
// returns its score and the byte offsets in name that matched.
//
// query and name must already be lowercased — see the package doc comment.
// Score performs no case folding, so a boundary check that depends on
// "uppercase starts a new word" (classic camelCase humps) is not available
// here: the fixed, lowercased-only signature loses that signal before
// Score ever sees it. Word boundaries are instead derived only from
// separators (- _ . space /) and letter/digit transitions, which is the
// design doc's second sanctioned option ("track boundaries from the
// caller's perspective") given a fixed two-argument, lowercased signature.
// In practice this means realistic filenames — which almost always use a
// separator between words rather than relying on bare camelCase — still
// get correct boundary bonuses; a name with no separators and no case
// information genuinely has no boundary signal left to find.
//
// query and name are decoded as UTF-8 runes, not raw bytes: a match always
// consumes a whole rune, so Positions never lands in the middle of a
// multi-byte character. ok is false when no complete alignment of query
// exists in name (including when query is longer than name, or empty
// after the caller failed to call Normalize on a non-ASCII query changing
// its byte length — callers should always route the query through
// Normalize first).
//
// An empty query matches every name with the same fixed score (0) and no
// positions — "no filter" behaves predictably rather than favouring one
// name over another.
func Score(query, name []byte) (Match, bool) {
	if len(query) == 0 {
		return Match{Score: 0, Positions: nil}, true
	}

	qRunes := decodeRunes(query)
	if len(qRunes) > maxQueryRunes {
		return Match{}, false
	}

	nRunes, offsets := decodeRunesWithOffsets(name)
	if len(nRunes) > maxNameRunes {
		nRunes = nRunes[:maxNameRunes]
		offsets = offsets[:maxNameRunes]
	}

	m := len(qRunes)
	n := len(nRunes)
	if m > n {
		return Match{}, false
	}

	// parent[i][j] records, for the DP row that matched qRunes[i] at
	// nRunes[j], which name-rune index the previous query character was
	// matched at (or -1 if i==0, i.e. this is the first matched char).
	parent := make([][]int, m)
	for i := range parent {
		row := make([]int, n)
		for j := range row {
			row[j] = -1
		}
		parent[i] = row
	}

	prev := make([]int, n)
	cur := make([]int, n)
	for j := range prev {
		prev[j] = negInf
	}

	// Row 0: match the first query rune anywhere in name. The "gap" is
	// the distance from the start of name to the match.
	for j := 0; j < n; j++ {
		if nRunes[j] != qRunes[0] {
			continue
		}
		gap := j
		prev[j] = matchBase + boundaryScore(nRunes, j) - gapCost*gap
	}

	for i := 1; i < m; i++ {
		for j := range cur {
			cur[j] = negInf
		}
		runningMax := negInf
		runningMaxPos := -1
		qr := qRunes[i]
		for j := 0; j < n; j++ {
			// Fold prev[j-1] into the running max *before* using it, so
			// it only ever covers positions strictly before j (a
			// character can't match at or before the one it follows).
			if j >= 1 && prev[j-1] != negInf {
				v := prev[j-1] + gapCost*(j-1)
				if v > runningMax {
					runningMax = v
					runningMaxPos = j - 1
				}
			}
			if nRunes[j] != qr {
				continue
			}
			bb := boundaryScore(nRunes, j)
			best := negInf
			bestParent := -1
			if j >= 1 && prev[j-1] != negInf {
				v := prev[j-1] + matchBase + bb + consecutiveBonus
				if v > best {
					best = v
					bestParent = j - 1
				}
			}
			if runningMax != negInf {
				v := runningMax + gapCost - gapCost*j + matchBase + bb
				if v > best {
					best = v
					bestParent = runningMaxPos
				}
			}
			cur[j] = best
			parent[i][j] = bestParent
		}
		prev, cur = cur, prev
	}

	// The DP above tracks, for each ending position j, only the "raw"
	// alignment score (matchBase/boundary/consecutive/gap). The
	// exactSubstringBonus and prefixBonus are terminal-only: they depend
	// on the *whole* matched alignment, not on any one step of it, so
	// they cannot be folded into the DP's per-position max without
	// tracking contiguity and start-position as extra DP dimensions.
	//
	// That means the raw-score argmax is not necessarily the argmax
	// after those bonuses are added: a scattered match ending at a
	// strong boundary can out-score a fully contiguous exact-substring
	// match in raw terms, while still losing once exactSubstringBonus
	// (60) and prefixBonus (25) are applied to the contiguous one. So
	// every candidate ending position with a valid alignment is
	// backtracked and totalled with its bonuses applied, and the true
	// max is taken over those totals — not over the raw DP scores.
	bestJ, bestTotal := -1, negInf
	var bestPositions []int
	for j := 0; j < n; j++ {
		if prev[j] == negInf {
			continue
		}

		runeIdx := make([]int, m)
		cj := j
		for i := m - 1; i >= 0; i-- {
			runeIdx[i] = cj
			if i > 0 {
				cj = parent[i][cj]
			}
		}

		contiguous := true
		for i := 1; i < m; i++ {
			if runeIdx[i] != runeIdx[i-1]+1 {
				contiguous = false
				break
			}
		}

		total := prev[j]
		if contiguous {
			total += exactSubstringBonus
		}
		if runeIdx[0] == 0 {
			total += prefixBonus
		}

		if total > bestTotal {
			bestTotal = total
			bestJ = j
			bestPositions = runeIdx
		}
	}
	if bestJ < 0 {
		return Match{}, false
	}

	positions := make([]int, m)
	for i, ri := range bestPositions {
		positions[i] = offsets[ri]
	}

	return Match{Score: bestTotal, Positions: positions}, true
}

// boundaryScore returns boundaryBonus if name-rune index j starts a "word":
// the very start of name, right after one of - _ . space /, or a
// letter<->digit transition. See the Score doc comment for why this does
// not include camelCase humps.
func boundaryScore(name []rune, j int) int {
	if !isBoundary(name, j) {
		return 0
	}
	return boundaryBonus
}

func isBoundary(name []rune, j int) bool {
	if j == 0 {
		return true
	}
	prevR := name[j-1]
	switch prevR {
	case '-', '_', '.', ' ', '/':
		return true
	}
	cur := name[j]
	prevAlnum := unicode.IsLetter(prevR) || unicode.IsDigit(prevR)
	curAlnum := unicode.IsLetter(cur) || unicode.IsDigit(cur)
	if !prevAlnum || !curAlnum {
		return false
	}
	return unicode.IsDigit(prevR) != unicode.IsDigit(cur)
}

// decodeRunes decodes b as UTF-8 into a slice of runes. Invalid byte
// sequences decode as utf8.RuneError, one byte at a time, same as range
// over a string — Score never panics on malformed input, it just won't
// match it.
func decodeRunes(b []byte) []rune {
	out := make([]rune, 0, len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		out = append(out, r)
		b = b[size:]
	}
	return out
}

// decodeRunesWithOffsets decodes b as UTF-8, returning both the runes and,
// for each one, the byte offset it started at in b — the values Positions
// is built from.
func decodeRunesWithOffsets(b []byte) ([]rune, []int) {
	runes := make([]rune, 0, len(b))
	offsets := make([]int, 0, len(b))
	off := 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		runes = append(runes, r)
		offsets = append(offsets, off)
		off += size
		b = b[size:]
	}
	return runes, offsets
}
