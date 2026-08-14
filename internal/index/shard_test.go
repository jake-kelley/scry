package index

import (
	"fmt"
	"sync"
	"testing"
)

func TestRootParentSelfReference(t *testing.T) {
	s := New("/root")
	if s.RootID() != 0 {
		t.Fatalf("RootID() = %d, want 0", s.RootID())
	}
	if s.parent[s.RootID()] != s.RootID() {
		t.Fatalf("root parent = %d, want self (%d)", s.parent[s.RootID()], s.RootID())
	}
	if got := s.Path(s.RootID()); got != "/root" {
		t.Fatalf("Path(RootID()) = %q, want %q", got, "/root")
	}
	if s.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", s.Len())
	}
}

func TestPathReconstructionThreeLevelsDeep(t *testing.T) {
	s := New("/root")
	a := s.Upsert(s.RootID(), "A", FlagDir, 0, 0)
	b := s.Upsert(a, "B", FlagDir, 0, 0)
	c := s.Upsert(b, "file.txt", 0, 123, 456)

	tests := []struct {
		id   uint32
		want string
	}{
		{s.RootID(), "/root"},
		{a, "/root/A"},
		{b, "/root/A/B"},
		{c, "/root/A/B/file.txt"},
	}
	for _, tt := range tests {
		if got := s.Path(tt.id); got != tt.want {
			t.Errorf("Path(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestUpsertIdempotent(t *testing.T) {
	s := New("/root")
	id1 := s.Upsert(s.RootID(), "dupe.txt", 0, 10, 100)
	lenAfterFirst := s.Len()
	id2 := s.Upsert(s.RootID(), "dupe.txt", 0, 20, 200)
	if id1 != id2 {
		t.Fatalf("Upsert twice with same name returned different ids: %d != %d", id1, id2)
	}
	if s.Len() != lenAfterFirst {
		t.Fatalf("Len() grew on idempotent Upsert: %d -> %d", lenAfterFirst, s.Len())
	}
	e, ok := s.Get(id1)
	if !ok {
		t.Fatal("Get() = false after idempotent Upsert")
	}
	if e.Size != 20 || e.MTime != 200 {
		t.Fatalf("Upsert did not update size/mtime: got %+v", e)
	}

	// Case-insensitive idempotency.
	id3 := s.Upsert(s.RootID(), "DUPE.TXT", 0, 30, 300)
	if id3 != id1 {
		t.Fatalf("Upsert with different case created new id: %d != %d", id3, id1)
	}
	e, _ = s.Get(id1)
	if e.Name != "DUPE.TXT" {
		t.Fatalf("Get().Name = %q, want %q (last-write case)", e.Name, "DUPE.TXT")
	}
}

func TestUpsertSameNameDifferentParentsAreDistinct(t *testing.T) {
	s := New("/root")
	a := s.Upsert(s.RootID(), "dir1", FlagDir, 0, 0)
	b := s.Upsert(s.RootID(), "dir2", FlagDir, 0, 0)
	f1 := s.Upsert(a, "same.txt", 0, 0, 0)
	f2 := s.Upsert(b, "same.txt", 0, 0, 0)
	if f1 == f2 {
		t.Fatalf("entries with same name under different parents got the same id")
	}
}

func TestRemoveRecursive(t *testing.T) {
	s := New("/root")
	a := s.Upsert(s.RootID(), "A", FlagDir, 0, 0)
	b := s.Upsert(a, "B", FlagDir, 0, 0)
	c := s.Upsert(b, "file.txt", 0, 0, 0)
	sibling := s.Upsert(s.RootID(), "sibling.txt", 0, 0, 0)

	lenBefore := s.Len()
	s.Remove(a)

	for _, id := range []uint32{a, b, c} {
		if _, ok := s.Get(id); ok {
			t.Errorf("Get(%d) still alive after recursive Remove", id)
		}
		if p := s.Path(id); p != "" {
			t.Errorf("Path(%d) = %q after Remove, want \"\"", id, p)
		}
		if kids := s.Children(id); kids != nil {
			t.Errorf("Children(%d) = %v after Remove, want nil", id, kids)
		}
	}
	if _, ok := s.Get(sibling); !ok {
		t.Error("sibling entry was removed by unrelated Remove")
	}
	wantLen := lenBefore - 3
	if s.Len() != wantLen {
		t.Fatalf("Len() = %d after removing subtree of 3, want %d", s.Len(), wantLen)
	}
	if kids := s.Children(s.RootID()); len(kids) != 1 || kids[0] != sibling {
		t.Fatalf("Children(root) = %v, want only [%d]", kids, sibling)
	}
}

func TestSlotReuseAfterRemove(t *testing.T) {
	s := New("/root")
	a := s.Upsert(s.RootID(), "a.txt", 0, 0, 0)
	s.Remove(a)
	b := s.Upsert(s.RootID(), "b.txt", 0, 0, 0)
	if b != a {
		t.Fatalf("freed slot %d was not reused, got new id %d", a, b)
	}
	e, ok := s.Get(b)
	if !ok || e.Name != "b.txt" {
		t.Fatalf("Get(%d) = %+v, %v, want b.txt entry", b, e, ok)
	}
}

func TestCompactPreservesLiveEntries(t *testing.T) {
	s := New("/root")
	a := s.Upsert(s.RootID(), "A", FlagDir, 0, 0)
	b := s.Upsert(a, "B", FlagDir, 0, 0)
	keep1 := s.Upsert(b, "keep1.txt", 0, 111, 222)
	doomed := s.Upsert(a, "doomed.txt", 0, 0, 0)
	keep2 := s.Upsert(s.RootID(), "keep2.txt", 0, 333, 444)

	s.Remove(doomed)

	type snapshot struct {
		path       string
		size       int64
		mtime      int64
		parentPath string
	}
	before := map[uint32]snapshot{}
	for _, id := range []uint32{s.RootID(), a, b, keep1, keep2} {
		e, ok := s.Get(id)
		if !ok {
			t.Fatalf("setup: Get(%d) = false before Compact", id)
		}
		before[id] = snapshot{
			path:       s.Path(id),
			size:       e.Size,
			mtime:      e.MTime,
			parentPath: s.Path(s.parent[id]),
		}
	}
	lenBefore := s.Len()

	s.Compact()

	if s.Len() != lenBefore {
		t.Fatalf("Len() changed across Compact: %d -> %d", lenBefore, s.Len())
	}
	_ = doomed // ids are remapped by Compact; checked indirectly via Children(A) below.

	// Ids may have been remapped; re-derive them by walking children from
	// the (possibly remapped) root, matching on name, and check invariants
	// on the new ids.
	rootID := s.RootID()
	if got := s.Path(rootID); got != "/root" {
		t.Fatalf("Path(RootID()) after Compact = %q, want /root", got)
	}
	findChild := func(parent uint32, name string) uint32 {
		for _, c := range s.Children(parent) {
			e, _ := s.Get(c)
			if e.Name == name {
				return c
			}
		}
		t.Fatalf("child %q not found under %d after Compact", name, parent)
		return 0
	}
	newA := findChild(rootID, "A")
	for _, c := range s.Children(newA) {
		e, _ := s.Get(c)
		if e.Name == "doomed.txt" {
			t.Fatalf("tombstoned entry resurrected by Compact under A: %+v", e)
		}
	}
	newB := findChild(newA, "B")
	newKeep1 := findChild(newB, "keep1.txt")
	newKeep2 := findChild(rootID, "keep2.txt")

	if s.Path(newKeep1) != "/root/A/B/keep1.txt" {
		t.Errorf("Path(keep1) after Compact = %q", s.Path(newKeep1))
	}
	e, _ := s.Get(newKeep1)
	if e.Size != before[keep1].size || e.MTime != before[keep1].mtime {
		t.Errorf("keep1 size/mtime changed across Compact: got %+v", e)
	}
	if s.parent[newKeep1] != newB {
		t.Errorf("keep1 parent not remapped to new B id")
	}
	if s.Path(newKeep2) != "/root/keep2.txt" {
		t.Errorf("Path(keep2) after Compact = %q", s.Path(newKeep2))
	}
}

func TestEntryAtRoundTrips(t *testing.T) {
	s := New("/root")
	var ids []uint32
	names := []string{"Alpha", "Beta.txt", "gamma", "Delta-Report.PDF", "e"}
	for _, n := range names {
		ids = append(ids, s.Upsert(s.RootID(), n, 0, 0, 0))
	}
	// Tombstone one and reuse its slot with a new name to exercise the
	// stale-offset-table path.
	s.Remove(ids[1])
	replacement := s.Upsert(s.RootID(), "replacement", 0, 0, 0)
	if replacement != ids[1] {
		t.Fatalf("expected slot reuse, got new id %d", replacement)
	}

	live := []uint32{ids[0], replacement, ids[2], ids[3], ids[4]}
	for _, id := range live {
		off, length := s.NameRange(id)
		for o := off; o < off+length; o++ {
			gotID, ok := s.EntryAt(o)
			if !ok {
				t.Errorf("EntryAt(%d) = false, want id %d (name range [%d,%d))", o, id, off, off+length)
				continue
			}
			if gotID != id {
				t.Errorf("EntryAt(%d) = %d, want %d", o, gotID, id)
			}
		}
		// The NUL separator immediately after the name must not resolve.
		if _, ok := s.EntryAt(off + length); ok {
			t.Errorf("EntryAt(%d) (NUL separator for id %d) = true, want false", off+length, id)
		}
	}

	// Offset past the end of the arena.
	if _, ok := s.EntryAt(uint32(len(s.Arena())) + 100); ok {
		t.Error("EntryAt(past end) = true, want false")
	}
}

func TestGetPathChildrenOnDeadOrUnknownID(t *testing.T) {
	s := New("/root")
	a := s.Upsert(s.RootID(), "a", FlagDir, 0, 0)
	s.Remove(a)

	if _, ok := s.Get(a); ok {
		t.Error("Get(dead) = true")
	}
	if _, ok := s.Get(9999); ok {
		t.Error("Get(unknown) = true")
	}
	if p := s.Path(a); p != "" {
		t.Errorf("Path(dead) = %q, want \"\"", p)
	}
	if p := s.Path(9999); p != "" {
		t.Errorf("Path(unknown) = %q, want \"\"", p)
	}
	if kids := s.Children(a); kids != nil {
		t.Errorf("Children(dead) = %v, want nil", kids)
	}
	if kids := s.Children(9999); kids != nil {
		t.Errorf("Children(unknown) = %v, want nil", kids)
	}
	if off, l := s.NameRange(a); off != 0 || l != 0 {
		t.Errorf("NameRange(dead) = (%d,%d), want (0,0)", off, l)
	}
}

func TestChildrenNotAllocatedForLeaves(t *testing.T) {
	s := New("/root")
	leaf := s.Upsert(s.RootID(), "leaf.txt", 0, 0, 0)
	if _, ok := s.children[leaf]; ok {
		t.Error("children map has an entry for a leaf with no children of its own")
	}
}

func Test100kSyntheticEntries(t *testing.T) {
	s := New("/root")
	const n = 100_000
	dirs := []uint32{s.RootID()}
	for i := 0; i < n; i++ {
		parent := dirs[i%len(dirs)]
		isDir := i%10 == 0
		var fl Flags
		if isDir {
			fl = FlagDir
		}
		id := s.Upsert(parent, fmt.Sprintf("entry-%d", i), fl, int64(i), int64(i))
		if isDir {
			dirs = append(dirs, id)
		}
	}
	if s.Len() != n+1 { // +1 for root
		t.Fatalf("Len() = %d, want %d", s.Len(), n+1)
	}
}

func TestConcurrentReadsAndWrites(t *testing.T) {
	s := New("/root")
	dir := s.Upsert(s.RootID(), "dir", FlagDir, 0, 0)
	seed := s.Upsert(dir, "seed.txt", 0, 1, 1)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			s.Upsert(dir, fmt.Sprintf("race-%d", i), 0, int64(i), int64(i))
		}
		close(stop)
	}()

	// Readers.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, ok := s.Get(seed); !ok {
					t.Error("Get(seed) = false during concurrent writes")
				}
				if p := s.Path(seed); p != "/root/dir/seed.txt" {
					t.Errorf("Path(seed) = %q during concurrent writes", p)
				}
				_ = s.Children(dir)
				_ = s.Len()
			}
		}()
	}

	wg.Wait()
}

func BenchmarkUpsert(b *testing.B) {
	s := New("/root")
	dir := s.RootID()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Upsert(dir, fmt.Sprintf("bench-%d", i), 0, int64(i), int64(i))
	}
}

func BenchmarkPath(b *testing.B) {
	s := New("/root")
	a := s.Upsert(s.RootID(), "A", FlagDir, 0, 0)
	bID := s.Upsert(a, "B", FlagDir, 0, 0)
	c := s.Upsert(bID, "file.txt", 0, 0, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Path(c)
	}
}
