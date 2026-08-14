// Command scry is a fuzzy filename-search CLI, phases 1-5 of
// "everything-macos-design.md": each invocation loads every configured
// root from its on-disk snapshot when one is valid, falling back to a
// fresh crawl (and saving a new snapshot) only for a root whose snapshot
// is missing, stale, or corrupt, then serves one query against the
// resulting in-memory shards. The resident daemon, FSEvents watcher, and
// unix-socket protocol described later in the design are not implemented
// yet — see the repository README for status.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"scry/internal/config"
	"scry/internal/crawler"
	"scry/internal/index"
	"scry/internal/ipc"
	"scry/internal/qsyntax"
	"scry/internal/query"
	"scry/internal/roots"
	"scry/internal/snapshot"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "scry:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: scry <query> | scry root add/rm/list | scry index | scry status | scry daemon | scry menubar | scry stop")
	}

	switch args[0] {
	case "root":
		return runRoot(args[1:])
	case "index":
		return runIndex(args[1:])
	case "status":
		return runStatus(args[1:])
	case "daemon":
		return runDaemon(args[1:])
	case "menubar":
		return runMenubar(args[1:])
	case "stop":
		return runStop(args[1:])
	default:
		return runSearch(args)
	}
}

// ipcAddr resolves the daemon's socket address from the default cache
// directory. Every command that talks to the daemon goes through this, so
// there is exactly one place that decides where the socket lives.
func ipcAddr() (ipc.Addr, error) {
	dir, err := config.CacheDir()
	if err != nil {
		return ipc.Addr{}, err
	}
	return ipc.Addr{CacheDir: dir}, nil
}

// loadConfig loads scry's config file from its default location.
func loadConfig() (config.Config, string, error) {
	path, err := config.ConfigPath()
	if err != nil {
		return config.Config{}, "", err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, "", err
	}
	return cfg, path, nil
}

// crawlOptions builds crawler.Options for root r, folding the per-root
// excludes on top of the global ones.
func crawlOptions(cfg config.Config, r config.Root) crawler.Options {
	return crawler.Options{
		Excludes:       append(append([]string(nil), cfg.Exclude.Names...), r.Exclude...),
		Globs:          cfg.Exclude.Globs,
		FollowSymlinks: cfg.Index.FollowSymlinks,
		Hidden:         cfg.Index.Hidden,
	}
}

// crawlAll crawls every configured root in parallel and returns one shard
// per root (in cfg.Roots order) plus each root's crawl Stats. It never
// touches snapshots; callers decide whether and when to save.
func crawlAll(cfg config.Config) ([]*index.Shard, []crawler.Stats) {
	shards := make([]*index.Shard, len(cfg.Roots))
	stats := make([]crawler.Stats, len(cfg.Roots))

	var wg sync.WaitGroup
	for i, r := range cfg.Roots {
		wg.Add(1)
		go func(i int, r config.Root) {
			defer wg.Done()
			shard, st, err := crawler.Crawl(r.Path, crawlOptions(cfg, r))
			if err != nil {
				st.Errors++
				if shard == nil {
					shard = index.New(r.Path)
				}
			}
			shards[i] = shard
			stats[i] = st
		}(i, r)
	}
	wg.Wait()

	return shards, stats
}

// saveAll saves every shard in shards to its snapshot file, in parallel.
// A save failure for one root is reported to stderr but never aborts the
// others or the caller's overall command.
func saveAll(shards []*index.Shard) {
	var wg sync.WaitGroup
	for _, s := range shards {
		if s == nil {
			continue
		}
		wg.Add(1)
		go func(s *index.Shard) {
			defer wg.Done()
			if err := snapshot.Save(s); err != nil {
				fmt.Fprintf(os.Stderr, "scry: warning: could not save snapshot for %s: %v\n", s.Root(), err)
			}
		}(s)
	}
	wg.Wait()
}

