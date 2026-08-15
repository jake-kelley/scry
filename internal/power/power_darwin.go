//go:build darwin

// This file is one of the project's cgo boundaries (§8 item 5 of the
// design doc). It calls IOKit's system-power notification API directly:
// register a callback with IORegisterForSystemPower, acknowledge the two
// sleep-phase messages immediately (kIOMessageCanSystemSleep,
// kIOMessageSystemWillSleep — this package never vetoes or delays a sleep
// the user asked for), and fire the Go side only on
// kIOMessageSystemHasPoweredOn.
//
// Callbacks are delivered on a private serial dispatch queue rather than a
// CFRunLoop, the same choice internal/fsevents/fsevents_darwin.go made and
// explains in its own package doc: IONotificationPortSetDispatchQueue is
// the modern equivalent of scheduling a run loop source, and taking it
// avoids everything a run loop needs to actually be pumped — no dedicated
// thread, no attaching to (and thereby competing for) the main run loop
// the menu bar's systray.Run() owns for AppKit (§7's "single biggest
// structural consequence"; internal/panel_darwin.go's SIGTRAP history is
// exactly what assuming the wrong thread costs here). A serial queue gives
// the same "callbacks never overlap" guarantee a run loop would, without
// owning a thread or touching thread 1.
package power

/*
#cgo LDFLAGS: -framework IOKit
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <IOKit/IOMessage.h>
#include <dispatch/dispatch.h>

extern void goPowerWake(unsigned long long handle);
extern io_connect_t goPowerRootPort(unsigned long long handle);

// power_callback is IORegisterForSystemPower's callback. It only ever does
// two things: acknowledge a sleep-phase message so this process never
// blocks or vetoes a sleep, and hand a wake notification off to Go. No
// index or channel work happens on this side of the boundary. refCon is
// the handle NewNotifier registered with, unpacked back into the
// io_connect_t IOAllowPowerChange needs via the Go-side registry — see
// goPowerRootPort.
static void power_callback(void *refCon, io_service_t service, natural_t messageType, void *messageArgument) {
    unsigned long long handle = (unsigned long long)(uintptr_t)refCon;

    switch (messageType) {
    case kIOMessageCanSystemSleep:
    case kIOMessageSystemWillSleep:
        IOAllowPowerChange(goPowerRootPort(handle), (long)messageArgument);
        break;
    case kIOMessageSystemHasPoweredOn:
        goPowerWake(handle);
        break;
    default:
        break;
    }
}

// power_register_result bundles everything power_teardown later needs to
// unwind, since IORegisterForSystemPower hands back three separate
// out-parameters and cgo cannot return them as one Go multi-value.
typedef struct {
    io_connect_t root_port;
    IONotificationPortRef notify_port;
    io_object_t notifier;
} power_register_result;

// power_register installs the callback, keyed by handle, and schedules its
// notification port on q.
static power_register_result power_register(unsigned long long handle, dispatch_queue_t q) {
    power_register_result result = {0, NULL, 0};

    IONotificationPortRef notifyPort = NULL;
    io_object_t notifier = 0;
    void *refCon = (void *)(uintptr_t)handle;
    io_connect_t root_port = IORegisterForSystemPower(refCon, &notifyPort, power_callback, &notifier);
    if (root_port == 0 || notifyPort == NULL) {
        return result;
    }

    IONotificationPortSetDispatchQueue(notifyPort, q);

    result.root_port = root_port;
    result.notify_port = notifyPort;
    result.notifier = notifier;
    return result;
}

static dispatch_queue_t power_queue_create(void) {
    return dispatch_queue_create("dev.scry.power", DISPATCH_QUEUE_SERIAL);
}

// power_queue_release releases a queue that never made it into a
// registration (power_register failed): dispatch_release takes
// dispatch_object_t, a type cgo maps differently from dispatch_queue_t, so
// the release has to happen on this side of the boundary rather than as a
// direct C.dispatch_release call from Go.
static void power_queue_release(dispatch_queue_t q) {
    dispatch_release(q);
}

// power_teardown stops the notification port and returns only once no
// callback can still be running, the same dispatch_sync-of-an-empty-block
// trick fsevents_darwin.go's fsevents_teardown uses and explains: the
// queue is serial, so a block submitted after the port is scheduled off
// cannot begin until any in-flight callback has finished.
static void power_teardown(io_connect_t root_port, IONotificationPortRef notify_port, io_object_t notifier, dispatch_queue_t q) {
    dispatch_sync(q, ^{});
    IODeregisterForSystemPower(&notifier);
    IOServiceClose(root_port);
    IONotificationPortDestroy(notify_port);
    dispatch_release(q);
}
*/
import "C"

