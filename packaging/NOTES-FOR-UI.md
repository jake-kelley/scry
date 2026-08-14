# Notes from packaging — RESOLVED

These were written while the packaging surface and the menu bar UI were
being built in parallel, by two workers who could not touch each other's
files. All four have since been resolved; kept because the reasoning still
explains why the plist looks the way it does.

1. **~~The LaunchAgent runs `scry daemon`, not a systray subcommand.~~**
   RESOLVED. `com.jakekelley.scry.plist` now runs `scry menubar`. Left as
   `daemon` it installed a perfectly working background indexer that drew
   no menu bar item at all — the one thing the bundle exists to provide.

2. **~~`systray.Run()` owns the main thread.~~**
   RESOLVED, and it is `scry menubar` that blocks on it, with the indexer,
   watcher, socket server and web server all started as goroutines. See
   `cmd/scry/menubar.go` and `internal/menubar`'s package doc.

3. **~~KeepAlive=true means a clean exit gets restarted.~~**
   RESOLVED, and this was a real bug, not a doc note: the Quit menu item
   exited the process and launchd relaunched it within a second, so Quit
   looked broken. Quit now boots its own job out of `gui/$UID` first —
   `bootoutSelf` in `internal/menubar/run_darwin.go`, which detects launchd
   supervision via the `XPC_SERVICE_NAME` launchd sets on its jobs. bootout
   is scoped to the login session rather than persistent like `launchctl
   disable`, so scry still starts at the next login: Quit, not Uninstall.
   `scry stop` alone still will not keep a managed instance stopped.

4. **A bug neither task could have found alone.** `scry menubar` used to
   return an error when no roots were configured. Combined with KeepAlive
   that is an endless respawn loop on a fresh install, with no status item
   ever drawn and therefore no Preferences… item to reach — the user could
   not configure the roots whose absence caused the crash. It only showed
   up when the bundle was actually installed and the launchd job inspected
   (`runs = 2, last exit code = 1`). The menu bar app now starts with no
   roots and says so. Worth remembering: building the artefact is not the
   same as installing it, and only the install found this.
