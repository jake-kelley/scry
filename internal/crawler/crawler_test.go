package crawler

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"scry/internal/index"
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

// findByName walks a shard's tree via Children looking for an entry named
// name (case-insensitive, per the shard's own name folding).
func findByName(s *index.Shard, name string) (uint32, bool) {
	var found uint32
	var ok bool
	var visit func(id uint32)
	visit = func(id uint32) {
		if ok {
			return
		}
		for _, c := range s.Children(id) {
			e, exists := s.Get(c)
			if !exists {
				continue
			}
			if e.Name == name {
				found, ok = c, true
				return
			}
			visit(c)
			if ok {
				return
			}
		}
	}
	visit(s.RootID())
	return found, ok
}

func TestCrawlBasicTree(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "src"))
	mustWrite(t, filepath.Join(root, "src", "main.go"), "package main")
	mustWrite(t, filepath.Join(root, "README.md"), "hi")

	shard, stats, err := Crawl(root, Options{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}

	if _, ok := findByName(shard, "main.go"); !ok {
		t.Error("main.go not found in shard")
	}
	if _, ok := findByName(shard, "README.md"); !ok {
		t.Error("README.md not found in shard")
	}
	if _, ok := findByName(shard, "src"); !ok {
		t.Error("src dir not found in shard")
	}

	if stats.Entries != 3 {
		t.Errorf("Entries = %d, want 3", stats.Entries)
	}
	if stats.Dirs != 1 {
		t.Errorf("Dirs = %d, want 1", stats.Dirs)
	}
	if stats.Duration < 0 {
		t.Error("Duration should not be negative")
	}
}

// TestCrawlStatsAgreeWithShardCount pins the invariant that made `scry
// index` and `scry status` disagree by one: Stats.Entries excludes the
// synthetic root entry, Shard.Len includes it, and CountIndexed is the
// bridge every user-facing count now goes through.
func TestCrawlStatsAgreeWithShardCount(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "a"))
	mustMkdir(t, filepath.Join(root, "a", "b"))
	mustWrite(t, filepath.Join(root, "a", "b", "deep.txt"), "x")
	mustWrite(t, filepath.Join(root, "top.txt"), "x")

	shard, stats, err := Crawl(root, Options{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}

	if got := shard.CountIndexed(); got != stats.Entries {
		t.Errorf("CountIndexed = %d, Stats.Entries = %d; the two numbers a user sees must agree", got, stats.Entries)
	}
	if got := shard.Len(); got != stats.Entries+1 {
		t.Errorf("Len = %d, want Stats.Entries+1 = %d (the extra one is the root entry)", got, stats.Entries+1)
	}
}

func TestCrawlExcludesDirectory(t *testing.T) {
	root := t.TempDir()
	nm := filepath.Join(root, "node_modules")
	mustMkdir(t, nm)
	// A deep, expensive-to-walk subtree that must never be descended into.
	mustMkdir(t, filepath.Join(nm, "pkg", "sub"))
	mustWrite(t, filepath.Join(nm, "pkg", "sub", "index.js"), "//")
	mustWrite(t, filepath.Join(root, "app.go"), "package main")

	shard, stats, err := Crawl(root, Options{Excludes: []string{"node_modules"}})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}

	if _, ok := findByName(shard, "node_modules"); ok {
		t.Error("node_modules should have been excluded")
	}
	if _, ok := findByName(shard, "index.js"); ok {
		t.Error("index.js inside excluded dir should never have been visited")
	}
	if _, ok := findByName(shard, "app.go"); !ok {
		t.Error("app.go should be indexed")
	}
	if stats.Skipped == 0 {
		t.Error("Skipped should count the excluded directory")
	}
	if stats.Entries != 1 {
		t.Errorf("Entries = %d, want 1 (only app.go; excluded subtree must not be walked)", stats.Entries)
	}
}

func TestCrawlGlobExclude(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.tmp"), "x")
	mustWrite(t, filepath.Join(root, "a.go"), "x")

	shard, stats, err := Crawl(root, Options{Globs: []string{"*.tmp"}})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if _, ok := findByName(shard, "a.tmp"); ok {
		t.Error("a.tmp should have been excluded by glob")
	}
	if _, ok := findByName(shard, "a.go"); !ok {
		t.Error("a.go should be indexed")
	}
	if stats.Entries != 1 {
		t.Errorf("Entries = %d, want 1", stats.Entries)
	}
}

func TestCrawlHiddenDefaultExcluded(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".hidden-dir"))
	mustWrite(t, filepath.Join(root, ".hidden-dir", "secret.txt"), "x")
	mustWrite(t, filepath.Join(root, ".dotfile"), "x")
	mustWrite(t, filepath.Join(root, "visible.txt"), "x")

	shard, stats, err := Crawl(root, Options{Hidden: false})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if _, ok := findByName(shard, ".dotfile"); ok {
		t.Error(".dotfile should be excluded when Hidden is false")
	}
	if _, ok := findByName(shard, ".hidden-dir"); ok {
		t.Error(".hidden-dir should be excluded when Hidden is false")
	}
	if _, ok := findByName(shard, "secret.txt"); ok {
		t.Error("contents of an excluded hidden dir should never be visited")
	}
	if _, ok := findByName(shard, "visible.txt"); !ok {
		t.Error("visible.txt should be indexed")
	}
	if stats.Entries != 1 {
		t.Errorf("Entries = %d, want 1", stats.Entries)
	}
}

