package index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// ErrStale is returned by ReadShard whenever the bytes it was given cannot
// be trusted as a complete, current shard: wrong magic, wrong format
// version, truncation, or a checksum mismatch. It is a sentinel, not a
// fatal error — the caller's correct response is to discard the file and
// recrawl that one root, never to treat the whole app as broken.
var ErrStale = errors.New("index: stale or corrupt shard snapshot")

// magic identifies a scry shard snapshot file.
const magic = "SCRY"

// formatVersion is bumped whenever the on-disk layout changes in a way that
// is not backward compatible. ReadShard rejects anything else as ErrStale
// rather than guessing at a different layout.
const formatVersion uint32 = 1

// byteOrder is the fixed endianness used throughout the format. The choice
// itself is arbitrary; what matters is that reader and writer agree.
var byteOrder = binary.LittleEndian

// WriteTo serializes the shard to w in scry's on-disk snapshot format:
//
//	magic      [4]byte   "SCRY"
//	version    uint32
//	rootLen    uint32
//	root       [rootLen]byte
//	lastEID    uint64
//	entryCount uint32
//	checksum   uint32    crc32(IEEE) of the payload that follows
//	payload    ...       one record per entry, id 0 first
//
// Each payload record is:
//
//	parent  uint32
//	flags   uint8
//	size    int64
//	mtime   int64
//	nameLen uint16
//	name    [nameLen]byte  original-case name (empty for the root entry)
//
// WriteTo compacts the shard first so tombstones are never persisted; the
// whole operation — compact, encode, write — happens under a single lock so
// a concurrent Upsert can never observe or corrupt a partial snapshot. The
// checksum guards against truncation and bit rot, not tampering.
func (s *Shard) WriteTo(w io.Writer) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compactLocked()

	payload := s.encodePayloadLocked()
	checksum := crc32.ChecksumIEEE(payload)

	var header bytes.Buffer
	header.WriteString(magic)
	writeUint32(&header, formatVersion)
	writeUint32(&header, uint32(len(s.root)))
	header.WriteString(s.root)
	writeUint64(&header, s.lastEID)
	writeUint32(&header, uint32(s.len))
	writeUint32(&header, checksum)

	n1, err := w.Write(header.Bytes())
	if err != nil {
		return int64(n1), fmt.Errorf("index: write shard: %w", err)
	}
	n2, err := w.Write(payload)
	if err != nil {
		return int64(n1 + n2), fmt.Errorf("index: write shard: %w", err)
	}
	return int64(n1 + n2), nil
}

// encodePayloadLocked builds the payload bytes described on WriteTo. Caller
// holds the lock and the shard must already be compacted (dead entries
// carry no tombstones, ids run 0..len-1 contiguously).
func (s *Shard) encodePayloadLocked() []byte {
	// Rough size estimate to avoid repeated grows: fixed 4+1+8+8+2 = 23
	// bytes per record plus the name itself.
	buf := make([]byte, 0, s.len*(23+12))
	for id := 0; id < s.len; id++ {
		name := s.origName[id]
		buf = appendUint32(buf, s.parent[id])
		buf = append(buf, s.flags[id])
		buf = appendInt64(buf, s.size[id])
		buf = appendInt64(buf, s.mtime[id])
		buf = appendUint16(buf, uint16(len(name)))
		buf = append(buf, name...)
	}
	return buf
}

// ReadShard reconstructs a Shard from bytes written by WriteTo. Any
// structural problem — bad magic, wrong version, truncation, or a checksum
// mismatch — is reported as ErrStale (wrapped, so errors.Is(err,
// index.ErrStale) finds it) rather than as a raw I/O or parse error: the
// caller's job is to recrawl that one root, not to decide what kind of
// corruption it was.
func ReadShard(r io.Reader) (*Shard, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("index: read shard: %w: %w", ErrStale, err)
	}

	br := &byteReader{data: data}

	gotMagic, ok := br.take(len(magic))
	if !ok || string(gotMagic) != magic {
		return nil, fmt.Errorf("index: read shard: bad magic: %w", ErrStale)
	}

	version, ok := br.uint32()
	if !ok || version != formatVersion {
		return nil, fmt.Errorf("index: read shard: unsupported version: %w", ErrStale)
	}

	rootLen, ok := br.uint32()
	if !ok {
		return nil, fmt.Errorf("index: read shard: truncated header: %w", ErrStale)
	}
	rootBytes, ok := br.take(int(rootLen))
	if !ok {
		return nil, fmt.Errorf("index: read shard: truncated header: %w", ErrStale)
	}
	root := string(rootBytes)

	lastEID, ok := br.uint64()
	if !ok {
		return nil, fmt.Errorf("index: read shard: truncated header: %w", ErrStale)
	}
	entryCount, ok := br.uint32()
	if !ok {
		return nil, fmt.Errorf("index: read shard: truncated header: %w", ErrStale)
	}
	wantChecksum, ok := br.uint32()
	if !ok {
		return nil, fmt.Errorf("index: read shard: truncated header: %w", ErrStale)
	}

	payload := br.rest()
	if crc32.ChecksumIEEE(payload) != wantChecksum {
		return nil, fmt.Errorf("index: read shard: checksum mismatch: %w", ErrStale)
	}

	shard, err := decodeShard(root, lastEID, int(entryCount), payload)
	if err != nil {
		return nil, fmt.Errorf("index: read shard: %w: %w", ErrStale, err)
	}
	return shard, nil
}

