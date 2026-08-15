# Manual verification (run these at the Mac, not over SSH)

Most of the install path *has* been verified over SSH, including things
that were expected to be unverifiable. `launchctl bootstrap gui/$UID`
loads the agent into the logged-in GUI session, so the app really does get
an Aqua session and AppKit really does initialise. What follows is only
what genuinely cannot be checked without eyes on the screen.

## Already verified over SSH — do not repeat

- Bundle structure, `plutil -lint` on both plists, `LSUIElement = true`,
  `CFBundleIdentifier = com.jakekelley.scry`.
- `codesign --verify --strict --deep` passes; ad-hoc fallback warns loudly
  when no `scry-codesign` identity exists.
- `install.sh` loads the agent, which reaches `state = running`, `runs = 1`,
  `last exit code = (never exited)`, with `arguments = {..., menubar}`.
- The resident core works through the installed bundle: `scry status` over
  the socket, the web UI on `127.0.0.1:8973` returning HTTP 200, and a live
  FSEvents add (`touch` a file, query it back through both the socket and
  the web UI) — all against the launchd-managed process.
- `KeepAlive` relaunch on bare exit (SIGTERM → new pid) and `launchctl
  bootout` stopping it for good. That pair is the mechanism the Quit menu
  item uses; see `bootoutSelf` in `internal/menubar/run_darwin.go`.
- `uninstall.sh` leaves no app, no agent, and no running process.

## Needs a human at the screen

1. **The status item appears, and the icon looks right.**
   `packaging/install.sh`, then look at the menu bar. A small monochrome
   scry glyph should appear. Then switch System Settings → Appearance
   between Light and Dark. The icon must stay legible in *both* — that is
   what `SetTemplateIcon` buys, and a template icon that was accidentally
   authored with colour looks correct in one mode and invisible or muddy
   in the other. This is the single most likely visual defect.

2. **No Dock icon, no app menu.**
   With the app running, the Dock must **not** show scry and there must be
   no scry application menu in the menu bar. That is `LSUIElement` working.

3. **The menu reads correctly.**
   Click the status item. Expect: Search…, a live count line, Rebuild
   index, Preferences…, Quit. On a machine with no roots configured the
   count line should read "No roots configured — see Preferences…", not
   "0 files indexed". Choosing Preferences… must open a config file even
   on a first-ever launch (it is created on demand if absent).

4. **Quit actually quits.**
   Choose Quit. The status item must disappear **and stay gone**. If it
   vanishes and reappears within a second or two, `bootoutSelf` failed and
   `KeepAlive` won — check stderr in `~/Library/Logs/scry/scry.err` for a
   `launchctl bootout` error. It should come back at your next login.

5. **The global hotkey and the panel.**
   With the app running, press ⌥Space (configurable — `[hotkey] combo` in
   the config file). A borderless Spotlight-style panel should appear;
   type a few characters and results should update as you type. Press it
   again, or Escape, to dismiss. Carbon's `RegisterEventHotKey` needs **no**
   Accessibility permission, so if macOS prompts you for Accessibility
   access, something has regressed — say so rather than granting it.
   Note ⌥Space is a plausible conflict with other launchers (Alfred,
   Raycast, Spotlight remaps); if nothing happens, check for a conflict
   before assuming the hotkey is broken.

6. **Login Items surfacing.**
   System Settings → General → Login Items. "scry" should be listed under
   background items, and its toggle should be able to disable it (macOS 13+
   behaviour described in design doc §7 — intentional, not to be defeated).

7. **TCC prompt behaviour across rebuilds.**
   Add a root under `~/Documents`, then run `uninstall.sh && install.sh`.
   With a `scry-codesign` identity set up (see `SIGNING.md`) you should
   **not** get a repeat Documents prompt. If you do, check
   `codesign -dv /Applications/scry.app` (or `~/Applications/scry.app` if
   `/Applications` wasn't writable — `install.sh` prints which one it used)
   for `Authority=scry-codesign`; if it says `Signature=adhoc`, the identity
   wasn't found and every rebuild will keep re-prompting. This is design doc
   §9 item 6.

8. **Wake-from-sleep triggers a resync, not a crawl.**
   This is the one piece of `internal/power` that genuinely cannot be
   checked over SSH — nothing can put the Mac to sleep and wake it back up
   remotely. First confirm the notifier registered at all: right after
   launch, `~/Library/Logs/scry/scry.err` should contain
   `scry: daemon: wake-from-sleep detection active`. If instead it says
   `scry: daemon: warning: wake-from-sleep detection not started: ...`,
   IOKit registration failed and none of the below will fire — stop and
   report that line rather than continuing the test.

   With the app running, watch the log:

   ```
   tail -f ~/Library/Logs/scry/scry.err
   ```

   Close the lid (or Apple menu -> Sleep) and wait a few seconds, then wake
   the machine. Within a couple of seconds you should see:

   ```
   scry: daemon: power: system woke; resyncing the FSEvents stream from the saved position
   scry: daemon: watcher: starting FSEvents stream (at event id ...)
   ```

   and **not** a `scry: daemon: power: falling back to a full reconcile
   pass` line, unless the resync itself logged a failure just above it
   (`scry: daemon: power: resync failed, the saved position could not be
   resumed: ...`). If `recrawl_interval` is `"off"` in your config and the
   resync does fail, expect instead:

   ```
   scry: daemon: power: recrawl_interval is off; not falling back to a full reconcile — the index may have drifted while the resync was down, and "Rebuild index" will repair it
   ```

   and no crawl. To see the escalation path instead, wake the machine
   twice in quick succession (within ~30s) and confirm the *second* wake
   logs `scry: daemon: power: wake ignored, ... since the last resync`
   rather than running anything - that's the debounce, also unverifiable
   without a real sleep/wake cycle.
