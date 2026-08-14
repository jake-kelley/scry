//go:build !darwin

package menubar

import "errors"

// Supported reports whether this platform has a real status item
// backend. False everywhere except darwin — an NSStatusItem is a macOS
// concept and this project ships no other platform's menu bar UI.
const Supported = false

// Run always fails on a platform with no status item backend. cmd/scry
// checks Supported before doing anything else (starting the daemon core,
// dialing the socket), so this is reached only if a caller skips that
// check; it exists purely to keep `go build ./...` and `go test ./...`
// green on Windows, where this project is mostly developed.
func Run(opts Options) error {
	return errors.New("menubar: not supported on this platform")
}
