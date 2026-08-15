# scry — an "Everything" for macOS

A fuzzy filename-search daemon with Everything's defining property — **results as you
type, with no perceptible latency** — over directories the user chooses.

Constraints driving every decision:

- **User-defined roots.** The indexed set is configuration, not policy. Add and remove
  roots at runtime without rebuilding anything else.
- **Fuzzy search only.** No embeddings, no vector store, no model. Decided; see §5.
- **Very lightweight.** Target under 30MB resident at a typical configuration.
- **Extremely reliable.** The index is a cache, never truth. Unplugging a drive must not
  destroy an index.
- **Always there.** Starts at login, lives in the menu bar, never needs a terminal.

## 1. Configuration

`~/.config/scry/config.toml`, falling back to `~/Library/Application Support/scry/`.

```toml
[[root]]
path = "~/Documents"

[[root]]
path = "~/code"
exclude = ["target", "vendor"]      # in addition to the global list

[[root]]
path = "/Volumes/Archive"
offline_policy = "keep"             # keep | drop  — behaviour when unmounted

[exclude]                            # applied to every root
names = ["node_modules", ".git", ".venv", "__pycache__", "build", "dist"]
globs = ["*.tmp", "*.o", "*.pyc"]

[index]
follow_symlinks = false
hidden = false
```

Hand-editing TOML is friction, so roots are primarily managed by CLI:

```
scry root add ~/code          # crawls just that root, ~1s, others untouched
scry root rm ~/code           # drops one shard file; others untouched
scry root list                # path, entry count, status, last verified
```

Excluding `node_modules` and friends by default matters more than it looks: on a
developer's machine they are frequently the *majority* of files and never the answer.

### Root normalization

Resolve symlinks, `filepath.Clean`, then **collapse containment**. If the user adds
`~/code` while `~/code/project` is already a root, the child is absorbed; adding a child
of an existing root is a no-op. Skipping this silently double-indexes and shows every
file twice.

## 2. Why Everything is fast, and what ports

Everything reads the NTFS **Master File Table** directly and subscribes to the **USN
Change Journal** — a durable, replayable log of every filesystem change.

| Windows | macOS | Notes |
|---|---|---|
| Read NTFS MFT | `getattrlistbulk(2)` | No public API reads the APFS catalog B-tree; the raw device is FileVault-encrypted. Bulk enumeration is the supported fast path. |
| USN Change Journal | **FSEvents** | Durable and replayable via persistent event IDs. The real analogue, and why restart-resume works. |

- There is **no MFT shortcut on APFS**. First scan of a root is a directory walk —
  a second or two at typical root sizes.
- **Use FSEvents, not `fsnotify`.** `fsnotify` uses kqueue on Darwin: one file descriptor
  per watched file, unusable at scale.
- Shelling out to `mdfind` fails the goal — Spotlight skips excluded directories, honours
  `.metadata_never_index`, has no fuzzy name matching, and goes stale.

## 3. Components

```
   config.toml ──▶┌──────────────┐
                  │ Root Manager │  normalize, collapse, add/remove
                  └──────┬───────┘
                         │
        ┌────────────────┼────────────────┐
        ▼                ▼                ▼
   ┌─────────┐      ┌─────────┐      ┌─────────┐
   │ Shard 1 │      │ Shard 2 │      │ Shard N │   one per root,
   │ ~/Docs  │      │ ~/code  │      │ /Volumes│   independently
   └────┬────┘      └────┬────┘      └────┬────┘   loaded & rebuilt
        └────────────────┼────────────────┘
                         │              ▲
   FSEvents ──▶┌──────────────┐         │ atomic per-shard snapshots
   (one stream │   Watcher    │─────────┘
    N paths)   └──────────────┘
                         │
                         │
                   ┌──────────────┐
                   │  Query API   │  scan all shards, merge, rank
                   └──────┬───────┘
                          │ unix socket ~/.cache/scry/sock
             ┌────────────┼────────────┐
             ▼            ▼            ▼
        ┌─────────┐  ┌─────────┐  ┌─────────┐
        │ menu bar│  │scry CLI │  │ web UI  │
        │  item   │  │         │  │ :PORT   │
        └─────────┘  └─────────┘  └─────────┘
```

Single process, resident, launched at login by a **launchd LaunchAgent** (§7). The warm
in-RAM index is not an optimization — it *is* the product.

## 4. Data model — per-root shards

