# scry

A fuzzy filename-search tool for macOS, built to match voidtools' Everything for
Windows: results as you type, with no perceptible latency, over directories you
choose. Design in full: [`everything-macos-design.md`](../everything-macos-design.md).

## Status

**All 8 phases complete** (see the design doc's §10 build order):

- ✅ Config parsing, root normalization/collapse (`internal/config`, `internal/roots`)
- ✅ Per-root shard data model (`internal/index`)
- ✅ Naive `filepath.WalkDir` crawler (`internal/crawler`)
- ✅ Two-pass fuzzy matching and scoring (`internal/fuzzy`)
- ✅ Multi-shard parallel query with merge and ranking (`internal/query`)
- ✅ `scry <query>`, `scry root add/rm/list`, `scry index` CLI (`cmd/scry`)
- ✅ FSEvents watcher, incremental updates, offline roots (`internal/fsevents`,
  `internal/watcher`)
- ✅ Atomic per-shard snapshots and the recrawl scheduler (`internal/snapshot`,
  `internal/reconcile`)
- ✅ Full query syntax (`internal/qsyntax`), the socket protocol (`internal/ipc`),
  resident daemon mode, and `--serve` web UI (`internal/web`)
- ✅ `.app` bundle, menu bar item, LaunchAgent (`packaging/`, `internal/menubar`)
- ✅ Global hotkey, Spotlight-style panel (`internal/hotkey`, `internal/panel`)

Verified green on Windows and on macOS 15.3.2 arm64, including
`CGO_ENABLED=1 go test -race ./...`. The installed `.app` has been exercised end to
end — LaunchAgent → status item process → socket → live FSEvents update → web UI.
The things that need a human at the screen (icon rendering in light and dark, the
panel drawing, the hotkey firing) are listed in
[`packaging/MANUAL-VERIFY.md`](packaging/MANUAL-VERIFY.md).

Without a running daemon, every command still loads each configured root from its
on-disk snapshot (crawling only a root whose snapshot is missing, corrupt, or
stale) — see [Daemon mode](#daemon-mode) for the resident, warm-in-RAM path the
design is actually built around. Load and query timings are printed to stderr so
either path stays visible; the label always says honestly whether a root was
**loaded** from snapshot or **crawled** fresh.

Development happens on Windows; the shipping target is macOS. The macOS-only code is
deliberately confined to four thin `//go:build darwin` files — `internal/fsevents`,
`internal/hotkey`, `internal/panel`, and `internal/menubar`'s systray wiring — each
paired with a non-darwin stub so `go build ./... && go test ./...` stays green on
Windows with no Mac present. Everything with real logic in it (the watcher's apply
rules, hotkey combo parsing, count formatting, query, ranking) lives outside those
files and is tested on Windows against fakes. The socket transport is one exception
worth calling out:
it targets a unix socket per the design, and that has been confirmed to work with
Go's stdlib on Windows 10+ too, so there has been no need for an OS-specific
fallback path in practice — the fallback exists and is tested, but as a
belt-and-suspenders measure for a machine where `AF_UNIX` genuinely isn't usable,
not because this dev machine needed it.

## Build

```
go build ./...
```

Requires Go 1.26.5+ (see `go.mod`). The only third-party dependency is
[`BurntSushi/toml`](https://github.com/BurntSushi/toml) for config parsing.

## Run

First, add at least one root to index:

```
scry root add ~/Documents
scry root add ~/code
```

Then search:

```
scry rprt24
```

prints ranked `score  path` lines to stdout, plus a timing line to stderr. If a
daemon (see below) is running, the query goes over the socket and the timing line
says `via daemon`; otherwise it says so and falls back to loading (or crawling)
every root in-process.

Other commands:

```
scry root list     # path, entry count, online status — loads/crawls to get the count
scry root rm ~/code
scry index         # crawl every configured root fresh and report per-root Stats
scry status         # per root: entry count, online status, snapshot size/age
```

`scry status` also prefers the daemon over the socket when one is running.

## Daemon mode

```
scry daemon                        # resident: warm shards in RAM, socket, recrawl scheduler
scry daemon --serve                # also serve the web UI at http://127.0.0.1:8973/
scry daemon --serve=9000           # web UI on a different port, still 127.0.0.1 only
scry daemon --serve=127.0.0.1:9000 # same, spelled out
scry stop                          # ask a running daemon to exit cleanly
```

This is the point of the design: `scry daemon` loads every configured root from
its snapshot once, holds the shards in RAM, and answers every `scry <query>` /
`scry status` from any other terminal over a socket instead of re-loading
anything. It also runs the recrawl scheduler (`internal/reconcile`) in the
background — once a day per root by default — saving a fresh snapshot after each
pass.

The daemon survives a bad query: a panic inside request handling is recovered per
request, logged, and turned into an error response, and the daemon keeps serving
every other connection.

**Transport:** a unix socket at `<CacheDir>/sock` (`~/.cache/scry/sock`, or
`~/Library/Caches/scry/sock` on macOS). A socket file left behind by a crashed
daemon is detected (nothing answers it) and replaced automatically on the next
`scry daemon` startup — it never blocks a restart. If `AF_UNIX` isn't usable on a
given machine, the daemon falls back to a loopback TCP listener and records the
port it bound in `<CacheDir>/port`; `internal/ipc`'s `Dial` tries the unix socket
first and falls back to reading that port file, so callers never need to know
which transport actually got used. Either way, only `127.0.0.1` / the current
user's own socket is ever bound — never `0.0.0.0`.

## Web UI (`--serve`)

`scry daemon --serve` serves one self-contained HTML page (`internal/web`,
embedded with `embed.FS` — no frameworks, no CDN, no build step) at
`http://127.0.0.1:8973/` by default:

- A single search box; results update as you type (50ms debounce).
- Each result shows name, path, size, and modified time.
- <kbd>&uarr;</kbd>/<kbd>&darr;</kbd> move the selection, <kbd>Enter</kbd> copies
  the selected path to the clipboard.
- Dark mode follows the OS via `prefers-color-scheme` — no toggle needed.
- `GET /search?q=...&limit=...` returns the same results as JSON, if you want to
  script against it directly.

`--serve`'s address argument can only ever change the *port*: the page always
binds to `127.0.0.1` regardless of what's passed, enforced independently of
whatever parsed the flag.

## Query syntax

| Form | Meaning |
|---|---|
| `rprt24` | fuzzy match against the name |
| `"foo bar"` | literal substring, no fuzzing, spaces preserved |
| `*.go`, `rep?rt` | glob against the base name |
| `ext:go,rs` | extension filter, comma-separated, leading dot optional |
| `path:src` | substring must appear in the full path, not just the base name |
| `root:code` | restrict results to roots whose path contains this substring |
| `!vendor` | negation — prefix any form with `!` to invert it |

Terms are whitespace-separated and ANDed together. Multiple bare fuzzy terms (e.g.
`report q24`) all have to match; their scores are summed. A query made up of only
filters (e.g. `ext:go`) is valid and returns every filter-matched entry, all at
score 0, in the same tiebreak order fuzzy results use.

One asymmetry worth knowing: a *negated fuzzy* term (`!vendor`) excludes by base
name only — there's no way to express "score this lower" as a boolean filter, so
it's evaluated as a substring exclusion on the name, not the full path. To exclude
by full path, negate a `path:` term instead (`!path:vendor`).

## Configuration

`~/.config/scry/config.toml` (or `~/Library/Application Support/scry/config.toml`
on macOS if `~/.config` doesn't exist). `scry root add/rm` is the primary way to
manage it; hand-editing works too:

```toml
[[root]]
path = "~/Documents"

[[root]]
path = "~/code"
exclude = ["target", "vendor"]      # in addition to the global list

[[root]]
path = "/Volumes/Archive"
offline_policy = "keep"             # keep | drop — behaviour when unmounted

[exclude]                            # applied to every root
names = ["node_modules", ".git", ".venv", "__pycache__", "build", "dist"]
globs = ["*.tmp", "*.o", "*.pyc"]

[index]
follow_symlinks = false
hidden = false
```

Adding a root that is already contained in (or contains) an existing root collapses
into the existing one — `scry root add` reports any roots absorbed this way.

## Tests

```
go test ./...
```

Covers config, roots, index, fuzzy, crawler, qsyntax, query, snapshot, reconcile,
ipc, and web in isolation, plus an end-to-end suite (`internal/integration`) that
crawls a real temp directory tree and exercises search ranking and every query
syntax filter against it. `internal/ipc`'s tests cover a full round trip over a
real listener, stale-socket recovery, a handler panic not taking down the
connection or the listener, and refusing to steal a socket from a daemon that's
actually still running.

## Install on macOS

Ships as a proper `.app` bundle plus a per-user LaunchAgent, per the design
doc's [§7](../everything-macos-design.md#7-menu-bar-app-and-launching-at-login).
Everything lives under [`packaging/`](packaging/).

```sh
packaging/install.sh
```

Builds `scry.app` (`packaging/build-app.sh`), installs it to `/Applications`
(or `~/Applications` if that isn't writable), and loads the LaunchAgent so
`scry daemon` starts at login and on every crash (`RunAtLoad` + `KeepAlive`).
It's a **LaunchAgent, not a LaunchDaemon** — a daemon has no window-server
connection and can't draw a menu bar item; see §7 for why that distinction is
load-bearing.

```sh
packaging/uninstall.sh            # reverses install.sh completely
packaging/uninstall.sh --purge    # ...and also deletes the index/config
```

By default, uninstalling leaves your index and config
(`~/.cache/scry`, `~/.config/scry`) in place — it says so when it runs.

**Before you do a lot of rebuild-and-reinstall cycles**, read
[`packaging/SIGNING.md`](packaging/SIGNING.md) and set up the self-signed
code signing certificate it describes. Without it, every rebuild changes the
app's ad-hoc signing identity, and macOS treats it as a brand-new app —
re-prompting for Documents/Desktop/Downloads access every single time.

See [`packaging/MANUAL-VERIFY.md`](packaging/MANUAL-VERIFY.md) for the
handful of checks (menu bar item, Login Items, TCC behavior) that need a
real GUI session and can't be confirmed from a script.
