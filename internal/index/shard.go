package index

import (
	"path/filepath"
	"sort"
	"strings"
)

// allocSlot returns an id ready to be filled in, reusing a tombstoned slot
// from the free list when one is available. Caller holds the write lock.
func (s *Shard) allocSlot() uint32 {
	if n := len(s.free); n > 0 {
		id := s.free[n-1]
		s.free = s.free[:n-1]
		return id
	}
	id := uint32(len(s.dead))
	s.nameOff = append(s.nameOff, 0)
	s.nameLen = append(s.nameLen, 0)
	s.parent = append(s.parent, 0)
	s.flags = append(s.flags, 0)
	s.size = append(s.size, 0)
	s.mtime = append(s.mtime, 0)
	s.origName = append(s.origName, "")
	s.dead = append(s.dead, false)
	return id
}

// Upsert inserts or updates a child of parent named name. It is idempotent:
// a second call with the same (parent, name) updates size/mtime/flags on the
// existing entry and returns its id, rather than creating a duplicate.
func (s *Shard) Upsert(parent uint32, name string, flags Flags, size, mtime int64) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if int(parent) >= len(s.dead) || s.dead[parent] {
		parent = s.rootID
	}

	lower := strings.ToLower(name)

	if m, ok := s.childIndex[parent]; ok {
		if id, ok := m[lower]; ok {
			s.flags[id] = uint8(flags)
			s.size[id] = size
			s.mtime[id] = mtime
			s.origName[id] = name
			return id
		}
	}

	id := s.allocSlot()

	off := len(s.names)
	s.names = append(s.names, lower...)
	s.names = append(s.names, 0)

	s.nameOff[id] = uint32(off)
	s.nameLen[id] = uint16(len(lower))
	s.parent[id] = parent
	s.flags[id] = uint8(flags)
	s.size[id] = size
	s.mtime[id] = mtime
	s.origName[id] = name
	s.dead[id] = false

	s.offsetIndex = append(s.offsetIndex, offsetEntry{
		start: uint32(off),
		end:   uint32(off + len(lower)),
		id:    id,
	})

	s.len++

	if s.children == nil {
		s.children = make(map[uint32][]uint32)
	}
	if s.childIndex == nil {
		s.childIndex = make(map[uint32]map[string]uint32)
	}
	s.children[parent] = append(s.children[parent], id)
	m := s.childIndex[parent]
	if m == nil {
		m = make(map[string]uint32)
		s.childIndex[parent] = m
	}
	m[lower] = id

	return id
}

// Remove tombstones id and its entire subtree, recursively. Freed slots are
// pushed onto the free list for reuse by later Upserts; ids never shift.
func (s *Shard) Remove(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeRecursive(id)
}

// removeRecursive tombstones id, first recursing into any children. Caller
// holds the write lock.
func (s *Shard) removeRecursive(id uint32) {
	if int(id) >= len(s.dead) || s.dead[id] {
		return
	}

	if kids, ok := s.children[id]; ok {
		// Copy: removeChildRef below mutates s.children[id] while we range.
		kidsCopy := append([]uint32(nil), kids...)
		for _, c := range kidsCopy {
			s.removeRecursive(c)
		}
		delete(s.children, id)
		delete(s.childIndex, id)
	}

	s.dead[id] = true
	s.len--
	s.free = append(s.free, id)

	if p := s.parent[id]; p != id {
		s.removeChildRef(p, id)
	}
}

// removeChildRef removes id from parent p's children/childIndex. Caller
// holds the write lock.
func (s *Shard) removeChildRef(p, id uint32) {
	lower := s.lowerNameOf(id)

	if kids, ok := s.children[p]; ok {
		for i, c := range kids {
			if c == id {
				kids = append(kids[:i], kids[i+1:]...)
				break
			}
		}
		if len(kids) == 0 {
			delete(s.children, p)
		} else {
			s.children[p] = kids
		}
	}
	if m, ok := s.childIndex[p]; ok {
		delete(m, lower)
		if len(m) == 0 {
			delete(s.childIndex, p)
		}
	}
}

// lowerNameOf returns the lowercased name currently stored for id, read
// straight from the arena. Caller holds a lock.
func (s *Shard) lowerNameOf(id uint32) string {
	off := s.nameOff[id]
	n := s.nameLen[id]
	return string(s.names[off : off+uint32(n)])
}

// Get returns the entry for id, or the zero value and false if id is unknown
// or has been removed.
func (s *Shard) Get(id uint32) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if int(id) >= len(s.dead) || s.dead[id] {
		return Entry{}, false
	}
	return Entry{
		ID:    id,
		Name:  s.origName[id],
		IsDir: Flags(s.flags[id])&FlagDir != 0,
		Size:  s.size[id],
		MTime: s.mtime[id],
	}, true
}

// Path reconstructs the full path of id by walking parent pointers up to the
// root. Returns "" if id is unknown or has been removed.
func (s *Shard) Path(id uint32) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if int(id) >= len(s.dead) || s.dead[id] {
		return ""
	}

	if id == s.rootID {
		return s.root
	}

	var parts []string
	cur := id
	for cur != s.rootID {
		if s.dead[cur] {
			return ""
		}
		parts = append(parts, s.origName[cur])
		cur = s.parent[cur]
	}

	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}

	// filepath.Join uses the OS separator consistently and cleans the
	// result, so a root ending in a trailing separator (e.g. `C:\Users\` on
	// Windows) never produces a mixed-separator path like
	// `C:\Users/foo/bar`. Only called for the ~200 displayed results, not
	// every entry, so the extra allocation here is not a hot-path concern.
	return filepath.Join(append([]string{s.root}, parts...)...)
}