Each root owns a completely independent index. This is the change that makes runtime root
management tractable: adding, removing, or rebuilding a root touches exactly one shard.

```go
type Shard struct {
    root    string
    online  bool     // false when the volume is unmounted

    // Hot arrays — the only memory touched during a search.
    names   []byte   // names concatenated, lowercased, NUL-separated
    nameOff []uint32
    nameLen []uint16
    parent  []uint32 // index within this shard; the root points at itself
    flags   []uint8  // isDir, isSymlink, isHidden

    // Cold arrays — parallel, read only for entries in the result set.
    size  []int64
    mtime []int64

    children map[uint32][]uint32 // directories only (~10%), for rescan diffs
    free     []uint32            // tombstoned slots awaiting reuse
    lastEID  uint64              // FSEvents ID this shard is current as of
}
```

Never store full paths — a name plus a parent pointer, with paths reconstructed only for
displayed results.

| Entries indexed | Resident | Query |
|---|---|---|
| 50k (a few doc folders) | ~4MB | <1ms |
| 250k (typical: home minus Library) | ~15MB | ~2ms |
| 1M (aggressive, many volumes) | ~60MB | ~8ms |

**Measured 2026-08-14** (phase 1–3 build, Ryzen 7 9800X3D, Windows): 5,971 entries crawled
in 48ms; query `design` took **518µs** end to end including path reconstruction and
sorting — about 87ns per entry. `fuzzy.Filter` alone benchmarks at ~20ns per name
(0 allocs); the remaining 67ns is `Score` on survivors plus result assembly.

**Measured 2026-08-14 on the real target** (macOS, Apple Silicon, whole home directory):
**579,781 entries** crawled in 33.5s; query `gomod` took **5.05ms**. That is ~9ns per
entry — an order of magnitude better than the Windows extrapolation above suggested, and
comfortably inside the table's estimate.

The extrapolation was wrong because the 5,971-entry corpus was unrepresentative: on a small
tree a large fraction of names survive `Filter`, so DP scoring and sorting dominate the
per-entry cost. On a real 580k-entry tree almost nothing survives the filter, so the
allocation-free byte scan dominates and the per-entry cost collapses. **Per-entry cost falls
as the index grows**, because selectivity rises faster than the entry count. Do not
extrapolate this design's query latency from a small corpus.

No query-layer optimisation is needed. 5ms is well under the ~16ms frame budget, so the
as-you-type panel in step 8 is viable as designed.

**Phase 5–6 measured, same machine:** snapshot load **2.1ms** against a 48ms cold crawl —
a 24x startup win, and the reason the daemon can restart without anyone noticing. Daemon
startup from snapshot is 2.1ms end to end; queries over the socket and the web UI both
return the same results as the in-process path.

Deletions **tombstone** (`nameLen = 0`, slot to `free`). Compact on snapshot write, never
during a query.

Snapshots are **one file per shard**, at `~/.cache/scry/shards/<hash-of-root>.idx`. A
corrupt shard recrawls only its own root; removing a root deletes one file.

## 5. Search — fuzzy

Fuzzy matching is the entire product, so it earns the detail. Semantic/embedding search
was evaluated and **rejected**: filenames are too short to embed usefully, content
embedding would need a model, a vector store, and 20+MB of resident vectors, and fuzzy
token matching covers the great majority of what "search my files smartly" actually
means — at zero dependency cost and with no failure modes.

### Two-pass matching

Scoring 250k names with a full alignment algorithm on every keystroke is too slow. Split
it:

1. **Filter (cheap).** A tight byte loop: does the name contain the query's characters in
   order at all? No scoring, no allocation. Scans the ~5MB arena in 1–2ms and typically
   rejects 95%+ of entries.
2. **Score (expensive, few candidates).** Full scoring with match positions runs only on
   survivors — usually a few thousand. Negligible.

Shards are scanned in parallel, one goroutine each, which also keeps the largest root
from dominating latency.

### Scoring

Rank by, in descending weight:

- **exact substring** > prefix-of-name > token-boundary subsequence > scattered subsequence
- **boundary bonuses:** match starts a path component, a camelCase hump, or follows
  `-`, `_`, `.`, or a digit boundary — this is what makes `qr24` find `QuarterlyReport24`
- **gap penalty:** proportional to the distance between matched characters
- **tiebreakers:** shorter name, more recent `mtime`, shallower path

