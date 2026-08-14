// Package hotkey registers a single system-wide keyboard shortcut that
// fires a callback regardless of which application has focus, per
// "everything-macos-design.md" §7 (the search window) and §8 build step 8.
//
// This file holds the platform-agnostic surface: parsing a combo string
// out of internal/config's TOML, and the Register/Handle shapes every
// caller programs against. hotkey_darwin.go supplies the real
// implementation; hotkey_other.go supplies a no-op stand-in so
// `go build ./...` and `go test ./...` stay green on every other platform
// — the same split fsevents.go / fsevents_darwin.go / fsevents_other.go
// uses.
//
// # Why this package does not use the vendored golang.design/x/hotkey
//
// The design doc (§7, "search window" option 3) says the global hotkey
// "uses Carbon RegisterEventHotKey on macOS and therefore needs no
// Accessibility permission." That is true of RegisterEventHotKey itself,
// but it is not what the vendored golang.design/x/hotkey v0.6.1 actually
// does: reading vendor/golang.design/x/hotkey/hotkey_darwin.m, every
// hotkey on macOS — regular keys and media keys alike — is delivered
// through a CGEventTap (see registerTap, which calls AXIsProcessTrusted
// and fails outright when the process is not trusted for Accessibility /
// Input Monitoring). Using that package as documented would silently
// trade away the no-permission guarantee the design doc is explicit
// about, and the task instructions are explicit that this is a "stop and
// reconsider" situation, not a "note it and move on" one.
//
// So this package implements RegisterEventHotKey directly (hotkey_darwin.go),
// a small contained cgo file of its own — not a new dependency, since
// nothing outside the standard Carbon/HIToolbox framework is linked. The
// golang.design/x/hotkey and golang.design/x/hotkey/mainthread modules
// stay vendored (nothing prunes source that's present in go.sum/go.mod
// unless nothing imports it — see the vendor note in the top-level report)
// but real code deliberately does not import them, for the Accessibility
// reason above and the thread-ownership reason in internal/menubar's
// package doc.
package hotkey

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Mod is one keyboard modifier a Combo can require.
type Mod string

const (
	ModCmd   Mod = "cmd"
	ModAlt   Mod = "alt" // Option, on macOS keyboards
	ModShift Mod = "shift"
	ModCtrl  Mod = "ctrl"
)

// modOrder fixes the canonical ordering Combo.String and Combo.Label use,
// matching the order macOS itself shows modifiers in (Settings > Keyboard
// > Shortcuts): Control, Option, Shift, Command.
var modOrder = []Mod{ModCtrl, ModAlt, ModShift, ModCmd}

// modAliases maps every accepted spelling of a modifier (case-insensitive,
// matched after lower-casing) to its canonical Mod.
var modAliases = map[string]Mod{
	"cmd":     ModCmd,
	"command": ModCmd,
	"alt":     ModAlt,
	"option":  ModAlt,
	"opt":     ModAlt,
	"shift":   ModShift,
	"ctrl":    ModCtrl,
	"control": ModCtrl,
}

// symbols renders a Mod the way macOS's own menus do.
var symbols = map[Mod]string{
	ModCtrl:  "⌃", // ⌃
	ModAlt:   "⌥", // ⌥
	ModShift: "⇧", // ⇧
	ModCmd:   "⌘", // ⌘
}

// keyLabels renders a few key names that don't just capitalize cleanly
// (space, arrows) the way macOS shows them in a menu shortcut.
var keyLabels = map[string]string{
	"space":  "Space",
	"return": "↩", // ↩
	"enter":  "↩",
	"tab":    "⇥", // ⇥
	"delete": "⌫", // ⌫
	"escape": "⎋", // ⎋
	"esc":    "⎋",
	"up":     "↑",
	"down":   "↓",
	"left":   "←",
	"right":  "→",
}