// decodeShard rebuilds a Shard from a validated (checksum already checked)
// payload. It reconstructs id 0's arena entry directly and every other
// entry via Upsert with parent ids in the order they were written — which,
// because WriteTo always wrote a parent before any of its descendants, is
// enough to rebuild the shard's children/childIndex/offset structures
// exactly, without duplicating that bookkeeping here.
func decodeShard(root string, lastEID uint64, entryCount int, payload []byte) (*Shard, error) {
	if entryCount < 1 {
		return nil, errors.New("entry count must include the root")
	}

	pr := &byteReader{data: payload}

	// Record 0 is always the root entry itself: parent == 0 (self), an
	// empty name. Consume it but don't route it through Upsert — New()
	// already installed the root entry the same way WriteTo encoded it.
	if _, ok := pr.uint32(); !ok {
		return nil, errors.New("truncated payload")
	}
	if _, ok := pr.take(1); !ok { // flags
		return nil, errors.New("truncated payload")
	}
	if _, ok := pr.int64(); !ok { // size
		return nil, errors.New("truncated payload")
	}
	if _, ok := pr.int64(); !ok { // mtime
		return nil, errors.New("truncated payload")
	}
	nameLen, ok := pr.uint16()
	if !ok {
		return nil, errors.New("truncated payload")
	}
	if _, ok := pr.take(int(nameLen)); !ok {
		return nil, errors.New("truncated payload")
	}

	shard := New(root)
	shard.lastEID = lastEID

	for i := 1; i < entryCount; i++ {
		parent, ok := pr.uint32()
		if !ok {
			return nil, errors.New("truncated payload")
		}
		flagsByte, ok := pr.take(1)
		if !ok {
			return nil, errors.New("truncated payload")
		}
		size, ok := pr.int64()
		if !ok {
			return nil, errors.New("truncated payload")
		}
		mtime, ok := pr.int64()
		if !ok {
			return nil, errors.New("truncated payload")
		}
		nameLen, ok := pr.uint16()
		if !ok {
			return nil, errors.New("truncated payload")
		}
		nameBytes, ok := pr.take(int(nameLen))
		if !ok {
			return nil, errors.New("truncated payload")
		}

		if int(parent) >= i {
			// A parent id must already have been assigned; anything else
			// means the payload is corrupt in a way the checksum somehow
			// missed (or was tampered with). Bail out as stale rather than
			// mis-parenting an entry.
			return nil, errors.New("parent id out of range")
		}

		id := shard.Upsert(parent, string(nameBytes), Flags(flagsByte[0]), size, mtime)
		if int(id) != i {
			// Upsert is idempotent on (parent, name); since every (parent,
			// id) pair here is unique to this decode, id should always
			// equal the insertion index. Anything else signals a payload
			// that doesn't match what WriteTo would have produced.
			return nil, errors.New("unexpected id assigned during decode")
		}
	}

	return shard, nil
}

// byteReader is a small bounds-checked cursor over an in-memory byte slice,
// used instead of encoding/binary.Read (which needs a reflect-driven,
// per-field io.Reader call) so decoding a large payload stays a handful of
// slice operations.
type byteReader struct {
	data []byte
	pos  int
}

func (b *byteReader) take(n int) ([]byte, bool) {
	if n < 0 || b.pos+n > len(b.data) {
		return nil, false
	}
	out := b.data[b.pos : b.pos+n]
	b.pos += n
	return out, true
}

func (b *byteReader) rest() []byte {
	out := b.data[b.pos:]
	b.pos = len(b.data)
	return out
}

func (b *byteReader) uint16() (uint16, bool) {
	buf, ok := b.take(2)
	if !ok {
		return 0, false
	}
	return byteOrder.Uint16(buf), true
}

func (b *byteReader) uint32() (uint32, bool) {
	buf, ok := b.take(4)
	if !ok {
		return 0, false
	}
	return byteOrder.Uint32(buf), true
}

func (b *byteReader) uint64() (uint64, bool) {
	buf, ok := b.take(8)
	if !ok {
		return 0, false
	}
	return byteOrder.Uint64(buf), true
}

func (b *byteReader) int64() (int64, bool) {
	u, ok := b.uint64()
	return int64(u), ok
}

func writeUint32(w *bytes.Buffer, v uint32) {
	var buf [4]byte
	byteOrder.PutUint32(buf[:], v)
	w.Write(buf[:])
}

func writeUint64(w *bytes.Buffer, v uint64) {
	var buf [8]byte
	byteOrder.PutUint64(buf[:], v)
	w.Write(buf[:])
}

func appendUint16(buf []byte, v uint16) []byte {
	var tmp [2]byte
	byteOrder.PutUint16(tmp[:], v)
	return append(buf, tmp[:]...)
}

func appendUint32(buf []byte, v uint32) []byte {
	var tmp [4]byte
	byteOrder.PutUint32(tmp[:], v)
	return append(buf, tmp[:]...)
}

func appendInt64(buf []byte, v int64) []byte {
	var tmp [8]byte
	byteOrder.PutUint64(tmp[:], uint64(v))
	return append(buf, tmp[:]...)
}
