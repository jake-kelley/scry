# Notes for the menu bar / cmd task, from packaging

Packaging is scoped to `packaging/**` only and can't touch `cmd/` or
`internal/`, so these are observations from building the bundle + LaunchAgent
that the systray/UI work will care about. Nothing here has been applied —
just flagging it.

1. **The LaunchAgent runs `scry daemon`, not a future systray subcommand.**
   `packaging/com.jakekelley.scry.plist` currently has `ProgramArguments`
   pointing at `scry daemon` (the resident process from `cmd/scry/daemon.go`,
   §3/§7). If the UI work adds a distinct entry point for the menu bar item
   (e.g. `scry menubar` that calls `systray.Run()` and starts the daemon
   logic from `onReady`), the LaunchAgent's `ProgramArguments` needs updating
   to match — packaging can't predict that shape, so it currently launches
   the same `daemon` subcommand the CLI already has.

2. **`systray.Run()` owns the main thread (design doc §7).** Whatever
   subcommand the LaunchAgent ends up invoking needs to be the one that
   blocks on `systray.Run()`, with the indexer/watcher/socket server started
   as goroutines from `onReady` — not the other way around. If a future
   `scry menubar` command exists, that's what `ProgramArguments` in the
   LaunchAgent plist should point to, and packaging will need a follow-up
   patch when that lands.

3. **KeepAlive=true means the daemon exiting cleanly gets restarted.**
   `scry stop` currently (per `cmd/scry/main.go`) talks to the daemon over
   the socket and presumably tells it to exit. Under the installed
   LaunchAgent, `KeepAlive=true` will immediately relaunch it — `scry stop`
   alone won't keep it stopped. To actually stop the managed instance, use
   `launchctl bootout gui/$(id -u)/com.jakekelley.scry` (what
   `packaging/uninstall.sh` does), not `scry stop`. Worth documenting
   user-facing once there's a "Quit" menu item — it should bootout the
   agent, not just exit the process.

4. **No code changes made to satisfy this** — packaging only builds around
   whatever `./cmd/scry` currently produces. `packaging/build-app.sh` builds
   with `go build -mod=vendor -o scry.app/Contents/MacOS/scry ./cmd/scry`
   and nothing else; if the UI task adds new subcommands, packaging doesn't
   need to change for that alone.