// DefaultCombo is what a fresh config gets: Option-Space, the Spotlight
// convention most users already have muscle memory for.
const DefaultCombo = "alt+space"

// Combo is a parsed global hotkey: zero or more modifiers plus exactly
// one key.
type Combo struct {
	Mods []Mod
	Key  string // normalized lower-case key name, e.g. "space", "k", "f5"
}

// Parse parses a "+"-joined combo string like "alt+space" or
// "cmd+shift+k", as it appears in config.toml's [hotkey] combo field.
// Modifier and key names are matched case-insensitively; "option"/"opt"
// are accepted as aliases for "alt" since that's Carbon's name for the
// same physical key. Exactly one non-modifier token is required, empty
// segments (from "++" or a leading/trailing "+") are rejected, and a
// modifier repeated in the same combo is rejected rather than silently
// deduplicated — that almost always means a typo, not intent.
func Parse(s string) (Combo, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Combo{}, errors.New("hotkey: empty combo")
	}
	parts := strings.Split(s, "+")

	var mods []Mod
	seen := make(map[Mod]bool)
	var key string
	keySet := false
	for _, raw := range parts {
		tok := strings.ToLower(strings.TrimSpace(raw))
		if tok == "" {
			return Combo{}, fmt.Errorf("hotkey: invalid combo %q: empty segment", s)
		}
		if m, ok := modAliases[tok]; ok {
			if seen[m] {
				return Combo{}, fmt.Errorf("hotkey: invalid combo %q: modifier %q repeated", s, tok)
			}
			seen[m] = true
			mods = append(mods, m)
			continue
		}
		if keySet {
			return Combo{}, fmt.Errorf("hotkey: invalid combo %q: more than one non-modifier key (%q and %q)", s, key, tok)
		}
		key = tok
		keySet = true
	}
	if !keySet {
		return Combo{}, fmt.Errorf("hotkey: invalid combo %q: no key given (modifiers only)", s)
	}

	sort.Slice(mods, func(i, j int) bool { return modIndex(mods[i]) < modIndex(mods[j]) })
	return Combo{Mods: mods, Key: key}, nil
}

func modIndex(m Mod) int {
	for i, mo := range modOrder {
		if mo == m {
			return i
		}
	}
	return len(modOrder)
}

// String renders c back to the config form Parse accepts, e.g.
// "ctrl+alt+space". Round-tripping through Parse and String always
// produces the same Combo, though not necessarily the same string (mod
// order and key case are normalized).
func (c Combo) String() string {
	parts := make([]string, 0, len(c.Mods)+1)
	for _, m := range c.Mods {
		parts = append(parts, string(m))
	}
	parts = append(parts, c.Key)
	return strings.Join(parts, "+")
}

// Label renders c the way a macOS menu shows a keyboard shortcut, e.g.
// "⌥Space" (⌥Space) or "⇧⌘K" (⇧⌘K) — modifier symbols in
// Control/Option/Shift/Command order, no separators, matching how
// Settings > Keyboard > Shortcuts displays combinations.
func (c Combo) Label() string {
	var b strings.Builder
	for _, m := range c.Mods {
		b.WriteString(symbols[m])
	}
	if lbl, ok := keyLabels[c.Key]; ok {
		b.WriteString(lbl)
	} else if len(c.Key) == 1 {
		b.WriteString(strings.ToUpper(c.Key))
	} else {
		b.WriteString(strings.ToUpper(c.Key[:1]) + c.Key[1:])
	}
	return b.String()
}

// ErrNotSupported is returned by Register on a platform with no global
// hotkey backend (see hotkey_darwin.go for the real implementation and
// hotkey_other.go for the stand-in used everywhere else, above all
// Windows, where this project is mostly developed).
var ErrNotSupported = errors.New("hotkey: not supported on this platform")

// Handle is a registered hotkey. Unregister releases it; it is safe to
// call at most once.
type Handle interface {
	Unregister()
}
