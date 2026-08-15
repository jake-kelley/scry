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
	"scry/internal/fsevents"
	"scry/internal/index"
	"scry/internal/ipc"
	"scry/internal/power"
	"scry/internal/qsyntax"
	"scry/internal/query"
	"scry/internal/reconcile"
	"scry/internal/snapshot"
	"scry/internal/watcher"
	"scry/internal/web"
)

// fsEventsLatency is the coalescing window passed to FSEventStreamCreate,
// per "everything-macos-design.md" §6's example.
const fsEventsLatency = 300 * time.Millisecond

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

// shardFor returns the *index.Shard currently resident for root, or nil if
// root is not (or no longer) configured. This is the watcher.Config.GetShard
// implementation: the watcher's own goroutine reads through here, and
// d.replace (== watcher.Config.SetShard) is how it swaps one in, exactly
// the same shape the recrawl Scheduler already uses.
func (d *daemonState) shardFor(root string) *index.Shard {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for i, r := range d.cfg.Roots {
		if r.Path == root {
			return d.shards[i]
		}
	}
	return nil
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

// startCore does everything both `scry daemon` and `scry menubar` need
// before they diverge: load every configured root from its snapshot
// (crawling only what's missing or stale), build the daemonState, and
// start the recrawl Scheduler and FSEvents watcher against ctx. It does
// not start the IPC socket or the web UI — callers do that themselves,
// because runDaemon blocks on ipc.Serve directly while runMenubar (see
// menubar.go) has to start it as a goroutine so systray.Run can own the
// main thread instead, per §7's "single biggest structural consequence."
func startCore(ctx context.Context, cfg config.Config, logPrefix string) (*daemonState, error) {
	loadStart := time.Now()
	shards, _, loaded := loadOrCrawlAll(cfg)
	fmt.Fprintf(os.Stderr, "%s: %s (%d roots)\n", logPrefix, loadLabel(loaded, time.Since(loadStart)), len(cfg.Roots))

	d := &daemonState{cfg: cfg, shards: shards}

	specs := make([]reconcile.RootSpec, len(cfg.Roots))
	for i, r := range cfg.Roots {
		specs[i] = reconcile.RootSpec{Path: r.Path, Opts: crawlOptions(cfg, r)}
	}
	// A zero here means "use reconcile's own default", which is exactly
	// what an unset or rejected recrawl_interval yields. recrawl_interval
	// = "off" is different: it asks for no timer at all, and the
	// Scheduler is still built so an explicit rebuild can drive it.
	interval, periodic := cfg.RecrawlInterval()
	if !periodic {
		interval = reconcile.NoInterval
	}
	sched := reconcile.NewScheduler(specs, interval, func(res reconcile.Result) {
		if res.Err != nil {
			fmt.Fprintf(os.Stderr, "%s: reconcile %s: %v\n", logPrefix, res.Root, res.Err)
			return
		}
		if res.Diff.Empty() {
			// Nothing changed: skip the snapshot write and the live
			// shard swap. See internal/reconcile's package doc.
			return
		}
		if err := snapshot.Save(res.Shard); err != nil {
			fmt.Fprintf(os.Stderr, "%s: warning: could not save snapshot for %s: %v\n", logPrefix, res.Root, err)
		}
		d.replace(res.Root, res.Shard)
	})
	d.wakeImpl = sched.WakeFromSleep
	go sched.Run(ctx)

	ws := &watcherSupervisor{}
	if err := ws.start(ctx, d, cfg); err != nil {
		// A watcher that fails to start is not fatal: the recrawl
		// Scheduler above still bounds drift to a day, per §8 item 3,
		// and every query path still works against whatever was loaded
		// at startup. With recrawl_interval = "off" there is no such
		// bound left, which is worth saying out loud in the log — that
		// combination means the index only changes on an explicit
		// rebuild.
		fmt.Fprintf(os.Stderr, "%s: warning: watcher not started: %v\n", logPrefix, err)
		if !periodic {
			fmt.Fprintf(os.Stderr, "%s: warning: recrawl_interval is off and the watcher is down; "+
				"the index will not update until you rebuild it\n", logPrefix)
		}
	}

	startPowerNotifier(ctx, ws, sched, d, cfg, logPrefix)

	return d, nil
}

// watcherSupervisor owns the currently-running watcher's lifetime so it
// can be restarted — stop the old one, start a fresh one — without
// tearing down anything else startCore built. This is what lets the wake-
// from-sleep resync path (see startPowerNotifier) reuse startWatcher
// exactly, per §8 item 3's "reuse that path rather than inventing a second
// one": restarting rebuilds the FSEvents stream from each shard's current
// saved lastEID, which is the cheap repair, not a crawl.
type watcherSupervisor struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

// start stops whatever watcher this supervisor was previously running (if
// any) and starts a new one, as a child of parentCtx so it is also torn
// down when the daemon itself shuts down. An error leaves no watcher
// running; the caller decides how to log that.
func (ws *watcherSupervisor) start(parentCtx context.Context, d *daemonState, cfg config.Config) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.cancel != nil {
		ws.cancel()
		ws.cancel = nil
	}

	wctx, cancel := context.WithCancel(parentCtx)
	if err := startWatcher(wctx, d, cfg); err != nil {
		cancel()
		return err
	}
	ws.cancel = cancel
	return nil
}

