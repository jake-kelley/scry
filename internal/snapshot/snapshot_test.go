package snapshot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"scry/internal/index"
)

// withTempCacheHome points HOME (and USERPROFILE on windows) at a fresh
// temp dir for the duration of the test, so config.CacheDir() — and
// therefore Dir() — resolves under it instead of touching the real user
// cache.
func withTempCacheHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func synth(root string, n int) *index.Shard {
	s := index.New(root)
	dirs := []uint32{s.RootID()}
	for i := 0; i < n; i++ {
		parent := dirs[i%len(dirs)]
		isDir := i%10 == 0
		var fl index.Flags
		if isDir {
			fl = index.FlagDir
		}
		id := s.Upsert(parent, fmt.Sprintf("entry-%d", i), fl, int64(i), int64(i))
		if isDir {
			dirs = append(dirs, id)
		}
	}
	return s
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withTempCacheHome(t)

	root := filepath.Join(t.TempDir(), "proj")
	s := synth(root, 5000)
	s.SetLastEID(42)

	if err := Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Len() != s.Len() {
		t.Fatalf("Len() = %d, want %d", got.Len(), s.Len())
	}
	if got.LastEID() != 42 {
		t.Fatalf("LastEID() = %d, want 42", got.LastEID())
	}
}

func TestPathForStableAndCaseFolded(t *testing.T) {
	withTempCacheHome(t)

	p1, err := PathFor(`C:\Users\test\Docs`)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	p2, err := PathFor(`c:\users\test\docs`)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	// hashName always case-folds regardless of host OS, per spec ("keep
	// stable across runs and case-fold on windows/darwin"); PathFor itself
	// doesn't need to vary by GOOS since the folding happens inside
	// hashName.
	if p1 != p2 {
		t.Fatalf("PathFor differs by case: %q vs %q", p1, p2)
	}

	p3, err := PathFor(`C:\Users\test\Docs`)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	if p1 != p3 {
		t.Fatalf("PathFor not stable across calls: %q vs %q", p1, p3)
	}
}

func TestLoadMissingReturnsErrStale(t *testing.T) {
	withTempCacheHome(t)

	_, err := Load(filepath.Join(t.TempDir(), "never-saved"))
	if err == nil {
		t.Fatal("Load(missing) = nil error, want one wrapping index.ErrStale")
	}
	if !isStale(err) {
		t.Fatalf("Load(missing) = %v, want an error wrapping index.ErrStale", err)
	}
}

func TestLoadCorruptReturnsErrStale(t *testing.T) {
	withTempCacheHome(t)

	root := filepath.Join(t.TempDir(), "proj")
	s := synth(root, 100)
	if err := Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path, err := PathFor(root)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	data[len(data)-5] ^= 0xFF
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err = Load(root)
	if !isStale(err) {
		t.Fatalf("Load(corrupt) = %v, want an error wrapping index.ErrStale", err)
	}
}

func TestSaveLeavesNoTempFile(t *testing.T) {
	withTempCacheHome(t)

	root := filepath.Join(t.TempDir(), "proj")
	s := synth(root, 500)
	if err := Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("Save left a temp file behind: %s", e.Name())
		}
	}
}

func TestPartialTempFileDoesNotClobberGoodSnapshot(t *testing.T) {
	withTempCacheHome(t)

	root := filepath.Join(t.TempDir(), "proj")
	good := synth(root, 1000)
	good.SetLastEID(7)
	if err := Save(good); err != nil {
		t.Fatalf("Save(good): %v", err)
	}

	path, err := PathFor(root)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	goodBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(good): %v", err)
	}

	// Simulate a crash mid-write: a temp file appears in the snapshot
	// directory, partially written, but the rename that would make it live
	// never happens.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".snapshot-*.tmp")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := tmp.Write(goodBytes[:len(goodBytes)/3]); err != nil {
		t.Fatalf("Write partial: %v", err)
	}
	tmp.Close()

	got, err := Load(root)
	if err != nil {
		t.Fatalf("Load after simulated crash: %v", err)
	}
	if got.Len() != good.Len() {
		t.Fatalf("Len() = %d, want %d (the good snapshot must survive an abandoned partial temp file)", got.Len(), good.Len())
	}
	if got.LastEID() != 7 {
		t.Fatalf("LastEID() = %d, want 7", got.LastEID())
	}

	// The leftover temp file itself is not something Load or Save cleans
	// up on its own — only the next successful Save (which uses a freshly
	// named temp file and only ever renames its own) does. What matters is
	// that its mere presence never corrupts the live snapshot.
	if err := os.Remove(tmp.Name()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestRemove(t *testing.T) {
	withTempCacheHome(t)

	root := filepath.Join(t.TempDir(), "proj")
	s := synth(root, 10)
	if err := Save(s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Remove(root); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := Load(root); !isStale(err) {
		t.Fatalf("Load after Remove = %v, want an error wrapping index.ErrStale", err)
	}

	// Removing an already-missing snapshot is not an error.
	if err := Remove(root); err != nil {
		t.Fatalf("Remove (already gone): %v", err)
	}
}

func TestList(t *testing.T) {
	withTempCacheHome(t)

	tmp := t.TempDir()
	rootA := filepath.Join(tmp, "a")
	rootB := filepath.Join(tmp, "b")

	if err := Save(synth(rootA, 10)); err != nil {
		t.Fatalf("Save(a): %v", err)
	}
	if err := Save(synth(rootB, 10)); err != nil {
		t.Fatalf("Save(b): %v", err)
	}

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List() = %v, want 2 entries", got)
	}
	want := map[string]bool{rootA: true, rootB: true}
	for _, r := range got {
		if !want[r] {
			t.Fatalf("List() contains unexpected root %q", r)
		}
		delete(want, r)
	}
	if len(want) != 0 {
		t.Fatalf("List() missing roots: %v", want)
	}
}

func isStale(err error) bool {
	return errors.Is(err, index.ErrStale)
}

// BenchmarkSave100k measures the cost of atomically persisting a
// 100k-entry shard: temp file, write, fsync, rename, directory fsync.
func BenchmarkSave100k(b *testing.B) {
	home := b.TempDir()
	b.Setenv("HOME", home)
	b.Setenv("USERPROFILE", home)

	root := filepath.Join(b.TempDir(), "proj")
	s := synth(root, 100_000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Save(s); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLoad100k measures the cost of loading a 100k-entry shard back
// from disk — the number that determines startup latency.
func BenchmarkLoad100k(b *testing.B) {
	home := b.TempDir()
	b.Setenv("HOME", home)
	b.Setenv("USERPROFILE", home)

	root := filepath.Join(b.TempDir(), "proj")
	s := synth(root, 100_000)
	if err := Save(s); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Load(root); err != nil {
			b.Fatal(err)
		}
	}
}
