# scry

A fuzzy filename-search tool for macOS, built to match voidtools' Everything for
Windows: results as you type, with no perceptible latency, over directories you
choose. Design in full: [`everything-macos-design.md`](../everything-macos-design.md).

## Status

**Phases 1–3 of 8 complete** (see the design doc's §10 build order):

- ✅ Config parsing, root normalization/collapse (`internal/config`, `internal/roots`)
- ✅ Per-root shard data model (`internal/index`)
- ✅ Naive `filepath.WalkDir` crawler (`internal/crawler`)
- ✅ Two-pass fuzzy matching and scoring (`internal/fuzzy`)
- ✅ Multi-shard parallel query with merge and ranking (`internal/query`)
- ✅ `scry <query>`, `scry root add/rm/list`, `scry index` CLI (`cmd/scry`)
- ⬜ FSEvents watcher, incremental updates, offline roots (phase 4)
- ⬜ Atomic snapshots and recrawl timer (phase 5)
- ⬜ Extended query syntax, unix socket protocol, `--serve` web UI (phase 6)
- ⬜ `.app` bundle, menu bar item, LaunchAgent (phase 7)
- ⬜ Global hotkey, Spotlight-style panel (phase 8)

**Every command right now crawls its configured roots fresh on startup** — there is
no resident daemon or on-disk snapshot yet, so each invocation pays the full crawl
cost. That cost, and query latency on top of it, are printed to stderr so it stays
visible. This is expected to go away in phase 5.

Development happens on Windows; the shipping target is macOS. Everything built so
far is deliberately OS-independent (no build tags, no platform-specific syscalls) —
phases 4 and 7 are where macOS-only code (FSEvents, `NSStatusItem`) has to start.

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

prints ranked `score  path` lines to stdout, plus a crawl/query timing line to
stderr.

Other commands:

```
scry root list     # path, entry count, online status — crawls to get the count
scry root rm ~/code
scry index         # crawl every configured root and report per-root Stats
```

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

Covers config, roots, index, fuzzy, crawler, and query in isolation, plus an
end-to-end test (`internal/integration`) that crawls a real temp directory tree and
asserts the expected file ranks first in a subsequent search.
