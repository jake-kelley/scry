// Command scry is a fuzzy filename-search CLI, phase 1-3 of
// "everything-macos-design.md": every invocation crawls its configured
// roots fresh, then serves one query against the resulting in-memory
// shards. The resident daemon, FSEvents watcher, and on-disk snapshots
// described later in the design are not implemented yet — see the
// repository README for status.
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
	"scry/internal/query"
	"scry/internal/roots"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "scry:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: scry <query> | scry root add/rm/list | scry index")
	}

	switch args[0] {
	case "root":
		return runRoot(args[1:])
	case "index":
		return runIndex(args[1:])
	default:
		return runSearch(args)
	}
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
// per root (in cfg.Roots order) plus each root's crawl Stats.
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

// runSearch implements `scry <query>`.
func runSearch(args []string) error {
	q := args[0]

	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Roots) == 0 {
		return fmt.Errorf("no roots configured; run `scry root add <path>` first")
	}

	crawlStart := time.Now()
	shards, _ := crawlAll(cfg)
	crawlDur := time.Since(crawlStart)

	queryStart := time.Now()
	results := query.Search(shards, q, 0)
	queryDur := time.Since(queryStart)

	for _, r := range results {
		fmt.Printf("%d\t%s\n", r.Score, r.Path)
	}
	fmt.Fprintf(os.Stderr, "crawl %s, query %s, %d results\n", crawlDur, queryDur, len(results))
	return nil
}

// runIndex implements `scry index`: crawl every configured root and report
// per-root stats.
func runIndex(args []string) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Roots) == 0 {
		return fmt.Errorf("no roots configured; run `scry root add <path>` first")
	}

	shards, stats := crawlAll(cfg)

	var totalEntries, totalErrors int
	for i, r := range cfg.Roots {
		st := stats[i]
		fmt.Printf("%s: %d entries, %d dirs, %d skipped, %d errors, %s\n",
			r.Path, st.Entries, st.Dirs, st.Skipped, st.Errors, st.Duration)
		totalEntries += st.Entries
		totalErrors += st.Errors
	}
	_ = shards
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

	shards, stats := crawlAll(cfg)

	type row struct {
		path    string
		entries int
		online  bool
	}
	rowsList := make([]row, len(cfg.Roots))
	for i, r := range cfg.Roots {
		rowsList[i] = row{
			path:    r.Path,
			entries: stats[i].Entries,
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