// Children returns the ids of the direct children of id. Returns nil for a
// leaf, an unknown id, or a removed id.
func (s *Shard) Children(id uint32) []uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if int(id) >= len(s.dead) || s.dead[id] {
		return nil
	}
	kids := s.children[id]
	if len(kids) == 0 {
		return nil
	}
	return append([]uint32(nil), kids...)
}

// Arena returns the raw name arena: lowercased names, NUL-separated. The
// returned slice is valid only as long as the caller does not concurrently
// hold or take a write lock on the shard (i.e. no Upsert/Remove/Compact runs
// while it is in use) — callers that keep it around across calls into the
// shard must copy it first.
func (s *Shard) Arena() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.names
}

// NameRange returns the byte offset and length of id's name within Arena().
// Returns (0, 0) if id is unknown or has been removed.
func (s *Shard) NameRange(id uint32) (off, length uint32) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if int(id) >= len(s.dead) || s.dead[id] {
		return 0, 0
	}
	return s.nameOff[id], uint32(s.nameLen[id])
}

// EntryAt maps an arena byte offset to the id whose name covers it, via
// binary search over the offset table. Returns false for an offset that
// falls on a NUL separator, past the end of the arena, or on a dead entry.
func (s *Shard) EntryAt(off uint32) (uint32, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	idx := sort.Search(len(s.offsetIndex), func(i int) bool {
		return s.offsetIndex[i].start > off
	})
	if idx == 0 {
		return 0, false
	}
	cand := s.offsetIndex[idx-1]
	if off < cand.start || off >= cand.end {
		return 0, false
	}
	if int(cand.id) >= len(s.dead) || s.dead[cand.id] {
		return 0, false
	}
	// The offset table entry may be stale: id may have been tombstoned and
	// its slot reused for a different name at a different offset. Only a
	// match against the id's *current* nameOff is a live hit.
	if s.nameOff[cand.id] != cand.start {
		return 0, false
	}
	return cand.id, true
}

// Compact rebuilds the arena and all parallel arrays, dropping tombstones
// and remapping ids and parent pointers consistently. Children lists survive
// the remap. Never call this from a query path; it takes the write lock for
// its full duration.
func (s *Shard) Compact() {
	s.mu.Lock()
	defer s.mu.Unlock()

	aliveOld := make([]uint32, 0, s.len)
	for i, dead := range s.dead {
		if !dead {
			aliveOld = append(aliveOld, uint32(i))
		}
	}

	oldToNew := make([]uint32, len(s.dead))
	for newID, oldID := range aliveOld {
		oldToNew[oldID] = uint32(newID)
	}

	n := len(aliveOld)
	newNames := make([]byte, 0, len(s.names))
	newOffsetIndex := make([]offsetEntry, 0, n)
	newNameOff := make([]uint32, n)
	newNameLen := make([]uint16, n)
	newOrigName := make([]string, n)
	newParent := make([]uint32, n)
	newFlags := make([]uint8, n)
	newSize := make([]int64, n)
	newMtime := make([]int64, n)
	newDead := make([]bool, n)
	newChildren := make(map[uint32][]uint32)

	for newID, oldID := range aliveOld {
		lowerOff := s.nameOff[oldID]
		lowerLen := s.nameLen[oldID]
		lower := s.names[lowerOff : lowerOff+uint32(lowerLen)]

		off := len(newNames)
		newNames = append(newNames, lower...)
		newNames = append(newNames, 0)

		newNameOff[newID] = uint32(off)
		newNameLen[newID] = lowerLen
		newOrigName[newID] = s.origName[oldID]
		newParent[newID] = oldToNew[s.parent[oldID]]
		newFlags[newID] = s.flags[oldID]
		newSize[newID] = s.size[oldID]
		newMtime[newID] = s.mtime[oldID]
		newDead[newID] = false

		newOffsetIndex = append(newOffsetIndex, offsetEntry{
			start: uint32(off),
			end:   uint32(off + len(lower)),
			id:    uint32(newID),
		})

		if kids, ok := s.children[oldID]; ok {
			remapped := make([]uint32, 0, len(kids))
			for _, c := range kids {
				if !s.dead[c] {
					remapped = append(remapped, oldToNew[c])
				}
			}
			if len(remapped) > 0 {
				newChildren[uint32(newID)] = remapped
			}
		}
	}

	newChildIndex := make(map[uint32]map[string]uint32, len(newChildren))
	for parentID, kids := range newChildren {
		m := make(map[string]uint32, len(kids))
		for _, c := range kids {
			off := newNameOff[c]
			l := newNameLen[c]
			m[string(newNames[off:off+uint32(l)])] = c
		}
		newChildIndex[parentID] = m
	}

	s.names = newNames
	s.nameOff = newNameOff
	s.nameLen = newNameLen
	s.origName = newOrigName
	s.parent = newParent
	s.flags = newFlags
	s.size = newSize
	s.mtime = newMtime
	s.dead = newDead
	s.children = newChildren
	s.childIndex = newChildIndex
	s.offsetIndex = newOffsetIndex
	s.free = nil
	s.rootID = oldToNew[s.rootID]
	s.len = n
}
