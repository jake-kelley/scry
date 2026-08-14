//go:build darwin

// This file is the project's second cgo boundary, alongside
// internal/fsevents (§8 item 5 names FSEvents as "one cgo boundary"; this
// one is new, required by build step 8, and deliberately kept just as
// small and single-purpose). It calls Carbon's RegisterEventHotKey
// directly instead of the vendored golang.design/x/hotkey — see the
// package doc in hotkey.go for why.
//
// RegisterEventHotKey delivers its callback through whatever run loop is
// already servicing GetApplicationEventTarget() — normally the
// NSApplication run loop AppKit apps already run on the main thread. In
// this project that run loop is the one fyne.io/systray's Run() starts
// (see internal/menubar's package doc for the fuller thread-ownership
// argument). Registering the hotkey therefore needs no thread or run
// loop of its own: no mainthread.Init, no second claimant for thread 1,
// nothing beyond installing an event handler once and letting systray's
// already-running loop pump it.
package hotkey

/*
#cgo LDFLAGS: -framework Carbon
#include <Carbon/Carbon.h>

extern void goHotkeyFired(uint32_t id);

// hotkeyHandler is the Carbon event handler installed once by
// installHotkeyHandler. It never does index/UI work itself — it just
// pulls the EventHotKeyID out of the event and hands the id to Go.
static OSStatus hotkeyHandler(EventHandlerCallRef nextHandler, EventRef theEvent, void *userData) {
	EventHotKeyID hkID;
	GetEventParameter(theEvent, kEventParamDirectObject, typeEventHotKeyID, NULL, sizeof(hkID), NULL, &hkID);
	goHotkeyFired(hkID.id);
	return noErr;
}

// installHotkeyHandler installs hotkeyHandler on the application event
// target exactly once, no matter how many hotkeys get registered — one
// handler dispatches by EventHotKeyID, per registerHotkeyC below.
static void installHotkeyHandler(void) {
	static int installed = 0;
	if (installed) {
		return;
	}
	EventTypeSpec eventType = { kEventClassKeyboard, kEventHotKeyPressed };
	InstallApplicationEventHandler(NewEventHandlerUPP(hotkeyHandler), 1, &eventType, NULL, NULL);
	installed = 1;
}

// registerHotkeyC registers one hotkey and returns the opaque
// EventHotKeyRef Go should hold onto to unregister it later, or NULL on
// failure (most commonly: the combination is already taken by another
// application — RegisterEventHotKey itself needs no special permission,
// unlike a CGEventTap).
static void* registerHotkeyC(uint32_t keyCode, uint32_t modifiers, uint32_t hkid) {
	installHotkeyHandler();
	EventHotKeyID hotKeyID;
	hotKeyID.signature = 'scRy';
	hotKeyID.id = hkid;
	EventHotKeyRef ref = NULL;
	OSStatus status = RegisterEventHotKey(keyCode, modifiers, hotKeyID, GetApplicationEventTarget(), 0, &ref);
	if (status != noErr) {
		return NULL;
	}
	return (void*)ref;
}

static void unregisterHotkeyC(void* ref) {
	if (ref != NULL) {
		UnregisterEventHotKey((EventHotKeyRef)ref);
	}
}
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

// Carbon modifier bit values (Carbon/Events.h's cmdKey/shiftKey/optionKey/
// controlKey). Copied as plain constants rather than referenced through
// cgo because they're part of Carbon's stable public ABI and never
// change; pulling them through cgo buys nothing here.
const (
	cmdKeyBit     = 1 << 8
	shiftKeyBit   = 1 << 9
	optionKeyBit  = 1 << 11
	controlKeyBit = 1 << 12
)

// carbonKeyCodes maps the key names Parse accepts to their macOS virtual
// keycode (kVK_* in Carbon/HIToolbox/Events.h, ANSI-US layout). Only the
// keys a global hotkey plausibly wants are listed; Register reports an
// unsupported key by name rather than silently registering the wrong one.
var carbonKeyCodes = map[string]uint32{
	"a": 0x00, "s": 0x01, "d": 0x02, "f": 0x03, "h": 0x04, "g": 0x05,
	"z": 0x06, "x": 0x07, "c": 0x08, "v": 0x09, "b": 0x0B, "q": 0x0C,
	"w": 0x0D, "e": 0x0E, "r": 0x0F, "y": 0x10, "t": 0x11,
	"1": 0x12, "2": 0x13, "3": 0x14, "4": 0x15, "6": 0x16, "5": 0x17,
	"9": 0x19, "7": 0x1A, "8": 0x1C, "0": 0x1D,
	"o": 0x1F, "u": 0x20, "i": 0x22, "p": 0x23, "l": 0x25, "j": 0x26,
	"k": 0x28, "n": 0x2D, "m": 0x2E,
	"space":  0x31,
	"tab":    0x30,
	"return": 0x24, "enter": 0x24,
	"delete": 0x33,
	"escape": 0x35, "esc": 0x35,
	"up": 0x7E, "down": 0x7D, "left": 0x7B, "right": 0x7C,
	"f1": 0x7A, "f2": 0x78, "f3": 0x63, "f4": 0x76, "f5": 0x60, "f6": 0x61,
	"f7": 0x62, "f8": 0x64, "f9": 0x65, "f10": 0x6D, "f11": 0x67, "f12": 0x6F,
}

var errRegisterFailed = errors.New("hotkey: failed to register, the combination might already be taken by another application")

func errUnsupportedKey(key string) error {
	return fmt.Errorf("hotkey: unsupported key %q", key)
}

var (
	callbacksMu sync.Mutex
	callbacks   = make(map[uint32]func())
	nextHandle  uint32
)

// darwinHandle implements Handle.
type darwinHandle struct {
	id  uint32
	ref unsafe.Pointer
}

// Register installs combo as a system-wide hotkey; onTrigger runs (on
// whatever goroutine the Carbon callback is delivered on, via
// goHotkeyFired below — in practice systray's main-thread run loop) each
// time it's pressed.
func Register(combo Combo, onTrigger func()) (Handle, error) {
	code, ok := carbonKeyCodes[combo.Key]
	if !ok {
		return nil, errUnsupportedKey(combo.Key)
	}

	var mods uint32
	for _, m := range combo.Mods {
		switch m {
		case ModCmd:
			mods |= cmdKeyBit
		case ModAlt:
			mods |= optionKeyBit
		case ModShift:
			mods |= shiftKeyBit
		case ModCtrl:
			mods |= controlKeyBit
		}
	}

	callbacksMu.Lock()
	nextHandle++
	id := nextHandle
	callbacks[id] = onTrigger
	callbacksMu.Unlock()

	ref := C.registerHotkeyC(C.uint32_t(code), C.uint32_t(mods), C.uint32_t(id))
	if ref == nil {
		callbacksMu.Lock()
		delete(callbacks, id)
		callbacksMu.Unlock()
		return nil, errRegisterFailed
	}

	return &darwinHandle{id: id, ref: ref}, nil
}

// Unregister releases the hotkey. Safe to call at most once, per Handle's
// contract.
func (h *darwinHandle) Unregister() {
	C.unregisterHotkeyC(h.ref)
	callbacksMu.Lock()
	delete(callbacks, h.id)
	callbacksMu.Unlock()
}

//export goHotkeyFired
func goHotkeyFired(id C.uint32_t) {
	callbacksMu.Lock()
	cb := callbacks[uint32(id)]
	callbacksMu.Unlock()
	if cb != nil {
		cb()
	}
}
