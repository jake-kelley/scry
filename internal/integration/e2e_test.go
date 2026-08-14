// Package integration exercises crawler -> index -> query end to end
// against a real temp directory tree, per DEFINITION OF DONE in the phase 1
// integration task: this is the seam where a bug in one package (e.g. a
// crawler mis-parenting entries, or query mis-scoring) would otherwise only
// show up once real users hit it.
package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scry/internal/crawler"
	"scry/internal/index"
	"scry/internal/query"
)

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCrawlThenSearchFindsExpectedFileFirst(t *testing.T) {
	root := t.TempDir()

	mustMkdir(t, filepath.Join(root, "docs"))
	mustWrite(t, filepath.Join(root, "docs", "QuarterlyReport24.pdf"), "x")
	mustWrite(t, filepath.Join(root, "docs", "notes.txt"), "x")
	mustMkdir(t, filepath.Join(root, "node_modules"))
	mustWrite(t, filepath.Join(root, "node_modules", "report-noise.js"), "x")
	mustWrite(t, filepath.Join(root, "report.txt"), "the actual target")
	mustWrite(t, filepath.Join(root, "unrelated.go"), "x")

	shard, stats, err := crawler.Crawl(root, crawler.Options{Excludes: []string{"node_modules"}})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if stats.Entries == 0 {
		t.Fatal("expected some entries to be crawled")
	}

	results, err := query.SearchString([]*index.Shard{shard}, "report", 10)
	if err != nil {
		t.Fatalf("SearchString: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result for query \"report\"")
	}

	top := results[0]
	if filepath.Base(top.Path) != "report.txt" {
		t.Fatalf("top result = %q, want a path ending in report.txt (got results: %+v)", top.Path, results)
	}

	for _, r := range results {
		if filepath.Base(r.Path) == "report-noise.js" {
			t.Errorf("excluded node_modules content leaked into results: %+v", r)
		}
	}
}

func TestCrawlThenSearchAcrossMultipleShards(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()

	mustWrite(t, filepath.Join(rootA, "invoice.pdf"), "x")
	mustWrite(t, filepath.Join(rootB, "receipt.pdf"), "x")

	shardA, _, err := crawler.Crawl(rootA, crawler.Options{})
	if err != nil {
		t.Fatalf("Crawl rootA: %v", err)
	}
	shardB, _, err := crawler.Crawl(rootB, crawler.Options{})
	if err != nil {
		t.Fatalf("Crawl rootB: %v", err)
	}

	results, err := query.SearchString([]*index.Shard{shardA, shardB}, "pdf", 10)
	if err != nil {
		t.Fatalf("SearchString: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2 (one per shard)", len(results))
	}
}

// TestQuerySyntaxFiltersAgainstRealTree crawls a real temp directory tree
// and checks that each qsyntax filter shrinks Search's results the way its
// package doc promises, against actually-crawled entries rather than
// hand-built shards.
func TestQuerySyntaxFiltersAgainstRealTree(t *testing.T) {
	root := t.TempDir()

	mustMkdir(t, filepath.Join(root, "src"))
	mustMkdir(t, filepath.Join(root, "vendor"))
	mustWrite(t, filepath.Join(root, "src", "main.go"), "x")
	mustWrite(t, filepath.Join(root, "src", "main.rs"), "x")
	mustWrite(t, filepath.Join(root, "src", "notes.txt"), "x")
	mustWrite(t, filepath.Join(root, "vendor", "main.go"), "x")

	shard, _, err := crawler.Crawl(root, crawler.Options{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	shards := []*index.Shard{shard}

	t.Run("ext filter shrinks to matching extension only", func(t *testing.T) {
		results, err := query.SearchString(shards, "ext:go", 10)
		if err != nil {
			t.Fatalf("SearchString: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("len(results) = %d, want 2 (both main.go files), got %+v", len(results), results)
		}
		for _, r := range results {
			if filepath.Ext(r.Path) != ".go" {
				t.Errorf("ext:go leaked a non-.go result: %+v", r)
			}
		}
	})

	t.Run("path filter matches the full path, not just the base name", func(t *testing.T) {
		// notes.txt's own name never mentions "vendor" — only its full
		// path does not contain it either, so this also proves path:
		// checks the reconstructed path and not just each candidate's
		// base name: vendor/main.go's path contains "vendor" and matches,
		// notes.txt's does not.
		results, err := query.SearchString(shards, "path:vendor/main", 10)
		if err != nil {
			t.Fatalf("SearchString: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("len(results) = %d, want 1, got %+v", len(results), results)
		}
		if filepath.Base(filepath.Dir(results[0].Path)) != "vendor" {
			t.Errorf("path:vendor/main matched a file outside vendor/: %+v", results[0])
		}
	})

	t.Run("negation excludes the negated term", func(t *testing.T) {
		// A negated *fuzzy* term only excludes by base name (see
		// internal/qsyntax's package doc), so excluding vendor/ paths
		// specifically needs a negated path: term, not a negated fuzzy
		// term — this is what actually reaches into a subdirectory by
		// full path rather than by name.
		results, err := query.SearchString(shards, "main !path:vendor", 10)
		if err != nil {
			t.Fatalf("SearchString: %v", err)
		}
		for _, r := range results {
			if strings.Contains(filepath.ToSlash(r.Path), "/vendor/") {
				t.Errorf("!path:vendor did not exclude a result under vendor/: %+v", r)
			}
		}
		if len(results) == 0 {
			t.Fatal("expected at least one main.* result outside vendor/")
		}
	})

	t.Run("root filter restricts which shard is searched", func(t *testing.T) {
		other := t.TempDir()
		mustWrite(t, filepath.Join(other, "unrelated.go"), "x")
		otherShard, _, err := crawler.Crawl(other, crawler.Options{})
		if err != nil {
			t.Fatalf("Crawl other: %v", err)
		}

		rootBase := filepath.Base(root)
		results, err := query.SearchString([]*index.Shard{shard, otherShard}, "root:"+rootBase+" main", 10)
		if err != nil {
			t.Fatalf("SearchString: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected results from the root-filtered shard")
		}
		for _, r := range results {
			if !strings.HasPrefix(filepath.ToSlash(r.Path), filepath.ToSlash(root)) {
				t.Errorf("root:%s leaked a result from another shard: %+v", rootBase, r)
			}
		}
	})
}
