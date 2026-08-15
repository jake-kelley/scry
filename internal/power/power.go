// Package power detects an actual macOS wake-from-sleep event and wires it
// to the resync-first, escalate-only-if-necessary behaviour described in
// "everything-macos-design.md" §8 item 3.
//
// This file holds the platform-agnostic surface: the Notifier interface
// every caller programs against, and ErrNotSupported. power_darwin.go
// supplies the real IOKit-backed implementation; power_other.go supplies a
// stub that always fails to start, so `go build ./...` and `go test ./...`
// stay green on every other platform (Windows above all — see
// internal/fsevents' package doc, which this mirrors). The actual
// decision logic — debounce, and whether a failed resync escalates to a
// full reconcile — lives in coordinator.go, has no build tag, and is unit
// tested on Windows against a fake Notifier; that split is deliberate so
// the only thing gated behind cgo is "does the OS say we woke up".
package power

import "errors"

// ErrNotSupported is returned by NewNotifier on a platform with no IOKit
// power-management backend (everywhere except darwin).
var ErrNotSupported = errors.New("power: not supported on this platform")

// Notifier reports one signal on Events each time the system wakes from
// sleep. It never signals for sleep itself — a caller that wants to veto
// or delay a sleep has no business here; this package only ever
// acknowledges kIOMessageCanSystemSleep/kIOMessageSystemWillSleep so it
// never gets in the way of a sleep the user asked for. Events never closes
// on its own; Stop closes it and releases whatever OS-level registration
// backs the notifier.
type Notifier interface {
	Events() <-chan struct{}
	Stop()
}