// loadOrCrawlAll loads each configured root from its snapshot; a root whose
// snapshot is missing, corrupt, or stale (index.ErrStale) is crawled fresh
// instead and its new snapshot saved, per §7/§8: a bad shard costs one
// root, never the whole app. Returns one shard per root (in cfg.Roots
// order), each root's crawl Stats (zero for a root that loaded from
// snapshot), and whether each root was loaded rather than crawled.
func loadOrCrawlAll(cfg config.Config) (shards []*index.Shard, stats []crawler.Stats, loaded []bool) {
	shards = make([]*index.Shard, len(cfg.Roots))
	stats = make([]crawler.Stats, len(cfg.Roots))
	loaded = make([]bool, len(cfg.Roots))

	var wg sync.WaitGroup
	var toSave []*index.Shard
	var mu sync.Mutex

	for i, r := range cfg.Roots {
		wg.Add(1)
		go func(i int, r config.Root) {
			defer wg.Done()

			if s, err := snapshot.Load(r.Path); err == nil {
				shards[i] = s
				loaded[i] = true
				return
			}

			shard, st, err := crawler.Crawl(r.Path, crawlOptions(cfg, r))
			if err != nil {
				st.Errors++
				if shard == nil {
					shard = index.New(r.Path)
				}
			}
			shards[i] = shard
			stats[i] = st

			mu.Lock()
			toSave = append(toSave, shard)
			mu.Unlock()
		}(i, r)
	}
	wg.Wait()

	saveAll(toSave)
	return shards, stats, loaded
}

// runSearch implements `scry <query>`: try the resident daemon over the
// socket first (§7 — every UI path should go through it), and only fall
// back to the in-process crawl-or-load path when no daemon answers.
func runSearch(args []string) error {
	q := args[0]

	if results, ok, err := searchViaDaemon(q); ok {
		if err != nil {
			return err
		}
		for _, r := range results {
			fmt.Printf("%d\t%s\n", r.Score, r.Path)
		}
		fmt.Fprintf(os.Stderr, "via daemon, %d results\n", len(results))
		return nil
	}
	fmt.Fprintln(os.Stderr, "scry: no daemon running; falling back to in-process search")

	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Roots) == 0 {
		return fmt.Errorf("no roots configured; run `scry root add <path>` first")
	}

	loadStart := time.Now()
	shards, _, loaded := loadOrCrawlAll(cfg)
	loadDur := time.Since(loadStart)

	parsed, err := qsyntax.Parse(q)
	if err != nil {
		return err
	}

	queryStart := time.Now()
	results := query.Search(shards, parsed, 0)
	queryDur := time.Since(queryStart)

	for _, r := range results {
		fmt.Printf("%d\t%s\n", r.Score, r.Path)
	}
	fmt.Fprintf(os.Stderr, "%s, query %s, %d results\n", loadLabel(loaded, loadDur), queryDur, len(results))
	return nil
}

// searchViaDaemon runs q against a running daemon over the socket, if one
// is listening. ok reports whether a daemon was found at all — the caller
// should fall back to the in-process path only when ok is false; if ok is
// true and err is non-nil, that is a real error from an already-contacted
// daemon, not a "no daemon" condition, and should be reported as-is.
func searchViaDaemon(q string) (results []query.Result, ok bool, err error) {
	addr, err := ipcAddr()
	if err != nil {
		return nil, false, nil
	}
	c, err := ipc.Dial(addr)
	if err != nil {
		return nil, false, nil
	}
	defer c.Close()

	resp, err := c.Call(ipc.Request{Op: "search", Query: q})
	if err != nil {
		return nil, true, fmt.Errorf("daemon: %w", err)
	}
	if resp.Err != "" {
		return nil, true, fmt.Errorf("daemon: %s", resp.Err)
	}
	return resp.Results, true, nil
}

