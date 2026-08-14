//go:build darwin

// This file is one of the project's three cgo boundaries (§8 item 5 of the
// design doc). The C surface is kept deliberately tiny: create a stream,
// attach it to a serial dispatch queue, start it, and tear it down.
// Everything else — routing events to shards, exclude rules, rescans,
// offline handling — is plain Go in internal/watcher and never touches cgo.
//
// The stream is driven by a dispatch queue rather than a CFRunLoop.
// FSEventStreamScheduleWithRunLoop has been deprecated since macOS 13 in
// favour of FSEventStreamSetDispatchQueue, and taking the replacement
// deletes a surprising amount of machinery: no dedicated thread, no
// runtime.LockOSThread, no waiting for CFRunLoopRun to be entered before
// NewStream may return, and no retry loop around CFRunLoopStop to close the
// race where a Stop arriving before the run loop started would land as a
// documented no-op and hang forever. A serial queue gives the same
// guarantee that made a run loop attractive — callbacks never overlap — and
// gives it without owning a thread.
package fsevents

/*
#cgo LDFLAGS: -framework CoreServices
#cgo CFLAGS: -fblocks

#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <stdlib.h>

// goFSEventsCallback is implemented in Go (see the //export below) and
// receives every batch of events FSEvents delivers to our stream.
extern void goFSEventsCallback(unsigned long long handle, char **paths, unsigned long long *eventIDs, unsigned int *flags, int numEvents);

// fsevents_callback is the actual C callback FSEventStreamCreate is given.
// It does nothing but reshape argument types and hand off to Go — no
// index or channel work happens on this side of the boundary.
static void fsevents_callback(ConstFSEventStreamRef streamRef,
                               void *clientCallBackInfo,
                               size_t numEvents,
                               void *eventPaths,
                               const FSEventStreamEventFlags eventFlags[],
                               const FSEventStreamEventId eventIds[]) {
    char **paths = (char **)eventPaths;
    goFSEventsCallback((unsigned long long)(uintptr_t)clientCallBackInfo,
                        paths,
                        (unsigned long long *)eventIds,
                        (unsigned int *)eventFlags,
                        (int)numEvents);
}

// fsevents_create builds a stream over numPaths paths, starting at
// sinceWhen, coalescing within latencySeconds, with flags as given by the
// Go side. handle is an opaque identifier (see the Go-side registry) that
// comes back unchanged as clientCallBackInfo on every callback.
static FSEventStreamRef fsevents_create(unsigned long long handle, char **paths, int numPaths,
                                         unsigned long long sinceWhen, double latencySeconds,
                                         unsigned int flags) {
    CFMutableArrayRef pathsArray = CFArrayCreateMutable(NULL, numPaths, &kCFTypeArrayCallBacks);
    for (int i = 0; i < numPaths; i++) {
        CFStringRef s = CFStringCreateWithCString(NULL, paths[i], kCFStringEncodingUTF8);
        CFArrayAppendValue(pathsArray, s);
        CFRelease(s);
    }

    FSEventStreamContext context;
    context.version = 0;
    context.info = (void *)(uintptr_t)handle;
    context.retain = NULL;
    context.release = NULL;
    context.copyDescription = NULL;

    FSEventStreamRef stream = FSEventStreamCreate(NULL, &fsevents_callback, &context, pathsArray,
                                                   (FSEventStreamEventId)sinceWhen, latencySeconds,
                                                   (FSEventStreamCreateFlags)flags);
    CFRelease(pathsArray);
    return stream;
}

// fsevents_queue_create makes the serial queue the stream's callbacks run
// on. Serial is the load-bearing part: it is what guarantees callbacks
// never overlap, which the Go side relies on when it tears the stream down.
static dispatch_queue_t fsevents_queue_create(void) {
    return dispatch_queue_create("dev.scry.fsevents", DISPATCH_QUEUE_SERIAL);
}

static void fsevents_start(FSEventStreamRef stream, dispatch_queue_t q) {
    FSEventStreamSetDispatchQueue(stream, q);
    FSEventStreamStart(stream);
}

// fsevents_teardown stops the stream and returns only once no callback can
// still be running. Unlike the run-loop API this is safe to call from any
// thread.
//
// The dispatch_sync of an empty block is the whole trick and is not
// decoration: FSEventStreamInvalidate stops future callbacks from being
// enqueued but says nothing about one already executing. Because the queue
// is serial, a block submitted after invalidate cannot begin until any
// in-flight callback has finished, so waiting for that block to run is
// exactly "no callback is in flight any more" — which is what lets the Go
// side close the events channel without racing a send on it.
static void fsevents_teardown(FSEventStreamRef stream, dispatch_queue_t q) {
    FSEventStreamStop(stream);
    FSEventStreamInvalidate(stream);
    FSEventStreamRelease(stream);
    dispatch_sync(q, ^{});
    dispatch_release(q);
}
*/
import "C"

import (
	"sync"
	"unsafe"
)

// Supported reports whether this platform has a real FSEvents backend.
const Supported = true

