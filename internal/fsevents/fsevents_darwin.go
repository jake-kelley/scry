//go:build darwin

// This file is the project's one cgo boundary (§8 item 5 of the design
// doc). The C surface is kept deliberately tiny: create a stream, schedule
// and start it on a run loop owned by a dedicated goroutine, and stop it.
// Everything else — routing events to shards, exclude rules, rescans,
// offline handling — is plain Go in internal/watcher and never touches cgo.
package fsevents

/*
#cgo LDFLAGS: -framework CoreServices

#include <CoreServices/CoreServices.h>
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

static void fsevents_schedule_and_start(FSEventStreamRef stream, CFRunLoopRef rl) {
    FSEventStreamScheduleWithRunLoop(stream, rl, kCFRunLoopDefaultMode);
    FSEventStreamStart(stream);
}

// fsevents_stop_and_invalidate must run on the same thread the stream was
// scheduled on, after CFRunLoopRun has returned there — see (*darwinStream).Stop.
static void fsevents_stop_and_invalidate(FSEventStreamRef stream) {
    FSEventStreamStop(stream);
    FSEventStreamInvalidate(stream);
    FSEventStreamRelease(stream);
}
*/
import "C"

import (
	"runtime"
	"sync"
	"time"
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
		// the CFRunLoop thread and risk the kernel deciding we're too
		// slow (UserDropped), which is strictly worse.
		ch <- ev
	}
}

// darwinStream is the real, cgo-backed Stream implementation.
type darwinStream struct {
	handle uint64
	events chan Event

	rl     C.CFRunLoopRef
	ready  chan struct{}
	stream C.FSEventStreamRef

	stopOnce sync.Once
	done     chan struct{}
}

// NewStream starts a single combined FSEvents stream over cfg.Paths. The
// callback runs on a dedicated CFRunLoop thread (per §9 item 7: this
// coexists with an NSApplication run loop on the main thread, it does not
// compete with it) that this call spins up and that lives until Stop is
// called.
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
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
	}

	latencySeconds := cfg.Latency.Seconds()

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		s.stream = C.fsevents_create(C.ulonglong(handle), cPathsPtr, C.int(len(cPaths)),
			C.ulonglong(sinceWhen), C.double(latencySeconds), flags)
		s.rl = C.CFRunLoopGetCurrent()
		close(s.ready)

		C.fsevents_schedule_and_start(s.stream, s.rl)
		C.CFRunLoopRun() // blocks until Stop calls CFRunLoopStop from elsewhere

		C.fsevents_stop_and_invalidate(s.stream)

		registryMu.Lock()
		delete(registry, handle)
		registryMu.Unlock()
		close(s.events)

		close(s.done)
	}()

	<-s.ready
	return s, nil
}

func (s *darwinStream) Events() <-chan Event { return s.events }

// Stop asks the stream's run loop to exit; the goroutine started in
// NewStream then invalidates and releases the stream from the same thread
// it was scheduled on, exactly as CoreServices expects, and closes the
// events channel once no more callbacks can fire. Stop blocks until that
// teardown has finished.
//
// CFRunLoopStop is a documented no-op if the target run loop is not
// actually inside CFRunLoopRun yet — and NewStream returns as soon as the
// stream is created, not once CFRunLoopRun has been entered on its
// goroutine. A Stop called immediately after NewStream can therefore race
// a CFRunLoopRun that has not started, land as a no-op, and hang forever
// waiting on s.done. Retrying the stop signal on a short tick until the
// run loop actually exits closes that window deterministically instead of
// depending on timing.
func (s *darwinStream) Stop() {
	s.stopOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(2 * time.Millisecond)
			defer ticker.Stop()
			for {
				C.CFRunLoopStop(s.rl)
				select {
				case <-s.done:
					return
				case <-ticker.C:
				}
			}
		}()
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