// loadLabel describes, honestly, how the shards backing a search were
// obtained: loaded from an on-disk snapshot (the fast path phase 5 exists
// for), freshly crawled (a root with no valid snapshot yet), or a mix of
// both. This exists so a mislabeled timing line can never again claim
// "crawl" for what was actually a snapshot load, or vice versa.
func loadLabel(loaded []bool, dur time.Duration) string {
	total := len(loaded)
	loadedCount := 0
	for _, l := range loaded {
		if l {
			loadedCount++
		}
	}
	switch loadedCount {
	case total:
		return fmt.Sprintf("loaded from snapshot in %s", dur)
	case 0:
		return fmt.Sprintf("crawled in %s", dur)
	default:
		return fmt.Sprintf("loaded %d/%d roots from snapshot, crawled the rest, in %s", loadedCount, total, dur)
	}
}

// runIndex implements `scry index`: force a fresh crawl of every configured
// root regardless of any existing snapshot, report per-root stats, and
// re-save each root's snapshot.
func runIndex(args []string) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Roots) == 0 {
		return fmt.Errorf("no roots configured; run `scry root add <path>` first")
	}

	shards, stats := crawlAll(cfg)
	saveAll(shards)

	var totalEntries, totalErrors int
	for i, r := range cfg.Roots {
		st := stats[i]
		fmt.Printf("%s: %d entries, %d dirs, %d skipped, %d errors, %s\n",
			r.Path, st.Entries, st.Dirs, st.Skipped, st.Errors, st.Duration)
		totalEntries += st.Entries
		totalErrors += st.Errors
	}
	fmt.Fprintf(os.Stderr, "%d roots, %d entries total, %d errors\n", len(cfg.Roots), totalEntries, totalErrors)
	return nil
}

// runRoot implements `scry root add/rm/list`.
func runRoot(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: scry root add/rm/list [path]")
	}

	switch args[0] {
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: scry root add <path>")
		}
		return runRootAdd(args[1])
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: scry root rm <path>")
		}
		return runRootRemove(args[1])
	case "list":
		return runRootList()
	default:
		return fmt.Errorf("unknown root subcommand %q", args[0])
	}
}

func runRootAdd(path string) error {
	cfg, cfgPath, err := loadConfig()
	if err != nil {
		return err
	}

	expanded, err := config.ExpandTilde(path)
	if err != nil {
		return err
	}

	existing := make([]string, len(cfg.Roots))
	for i, r := range cfg.Roots {
		existing[i] = r.Path
	}

	kept, warnings := roots.Normalize(append(existing, expanded))
	for _, w := range warnings {
		if w.Path == filepath.Clean(expanded) || w.Path == expanded {
			fmt.Printf("absorbed: %s (%s)\n", w.Path, w.Reason)
		}
	}

	byPath := make(map[string]config.Root, len(cfg.Roots))
	for _, r := range cfg.Roots {
		byPath[r.Path] = r
	}

	newRoots := make([]config.Root, 0, len(kept))
	for _, p := range kept {
		if r, ok := byPath[p]; ok {
			newRoots = append(newRoots, r)
		} else {
			newRoots = append(newRoots, config.Root{Path: p})
		}
	}
	cfg.Roots = newRoots

	if err := config.Save(cfgPath, cfg); err != nil {
		return err
	}
	fmt.Printf("root list now: %d root(s)\n", len(cfg.Roots))
	return nil
}

func runRootRemove(path string) error {
	cfg, cfgPath, err := loadConfig()
	if err != nil {
		return err
	}

	// Resolve the root's canonical path before it's dropped from cfg, so the
	// snapshot file (hashed on the normalized path) can be found.
	expanded, err := config.ExpandTilde(path)
	if err != nil {
		return err
	}
	var snapPath string
	for _, r := range cfg.Roots {
		if r.Path == path || r.Path == expanded {
			snapPath = r.Path
			break
		}
	}

	removed, err := cfg.RemoveRoot(path)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("root %q is not configured", path)
	}

	if err := config.Save(cfgPath, cfg); err != nil {
		return err
	}

	if snapPath != "" {
		if err := snapshot.Remove(snapPath); err != nil {
			fmt.Fprintf(os.Stderr, "scry: warning: could not remove snapshot for %s: %v\n", snapPath, err)
		}
	}

	fmt.Printf("removed %s\n", path)
	return nil
}

