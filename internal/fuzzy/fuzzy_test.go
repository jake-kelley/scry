package fuzzy

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// --- Normalize -------------------------------------------------------------

func TestNormalizeLowercasesASCII(t *testing.T) {
	got := Normalize("QuArTeRlY")
	if string(got) != "quarterly" {
		t.Fatalf("Normalize(%q) = %q, want %q", "QuArTeRlY", got, "quarterly")
	}
}

func TestNormalizeLowercasesUnicode(t *testing.T) {
	got := Normalize("CAFÉ")
	want := strings.ToLower("CAFÉ")
	if string(got) != want {
		t.Fatalf("Normalize(%q) = %q, want %q", "CAFÉ", got, want)
	}
}

func TestNormalizeEmpty(t *testing.T) {
	got := Normalize("")
	if len(got) != 0 {
		t.Fatalf("Normalize(\"\") = %q, want empty", got)
	}
}

// --- Filter: correctness ----------------------------------------------------

func TestFilterCases(t *testing.T) {
	cases := []struct {
		name  string
		query string
		file  string
		want  bool
	}{
		{"empty query matches", "", "anything.txt", true},
		{"empty query matches empty name", "", "", true},
		{"subsequence in order", "qr24", "quarterlyreport24.xlsx", true},
		{"out of order fails", "42rq", "quarterlyreport24.xlsx", false},
		{"missing character fails", "qrz24", "quarterlyreport24.xlsx", false},
		{"exact match", "readme", "readme", true},
		{"query longer than name never matches", "toolongquery", "short", false},
		{"case-sensitive: caller must lowercase", "readme", "README", false},
		{"unicode subsequence", "café", "the café menu", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Filter([]byte(c.query), []byte(c.file))
			if got != c.want {
				t.Fatalf("Filter(%q, %q) = %v, want %v", c.query, c.file, got, c.want)
			}
		})
	}
}

func TestFilterAllocationFree(t *testing.T) {
	query := []byte("qr24")
	name := []byte("quarterly_report_24_summary_final_v2.xlsx")
	allocs := testing.AllocsPerRun(100, func() {
		Filter(query, name)
	})
	if allocs != 0 {
		t.Fatalf("Filter allocated %.1f times per run, want 0", allocs)
	}
}

func TestFilterUnicodeNoPanic(t *testing.T) {
	names := []string{
		"日本語ファイル名.txt",
		"emoji😀name.txt",
		"café_ünïcödé.txt",
		string([]byte{0xff, 0xfe, 0x00}), // invalid UTF-8
	}
	for _, n := range names {
		Filter([]byte("abc"), []byte(n))
	}
}

// --- Score: correctness -----------------------------------------------------

func TestScoreEmptyQueryMatchesEverythingStably(t *testing.T) {
	names := []string{"a.txt", "quarterly_report_24.xlsx", "", "日本語.txt"}
	var first Match
	for i, n := range names {
		m, ok := Score(nil, []byte(n))
		if !ok {
			t.Fatalf("Score(empty, %q) ok=false, want true", n)
		}
		if i == 0 {
			first = m
		}
		if m.Score != first.Score {
			t.Fatalf("Score(empty, %q).Score = %d, want stable %d", n, m.Score, first.Score)
		}
		if m.Positions != nil {
			t.Fatalf("Score(empty, %q).Positions = %v, want nil", n, m.Positions)
		}
	}
}

func TestScoreQueryLongerThanNameNeverMatches(t *testing.T) {
	_, ok := Score([]byte("waytoolongquery"), []byte("short"))
	if ok {
		t.Fatalf("Score with query longer than name: ok=true, want false")
	}
}

func TestScoreNoSubsequenceFails(t *testing.T) {
	_, ok := Score([]byte("xyz"), []byte("abcdef"))
	if ok {
		t.Fatalf("Score with no subsequence: ok=true, want false")
	}
}

func TestScorePositionsAreAscending(t *testing.T) {
	m, ok := Score([]byte("qr24"), []byte("quarterly_report_24.xlsx"))
	if !ok {
		t.Fatalf("Score ok=false, want true")
	}
	for i := 1; i < len(m.Positions); i++ {
		if m.Positions[i] <= m.Positions[i-1] {
			t.Fatalf("Positions not strictly ascending: %v", m.Positions)
		}
	}
}

func TestScoreBestAlignmentNotGreedy(t *testing.T) {
	// "bb" against "abcb": a greedy left-to-right scan matches the same
	// first b twice (impossible) or fails; the only valid subsequence
	// alignment is positions [1, 3].
	m, ok := Score([]byte("bb"), []byte("abcb"))
	if !ok {
		t.Fatalf("Score(bb, abcb) ok=false, want true")
	}
	want := []int{1, 3}
	if len(m.Positions) != len(want) || m.Positions[0] != want[0] || m.Positions[1] != want[1] {
		t.Fatalf("Score(bb, abcb).Positions = %v, want %v", m.Positions, want)
	}
}

func TestScoreUnicodePositionsDoNotSplitRunes(t *testing.T) {
	name := "日本語_café.txt"
	m, ok := Score([]byte("café"), []byte(name))
	if !ok {
		t.Fatalf("Score ok=false, want true")
	}
	for _, pos := range m.Positions {
		if pos < 0 || pos >= len(name) {
			t.Fatalf("position %d out of range for %q", pos, name)
		}
		if !utf8.RuneStart(name[pos]) {
			t.Fatalf("position %d in %q splits a rune", pos, name)
		}
	}
}

