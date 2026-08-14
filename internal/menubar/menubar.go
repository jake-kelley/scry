// Package menubar implements the macOS status item described in
// "everything-macos-design.md" §7 (build step 7) and, on top of it, the
// global-hotkey search panel from build step 8.
//
// # The thread-ownership problem, and how this package resolves it
//
// §7 says "systray.Run() owns the main thread" and calls that "the
// single biggest structural consequence of adding a UI" — the indexer,
// watcher and IPC socket server all have to start as goroutines from
// onReady instead of running inline in main(), because AppKit requires
// every UI call to happen on thread 1 and fyne.io/systray.Run blocks
// that thread for the process's whole lifetime.
//
// golang.design/x/hotkey/mainthread (and the more general
// golang.design/x/mainthread) make the identical claim on thread 1:
// Init(fn) locks the OS thread, then blocks it running fn and dispatching
// queued main-thread calls, exactly like systray.Run does. Two functions
// that each want to permanently own and block thread 1 cannot both run —
// there is no way to call both systray.Run and mainthread.Init from the
// same process without one of them never getting the thread it needs,
// which is why this package does not import either mainthread package at
// all (see internal/hotkey's package doc for the second, independent
// reason: the vendored golang.design/x/hotkey's actual macOS
// implementation needs Accessibility permission, which the design doc
// explicitly rules out).
//
// The resolution: systray.Run is the *only* thing that claims thread 1.
// internal/hotkey's RegisterEventHotKey callback and internal/panel's
// AppKit/WebKit calls both run through dispatch_sync onto the main
// queue, which is serviced by whatever run loop is already active on
// thread 1 — systray's. Nothing else needs to own or spin its own run
// loop the way internal/fsevents' CFRunLoop thread does (§9 item 7); the
// hotkey and panel pieces are guests on systray's loop, not competing
// owners of it.
//
// # Everything else
//
// Every UI path here — the live indexed-file count, Rebuild index,
// Quit — goes through the IPC socket (internal/ipc), the same "search",
// "status", "reindex" and "stop" ops the CLI already uses, never
// touching an index.Shard directly. That is what §7 ("one process or
// two") calls out as the seam that makes a later headless-scryd split a
// build change rather than a rewrite: this package is written as a pure
// IPC client, even though for v1 it happens to be running in the same
// process as the daemon it's dialing.
//
// This file holds the platform-agnostic logic: count formatting, the
// search-window URL, and the browser-open fallback command. onready.go
// (build-tagged, like internal/fsevents) wires that logic to the real
// systray/hotkey/panel calls.
package menubar

import (
	"fmt"
	"os/exec"
	"strconv"

	"scry/internal/ipc"
)

// SearchURL is the URL both the §7 option 1 browser fallback and the §8
// panel point at: internal/web's index page, served by this same process
// (see cmd/scry's menubar command).
func SearchURL(webAddr string) string {
	return "http://" + webAddr + "/"
}

// openBrowserCmd returns the command Search… and the hotkey fallback run
// to open url in the default browser (§7 option 1: "zero new code,
// reuses the --serve web UI"). Split out from the call site so the
// *exec.Cmd shape is testable without actually spawning a browser: `open`
// is the macOS way to do this and does nothing useful on the platforms
// this project is developed on, so no caller here invokes Start/Run.
func openBrowserCmd(url string) *exec.Cmd {
	return exec.Command("open", url)
}

// openPathCmd returns the command Preferences… runs to open path (the
// config file) in the user's default application for it.
func openPathCmd(path string) *exec.Cmd {
	return exec.Command("open", path)
}

// FormatStatus renders the status line in the menu for a "status" reply.
// A fresh install has no roots at all, and "0 files indexed" reads there
// like a broken indexer rather than an unconfigured one — it tells the
// user nothing about what to do next. With no roots, point at the item
// that fixes it instead.
func FormatStatus(rows []ipc.RootStatus) string {
	if len(rows) == 0 {
		return "No roots configured — see Preferences…"
	}
	return FormatCount(TotalEntries(rows))
}

// FormatCount renders the live indexed-file count the way the status
// menu item shows it: "0 files indexed", "1 file indexed",
// "247,391 files indexed" — comma-grouped so a six-digit count is still
// readable at a glance in a menu, matching the design doc's own mockup
// in §7.
func FormatCount(n int) string {
	word := "files"
	if n == 1 {
		word = "file"
	}
	return fmt.Sprintf("%s %s indexed", groupThousands(n), word)
}

// groupThousands renders a non-negative int with "," thousands
// separators (1234567 -> "1,234,567"). n is always a count of indexed
// entries, so negative input is not a case this needs to handle; it is
// rendered as-is (with a leading "-") rather than panicking, since a
// formatting helper should never be where a bug becomes a crash.
func groupThousands(n int) string {
	s := strconv.Itoa(n)
	neg := false
	if len(s) > 0 && s[0] == '-' {
		neg = true
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// TotalEntries sums Entries across every root in a "status" response.
// ipc.RootStatus.Entries already excludes the synthetic root entry as of
// 8718da2 (index.Shard.CountIndexed), so this is a plain sum, not a
// subtraction — kept as its own function so that fact only has to be
// remembered in one place.
func TotalEntries(rows []ipc.RootStatus) int {
	total := 0
	for _, r := range rows {
		total += r.Entries
	}
	return total
}
