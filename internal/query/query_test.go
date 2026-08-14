package query

import (
	"path/filepath"
	"testing"

	"scry/internal/index"
	"scry/internal/qsyntax"
)

func newTestShard(root string, files []string) *index.Shard {
	s := index.New(root)
	for _, f := range files {
		s.Upsert(s.RootID(), f, 0, 0, 0)
	}
	return s
}

// mustSearch is a test helper wrapping SearchString for the common case
// where a malformed query would be a test bug, not something to assert on.
func mustSearch(t *testing.T, shards []*index.Shard, q string, limit int) []Result {
	t.Helper()
	results, err := SearchString(shards, q, limit)
	if err != nil {
		t.Fatalf("SearchString(%q): %v", q, err)
	}
	return results
}

func TestSearchRankingOrderAcrossShards(t *testing.T) {
	s1 := newTestShard("/root1", []string{"report.txt", "quarterlyreport24.doc", "other.txt"})
	s2 := newTestShard("/root2", []string{"qreport.txt"})

	results := mustSearch(t, []*index.Shard{s1, s2}, "report", 10)
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	// The exact-substring, shortest name should win.
	if results[0].Name != "report.txt" {
		t.Errorf("top result = %q, want %q", results[0].Name, "report.txt")
	}
	for _, r := range results {
		if r.Name == "other.txt" {
			t.Errorf("other.txt should not match query %q", "report")
		}
	}
}

func TestSearchLimit(t *testing.T) {
	var files []string
	for i := 0; i < 20; i++ {
		files = append(files, "match"+string(rune('a'+i))+".txt")
	}
	s := newTestShard("/root", files)

	results := mustSearch(t, []*index.Shard{s}, "match", 5)
	if len(results) != 5 {
		t.Errorf("len(results) = %d, want 5", len(results))
	}
}

func TestSearchDefaultLimitCapsAt200(t *testing.T) {
	var files []string
	for i := 0; i < 250; i++ {
		files = append(files, "file"+itoa(i)+".txt")
	}
	s := newTestShard("/root", files)

	results := mustSearch(t, []*index.Shard{s}, "file", 0)
	if len(results) != 200 {
		t.Errorf("len(results) = %d, want 200 (default cap)", len(results))
	}

	results = mustSearch(t, []*index.Shard{s}, "file", 10000)
	if len(results) != 200 {
		t.Errorf("len(results) with oversized limit = %d, want 200 (hard cap)", len(results))
	}
}

func TestSearchSkipsOfflineShard(t *testing.T) {
	online := newTestShard("/root1", []string{"target.txt"})
	offline := newTestShard("/root2", []string{"target-also.txt"})
	offline.SetOnline(false)

	results := mustSearch(t, []*index.Shard{online, offline}, "target", 10)
	for _, r := range results {
		if r.Name == "target-also.txt" {
			t.Error("offline shard's entries should not appear in results")
		}
	}
	if len(results) != 1 {
		t.Errorf("len(results) = %d, want 1 (offline shard skipped)", len(results))
	}
}

func TestSearchEmptyQueryExcludesRoot(t *testing.T) {
	s := newTestShard("/root", []string{"a.txt"})
	results := mustSearch(t, []*index.Shard{s}, "", 10)
	for _, r := range results {
		if r.Path == "/root" {
			t.Error("root entry (empty name) should never appear in results")
		}
	}
}

// TestSearchExtFilterShrinksCandidates checks that a bare "ext:" query
// (no fuzzy term at all) still returns every filter-matched entry, with
// score 0, per Search's doc comment.
func TestSearchExtFilterShrinksCandidates(t *testing.T) {
	s := newTestShard("/root", []string{"a.go", "b.go", "c.rs", "d.txt"})

	results := mustSearch(t, []*index.Shard{s}, "ext:go", 10)
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	for _, r := range results {
		if r.Score != 0 {
			t.Errorf("%s: score = %d, want 0 (no fuzzy term)", r.Name, r.Score)
		}
		if r.Name != "a.go" && r.Name != "b.go" {
			t.Errorf("unexpected result %q for ext:go", r.Name)
		}
	}
}

// TestSearchExtFilterCombinesWithFuzzy checks ext: and a fuzzy term
// together: the ext: filter shrinks the candidate set before scoring, and
// only entries satisfying both come back.
func TestSearchExtFilterCombinesWithFuzzy(t *testing.T) {
	s := newTestShard("/root", []string{"report.go", "report.txt", "reportish.go"})

	results := mustSearch(t, []*index.Shard{s}, "report ext:go", 10)
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2, got %+v", len(results), results)
	}
	for _, r := range results {
		if r.Name == "report.txt" {
			t.Error("report.txt matches the fuzzy term but not ext:go, should be excluded")
		}
	}
}

// TestSearchPathFilter checks that path: matches against the full
// reconstructed path, not just the base name.
func TestSearchPathFilter(t *testing.T) {
	s := index.New("/root")
	docs := s.Upsert(s.RootID(), "docs", index.FlagDir, 0, 0)
	s.Upsert(docs, "notes.txt", 0, 0, 0)
	s.Upsert(s.RootID(), "notes.txt", 0, 0, 0)

	results := mustSearch(t, []*index.Shard{s}, "path:docs/notes", 10)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1, got %+v", len(results), results)
	}
	if results[0].Path != filepath.Join("/root", "docs", "notes.txt") {
		t.Errorf("path = %q, want the one inside docs/", results[0].Path)
	}
}

// TestSearchNegation checks that a negated term excludes matching entries.
func TestSearchNegation(t *testing.T) {
	s := newTestShard("/root", []string{"report.txt", "report-vendor.txt"})

	results := mustSearch(t, []*index.Shard{s}, "report !vendor", 10)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1, got %+v", len(results), results)
	}
	if results[0].Name != "report.txt" {
		t.Errorf("result = %q, want report.txt", results[0].Name)
	}
}

// TestSearchRootFilter checks that root: restricts which shards are
// scanned at all, before any arena is walked.
func TestSearchRootFilter(t *testing.T) {
	s1 := newTestShard("/code/project", []string{"main.go"})
	s2 := newTestShard("/docs", []string{"main-notes.txt"})

	results := mustSearch(t, []*index.Shard{s1, s2}, "root:code main", 10)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1, got %+v", len(results), results)
	}
	if results[0].Name != "main.go" {
		t.Errorf("result = %q, want main.go", results[0].Name)
	}
}

// TestSearchWithParsedQuery checks the non-string Search entry point
// directly, using an already-parsed qsyntax.Query.
func TestSearchWithParsedQuery(t *testing.T) {
	s := newTestShard("/root", []string{"a.go", "b.txt"})

	q, err := qsyntax.Parse("ext:go")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	results := Search([]*index.Shard{s}, q, 10)
	if len(results) != 1 || results[0].Name != "a.go" {
		t.Fatalf("results = %+v, want just a.go", results)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