func runRootList() error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Roots) == 0 {
		fmt.Println("no roots configured")
		return nil
	}

	shards, _, _ := loadOrCrawlAll(cfg)

	type row struct {
		path    string
		entries int
		online  bool
	}
	rowsList := make([]row, len(cfg.Roots))
	for i, r := range cfg.Roots {
		entries := 0
		if shards[i] != nil {
			entries = shards[i].CountIndexed()
		}
		rowsList[i] = row{
			path:    r.Path,
			entries: entries,
			online:  shards[i] != nil && shards[i].Online(),
		}
	}
	sort.Slice(rowsList, func(i, j int) bool { return rowsList[i].path < rowsList[j].path })

	for _, rw := range rowsList {
		status := "online"
		if !rw.online {
			status = "offline"
		}
		fmt.Printf("%s\t%d entries\t%s\n", rw.path, rw.entries, status)
	}
	return nil
}

// runStatus implements `scry status`: per configured root, the live entry
// count, and the on-disk snapshot's size and modification time (if a
// snapshot exists yet). Tries the daemon over the socket first, per §7;
// falls back to loading (or crawling) every root in-process only when no
// daemon answers.
func runStatus(args []string) error {
	if addr, err := ipcAddr(); err == nil {
		if c, dialErr := ipc.Dial(addr); dialErr == nil {
			defer c.Close()
			resp, callErr := c.Call(ipc.Request{Op: "status"})
			if callErr != nil {
				return fmt.Errorf("daemon: %w", callErr)
			}
			if resp.Err != "" {
				return fmt.Errorf("daemon: %s", resp.Err)
			}
			if len(resp.Stats.Roots) == 0 {
				fmt.Println("no roots configured")
				return nil
			}
			printStatusRows(resp.Stats.Roots)
			return nil
		}
	}

	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Roots) == 0 {
		fmt.Println("no roots configured")
		return nil
	}

	shards, _, _ := loadOrCrawlAll(cfg)
	printStatusRows(statusRows(cfg, shards))
	return nil
}

// statusRows builds one ipc.RootStatus per configured root from shards
// already resident (loaded or crawled) plus each root's on-disk snapshot
// metadata. Shared between the in-process `scry status` path and the
// daemon's "status" op handler so the two never drift apart.
func statusRows(cfg config.Config, shards []*index.Shard) []ipc.RootStatus {
	rows := make([]ipc.RootStatus, len(cfg.Roots))
	for i, r := range cfg.Roots {
		rw := ipc.RootStatus{Path: r.Path}
		if shards[i] != nil {
			rw.Entries = shards[i].CountIndexed()
			rw.Online = shards[i].Online()
		}

		if path, err := snapshot.PathFor(r.Path); err == nil {
			if fi, err := os.Stat(path); err == nil {
				rw.SnapshotExists = true
				rw.SnapshotSize = fi.Size()
				rw.SnapshotMTime = fi.ModTime()
			}
		}
		rows[i] = rw
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	return rows
}

// printStatusRows prints statusRows' output in `scry status`'s stable
// text format, whether it came from the daemon or the in-process fallback.
func printStatusRows(rows []ipc.RootStatus) {
	for _, rw := range rows {
		status := "online"
		if !rw.Online {
			status = "offline"
		}
		if !rw.SnapshotExists {
			fmt.Printf("%s\t%d entries\t%s\tno snapshot\n", rw.Path, rw.Entries, status)
			continue
		}
		fmt.Printf("%s\t%d entries\t%s\tsnapshot %d bytes, %s\n",
			rw.Path, rw.Entries, status, rw.SnapshotSize, rw.SnapshotMTime.Format(time.RFC3339))
	}
}
