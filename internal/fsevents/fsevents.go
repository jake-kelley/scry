// Package fsevents wraps the macOS CoreServices FSEvents API just enough
// to drive the incremental index updates described in
// "everything-macos-design.md" §6. This file holds the platform-agnostic
// surface: types, flag bits, and the Stream/Config shapes every caller
// programs against. fsevents_darwin.go supplies the real cgo-backed
// implementation; fsevents_other.go supplies a no-op stand-in so
// `go build ./...` and `go test ./...` stay green on every other platform
// (Windows above all — that is where most of this project is developed).
package fsevents

import (
	"errors"
	"time"
)

// ErrNotSupported is returned by LatestEventID (and would be returned by
// NewStream, if it ever needed to fail) on a platform with no real FSEvents
// backend. NewStream itself never returns it: on an unsupported platform it
// hands back a Stream that simply never emits, so callers do not need a
// platform switch just to start the daemon.
var ErrNotSupported = errors.New("fsevents: not supported on this platform")

// EventFlags mirrors the subset of FSEventStreamEventFlags this package
// surfaces. Bit values match the CoreServices constants exactly (see
// <CoreServices/CoreServices.h>, FSEvents.h) so the darwin implementation
// can hand them straight through without translation.
type EventFlags uint32

const (
	FlagMustScanSubDirs   EventFlags = 0x00000001 // re-enumerate this subtree; some events below it were coalesced away
	FlagUserDropped       EventFlags = 0x00000002 // kernel dropped events because we (the client) were too slow; implies MustScanSubDirs
	FlagKernelDropped     EventFlags = 0x00000004 // kernel dropped events for its own reasons; implies MustScanSubDirs
	FlagEventIdsWrapped   EventFlags = 0x00000008 // the 64-bit event id counter wrapped; any stored id is meaningless
	FlagHistoryDone       EventFlags = 0x00000010 // marks the end of replayed history; events after this are live
	FlagRootChanged       EventFlags = 0x00000020 // something changed along the path to one of the watched roots
	FlagMount             EventFlags = 0x00000040 // a volume was mounted at this path
	FlagUnmount           EventFlags = 0x00000080 // a volume was unmounted at this path
	FlagItemCreated       EventFlags = 0x00000100
	FlagItemRemoved       EventFlags = 0x00000200
	FlagItemInodeMetaMod  EventFlags = 0x00000400
	FlagItemRenamed       EventFlags = 0x00000800
	FlagItemModified      EventFlags = 0x00001000
	FlagItemFinderInfoMod EventFlags = 0x00002000
	FlagItemChangeOwner   EventFlags = 0x00004000
	FlagItemXattrMod      EventFlags = 0x00008000
	FlagItemIsFile        EventFlags = 0x00010000
	FlagItemIsDir         EventFlags = 0x00020000
	FlagItemIsSymlink     EventFlags = 0x00040000
	FlagOwnEvent          EventFlags = 0x00080000
)

// Has reports whether bit is set in f.
func (f EventFlags) Has(bit EventFlags) bool { return f&bit != 0 }

// Event is one FSEvents notification, translated off the CFRunLoop callback
// thread and onto a plain Go channel.
type Event struct {
	Path  string // absolute path of the affected file or directory
	ID    uint64 // FSEvents event id; callers persist this as Shard.lastEID
	Flags EventFlags
}

// Config configures a Stream. It mirrors the parameters named in
// "everything-macos-design.md" §6's example, so a caller can build one
// straight from a RootManager.
type Config struct {
	Paths        []string      // roots to watch; one combined stream covers all of them
	Latency      time.Duration // FSEventStreamCreate's latency parameter: coalescing window
	SinceEventID uint64        // resume point; 0 means "since now" (see LatestEventID)
	FileEvents   bool          // kFSEventStreamCreateFlagFileEvents: per-file, not per-directory
	WatchRoot    bool          // kFSEventStreamCreateFlagWatchRoot: deliver RootChanged
	NoDefer      bool          // kFSEventStreamCreateFlagNoDefer: report the first event immediately
}

// Stream is a running FSEvents watch over Config.Paths. Events arrive on
// the channel Events returns until Stop is called, after which that
// channel is closed. The callback that produces events runs on a
// dedicated CFRunLoop thread inside the darwin implementation and does no
// work beyond handing the event to Go and returning; Events() itself is
// always safe to range over from any goroutine.
type Stream interface {
	Events() <-chan Event
	Stop()
}
