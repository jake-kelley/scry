//go:build !darwin

package panel

// New always fails on a platform with no WKWebView. internal/menubar
// treats this the same as any other panel failure: log it and fall back
// to opening a browser tab for the hotkey too, per §7 option 1. On
// Windows this just keeps `go build ./...` and `go vet ./...` green.
func New(url string) (Panel, error) {
	return nil, ErrNotSupported
}