// startPowerNotifier wires internal/power's wake-from-sleep detection to
// the resync-first, escalate-only-if-necessary behaviour §8 item 3
// requires: on an actual system wake, restart the FSEvents stream from
// each shard's saved position (ws.start, the same path startCore used at
// launch) rather than crawling, and only fall back to sched's full
// reconcile pass — never automatically if recrawl_interval is off — when
// that restart itself could not work.
//
// A notifier that fails to start (every non-darwin platform, and any
// darwin failure to register with IOKit) is a warning, not fatal, the
// same treatment startWatcher gets above: every other code path, wake
// detection included, degrades gracefully without it.
func startPowerNotifier(ctx context.Context, ws *watcherSupervisor, sched *reconcile.Scheduler, d *daemonState, cfg config.Config, logPrefix string) {
	notifier, err := power.NewNotifier()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: warning: wake-from-sleep detection not started: %v\n", logPrefix, err)
		return
	}

	coord := &power.Coordinator{
		Resync:   func() error { return ws.start(ctx, d, cfg) },
		Fallback: sched.WakeFromSleep,
		RecrawlEnabled: func() bool {
			_, on := cfg.RecrawlInterval()
			return on
		},
		Logf: func(format string, args ...interface{}) {
			fmt.Fprintf(os.Stderr, "%s: "+format+"\n", append([]interface{}{logPrefix}, args...)...)
		},
	}
	// Say so on success, not only on failure. Everything this subsystem
	// does afterwards is triggered by an event that may be hours away, so
	// without this line the log gives a reader no way to tell "started and
	// waiting" apart from "never started" — which is the first question
	// packaging/MANUAL-VERIFY.md's sleep/wake check has to answer.
	fmt.Fprintf(os.Stderr, "%s: wake-from-sleep detection active\n", logPrefix)

	go coord.Run(ctx, notifier)
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := startCore(ctx, cfg, "scry: daemon")
	if err != nil {
		return err
	}

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

