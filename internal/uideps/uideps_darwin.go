//go:build darwin

// Package uideps exists only to keep the macOS UI dependencies present in
// go.mod and, critically, in the committed vendor/ tree.
//
// It is scaffolding with a stated lifetime: DELETE THIS PACKAGE as soon as
// internal/menubar (build step 7) and the hotkey panel (step 8) import
// these modules for real. `go mod vendor` prunes any module no package
// imports, so without a real importer the vendored source would vanish on
// the next tidy — and the Mac this project is built on cannot re-fetch it,
// because proxy.golang.org answers 403 Forbidden on that network. Vendoring
// from Windows is the only way these dependencies reach the Mac at all.
package uideps

import (
	_ "fyne.io/systray"
	_ "golang.design/x/hotkey"
	_ "golang.design/x/hotkey/mainthread"
)
