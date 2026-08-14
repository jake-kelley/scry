package panel

import (
	"errors"
	"runtime"
	"testing"
)

// TestNewNotSupportedOffDarwin exercises panel_other.go's stand-in
// directly: on every platform except darwin, New must fail with
// ErrNotSupported rather than silently returning a Panel that does
// nothing, so a caller (internal/menubar) can reliably tell "no panel
// backend here" from "panel created but broken."
func TestNewNotSupportedOffDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin has a real panel backend; nothing to assert here without a window server")
	}
	p, err := New("http://127.0.0.1:8973/")
	if p != nil {
		t.Errorf("New() panel = %v, want nil", p)
	}
	if !errors.Is(err, ErrNotSupported) {
		t.Errorf("New() error = %v, want ErrNotSupported", err)
	}
}