// startWatcher builds and starts the FSEvents watcher described in §6: one
// combined stream over every configured root's path, resuming from the
// oldest shard's lastEID so no shard misses events, at the cost of
// replaying some events shards further ahead already applied — safe only
// because internal/watcher's apply logic is idempotent by construction.
// The watcher runs until ctx is cancelled, alongside the recrawl
// Scheduler already started by the caller.
func startWatcher(ctx context.Context, d *daemonState, cfg config.Config) error {
	shards := d.snapshotShards()

	roots := make([]watcher.Root, len(cfg.Roots))
	paths := make([]string, len(cfg.Roots))
	for i, r := range cfg.Roots {
		roots[i] = watcher.Root{
			Path:          r.Path,
			Opts:          crawlOptions(cfg, r),
			OfflinePolicy: r.OfflinePolicy,
		}
		paths[i] = r.Path
	}

	sinceID := minLastEID(shards)
	if fresh := unwatchedRoots(shards); len(fresh) > 0 && len(fresh) < len(shards) {
		fmt.Fprintf(os.Stderr, "scry: daemon: watcher: no saved event position for %s; "+
			"current only as of the last crawl, and history replay cannot fill that gap\n",
			strings.Join(fresh, ", "))
	}

	source, err := fsevents.NewStream(fsevents.Config{
		Paths:        paths,
		Latency:      fsEventsLatency,
		SinceEventID: sinceID,
		FileEvents:   true,
		WatchRoot:    true,
		NoDefer:      true,
	})
	if err != nil {
		return err
	}

	w := watcher.New(watcher.Config{
		Roots:    roots,
		Source:   source,
		GetShard: d.shardFor,
		SetShard: d.replace,
		Persist: func(s *index.Shard) {
			if err := snapshot.Save(s); err != nil {
				fmt.Fprintf(os.Stderr, "scry: daemon: warning: could not save snapshot for %s: %v\n", s.Root(), err)
			}
		},
		Logf: func(format string, args ...interface{}) {
			fmt.Fprintf(os.Stderr, "scry: "+format+"\n", args...)
		},
	})

	label := "since now"
	if sinceID != 0 {
		label = fmt.Sprintf("at event id %d", sinceID)
	}
	fmt.Fprintf(os.Stderr, "scry: daemon: watcher: starting %s (%s)\n", eventsLabel(fsevents.Supported), label)

	go w.Run(ctx)
	return nil
}

// eventsLabel names what kind of event source the watcher is actually
// running against, so a Windows or Linux daemon log never implies live
// FSEvents coverage it does not have.
func eventsLabel(supported bool) string {
	if supported {
		return "FSEvents stream"
	}
	return "no-op event source (FSEvents unsupported on this platform)"
}

// minLastEID returns the smallest lastEID across shards, per §6: "resuming
// the combined stream from the oldest shard's ID replays events other
// shards have already applied" — safe because updates are idempotent.
//
// Shards reporting 0 are skipped rather than counted as the minimum. A 0
// means "never watched", which is not a position on the event stream at
// all, and letting it win the comparison used to pin the whole combined
// stream to "now": the darwin implementation resolves 0 to LatestEventID,
// so adding one fresh root silently threw away every other root's resume
// point, and anything that landed in a long-watched root while the daemon
// was down was never replayed. Skipping zeros costs the fresh root
// nothing — it was already getting no replay, since it has no position to
// replay from — and preserves the real resume point for the roots that
// have one. When every shard is 0 the result is still 0, which is correct:
// nothing has a position, so the stream starts from now.
//
// A root left at 0 is only as current as its last crawl. That was formerly
// bounded by the recrawl scheduler; with recrawl_interval = "off" it is
// bounded by nothing, so startWatcher names those roots in the log rather
// than leaving the gap silent.
func minLastEID(shards []*index.Shard) uint64 {
	var min uint64
	first := true
	for _, s := range shards {
		if s == nil {
			continue
		}
		eid := s.LastEID()
		if eid == 0 {
			continue
		}
		if first || eid < min {
			min = eid
			first = false
		}
	}
	return min
}

// unwatchedRoots names the shards with no event-stream position, for the
// startup log. See minLastEID for why they are worth naming.
func unwatchedRoots(shards []*index.Shard) []string {
	var out []string
	for _, s := range shards {
		if s != nil && s.LastEID() == 0 {
			out = append(out, s.Root())
		}
	}
	return out
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
