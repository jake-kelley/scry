//go:build !darwin

package power

// Supported reports whether this platform has a real IOKit power-
// management backend. False everywhere except darwin.
const Supported = false

// NewNotifier always fails on a platform with no IOKit power-management
// backend. Callers (cmd/scry/daemon.go) treat this the same as any other
// notifier failure: log a warning and continue without wake detection —
// the periodic recrawl (or, if that's off, "Rebuild index") remains the
// only backstop. This keeps `go build ./...` and `go test ./...` green on
// Windows, per §8 item 5's stub-pair discipline, the same shape
// internal/fsevents, internal/hotkey and internal/panel each already
// follow.
func NewNotifier() (Notifier, error) {
	return nil, ErrNotSupported
}