func TestScoreUnicodeNoPanic(t *testing.T) {
	names := []string{
		"日本語ファイル名.txt",
		"emoji😀name.txt",
		string([]byte{0xff, 0xfe, 0x00}),
	}
	for _, n := range names {
		Score([]byte("abc"), []byte(n))
	}
}

// --- Score: ranking (order only, weights may be retuned later) -------------

func mustScore(t *testing.T, query, name string) int {
	t.Helper()
	m, ok := Score([]byte(query), []byte(name))
	if !ok {
		t.Fatalf("Score(%q, %q) ok=false, want true", query, name)
	}
	return m.Score
}

func TestRankingBoundaryBonusPrefersWordStart(t *testing.T) {
	// qr24 should prefer a name where the 'r' lands on a word boundary
	// (after a separator) over one where the whole match is a tight but
	// boundary-less scattered subsequence. Filter/Score work on
	// already-lowercased bytes only (see package doc), so this uses a
	// realistic separator-bearing filename rather than a bare camelCase
	// one — camelCase humps need original-case info the fixed,
	// lowercased-only Score signature does not have.
	good := mustScore(t, "qr24", "quarterly_report_24.xlsx")
	bad := mustScore(t, "qr24", "quarry24.txt")
	if good <= bad {
		t.Fatalf("boundary-aligned match scored %d, want > scattered match %d", good, bad)
	}
}

func TestRankingExactPrefixBeatsScatteredSubstring(t *testing.T) {
	readme := mustScore(t, "readme", "readme.md")
	scattered := mustScore(t, "readme", "my_read_me_later.txt")
	if readme <= scattered {
		t.Fatalf("README.md scored %d, want > scattered match %d", readme, scattered)
	}
}

func TestRankingExactSubstringBeatsScatteredSubsequence(t *testing.T) {
	exact := mustScore(t, "abc", "abcdef.txt")
	scattered := mustScore(t, "abc", "zazbzc_scattered.txt")
	if exact <= scattered {
		t.Fatalf("exact substring scored %d, want > scattered match %d", exact, scattered)
	}
}

func TestRankingPrefixBeatsMidName(t *testing.T) {
	prefix := mustScore(t, "report", "report_final_24.xlsx")
	mid := mustScore(t, "report", "quarterly_report_24.xlsx")
	if prefix <= mid {
		t.Fatalf("prefix match scored %d, want > mid-name match %d", prefix, mid)
	}
}

func TestRankingExactSubstringBeatsPositionZeroSubsequence(t *testing.T) {
	// Sanity check that the DP is actually preferring contiguity over
	// merely "starts at zero": both start their match at byte 0.
	exact := mustScore(t, "qtr", "qtr_report.txt")
	subsequence := mustScore(t, "qtr", "q_weird_t_gap_r_report.txt")
	if exact <= subsequence {
		t.Fatalf("contiguous prefix match scored %d, want > spread-out prefix match %d", exact, subsequence)
	}
}

// --- Synthetic corpus + benchmarks ------------------------------------------

var corpusWords = []string{
	"quarterly", "report", "invoice", "summary", "final", "draft", "notes",
	"budget", "meeting", "project", "design", "photo", "vacation", "backup",
	"archive", "presentation", "spreadsheet", "readme", "license", "contract",
	"taxes", "resume", "receipt", "screenshot", "document", "letter", "email",
}

var corpusExts = []string{".txt", ".pdf", ".docx", ".xlsx", ".md", ".png", ".jpg", ".zip", ".go", ".json"}

// buildCorpus generates n realistic, lowercased filenames deterministically
// (fixed seed) so benchmarks are reproducible.
func buildCorpus(n int) [][]byte {
	rng := rand.New(rand.NewSource(1))
	out := make([][]byte, n)
	for i := 0; i < n; i++ {
		var b strings.Builder
		words := 1 + rng.Intn(4)
		for w := 0; w < words; w++ {
			if w > 0 {
				sep := []byte{'_', '-', ' '}[rng.Intn(3)]
				b.WriteByte(sep)
			}
			b.WriteString(corpusWords[rng.Intn(len(corpusWords))])
		}
		if rng.Intn(3) == 0 {
			fmt.Fprintf(&b, "%d", rng.Intn(1000))
		}
		b.WriteString(corpusExts[rng.Intn(len(corpusExts))])
		out[i] = []byte(strings.ToLower(b.String()))
	}
	return out
}

const corpusSize = 250_000

func BenchmarkFilter(b *testing.B) {
	corpus := buildCorpus(corpusSize)
	query := Normalize("qr24")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hits := 0
		for _, name := range corpus {
			if Filter(query, name) {
				hits++
			}
		}
		_ = hits
	}
}

func BenchmarkScore(b *testing.B) {
	corpus := buildCorpus(corpusSize)
	query := Normalize("qr24")
	// Score only ever runs on Filter survivors; benchmark that realistic
	// subset rather than the full 250k corpus.
	var survivors [][]byte
	for _, name := range corpus {
		if Filter(query, name) {
			survivors = append(survivors, name)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, name := range survivors {
			Score(query, name)
		}
	}
}