`sahilm/fuzzy` gets you moving in an afternoon. Expect to reimplement it (~200 lines) once
you want control over the boundary bonuses and match positions for result highlighting.

### Query syntax

| Form | Meaning |
|---|---|
| `rprt24` | fuzzy match against the name |
| `"foo bar"` | literal substring, no fuzzing |
| `*.go`, `rep?rt` | glob |
| `ext:go,rs` | extension filter |
| `path:src` | match against the full path, not just the name |
| `root:code` | restrict to one root |
| `!vendor` | negation |

Cap at ~200 results before sorting; nobody reads result 900.

## 6. Incremental updates

**One FSEvents stream with N paths**, not one stream per root — cheaper, and it handles
per-path `RootChanged` correctly anyway. Recreate the stream whenever the root set changes.

```go
stream := &fsevents.EventStream{
    Paths:   rootManager.Paths(),
    Latency: 300 * time.Millisecond,
    EventID: rootManager.MinLastEID(),    // oldest shard's position
    Flags:   fsevents.FileEvents | fsevents.WatchRoot | fsevents.NoDefer,
}
```

FSEvents IDs are host-global, so resuming the combined stream from the **oldest** shard's
ID replays events other shards have already applied. That is safe only because updates are
**idempotent** — add-if-absent, remove-if-present. Preserve that property deliberately; it
is what lets you add a root without invalidating every other shard.

**FSEvents does not promise per-file fidelity.** It coalesces under load and then tells you
to look for yourself:

- `MustScanSubDirs` — re-enumerate that subtree, diff against `children`.
- `RootChanged` / `Unmount` — see offline handling below.
- `EventIdsWrapped` — the stored ID is meaningless; recrawl all shards.
- `HistoryDone` — the replayed backlog ended; you are now live.

A rescan diffs a fresh directory listing against `children[dirID]`: new entries take slots,
missing entries tombstone recursively. **This is the only genuinely subtle correctness
surface in the project — put your tests here.**

### Offline roots

An unmounted external drive must **not** destroy its index. On `Unmount`, mark the shard
`online = false` and keep the snapshot; results from it are hidden by default
(`offline_policy = "keep"`). On remount, verify with a recrawl and bring it back. Deleting
200k entries because someone unplugged a drive is the single worst bug this design can
have, and the shard model makes avoiding it trivial.

## 7. Menu bar app, and launching at login

### LaunchAgent, not LaunchDaemon

`~/Library/LaunchAgents/com.<you>.scry.plist`, with `RunAtLoad`, `KeepAlive`, and
`ProcessType = Background`.

The distinction matters: a **LaunchDaemon** in `/Library/LaunchDaemons` runs in session 0
with no window server connection and **cannot draw a menu bar item at all**. It must be a
per-user LaunchAgent. macOS 13+ additionally surfaces it under Settings → General → Login
Items where the user can switch it off — correct behaviour, not something to defeat.

### Ship an `.app` bundle

A bare Go binary *can* create an `NSStatusItem`, but you want the bundle:

```
scry.app/Contents/
  MacOS/scry
  Info.plist          LSUIElement = true
  Resources/icon.icns
```

`LSUIElement = true` is the load-bearing key: **no Dock icon, no application menu**, just
the status item. Without it, a background utility takes up a Dock slot.

### Status item

`fyne.io/systray` — the maintained fork of `getlantern/systray` — wraps `NSStatusItem` in
about twenty lines. Two details that are easy to get wrong:

- **Use a template icon** (`SetTemplateIcon`): a monochrome image with an alpha channel,
  which macOS recolours for light and dark menu bars. A coloured PNG looks broken in one
  of the two, and you will only notice after shipping.
- **`systray.Run()` owns the main thread.** AppKit requires all UI on thread 1, so the
  indexer, watcher and HTTP server all start as goroutines from `onReady`. This inverts
  the usual `main()` structure and is the **single biggest structural consequence** of
  adding a UI to this design.

```
  scry
  ─────────────────────
  Search…          ⌥Space
  247,391 files indexed
  Rebuild index
  Preferences…
  ─────────────────────
  Quit
```

`NSMenu` cannot hold a usable text field, so the menu is status and commands only —
**"Search…" has to open a window.**

### The search window

Three options, increasing effort:

1. **`open http://localhost:PORT`** — zero new code, reuses the `--serve` web UI. Start here.
2. **WebView window** (`wails`, or a small `WKWebView` cgo wrapper) — same HTML, no browser tab.
3. **Spotlight-style borderless panel** with a global hotkey via `golang.design/x/hotkey`,
   which uses Carbon `RegisterEventHotKey` on macOS and therefore needs **no Accessibility
   permission**.

