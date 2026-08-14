package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"scry/internal/config"
	"scry/internal/index"
	"scry/internal/ipc"
	"scry/internal/qsyntax"
	"scry/internal/query"
	"scry/internal/reconcile"
	"scry/internal/snapshot"
	"scry/internal/web"
)

// daemonState holds a resident daemon's live shards, guarded by a lock
// because the recrawl Scheduler (see internal/reconcile) replaces a root's
// *index.Shard wholesale from its own goroutine while queries are reading
// the slice concurrently from ipc connection goroutines. The Shard values
// themselves are separately safe for concurrent use (index.Shard has its
// own internal lock); this lock only protects which *index.Shard object
// each root currently points at.
type daemonState struct {
	mu     sync.RWMutex
	cfg    config.Config
	shards []*index.Shard

	// wakeImpl is the recrawl Scheduler's WakeFromSleep, set once runDaemon
	// has created one. nil (wake becomes a no-op) in tests that construct a
	// daemonState directly without a Scheduler.
	wakeImpl func()
}

// wake triggers an immediate out-of-band recrawl pass, if a Scheduler is
// attached.
func (d *daemonState) wake() {
	if d.wakeImpl != nil {
		d.wakeImpl()
	}
}

// shards returns a snapshot of the current shard list, safe to pass to
// query.Search without holding daemonState's lock for the duration of a
// search.
func (d *daemonState) snapshotShards() []*index.Shard {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*index.Shard, len(d.shards))
	copy(out, d.shards)
	return out
}

// replace swaps in a freshly reconciled shard for root, if root is still
// configured. A root removed from cfg between a Scheduler pass starting and
// finishing is simply dropped here — there is nothing left to update.
func (d *daemonState) replace(root string, shard *index.Shard) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, r := range d.cfg.Roots {
		if r.Path == root {
			d.shards[i] = shard
			return
		}
	}
}

// status builds the current status rows, reusing the same shape `scry
// status` prints in-process (see statusRows in main.go).
func (d *daemonState) status() []ipc.RootStatus {
	d.mu.RLock()
	cfg := d.cfg
	shards := make([]*index.Shard, len(d.shards))
	copy(shards, d.shards)
	d.mu.RUnlock()
	return statusRows(cfg, shards)
}

// search runs q (a raw query string) against the current shards. It is the
// shape both the ipc handler and the --serve web UI want, so it also backs
// web.SearchFunc.
func (d *daemonState) search(q string, limit int) ([]query.Result, error) {
	parsed, err := qsyntax.Parse(q)
	if err != nil {
		return nil, err
	}
	return query.Search(d.snapshotShards(), parsed, limit), nil
}

// handle implements ipc.Handler against d. cancel is called to shut the
// daemon down cleanly in response to a "stop" request — it is safe to call
// from here because it only signals ipc.Serve's context; the response for
// this very request still gets written back over its own already-open
// connection first.
func handleRequest(d *daemonState, cancel context.CancelFunc) ipc.Handler {
	return func(req ipc.Request) ipc.Response {
		switch req.Op {
		case "search":
			results, err := d.search(req.Query, req.Limit)
			if err != nil {
				return ipc.Response{Err: err.Error()}
			}
			return ipc.Response{Results: results}

		case "status":
			return ipc.Response{Stats: ipc.Stats{Roots: d.status()}}

		case "reindex":
			// Trigger an immediate out-of-band reconcile pass rather than
			// waiting for it inline: reconciling every root can take a
			// while, and a socket request shouldn't block on it. The
			// caller can poll "status" afterward.
			d.wake()
			return ipc.Response{Stats: ipc.Stats{Roots: d.status()}}

		case "stop":
			cancel()
			return ipc.Response{}

		default:
			return ipc.Response{Err: fmt.Sprintf("unknown op %q", req.Op)}
		}
	}
}

