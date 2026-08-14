//go:build darwin

package menubar

import (
	"fmt"
	"time"

	"fyne.io/systray"

	"scry/internal/config"
	"scry/internal/ipc"
)

// pollCount refreshes mCount's title from the daemon's "status" op every
// Options.pollInterval, for as long as the process runs — there is no
// stop signal because the whole process exits together with the menu bar
// (systray.Quit unblocks Run, which returns, which ends main). A failed
// dial (daemon not answering yet, or briefly restarting) leaves the last
// good count showing rather than replacing it with an alarming error —
// the count catches up on the next tick.
func pollCount(opts Options, mCount *systray.MenuItem) {
	interval := opts.pollInterval()
	for {
		if rows, err := status(opts.Addr); err == nil {
			mCount.SetTitle(FormatCount(TotalEntries(rows)))
		}
		time.Sleep(interval)
	}
}

// status asks the daemon for its current per-root status, the same
// "status" op `scry status` uses.
func status(addr ipc.Addr) ([]ipc.RootStatus, error) {
	c, err := ipc.Dial(addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	resp, err := c.Call(ipc.Request{Op: "status"})
	if err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, fmt.Errorf("daemon: %s", resp.Err)
	}
	return resp.Stats.Roots, nil
}

// configPath returns scry's config file path, for Preferences… to open.
func configPath() (string, error) {
	return config.ConfigPath()
}
