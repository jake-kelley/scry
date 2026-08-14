// Package integration exercises crawler -> index -> query end to end
// against a real temp directory tree, per DEFINITION OF DONE in the phase 1
// integration task: this is the seam where a bug in one package (e.g. a
// crawler mis-parenting entries, or query mis-scoring) would otherwise only
// show up once real users hit it.
package integration

import (
	"os"
	"path/filepath"
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

	results := query.Search([]*index.Shard{shard}, "report", 10)
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

	results := query.Search([]*index.Shard{shardA, shardB}, "pdf", 10)
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2 (one per shard)", len(results))
	}
}