func TestCrawlHiddenIncluded(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".dotfile"), "x")

	shard, _, err := Crawl(root, Options{Hidden: true})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if _, ok := findByName(shard, ".dotfile"); !ok {
		t.Error(".dotfile should be indexed when Hidden is true")
	}
}

func TestCrawlUnreadableDirCountedNotFatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced the same way on Windows")
	}

	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	mustMkdir(t, blocked)
	mustWrite(t, filepath.Join(blocked, "secret"), "x")
	mustWrite(t, filepath.Join(root, "ok.txt"), "x")

	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(blocked, 0o755)

	shard, stats, err := Crawl(root, Options{})
	if err != nil {
		t.Fatalf("Crawl must not fail on an unreadable directory: %v", err)
	}
	if stats.Errors == 0 {
		t.Error("Errors should count the unreadable directory")
	}
	if _, ok := findByName(shard, "ok.txt"); !ok {
		t.Error("ok.txt should still be indexed despite the sibling error")
	}
}

func TestCrawlRootPath(t *testing.T) {
	root := t.TempDir()
	shard, _, err := Crawl(root, Options{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	abs, _ := filepath.Abs(root)
	if shard.Root() != abs {
		t.Errorf("Root() = %q, want %q", shard.Root(), abs)
	}
}

// TestCrawlExcludePathIsAnchored is the distinction that motivated
// ExcludePaths existing at all: excluding one specific directory must not
// also exclude every same-named directory elsewhere in the tree, which is
// exactly what a base-name exclude would have done.
func TestCrawlExcludePathIsAnchored(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "Library", "Caches"))
	mustWrite(t, filepath.Join(root, "Library", "Caches", "junk.dat"), "x")
	mustMkdir(t, filepath.Join(root, "code", "proj", "Library"))
	mustWrite(t, filepath.Join(root, "code", "proj", "Library", "keep.go"), "package p")
	mustWrite(t, filepath.Join(root, "notes.txt"), "hi")

	shard, stats, err := Crawl(root, Options{
		ExcludePaths: []string{filepath.Join(root, "Library")},
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}

	if _, ok := findByName(shard, "junk.dat"); ok {
		t.Error("junk.dat under the excluded path was indexed")
	}
	if _, ok := findByName(shard, "Caches"); ok {
		t.Error("Caches under the excluded path was indexed")
	}
	if _, ok := findByName(shard, "keep.go"); !ok {
		t.Error("keep.go under a same-named but different directory was excluded; " +
			"ExcludePaths must be anchored, not matched by base name")
	}
	if _, ok := findByName(shard, "notes.txt"); !ok {
		t.Error("notes.txt outside the excluded path was not indexed")
	}
	if stats.Skipped == 0 {
		t.Error("expected the excluded directory to count as skipped")
	}
}

func TestMatchesExcludePath(t *testing.T) {
	base := filepath.Join(string(filepath.Separator), "Users", "me")
	lib := filepath.Join(base, "Library")

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"the excluded directory itself", lib, true},
		{"a child of it", filepath.Join(lib, "Caches"), true},
		{"a deep descendant", filepath.Join(lib, "Caches", "scry", "shards"), true},
		{"trailing-slash and dot noise still match", filepath.Join(lib, "Caches", "."), true},
		{"a sibling sharing a prefix", filepath.Join(base, "LibraryOfCongress"), false},
		{"a same-named dir elsewhere", filepath.Join(base, "code", "Library"), false},
		{"the parent", base, false},
		{"an unrelated path", filepath.Join(base, "Documents", "notes.txt"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesExcludePath(tc.path, []string{lib}); got != tc.want {
				t.Errorf("MatchesExcludePath(%q, [%q]) = %v, want %v", tc.path, lib, got, tc.want)
			}
		})
	}

	if MatchesExcludePath(lib, nil) {
		t.Error("no exclude paths configured must never match")
	}
	if MatchesExcludePath(lib, []string{""}) {
		t.Error("an empty exclude path must not match everything")
	}
}

// TestMatchesExcludePathCaseFolding pins the platform-dependent half:
// darwin and windows compare paths case-insensitively, linux does not.
func TestMatchesExcludePathCaseFolding(t *testing.T) {
	lib := filepath.Join(string(filepath.Separator), "Users", "me", "Library")
	shouted := filepath.Join(string(filepath.Separator), "Users", "me", "LIBRARY", "Caches")

	want := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	if got := MatchesExcludePath(shouted, []string{lib}); got != want {
		t.Errorf("MatchesExcludePath(%q, [%q]) = %v, want %v on %s", shouted, lib, got, want, runtime.GOOS)
	}
}
