package fsevents

import "testing"

func TestEventFlagsHas(t *testing.T) {
	f := FlagMustScanSubDirs | FlagRootChanged
	if !f.Has(FlagMustScanSubDirs) {
		t.Fatalf("expected FlagMustScanSubDirs set")
	}
	if !f.Has(FlagRootChanged) {
		t.Fatalf("expected FlagRootChanged set")
	}
	if f.Has(FlagUnmount) {
		t.Fatalf("did not expect FlagUnmount set")
	}
}

// TestNewStreamNeverErrors holds on every platform: the design deliberately
// makes the stub a no-op rather than an error so cmd/scry/daemon.go never
// needs a platform switch just to start the watcher.
func TestNewStreamNeverErrors(t *testing.T) {
	s, err := NewStream(Config{Paths: []string{"."}})
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	defer s.Stop()

	select {
	case ev, ok := <-s.Events():
		if ok {
			t.Fatalf("unexpected event from a freshly created stream: %+v", ev)
		}
	default:
	}
}

func TestStopClosesEventsChannel(t *testing.T) {
	s, err := NewStream(Config{Paths: []string{"."}})
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	s.Stop()

	if _, ok := <-s.Events(); ok {
		t.Fatalf("expected Events() to be closed after Stop")
	}
}
