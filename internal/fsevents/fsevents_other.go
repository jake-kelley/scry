//go:build !darwin

package fsevents

// Supported reports whether this platform has a real FSEvents backend.
// False everywhere except darwin.
const Supported = false

// noopStream is the non-darwin Stream: it never emits an event, and Stop
// closes its channel so a Run loop ranging over Events() exits cleanly.
type noopStream struct {
	ch chan Event
}

func (s *noopStream) Events() <-chan Event { return s.ch }

func (s *noopStream) Stop() { close(s.ch) }

// NewStream returns a Stream that never emits anything. It never errors:
// the daemon starts identically on every platform, and internal/watcher's
// Run loop simply blocks on an empty channel until Stop is called. This is
// deliberate per the design doc's build-tag requirement — Windows callers
// exercise the exact same watcher logic against a fake fsevents.Stream in
// tests, and the daemon itself needs no platform switch to start one.
func NewStream(cfg Config) (Stream, error) {
	return &noopStream{ch: make(chan Event)}, nil
}

// LatestEventID is unsupported on this platform.
func LatestEventID() (uint64, error) {
	return 0, ErrNotSupported
}
