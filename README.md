---
type: Project Documentation
title: scry
description: An "Everything" for macOS — instant fuzzy filename search over the directories you choose, from the menu bar, a hotkey panel, the terminal, or a local web page.
resource: https://github.com/jake-kelley/scry
tags: [macos, search, go, menu-bar, fsevents, launchagent]
timestamp: 2026-08-14T19:30:00-06:00
---

<h1 align="center">
  <img src="docs/logo.png" alt="" width="80"><br>
  scry
</h1>

**Instant filename search for macOS.** Press <kbd>⌥</kbd><kbd>Space</kbd>, type a
few characters, get ranked matches — from an index of your own directories that
lives in RAM and updates itself as files change. A menu bar app, a search panel,
a CLI, and a local web page, from one small Go binary with a single dependency.

Modelled on [voidtools' Everything](https://www.voidtools.com/) for Windows,
which scry exists because macOS has no equivalent of. The full design, and the
reasoning behind every structural choice, is in [`docs/design.md`](docs/design.md).

## Why scry

- **It only indexes what you point it at.** Not the whole disk, not your mail, not
  file *contents* — names and paths, for the roots you add. That is why the index
  is small enough to hold open: a 43,000-entry home directory is a 2.5 MB snapshot.
- **It answers from memory.** A resident daemon holds every root's shard in RAM
  and serves queries over a local socket, so a search is a scan of a slice, not a
  disk read. Every query prints its own timing to stderr — you never have to take
  that on faith.
- **It stays current on its own.** FSEvents applies creates, renames and deletes
  within about a second. Restart, reboot, or wake from sleep and it replays the
  event history it missed rather than re-walking your disk.
- **Nothing leaves the machine.** The socket is your own user's; the web UI binds
  to `127.0.0.1` and cannot be told to bind anywhere else. There is no network
  code beyond that, no telemetry, and no update check.

## What's in the box

| | |
|---|---|
| **Search** | Two-pass fuzzy matching — exact substring beats prefix beats token-boundary beats scattered subsequence — with ranking, so the file you meant is at the top rather than merely present. |
| **Query syntax** | `ext:go,rs`, `path:src`, `root:code`, `"literal phrase"`, `*.go` globs, and `!` to negate any of them. [Details below](#query-syntax). |
| **Menu bar** | A template icon that follows light and dark automatically, over a menu with Search, a live entry count, Rebuild index, Preferences and Quit. No Dock icon, no app menu — `LSUIElement`. |
| **Hotkey panel** | <kbd>⌥</kbd><kbd>Space</kbd> opens a Spotlight-style panel from anywhere. Configurable. |
| **Web UI** | `scry daemon --serve` puts the same search at `http://127.0.0.1:8973/` — one embedded HTML page, no frameworks, no CDN, no build step, dark mode included. |
| **CLI** | `scry <query>`, `scry root add/rm/list`, `scry index`, `scry status`. Tab-separated `score<TAB>path` on stdout, timings on stderr — it pipes. |
| **Runs at login** | Installs as a per-user LaunchAgent, restarts on crash, comes back after a reboot. |

## Get started

You need macOS (Apple Silicon or Intel) and [Go 1.26.5+](https://go.dev/dl/).
scry is built from source on the machine it runs on — it uses cgo for FSEvents,
IOKit and the menu bar, so there is no cross-compiled binary to download.

```sh
git clone https://github.com/jake-kelley/scry.git
cd scry
packaging/install.sh
```

That builds `scry.app`, installs it to `/Applications` (or `~/Applications` if
that isn't writable), symlinks the `scry` CLI onto your PATH, and loads the
LaunchAgent so it starts at every login. The magnifying glass appears in your
menu bar. Re-running it is safe — that is how you upgrade.

Then tell it what to index:

```sh
scry root add ~/Documents
scry root add ~/code
```

macOS will ask for permission the first time a root touches Documents, Desktop or
Downloads. Grant it, then use **Rebuild index** from the menu.

> **Doing repeated rebuild-and-reinstall cycles?** Read
> [`packaging/SIGNING.md`](packaging/SIGNING.md) first and set up the self-signed
> certificate it describes. Without one, every rebuild changes the app's ad-hoc
> signing identity, macOS sees a brand-new app, and you re-answer every
> permission prompt — every time.

## Everyday use

| | |
|---|---|
| <kbd>⌥</kbd><kbd>Space</kbd> | Open the search panel from any app |
| <kbd>↑</kbd> <kbd>↓</kbd> | Move the selection |
| <kbd>Enter</kbd> | Copy the selected path to the clipboard |
| <kbd>Esc</kbd> | Dismiss |

From a terminal:

```sh
scry rprt24            # ranked score/path lines on stdout, timing on stderr
scry ext:go path:cmd   # filters compose; terms are ANDed
scry status            # per root: entry count, online status, snapshot size and age
scry root list
scry index             # crawl every root fresh, report per-root stats
```

Every command prefers a running daemon over doing the work itself, and says which
one it used. With no daemon, each command loads the roots from their on-disk
snapshots (crawling only a root whose snapshot is missing, corrupt, or stale), so
nothing depends on the menu bar app being up.

### Running the daemon by hand

```sh
scry daemon                        # resident: shards in RAM, socket, scheduler
scry daemon --serve                # also serve the web UI on 127.0.0.1:8973
scry daemon --serve=9000           # different port, still 127.0.0.1 only
scry stop                          # ask a running daemon to exit cleanly
```

The installed LaunchAgent runs `scry menubar`, which is the same resident core
with the status item on top — you do not need to run `scry daemon` yourself
unless you want the web UI or are debugging.

The socket is `~/Library/Caches/scry/sock`. One left behind by a crashed daemon
is detected and replaced on the next start; it never blocks a restart. If
`AF_UNIX` isn't usable, the daemon falls back to a loopback TCP listener and
records the port in `<CacheDir>/port` — callers never need to know which
transport was used. A panic inside one request is recovered, logged, and returned
as an error; the daemon keeps serving everything else.

## Query syntax

| Form | Meaning |
|---|---|
| `rprt24` | fuzzy match against the name |
| `"foo bar"` | literal substring, no fuzzing, spaces preserved |
| `*.go`, `rep?rt` | glob against the base name |
| `ext:go,rs` | extension filter, comma-separated, leading dot optional |
| `path:src` | substring must appear in the full path, not just the base name |
| `root:code` | restrict to roots whose path contains this substring |
| `!vendor` | negation — prefix any form with `!` to invert it |

Terms are whitespace-separated and ANDed. Multiple bare fuzzy terms (`report q24`)
must all match, and their scores are summed. A query of only filters (`ext:go`) is
valid and returns everything that matches, at score 0, in the same tiebreak order
fuzzy results use.

One asymmetry worth knowing: a *negated fuzzy* term (`!vendor`) excludes by base
name only — there is no way to express "score this lower" as a boolean, so it is
evaluated as a substring exclusion on the name. To exclude by full path, negate a
`path:` term instead: `!path:vendor`.

## Your data

Two directories, both yours, both plain files:

```
~/.config/scry/config.toml           # or ~/Library/Application Support/scry/
~/Library/Caches/scry/
  shards/<hash>.idx                  # one snapshot per root, written atomically
  sock                               # daemon socket
~/Library/Logs/scry/scry.{log,err}   # daemon output
```

`scry root add/rm` is the normal way to edit the config. Hand-editing works too:

```toml
[[root]]
path = "~/Documents"

[[root]]
path = "~/code"
exclude = ["target", "vendor"]      # in addition to the global list
exclude_paths = ["~/code/scratch"]  # one specific tree, not a name

[[root]]
path = "/Volumes/Archive"
offline_policy = "keep"             # keep | drop — behaviour when unmounted

[exclude]                            # applied to every root
names = ["node_modules", ".git", ".venv", "__pycache__", "build", "dist"]
globs = ["*.tmp", "*.o", "*.pyc"]
paths = ["~/Library"]                # anchored: this directory only

[index]
follow_symlinks = false
hidden = false
recrawl_interval = "24h"             # minimum 30s; "off" to disable

[hotkey]
combo = "alt+space"                  # e.g. "cmd+shift+space", "ctrl+alt+f"
```

**`names`/`globs` versus `paths`.** `names` and `globs` match a *base name*, so
they skip every matching directory anywhere in the tree — right for
`node_modules`, wrong when you mean one particular directory. `paths` entries are
*anchored*: each names a single absolute (or `~`-prefixed) location and excludes
it and everything under it. Excluding `~/Library` as a `name` would also lose
`~/Pictures/Photos Library.photoslibrary` and any `Library/` inside a checked-out
project; as a `path` it excludes exactly the one directory. A bare name in `paths`
is rejected at load rather than silently matching nothing.

Adding a root already contained in (or containing) an existing root collapses into
the existing one — `scry root add` reports what it absorbed.

**After any config change, use the menu bar's "Rebuild index".** A running daemon
holds snapshots built under the *old* rules, so an edited exclude list otherwise
has no visible effect.

## How it stays current

FSEvents is the update mechanism. The watcher applies creates, renames and deletes
to the live index within about a second, and every daemon start replays the event
history from each shard's saved position — so a restart or a reboot catches up on
what changed while it was down, without re-walking anything.

**The recrawl is a backstop, not the mechanism.** `recrawl_interval` (default 24h)
re-walks each root from scratch to catch drift the watcher could have missed:
events the kernel dropped under pressure, or a root that was unmounted and came
back changed. It is not cheap — measured on a 43,000-entry macOS home directory, a
full crawl is about 50 seconds warm and over two minutes cold. Anything under 30s
is rejected.

The interval is the gap *between* passes, not a fixed rate: the timer restarts
when a pass finishes, so the observed period is the interval plus the crawl. Two
passes never overlap.

`recrawl_interval = "off"` (or `"never"`, `"none"`, `"0"`) turns it off entirely,
leaving FSEvents as the only automatic update. That is a reasonable choice, and
what this project's own machine runs — what you give up is exactly the drift case
above, which then persists until you use **Rebuild index**.

A reconcile pass diffs against the previous snapshot instead of blind-swapping,
skips the write when nothing changed, and logs `+added -removed ~changed`. It also
refuses two shapes of accident: a crawl that returns zero entries while reporting
errors, and a crawl that reports errors *and* would remove more than half the
index. Both look like "your files are gone" and are almost always an unmounted or
unreadable subtree. An error-free crawl is always trusted, however much it removes
— deleting a lot of files is a legitimate thing to do, and it produces no errors.

**Waking from sleep.** The daemon does not restart when the lid closes, so the
FSEvents stream has to be resynchronised rather than recreated. On a real system
wake — detected via IOKit, not a timer — scry always does the cheap thing first:
restart the stream from each shard's saved position, replaying what happened while
the machine slept. Only if that fails does it consider anything heavier, and if
`recrawl_interval = "off"` it will never escalate to a full crawl. A wake alone
never costs you the crawl you turned off.

## Build from source

```sh
go build ./...     # or: go build -mod=vendor -o scry ./cmd/scry
go test ./...
```

Go 1.26.5+. The only third-party dependency is
[`BurntSushi/toml`](https://github.com/BurntSushi/toml), vendored.

Tests cover config, roots, index, fuzzy, crawler, qsyntax, query, snapshot,
reconcile, power, ipc and web in isolation, plus an end-to-end suite
(`internal/integration`) that crawls a real temp tree and exercises ranking and
every query filter against it. `internal/ipc`'s tests cover a full round trip over
a real listener, stale-socket recovery, a handler panic taking down neither the
connection nor the listener, and refusing to steal a socket from a daemon that is
still alive. Green on macOS 15.3.2 arm64 including `CGO_ENABLED=1 go test -race
./...`, and on Windows.

**Development happens on Windows; the target is macOS.** The macOS-only code is
confined to a handful of thin `//go:build darwin` files across five packages —
`internal/fsevents`, `internal/hotkey`, `internal/panel`, `internal/power`, and
`internal/menubar`'s systray wiring — each paired with a non-darwin stub, so
`go build ./... && go test ./...` stays green with no Mac present. Everything with real logic in it (the
watcher's apply rules, combo parsing, query, ranking, the wake coordinator's
policy) lives outside those files and is tested against fakes.

The checks that genuinely need a human at a screen — icon legibility in light and
dark, the panel drawing, the hotkey firing, the sleep/wake path — are listed in
[`packaging/MANUAL-VERIFY.md`](packaging/MANUAL-VERIFY.md).

## Uninstall

```sh
packaging/uninstall.sh            # reverses install.sh completely
packaging/uninstall.sh --purge    # ...and also deletes the index and config
```

By default it leaves `~/.config/scry` and `~/Library/Caches/scry` in place, and
says so as it runs.

One thing to know about **Quit**: the LaunchAgent sets `KeepAlive`, so Quit boots
the job out of your GUI session first — otherwise launchd would relaunch it
instantly and Quit would look broken. That bootout lasts for the login session
only, so scry comes back at your next login. `uninstall.sh` is what removes it for
good.

## License

[MIT](LICENSE). Use it anywhere, for anything, including commercially — the only
condition is that the copyright notice travels with copies of the source.
