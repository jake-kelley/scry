// Package index implements per-root shards: the in-memory index of a single
// indexed directory tree, laid out as described in "everything-macos-design.md" §4.
//
// A Shard never stores full paths. Each entry is a lowercased name in a shared
// byte arena plus a parent pointer; paths are reconstructed on demand only for
// the handful of results actually displayed. This keeps ~250k entries in
// ~15MB and lets fuzzy search scan one contiguous byte slice instead of
// chasing hundreds of thousands of string pointers.
package index

import "sync"

// Flags records per-entry boolean attributes in a single byte.
type Flags uint8

const (
	FlagDir Flags = 1 << iota
	FlagSymlink
	FlagHidden
)

// Entry is the caller-facing view of one indexed file or directory.
type Entry struct {
	ID    uint32
	Name  string // original case
	IsDir bool
	Size  int64
	MTime int64 // unix nanos
}

// offsetEntry maps a byte range in the name arena back to the id that
// currently occupies it. Entries are appended in strictly increasing order
// of start (the arena is append-only until Compact), so the slice stays
// sorted by construction and EntryAt can binary search it.
type offsetEntry struct {
	start uint32
	end   uint32 // exclusive, before the NUL separator
	id    uint32
}

// Shard is the complete index for one configured root. All exported methods
// are safe for concurrent use: reads take an RLock, writes take a Lock.
type Shard struct {
	mu sync.RWMutex

	root   string
	rootID uint32
	online bool

	// Hot arrays — the only memory touched during a search.
	names   []byte   // names concatenated, lowercased, NUL-separated
	nameOff []uint32 // per id: offset into names
	nameLen []uint16 // per id: length of the lowercased name, excluding the NUL

	parent []uint32 // per id: parent id; the root's parent is itself
	flags  []uint8  // per id: Flags bits

	// Cold arrays — parallel, read only for entries in the result set.
	size  []int64
	mtime []int64

	// origName holds the original-case name for each id, parallel to the
	// arrays above. The arena only ever holds the lowercased form.
	origName []string

	dead []bool // per id: tombstoned

	children   map[uint32][]uint32          // directories only, and only if non-empty
	childIndex map[uint32]map[string]uint32 // parent id -> lowercased name -> child id, for idempotent Upsert

	offsetIndex []offsetEntry // sorted by start; used by EntryAt

	free []uint32 // tombstoned slots awaiting reuse

	len     int    // live entry count
	lastEID uint64 // FSEvents id this shard is current as of
}

// New creates a Shard for root. Entry 0 is the root itself; its parent is
// itself, and Path(RootID()) == root.
func New(root string) *Shard {
	s := &Shard{
		root:   root,
		rootID: 0,
		online: true,
	}
	// Manually install the root entry rather than going through Upsert,
	// since the root has no parent to be a child of.
	off := len(s.names)
	s.names = append(s.names, 0) // root has an empty lowercased name in the arena
	s.nameOff = append(s.nameOff, uint32(off))
	s.nameLen = append(s.nameLen, 0)
	s.parent = append(s.parent, 0)
	s.flags = append(s.flags, uint8(FlagDir))
	s.size = append(s.size, 0)
	s.mtime = append(s.mtime, 0)
	s.origName = append(s.origName, "")
	s.dead = append(s.dead, false)
	s.offsetIndex = append(s.offsetIndex, offsetEntry{start: uint32(off), end: uint32(off), id: 0})
	s.len = 1
	return s
}

// Root returns the root path this shard was created with.
func (s *Shard) Root() string {
	return s.root
}

// RootID returns the id of the root entry.
func (s *Shard) RootID() uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rootID
}

// Len returns the number of live (non-tombstoned) entries.
func (s *Shard) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.len
}

// Online reports whether the shard's root volume is currently mounted.
func (s *Shard) Online() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.online
}

// SetOnline marks the shard's root volume mounted or unmounted. Going
// offline never drops entries; it only hides the shard from results.
func (s *Shard) SetOnline(online bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.online = online
}
