package query

import (
	"testing"

	"scry/internal/index"
)

func newTestShard(root string, files []string) *index.Shard {
	s := index.New(root)
	for _, f := range files {
		s.Upsert(s.RootID(), f, 0, 0, 0)
	}
	return s
}

func TestSearchRankingOrderAcrossShards(t *testing.T) {
	s1 := newTestShard("/root1", []string{"report.txt", "quarterlyreport24.doc", "other.txt"})
	s2 := newTestShard("/root2", []string{"qreport.txt"})

	results := Search([]*index.Shard{s1, s2}, "report", 10)
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

	results := Search([]*index.Shard{s}, "match", 5)
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

	results := Search([]*index.Shard{s}, "file", 0)
	if len(results) != 200 {
		t.Errorf("len(results) = %d, want 200 (default cap)", len(results))
	}

	results = Search([]*index.Shard{s}, "file", 10000)
	if len(results) != 200 {
		t.Errorf("len(results) with oversized limit = %d, want 200 (hard cap)", len(results))
	}
}

func TestSearchSkipsOfflineShard(t *testing.T) {
	online := newTestShard("/root1", []string{"target.txt"})
	offline := newTestShard("/root2", []string{"target-also.txt"})
	offline.SetOnline(false)

	results := Search([]*index.Shard{online, offline}, "target", 10)
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
	results := Search([]*index.Shard{s}, "", 10)
	for _, r := range results {
		if r.Path == "/root" {
			t.Error("root entry (empty name) should never appear in results")
		}
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