Option 1 gets a working product in an hour; option 3 is what makes it feel like Alfred.

### One process or two

**One**, for v1. But route every UI path — menu bar, CLI, web — through the unix socket at
`~/.cache/scry/sock` rather than touching the index directly. The CLI needs that socket
anyway, and preserving the seam means splitting into a headless `scryd` plus a thin menu bar
client later is a build change, not a rewrite. Two launchd agents to keep alive is more
operational surface than the crash isolation is worth at this size.

## 8. Reliability

Specific commitments, not a posture:

1. **The index is a cache, never truth.** `lstat` a result before acting on it; stale hits
   correct themselves lazily rather than surfacing files that are gone.
2. **Atomic snapshots.** Temp file, `fsync`, `rename`. A crash mid-write can never leave a
   partial shard. Version-tag and checksum each; on mismatch, discard and recrawl that
   root only.
3. **A recrawl safety net.** Full reconciliation per root on a low-priority timer — daily,
   and on wake from sleep — bounds FSEvents drift to hours rather than forever. Affordable
   precisely because roots are small and independent.
4. **Bounded everything.** Cap result sets, the rescan queue, and tombstone growth.
   Unbounded queues are how resident daemons become 4GB surprises.
5. **Few cgo boundaries, each small and audited.** This originally read "one cgo boundary —
   FSEvents requires cgo; nothing else in this design does." That was wrong as soon as
   step 8 was taken seriously: a borderless Spotlight-style panel is AppKit, and AppKit is
   cgo. As built there are **three**, and the count is the thing to watch, not the
   principle:

   - `internal/fsevents` — FSEvents, the original one.
   - `internal/hotkey` — Carbon `RegisterEventHotKey`. Called directly rather than via
     `golang.design/x/hotkey`, because that library wants the main thread through its
     `mainthread` package and `systray.Run()` has already claimed it. Registering the
     hotkey on the run loop systray is *already* pumping avoids a second claimant for
     thread 1 entirely — see §9 item 7, which anticipated exactly this.
   - `internal/panel` — a borderless `NSPanel` hosting a `WKWebView` that loads the same
     `internal/web` page the browser fallback uses. The panel is a window, not a second
     search implementation.

   Each is one file, single-purpose, behind a `//go:build darwin` stub pair so the whole
   project still builds and tests on Windows. The commitment that actually matters is that
   these stay small and that no *fourth* one appears without a reason this good.

## 9. macOS gotchas

1. **TCC consent.** `~/Desktop`, `~/Documents`, `~/Downloads` each prompt individually. A
   CLI run from Terminal inherits Terminal's grants; **a launchd agent needs its own**, and
   denial surfaces as ordinary walk errors. Probe each configured root at startup and
   report inaccessible ones in `scry root list` rather than shipping a quietly empty shard.
2. **Firmlinks.** If a user configures `/` as a root, it is a synthesized merge of the
   read-only system volume and the data volume, and will double-index. Detect `/` and
   rewrite it to `/System/Volumes/Data`.
3. **Time Machine snapshots** under `/Volumes/com.apple.TimeMachine.*` multiply the index
   by the snapshot count. Exclude by default even inside a user-chosen root.
4. **Symlink escape.** Default `follow_symlinks = false`: following them lets a crawl leave
   the configured root, which breaks the user's mental model of what is indexed. If enabled,
   track visited inodes to break cycles.
5. **`getattrlistbulk` binding.** `x/sys/unix` exposes `Getattrlist`; confirm whether
   `Getattrlistbulk` is bound in your Go version. A `syscall.Syscall6` wrapper is ~60 lines
   if not — but at these root sizes `filepath.WalkDir` is likely fast enough. Measure first.
6. **Signing identity and TCC.** TCC grants for Documents, Desktop and Downloads are keyed
   to the app's code signing identity. Ad-hoc signing (`codesign -s -`) derives that from
   the binary hash, so **every rebuild looks like a brand-new app and re-prompts**. Make a
   self-signed certificate once and sign with it consistently, or the development loop
   becomes a permission-prompt treadmill.
