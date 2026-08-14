// Package panel implements the borderless, Spotlight-style search window
// described in "everything-macos-design.md" §7's search-window option 3:
// the global hotkey (internal/hotkey) toggles it instead of opening a
// browser tab.
//
// Rather than reimplement search rendering, the panel hosts a WKWebView
// pointed at internal/web's existing page (see menubar.SearchURL) — the
// same HTML/JS the §7 option 1 browser fallback uses, just framed without
// browser chrome. This file holds the platform-agnostic surface;
// panel_darwin.go supplies the real WKWebView-backed implementation and
// panel_other.go a no-op stand-in, the same split as internal/fsevents
// and internal/hotkey.
package panel

import "errors"

// ErrNotSupported is returned by New on a platform with no panel backend.
var ErrNotSupported = errors.New("panel: not supported on this platform")

// Panel is a borderless window loading a single URL, shown and hidden by
// the global hotkey.
type Panel interface {
	// Toggle shows the panel (raising and focusing it) if hidden, or
	// hides it if currently shown — the press-to-open,
	// press-again-to-close convention Spotlight itself uses. This is
	// what a hotkey press calls.
	Toggle()

	// Close hides the panel unconditionally. Called on Escape and when
	// the panel loses key focus (the user clicked away), so it never
	// lingers on top of other work.
	Close()
}
