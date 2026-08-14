//go:build darwin

package menubar

import (
	"fmt"
	"os"

	"fyne.io/systray"

	"scry/internal/ipc"
)

// Supported is true on darwin; see run_other.go for every other
// platform.
const Supported = true

// Run blocks on systray.Run for the rest of the process's life — see
// this package's doc comment for why nothing else in the process is
// allowed to also claim thread 1. It returns only after Quit is chosen
// from the menu (or systray.Quit is otherwise called).
func Run(opts Options) error {
	state := &appState{opts: opts}
	systray.Run(state.onReady, state.onExit)
	return nil
}

// appState holds everything onReady wires up and the goroutine loop
// below it needs to reach.
type appState struct {
	opts Options
}

// trigger is what "Search…" calls: open the search window in the
// default browser, §7 option 1 — "zero new code, reuses the --serve web
// UI." A hotkey-driven borderless panel is build step 8, not this one.
func (s *appState) trigger() {
	if err := openBrowserCmd(SearchURL(s.opts.WebAddr)).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "scry: menubar: opening browser: %v\n", err)
	}
}

func (s *appState) onReady() {
	systray.SetTemplateIcon(templateIconPNG(), templateIconPNG())
	systray.SetTooltip("scry")

	mSearch := systray.AddMenuItem("Search…", "Open the search window")
	mCount := systray.AddMenuItem(FormatCount(0), "")
	mCount.Disable()
	systray.AddSeparator()
	mRebuild := systray.AddMenuItem("Rebuild index", "Force a full recrawl of every root")
	mPrefs := systray.AddMenuItem("Preferences…", "Edit scry's configuration")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit scry")

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
			if err := stopDaemon(s.opts.Addr); err != nil {
				// Not fatal to quitting the menu bar app itself — the
				// process exits either way once systray.Quit runs the
				// AppKit teardown, it just means the daemon's own
				// goroutines (scheduler, watcher, socket) had to be
				// reaped by process exit instead of a clean "stop".
				fmt.Fprintf(os.Stderr, "scry: menubar: stopping daemon: %v\n", err)
			}
			systray.Quit()
			return
		}
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