// registry maps the opaque uint64 handle passed through C's
// clientCallBackInfo back to the Go channel a given stream's events should
// land on. A registry (rather than a single global) lets more than one
// stream exist across the process's lifetime — internal/watcher recreates
// its stream whenever the configured root set changes, per §6.
var (
	registryMu sync.Mutex
	registry   = make(map[uint64]chan<- Event)
	nextHandle uint64
)

//export goFSEventsCallback
func goFSEventsCallback(handle C.ulonglong, paths **C.char, eventIDs *C.ulonglong, flags *C.uint, numEvents C.int) {
	registryMu.Lock()
	ch, ok := registry[uint64(handle)]
	registryMu.Unlock()
	if !ok {
		return
	}

	n := int(numEvents)
	pathSlice := unsafe.Slice(paths, n)
	idSlice := unsafe.Slice(eventIDs, n)
	flagSlice := unsafe.Slice(flags, n)

	for i := 0; i < n; i++ {
		ev := Event{
			Path:  C.GoString(pathSlice[i]),
			ID:    uint64(idSlice[i]),
			Flags: EventFlags(flagSlice[i]),
		}
		// Blocking send is deliberate backpressure, not a bug: FSEvents
		// itself already buffers and coalesces on the kernel side, so a
		// slow consumer delays the next callback invocation rather than
		// losing anything. Doing index work here instead would tie up
		// the dispatch queue and risk the kernel deciding we're too
		// slow (UserDropped), which is strictly worse.
		ch <- ev
	}
}

// darwinStream is the real, cgo-backed Stream implementation.
type darwinStream struct {
	handle uint64
	events chan Event

	stream C.FSEventStreamRef
	queue  C.dispatch_queue_t

	stopOnce sync.Once
	done     chan struct{}
}

// NewStream starts a single combined FSEvents stream over cfg.Paths.
// Callbacks are delivered on a private serial dispatch queue, which coexists
// with an NSApplication run loop on the main thread rather than competing
// with it (§9 item 7) and, unlike the deprecated run-loop scheduling, needs
// no thread of its own.
func NewStream(cfg Config) (Stream, error) {
	sinceWhen := cfg.SinceEventID
	if sinceWhen == 0 {
		// 0 is not "since now" to FSEvents — it can trigger a very large
		// historical replay. Callers that mean "no prior position" should
		// resolve LatestEventID() themselves (see internal/watcher), but
		// fall back to it here too so a caller that forgets never pays
		// for a full-volume history replay by accident.
		if id, err := LatestEventID(); err == nil {
			sinceWhen = id
		}
	}

	var flags C.uint
	if cfg.FileEvents {
		flags |= C.kFSEventStreamCreateFlagFileEvents
	}
	if cfg.WatchRoot {
		flags |= C.kFSEventStreamCreateFlagWatchRoot
	}
	if cfg.NoDefer {
		flags |= C.kFSEventStreamCreateFlagNoDefer
	}

	cPaths := make([]*C.char, len(cfg.Paths))
	for i, p := range cfg.Paths {
		cPaths[i] = C.CString(p)
	}
	defer func() {
		for _, p := range cPaths {
			C.free(unsafe.Pointer(p))
		}
	}()

	var cPathsPtr **C.char
	if len(cPaths) > 0 {
		cPathsPtr = &cPaths[0]
	}

	registryMu.Lock()
	nextHandle++
	handle := nextHandle
	events := make(chan Event, 4096)
	registry[handle] = events
	registryMu.Unlock()

	s := &darwinStream{
		handle: handle,
		events: events,
		done:   make(chan struct{}),
	}

	// Registered before the stream can fire, so the first callback always
	// finds its channel.
	s.stream = C.fsevents_create(C.ulonglong(handle), cPathsPtr, C.int(len(cPaths)),
		C.ulonglong(sinceWhen), C.double(cfg.Latency.Seconds()), flags)
	s.queue = C.fsevents_queue_create()
	C.fsevents_start(s.stream, s.queue)

	return s, nil
}

func (s *darwinStream) Events() <-chan Event { return s.events }

// Stop tears the stream down and closes the events channel. It blocks until
// no callback can still be running, and is safe to call more than once and
// from any thread — including immediately after NewStream, which the
// run-loop implementation this replaced could not survive.
func (s *darwinStream) Stop() {
	s.stopOnce.Do(func() {
		// Tear down with a reader still running. A callback blocked on the
		// deliberate backpressure send into s.events occupies the serial
		// queue, and fsevents_teardown's dispatch_sync waits for exactly
		// that queue — so without draining here, a Stop issued while the
		// consumer has stopped reading would deadlock against this
		// package's own backpressure.
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for range s.events {
			}
		}()

		C.fsevents_teardown(s.stream, s.queue)

		registryMu.Lock()
		delete(registry, s.handle)
		registryMu.Unlock()

		// Safe: fsevents_teardown returned, so no callback is in flight and
		// no further one can be enqueued.
		close(s.events)
		<-drained

		close(s.done)
	})
	<-s.done
}

// LatestEventID returns the current, host-global FSEvents id: the position
// a caller should resume from when it has no prior lastEID of its own
// (e.g. a shard that was only ever crawled, never watched) rather than
// literal 0, which risks a large historical replay.
func LatestEventID() (uint64, error) {
	return uint64(C.FSEventsGetCurrentEventId()), nil
}
