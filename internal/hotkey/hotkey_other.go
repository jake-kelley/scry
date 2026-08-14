//go:build !darwin

package hotkey

// Register always fails on a platform with no global hotkey backend.
// Callers (internal/menubar) treat this the same as any other
// registration failure: log it and fall back to opening a browser tab
// instead of the borderless panel, per §7 option 1. On Windows, where
// menu bar builds never ship, this just keeps `go build ./...` and
// `go vet ./...` green.
func Register(combo Combo, onTrigger func()) (Handle, error) {
	return nil, ErrNotSupported
}