import "sync"

// Supported reports whether this platform has a real IOKit power-
// management backend.
const Supported = true

// registration is what the Go-side registry keeps per handle: the channel
// wake events land on, and the io_connect_t the callback needs to
// acknowledge a sleep-phase message via IOAllowPowerChange.
type registration struct {
	events   chan<- struct{}
	rootPort C.io_connect_t
}

// registry maps the opaque uint64 handle IORegisterForSystemPower's refCon
// carries back to the Go state a given notifier's callback needs. A
// registry (rather than a single global) mirrors internal/fsevents': more
// than one Notifier could in principle exist across the process's
// lifetime, even though cmd/scry only ever starts one.
var (
	registryMu sync.Mutex
	registry   = make(map[uint64]registration)
	nextHandle uint64
)

//export goPowerWake
func goPowerWake(handle C.ulonglong) {
	registryMu.Lock()
	reg, ok := registry[uint64(handle)]
	registryMu.Unlock()
	if !ok {
		return
	}
	// Non-blocking: wake notifications are rare and the Coordinator only
	// needs to know "at least one wake happened since it last looked",
	// which OnWake's own debounce already collapses. Blocking here would
	// tie up the IOKit callback (and, transitively, the sleep/wake
	// notification pipeline for every other subscriber) if the consumer
	// goroutine were ever slow to drain — worse than dropping a
	// duplicate signal the debounce would have discarded anyway.
	select {
	case reg.events <- struct{}{}:
	default:
	}
}

//export goPowerRootPort
func goPowerRootPort(handle C.ulonglong) C.io_connect_t {
	registryMu.Lock()
	reg, ok := registry[uint64(handle)]
	registryMu.Unlock()
	if !ok {
		return 0
	}
	return reg.rootPort
}

// darwinNotifier is the real, cgo-backed Notifier implementation.
type darwinNotifier struct {
	handle uint64
	events chan struct{}

	rootPort   C.io_connect_t
	notifyPort C.IONotificationPortRef
	notifier   C.io_object_t
	queue      C.dispatch_queue_t

	stopOnce sync.Once
}

// NewNotifier registers for macOS system power notifications and starts
// delivering a signal on the returned Notifier's Events channel each time
// the system wakes from sleep.
func NewNotifier() (Notifier, error) {
	registryMu.Lock()
	nextHandle++
	handle := nextHandle
	events := make(chan struct{}, 4)
	registryMu.Unlock()

	queue := C.power_queue_create()
	result := C.power_register(C.ulonglong(handle), queue)
	if result.root_port == 0 || result.notify_port == nil {
		C.power_queue_release(queue)
		return nil, ErrNotSupported
	}

	// Registered only once the C call that can actually fire callbacks
	// has succeeded, so a failed registration never leaves an orphaned
	// registry entry behind.
	registryMu.Lock()
	registry[handle] = registration{events: events, rootPort: result.root_port}
	registryMu.Unlock()

	return &darwinNotifier{
		handle:     handle,
		events:     events,
		rootPort:   result.root_port,
		notifyPort: result.notify_port,
		notifier:   result.notifier,
		queue:      queue,
	}, nil
}

func (n *darwinNotifier) Events() <-chan struct{} { return n.events }

// Stop tears the registration down and closes the events channel. Safe to
// call more than once and from any goroutine.
func (n *darwinNotifier) Stop() {
	n.stopOnce.Do(func() {
		C.power_teardown(n.rootPort, n.notifyPort, n.notifier, n.queue)

		registryMu.Lock()
		delete(registry, n.handle)
		registryMu.Unlock()

		close(n.events)
	})
}
