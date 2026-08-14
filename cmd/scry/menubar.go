package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"scry/internal/ipc"
	"scry/internal/menubar"
	"scry/internal/web"
)

// runMenubar implements `scry menubar`: the LSUIElement entry point
// described in "everything-macos-design.md" §7 and §8. It is the same
// resident process runDaemon is — same startCore, same socket, same
// snapshot/watcher machinery — except systray.Run (inside
// internal/menubar.Run) owns the main thread instead of ipc.Serve
// blocking it, so the IPC socket and the --serve web UI both start as
// goroutines here rather than being called inline.
//
// packaging/NOTES-FOR-UI.md flags that the LaunchAgent plist currently
// points ProgramArguments at `scry daemon`, not this command — packaging
// owns that file and needs a follow-up patch to point at `scry menubar`
// once this lands; see this task's final report.
func runMenubar(args []string) error {
	if !menubar.Supported {
		return errors.New("scry: menubar is only supported on macOS")
	}
	if len(args) != 0 {
		return fmt.Errorf("usage: scry menubar (no arguments)")
	}

	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Roots) == 0 {
		return fmt.Errorf("no roots configured; run `scry root add <path>` first")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := startCore(ctx, cfg, "scry: menubar")
	if err != nil {
		return err
	}

	addr, err := ipcAddr()
	if err != nil {
		return err
	}
	go func() {
		err := ipc.Serve(ctx, addr, handleRequest(d, cancel))
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "scry: menubar: ipc: %v\n", err)
		}
	}()

	// Unlike `scry daemon --serve`, the menu bar app always runs the web
	// UI: Search… and the hotkey panel both need it, per §7's "one
	// process or two" — everything routes through the socket or this
	// same-process HTTP server, never through d directly from
	// internal/menubar.
	go func() {
		if err := web.Serve(web.DefaultAddr, d.search); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "scry: menubar: web: %v\n", err)
		}
	}()

	// menubar.Run blocks on systray.Run for the process's life; it
	// returns once Quit has told the daemon (over the socket, like `scry
	// stop`) to shut down and unregistered the hotkey. cancel() here
	// stops whatever startCore goroutines (scheduler, watcher) are still
	// running, mirroring what ctx cancellation already did for ipc.Serve
	// via the "stop" op's own cancel call.
	err = menubar.Run(menubar.Options{
		Addr:        addr,
		WebAddr:     web.DefaultAddr,
		HotkeyCombo: cfg.Hotkey.Combo,
	})
	cancel()
	return err
}