7. **Run loop coexistence — the prediction was right, the answer was to stop competing.**
   `systray` runs an `NSApplication` run loop on the main thread, and this was indeed where
   the threading problems showed up. Two of them, and neither was solved by a second run
   loop:

   - **FSEvents.** Originally scheduled on a `CFRunLoop` on its own locked thread. That
     works, but `FSEventStreamScheduleWithRunLoop` is deprecated as of macOS 13, and the
     arrangement carried a real race: `CFRunLoopStop` is a documented no-op if the loop is
     not yet inside `CFRunLoopRun`, so a `Stop` immediately after `NewStream` could land as
     a no-op and hang forever. `FSEventStreamSetDispatchQueue` on a **serial** queue keeps
     the only guarantee that mattered — callbacks never overlap — and needs no thread at
     all. Teardown then leans on serialness directly: `dispatch_sync` an empty block, which
     cannot begin until any in-flight callback has finished, and that is exactly the
     condition for closing the events channel without racing a send.
   - **The global hotkey.** `golang.design/x/hotkey` wants the main thread through its
     `mainthread` package, which is a second claimant for thread 1 that `systray.Run()` has
     already taken. Calling Carbon's `RegisterEventHotKey` directly avoids the fight:
     it delivers through whatever run loop is already servicing
     `GetApplicationEventTarget()`, which *is* the loop systray is running. No new thread,
     no new run loop, no coordination.

   The general lesson: when something wants "the main thread" or "a run loop", first ask
   whether it can attach to the one already running. Twice here the answer was yes, and
   both times that deleted code rather than adding it.

## 10. Build order

**Status 2026-08-14: all eight steps are built, tested and committed.** Green on Windows
and on macOS 15.3.2 arm64, including `CGO_ENABLED=1 go test -race ./...`. The installed
`.app` has been verified end to end: LaunchAgent → status item process → socket → live
FSEvents update → web UI. What remains unverified is only what needs eyes on a screen (the
icon rendering in both appearance modes, the panel drawing, the hotkey firing) —
`packaging/MANUAL-VERIFY.md` lists exactly those and nothing else.

Two things this build order got wrong, worth keeping:

- **Step 4 and steps 7–8 were called "require a Mac".** True for *running*, misleading for
  *developing*. Almost all of it — the watcher's apply logic, hotkey combo parsing, count
  formatting, the panel's URL construction — is platform-independent and was written and
  tested on Windows against fakes, with only a thin `//go:build darwin` shim needing the
  Mac. Splitting each feature that way is what kept the remote build loop cheap.
- **"1 day" for step 7 assumed packaging and UI were one job.** They are two independent
  surfaces (`packaging/**` versus `cmd/` + `internal/`) and were built in parallel. The
  bugs that cost the most were neither surface's fault but the seam between them: the
  agent launching the wrong subcommand, and Quit losing a fight with `KeepAlive`. Budget
  for integration, and *install the artefact* — building it proves much less than you
  think.

1. **Weekend.** Config parsing, root normalization/collapse, `filepath.WalkDir` crawler,
   one shard in memory, `scry <query>` CLI with substring matching. Naive walker on purpose.
2. **1 day.** Two-pass fuzzy matching and scoring. This is the product — the point at which
   it starts feeling better than Spotlight.
3. **1 day.** Multi-shard: `scry root add/rm/list`, parallel query across shards, merge.
4. **2–3 days.** FSEvents watcher, incremental updates, rescan flags, offline roots.
5. **1 day.** Atomic per-shard snapshots and the recrawl timer. Startup is now warm and
   survives reboots.
6. **2 days.** Remaining query syntax, the unix socket protocol, and `--serve` — localhost
   HTTP with a single-page as-you-type UI.
7. **1 day.** `.app` bundle, `Info.plist` with `LSUIElement`, self-signed certificate, and
   the LaunchAgent. Status item with a template icon, live index count, and a "Search…"
   item that opens the web UI. **This is the point it stops being a script and starts
   being an app you forget is running.**
8. **1–2 days.** Global hotkey and a borderless Spotlight-style panel, replacing the
   browser tab.

Roughly **a week and a half of evenings** to something you would actually keep installed,
with a usable CLI at the end of step 2 and a real menu bar app at the end of step 7.

## 11. Out of scope

Property indexing beyond name/size/date, thumbnails and preview panes, duplicate finding,
multi-file rename, undo, plugins, and the network server for remote search.

**Full-text keyword search** is the one worth an early answer, and it is cheap: shell out
to `ripgrep` scoped to the filename results. Do not build an inverted index.
