# Manual verification (run these at the Mac, not over SSH)

An SSH session has no Aqua/window-server connection, so none of this can be
confirmed remotely — everything else about the bundle (structure, signing,
plist validity, install/uninstall) was verified over SSH and doesn't need
repeating here.

1. **Menu bar item actually appears.**
   Run `packaging/install.sh`, then look at the menu bar.
   *Expected*: as of this task, nothing appears yet — the systray/status-item
   code is phase 7's UI work (see `packaging/NOTES-FOR-UI.md`), not part of
   this packaging task. What you're actually confirming here is that the
   LaunchAgent starts the bundled binary without crashing and without a
   Dock icon or app menu showing up (`LSUIElement` doing its job). Check
   Activity Monitor for a running `scry` process, and check the Dock: it
   should **not** have a scry icon.

2. **Login Items surfacing.**
   System Settings → General → Login Items. After `install.sh`, "scry"
   should be listed under the LaunchAgents/background items section, and its
   toggle should be able to disable it (macOS 13+ behavior described in
   design doc §7 — this is intentional, not something to defeat).

3. **TCC prompt behavior.**
   Add a root under `~/Documents` (`scry root add ~/Documents`), then
   `packaging/uninstall.sh && packaging/install.sh` (a full rebuild+reinstall
   cycle). If a `scry-codesign` identity is set up (`packaging/SIGNING.md`),
   you should **not** get a repeat Documents permission prompt on the second
   install. If you do get repeat prompts, check
   `codesign -dv /Applications/scry.app` for `Authority=scry-codesign` — if
   that's missing, the identity wasn't found or wasn't used.

4. **Quitting/relaunch via launchctl.**
   `launchctl kickstart -k gui/$(id -u)/com.jakekelley.scry` should kill and
   restart the daemon (KeepAlive) without you having to re-run install.sh.
   Confirm a new `scry` process appears in Activity Monitor shortly after.

5. **Uninstall leaves no trace.**
   After `packaging/uninstall.sh`, confirm: no `scry` process running, no
   scry.app in /Applications or ~/Applications, no entry in Login Items, and
   (per uninstall.sh's own printed note) your index/config under
   `~/.cache/scry` and `~/.config/scry` are still there unless you passed
   `--purge`.