// runDaemon implements `scry daemon [--serve[=host:port]]`: the resident
// process described in §3 and §7 — load every configured root from its
// snapshot (crawling only what's missing or stale), hold the shards in
// RAM, serve the socket, and run the recrawl scheduler, until stopped.
func runDaemon(args []string) error {
	serveAddr, serve, err := parseServeFlag(args)
	if err != nil {
		return err
	}

	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Roots) == 0 {
		return fmt.Errorf("no roots configured; run `scry root add <path>` first")
	}

	loadStart := time.Now()
	shards, _, loaded := loadOrCrawlAll(cfg)
	fmt.Fprintf(os.Stderr, "scry: daemon: %s (%d roots)\n", loadLabel(loaded, time.Since(loadStart)), len(cfg.Roots))

	d := &daemonState{cfg: cfg, shards: shards}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	specs := make([]reconcile.RootSpec, len(cfg.Roots))
	for i, r := range cfg.Roots {
		specs[i] = reconcile.RootSpec{Path: r.Path, Opts: crawlOptions(cfg, r)}
	}
	sched := reconcile.NewScheduler(specs, reconcile.DefaultInterval, func(res reconcile.Result) {
		if res.Err != nil {
			fmt.Fprintf(os.Stderr, "scry: daemon: reconcile %s: %v\n", res.Root, res.Err)
			return
		}
		if err := snapshot.Save(res.Shard); err != nil {
			fmt.Fprintf(os.Stderr, "scry: daemon: warning: could not save snapshot for %s: %v\n", res.Root, err)
		}
		d.replace(res.Root, res.Shard)
	})
	d.wakeImpl = sched.WakeFromSleep
	go sched.Run(ctx)

	// A plain Ctrl-C should shut the daemon down the same clean way
	// `scry stop` does, not leave a half-closed socket behind.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		cancel()
	}()

	if serve {
		go func() {
			fmt.Fprintf(os.Stderr, "scry: web UI at http://%s/\n", serveAddr)
			if err := web.Serve(serveAddr, d.search); err != nil {
				fmt.Fprintf(os.Stderr, "scry: web UI: %v\n", err)
			}
		}()
	}

	addr, err := ipcAddr()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "scry: daemon listening at %s\n", addr.CacheDir)

	err = ipc.Serve(ctx, addr, handleRequest(d, cancel))
	if err != nil && errors.Is(err, ipc.ErrAlreadyRunning) {
		return fmt.Errorf("scry: %w — is a daemon already running?", err)
	}
	return err
}

// parseServeFlag scans daemon's arguments for --serve or --serve=host:port.
// A bare "--serve" (or "--serve=PORT" with no host) binds to 127.0.0.1;
// web.Serve independently refuses anything else regardless of what's
// passed here, so this is a convenience for the common "just give me a
// port" case, not the enforcement point.
func parseServeFlag(args []string) (addr string, serve bool, err error) {
	for _, a := range args {
		switch {
		case a == "--serve":
			return web.DefaultAddr, true, nil
		case strings.HasPrefix(a, "--serve="):
			v := strings.TrimPrefix(a, "--serve=")
			if v == "" {
				return web.DefaultAddr, true, nil
			}
			if !strings.Contains(v, ":") {
				v = "127.0.0.1:" + v
			}
			return v, true, nil
		default:
			return "", false, fmt.Errorf("unknown daemon argument %q", a)
		}
	}
	return "", false, nil
}

// runStop implements `scry stop`: ask a running daemon to exit cleanly.
func runStop(args []string) error {
	addr, err := ipcAddr()
	if err != nil {
		return err
	}
	c, err := ipc.Dial(addr)
	if err != nil {
		return fmt.Errorf("no daemon running: %w", err)
	}
	defer c.Close()

	resp, err := c.Call(ipc.Request{Op: "stop"})
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	if resp.Err != "" {
		return fmt.Errorf("daemon: %s", resp.Err)
	}
	fmt.Println("daemon stopped")
	return nil
}
