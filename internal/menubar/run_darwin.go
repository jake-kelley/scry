//go:build darwin

package menubar

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"fyne.io/systray"

	"scry/internal/hotkey"
	"scry/internal/ipc"
	"scry/internal/panel"
)

// Supported is true on darwin; see run_other.go for every other
// platform.
const Supported = true

// Run parses opts.HotkeyCombo, then blocks on systray.Run for the rest
// of the process's life — see this package's doc comment for why nothing
// else in the process is allowed to also claim thread 1. It returns only
// after Quit is chosen from the menu (or systray.Quit is otherwise
// called), by which point the hotkey has been unregistered and the
// daemon told to stop.
func Run(opts Options) error {
	combo, err := hotkey.Parse(opts.HotkeyCombo)
	if err != nil {
		return fmt.Errorf("menubar: %w", err)
	}

	state := &appState{opts: opts, combo: combo}
	systray.Run(state.onReady, state.onExit)
	return nil
}

// appState holds everything onReady wires up and the goroutine loop
// below it needs to reach: the menu items themselves plus the hotkey and
// panel handles it must clean up on Quit.
type appState struct {
	opts  Options
	combo hotkey.Combo

	panel    panel.Panel
	usePanel bool
	hk       hotkey.Handle
}

// trigger is what both the hotkey and the "Search…" menu item call:
// toggle the panel if one was created, or fall back to opening a browser
// tab (§7 option 1) if the panel could not be created — e.g. on an older
// macOS without the WebKit entitlement this process happens to be running
// under. Either fallback direction (hotkey without panel, panel without
// hotkey) degrades to something that still works rather than doing
// nothing.
func (s *appState) trigger() {
	if s.usePanel {
		s.panel.Toggle()
		return
	}
	if err := openBrowserCmd(SearchURL(s.opts.WebAddr)).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "scry: menubar: opening browser: %v\n", err)
	}
}

func (s *appState) onReady() {
	systray.SetTemplateIcon(templateIconPNG(), templateIconPNG())
	systray.SetTooltip("scry")

	searchLabel := "Search…"
	if lbl := s.combo.Label(); lbl != "" {
		searchLabel = fmt.Sprintf("Search…\t%s", lbl)
	}
	mSearch := systray.AddMenuItem(searchLabel, "Open the search window")
	mCount := systray.AddMenuItem(FormatCount(0), "")
	mCount.Disable()
	systray.AddSeparator()
	mRebuild := systray.AddMenuItem("Rebuild index", "Force a full recrawl of every root")
	mPrefs := systray.AddMenuItem("Preferences…", "Edit scry's configuration")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit scry")

	if p, err := panel.New(SearchURL(s.opts.WebAddr)); err != nil {
		fmt.Fprintf(os.Stderr, "scry: menubar: panel unavailable (%v); Search and the hotkey will open a browser tab instead\n", err)
	} else {
		s.panel = p
		s.usePanel = true
	}

	if hk, err := hotkey.Register(s.combo, s.trigger); err != nil {
		fmt.Fprintf(os.Stderr, "scry: menubar: hotkey %s not registered: %v\n", s.combo, err)
	} else {
		s.hk = hk
	}

	go pollCount(s.opts, mCount)
	go s.eventLoop(mSearch, mRebuild, mPrefs, mQuit)
}

func (s *appState) onExit() {
	// systray calls this after Quit(); cleanup itself already happened
	// in eventLoop's mQuit case, which is what called Quit() in the
	// first place. Nothing left to do here — it exists because
	// systray.Run requires an onExit callback, not because this process
	// has more state to release.
}

func (s *appState) eventLoop(mSearch, mRebuild, mPrefs, mQuit *systray.MenuItem) {
	for {
		select {
		case <-mSearch.ClickedCh:
			s.trigger()

		case <-mRebuild.ClickedCh:
			if err := reindex(s.opts.Addr); err != nil {
				fmt.Fprintf(os.Stderr, "scry: menubar: rebuild index: %v\n", err)
			}

		case <-mPrefs.ClickedCh:
			if path, err := configPath(); err != nil {
				fmt.Fprintf(os.Stderr, "scry: menubar: preferences: %v\n", err)
			} else if err := openPathCmd(path).Start(); err != nil {
				fmt.Fprintf(os.Stderr, "scry: menubar: opening config: %v\n", err)
			}

		case <-mQuit.ClickedCh:
			if s.hk != nil {
				s.hk.Unregister()
			}
			if err := stopDaemon(s.opts.Addr); err != nil {
				// Not fatal to quitting the menu bar app itself — the
				// process exits either way once systray.Quit runs the
				// AppKit teardown, it just means the daemon's own
				// goroutines (scheduler, watcher, socket) had to be
				// reaped by process exit instead of a clean "stop".
				fmt.Fprintf(os.Stderr, "scry: menubar: stopping daemon: %v\n", err)
			}
			// Under the installed LaunchAgent, exiting is not enough:
			// packaging/com.scry.app.plist sets KeepAlive, so
			// launchd relaunches us the instant the process goes away and
			// "Quit" appears to do nothing at all. Boot the job out of the
			// GUI domain first so launchd stops supervising it. bootout is
			// scoped to this login session, not persistent like
			// `launchctl disable`, so scry still starts at the next login
			// — which is what a user means by Quit rather than Uninstall.
			bootoutSelf()
			systray.Quit()
			return
		}
	}
}

// bootoutSelf removes this process's launchd job from the GUI domain, if
// this process is in fact launchd-managed. launchd sets XPC_SERVICE_NAME
// to the job label for the processes it supervises; a scry started by hand
// from a shell either has it unset or has the placeholder "0", and then
// there is no job to boot out and plain process exit is correct.
//
// Failures are logged, never fatal: the worst case is the KeepAlive
// relaunch this exists to prevent, and reporting that beats hanging on to
// a menu bar item the user has already dismissed.
func bootoutSelf() {
	label := os.Getenv("XPC_SERVICE_NAME")
	if label == "" || label == "0" {
		return
	}
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
	// Synchronous: launchd sends this process SIGTERM as part of booting
	// the job out, and racing that against our own exit is how the
	// relaunch sneaks back in.
	if out, err := exec.Command("launchctl", "bootout", target).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "scry: menubar: launchctl bootout %s: %v: %s\n",
			target, err, strings.TrimSpace(string(out)))
	}
}

// reindex asks the daemon to trigger an out-of-band reconcile pass, the
// same "reindex" op `scry index` triggers through the socket rather than
// touching a Shard directly.
func reindex(addr ipc.Addr) error {
	c, err := ipc.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	resp, err := c.Call(ipc.Request{Op: "reindex"})
	if err != nil {
		return err
	}
	if resp.Err != "" {
		return fmt.Errorf("daemon: %s", resp.Err)
	}
	return nil
}

// stopDaemon asks the daemon to exit cleanly, the same "stop" op `scry
// stop` uses.
func stopDaemon(addr ipc.Addr) error {
	c, err := ipc.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	resp, err := c.Call(ipc.Request{Op: "stop"})
	if err != nil {
		return err
	}
	if resp.Err != "" {
		return fmt.Errorf("daemon: %s", resp.Err)
	}
	return nil
}
