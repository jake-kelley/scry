package index

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// synthesize builds a Shard with n entries under root, in the same shape
// Test100kSyntheticEntries uses, so round-trip tests exercise the same
// depth/branching mix as the existing benchmarks.
func synthesize(root string, n int) *Shard {
	s := New(root)
	dirs := []uint32{s.RootID()}
	for i := 0; i < n; i++ {
		parent := dirs[i%len(dirs)]
		isDir := i%10 == 0
		var fl Flags
		if isDir {
			fl = FlagDir
		}
		if i%7 == 0 {
			fl |= FlagHidden
		}
		id := s.Upsert(parent, fmt.Sprintf("Entry-%d", i), fl, int64(i)*997, int64(i)*1_000_003)
		if isDir {
			dirs = append(dirs, id)
		}
	}
	return s
}

// snapshotEqual reports whether a and b have identical live entries: same
// Path/Size/MTime/flags for every id, same Children sets, and the same
// lastEID. It assumes both shards have already been compacted (ids run
// 0..Len()-1 contiguously), which is true immediately after a round trip.
func snapshotEqual(t *testing.T, a, b *Shard) {
	t.Helper()

	if a.Len() != b.Len() {
		t.Fatalf("Len() = %d, want %d", b.Len(), a.Len())
	}
	if a.Root() != b.Root() {
		t.Fatalf("Root() = %q, want %q", b.Root(), a.Root())
	}
	if a.lastEID != b.lastEID {
		t.Fatalf("lastEID = %d, want %d", b.lastEID, a.lastEID)
	}

	for id := uint32(0); id < uint32(a.Len()); id++ {
		ea, okA := a.Get(id)
		eb, okB := b.Get(id)
		if okA != okB {
			t.Fatalf("id %d: Get ok = %v, want %v", id, okB, okA)
		}
		if !okA {
			continue
		}
		if ea.Name != eb.Name || ea.IsDir != eb.IsDir || ea.Size != eb.Size || ea.MTime != eb.MTime {
			t.Fatalf("id %d: entry = %+v, want %+v", id, eb, ea)
		}
		if pa, pb := a.Path(id), b.Path(id); pa != pb {
			t.Fatalf("id %d: Path = %q, want %q", id, pb, pa)
		}

		ka := append([]uint32(nil), a.Children(id)...)
		kb := append([]uint32(nil), b.Children(id)...)
		if len(ka) != len(kb) {
			t.Fatalf("id %d: Children = %v, want %v", id, kb, ka)
		}
		seen := make(map[uint32]bool, len(ka))
		for _, c := range ka {
			seen[c] = true
		}
		for _, c := range kb {
			if !seen[c] {
				t.Fatalf("id %d: Children = %v, want %v", id, kb, ka)
			}
		}
	}
}

func TestWriteToReadShardRoundTrip50k(t *testing.T) {
	s := synthesize(`C:\Users\test\docs`, 50_000)
	s.SetLastEID(123456789)

	var buf bytes.Buffer
	n, err := s.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n != int64(buf.Len()) {
		t.Fatalf("WriteTo returned %d, but wrote %d bytes", n, buf.Len())
	}

	got, err := ReadShard(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadShard: %v", err)
	}

	snapshotEqual(t, s, got)
}

func TestWriteToCompactsTombstones(t *testing.T) {
	s := New("/root")
	a := s.Upsert(s.RootID(), "a", FlagDir, 0, 0)
	b := s.Upsert(a, "b.txt", 0, 1, 1)
	s.Remove(b)

	var buf bytes.Buffer
	if _, err := s.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	got, err := ReadShard(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadShard: %v", err)
	}
	if got.Len() != 2 { // root + "a", "b.txt" gone
		t.Fatalf("Len() = %d, want 2 (tombstone must not survive a round trip)", got.Len())
	}
}

func TestReadShardTruncatedMidPayload(t *testing.T) {
	s := synthesize("/root", 500)
	var buf bytes.Buffer
	if _, err := s.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	truncated := buf.Bytes()[:buf.Len()/2]

	_, err := ReadShard(bytes.NewReader(truncated))
	if err == nil {
		t.Fatal("ReadShard(truncated) = nil error, want ErrStale")
	}
	if !errors.Is(err, ErrStale) {
		t.Fatalf("ReadShard(truncated) = %v, want an error wrapping ErrStale", err)
	}
}

func TestReadShardTruncatedMidHeader(t *testing.T) {
	s := synthesize("/root", 5)
	var buf bytes.Buffer
	if _, err := s.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	truncated := buf.Bytes()[:6] // cuts off inside the header, before root path

	_, err := ReadShard(bytes.NewReader(truncated))
	if !errors.Is(err, ErrStale) {
		t.Fatalf("ReadShard(header-truncated) = %v, want an error wrapping ErrStale", err)
	}
}

func TestReadShardCorruptByteCaughtByChecksum(t *testing.T) {
	s := synthesize("/root", 2000)
	var buf bytes.Buffer
	if _, err := s.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	data := append([]byte(nil), buf.Bytes()...)
	// Flip one byte well inside the payload (past the fixed-size header).
	flipAt := len(data) - 10
	data[flipAt] ^= 0xFF

	_, err := ReadShard(bytes.NewReader(data))
	if !errors.Is(err, ErrStale) {
		t.Fatalf("ReadShard(corrupt) = %v, want an error wrapping ErrStale (checksum mismatch)", err)
	}
}

func TestReadShardBadMagic(t *testing.T) {
	s := synthesize("/root", 5)
	var buf bytes.Buffer
	if _, err := s.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	data := buf.Bytes()
	data[0] = 'X'

	_, err := ReadShard(bytes.NewReader(data))
	if !errors.Is(err, ErrStale) {
		t.Fatalf("ReadShard(bad magic) = %v, want an error wrapping ErrStale", err)
	}
}

func TestReadShardWrongVersion(t *testing.T) {
	s := synthesize("/root", 5)
	var buf bytes.Buffer
	if _, err := s.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	data := buf.Bytes()
	// version is the 4 bytes right after the 4-byte magic.
	byteOrder.PutUint32(data[4:8], formatVersion+1)

	_, err := ReadShard(bytes.NewReader(data))
	if !errors.Is(err, ErrStale) {
		t.Fatalf("ReadShard(wrong version) = %v, want an error wrapping ErrStale", err)
	}
}

func BenchmarkWriteTo100k(b *testing.B) {
	s := synthesize("/root", 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		if _, err := s.WriteTo(&buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadShard100k(b *testing.B) {
	s := synthesize("/root", 100_000)
	var buf bytes.Buffer
	if _, err := s.WriteTo(&buf); err != nil {
		b.Fatal(err)
	}
	data := buf.Bytes()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ReadShard(bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}
