// Package ipc implements the socket protocol between the resident scry
// daemon and its clients (the CLI, the web UI, eventually the menu bar
// item), per "everything-macos-design.md" §3 and §7 ("One process or two":
// every UI path goes through this socket, never the index directly).
//
// The wire format is newline-delimited JSON: one Request per line in,
// one Response per line out, on a long-lived connection a client may reuse
// for multiple round trips.
//
// # Transport
//
// The design targets a unix socket at <CacheDir>/sock — the only transport
// darwin needs. Go's net package supports AF_UNIX on Windows 10+ too, and
// that is confirmed working in this repo's dev environment (see the ipc_test
// unix-specific tests), so Serve and Dial always try the unix socket first
// regardless of OS. If binding it fails for a reason other than "a daemon is
// already listening here" (e.g. a machine where AF_UNIX genuinely isn't
// available), Serve falls back to a loopback TCP listener and records the
// port it bound in <CacheDir>/port; Dial mirrors that by trying the unix
// socket first and falling back to reading the port file.
package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"scry/internal/query"
)

// sockFile and portFile are the well-known filenames Serve and Dial agree
// on within Addr.CacheDir.
const (
	sockFile = "sock"
	portFile = "port"
)

// dialProbeTimeout bounds how long Dial (and Serve's own stale-socket
// check) waits to find out whether something is actually listening.
const dialProbeTimeout = 500 * time.Millisecond

// pingOp is a reserved op that serveConn answers itself, without ever
// reaching the Handler. bind uses it to confirm that whatever answered on
// a recorded TCP port is really a scry daemon and not an unrelated process
// that happened to be assigned a recycled ephemeral port.
const pingOp = "__ping"

// Request is one call a client sends the daemon.
type Request struct {
	Op    string // "search", "status", "reindex", "stop"
	Query string // the query string for "search"; a root path (or "" for all) for "reindex"
	Limit int    // result cap for "search"; <= 0 uses query.Search's default
}

// RootStatus is one root's entry in a "status" Response, mirroring what
// `scry status` prints per root.
type RootStatus struct {
	Path           string
	Entries        int
	Online         bool
	SnapshotExists bool
	SnapshotSize   int64
	SnapshotMTime  time.Time
}

// Stats is the non-search payload of a Response: populated for "status"
// (and, incidentally, "reindex", which returns the post-reindex status)
// and left zero for "search" and "stop".
type Stats struct {
	Roots []RootStatus
}

// Response is what the daemon sends back for one Request. Err is non-empty
// exactly when the request failed (including a recovered handler panic);
// Results and Stats are the zero value in that case.
type Response struct {
	Results []query.Result
	Stats   Stats
	Err     string
}

// Handler answers one Request. Serve recovers a panic from Handler and
// turns it into an Err response rather than letting it take down the
// daemon or even just the one connection it arrived on.
type Handler func(Request) Response

// Addr names where the daemon's socket lives: a directory (normally
// config.CacheDir()) holding a "sock" file (the primary transport) and,
// only if the unix socket could not be bound, a "port" file recording the
// TCP fallback's port.
type Addr struct {
	CacheDir string
}

// sockPath and portPath are Addr's two well-known file paths.
func (a Addr) sockPath() string { return filepath.Join(a.CacheDir, sockFile) }
func (a Addr) portPath() string { return filepath.Join(a.CacheDir, portFile) }

// ErrAlreadyRunning is returned by Serve when another process is already
// listening at addr's socket — the caller should report that and exit
// rather than falling back to a second, competing listener.
var ErrAlreadyRunning = errors.New("ipc: a daemon is already running")

// Serve listens at addr and calls h once per Request on every connection,
// until ctx is cancelled. It never returns nil on any error except a clean
// shutdown via ctx.
//
// A handler panic is recovered per request (not just per connection): it
// is logged and turned into an Err response, and the daemon keeps serving
// every other connection and every subsequent request on the same one —
// per §7's reliability commitment, a bad query must never take the whole
// daemon down.
func Serve(ctx context.Context, addr Addr, h Handler) error {
	l, usingUnix, err := bind(addr)
	if err != nil {
		return err
	}
	defer l.Close()
	if usingUnix {
		defer os.Remove(addr.sockPath())
	} else {
		defer os.Remove(addr.portPath())
	}

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("ipc: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveConn(conn, h)
		}()
	}
}

// bind binds addr's unix socket if possible, falling back to a loopback
// TCP listener (and writing its port to addr.portPath()) if it is not. It
// reports which transport actually got used.
func bind(addr Addr) (l net.Listener, usingUnix bool, err error) {
	if err := os.MkdirAll(addr.CacheDir, 0o700); err != nil {
		return nil, false, fmt.Errorf("ipc: %w", err)
	}

	l, err = bindUnix(addr.sockPath())
	if err == nil {
		return l, true, nil
	}
	if errors.Is(err, ErrAlreadyRunning) {
		return nil, false, err
	}

	// AF_UNIX didn't work on this machine for some other reason — fall
	// back to loopback TCP. The already-running check has to be repeated
	// here: binding 127.0.0.1:0 asks for an ephemeral port and therefore
	// always succeeds, so without this a second daemon would start happily
	// and overwrite the first one's port file, stranding it with a full
	// index resident and no clients.
	if tcpDaemonAlive(addr) {
		return nil, false, fmt.Errorf("%w on the tcp port recorded at %s", ErrAlreadyRunning, addr.portPath())
	}

	l, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, false, fmt.Errorf("ipc: listen: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(addr.portPath(), []byte(strconv.Itoa(port)), 0o600); err != nil {
		l.Close()
		return nil, false, fmt.Errorf("ipc: writing port file: %w", err)
	}
	return l, false, nil
}

// bindUnix binds a unix socket at path, first clearing a stale socket file
// left behind by a crashed daemon: if the path exists but a connect probe
// times out or is refused, it is safe to remove and rebind. If the path
// exists AND something answers, that is a real running daemon and bindUnix
// returns ErrAlreadyRunning instead of stealing its socket.
func bindUnix(path string) (net.Listener, error) {
	if _, statErr := os.Stat(path); statErr == nil {
		if c, dialErr := net.DialTimeout("unix", path, dialProbeTimeout); dialErr == nil {
			c.Close()
			return nil, fmt.Errorf("%w at %s", ErrAlreadyRunning, path)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("ipc: removing stale socket %s: %w", path, err)
		}
	}
	return net.Listen("unix", path)
}

// tcpDaemonAlive reports whether addr's port file records a port that a
// live scry daemon is still answering on. It is the TCP counterpart of
// bindUnix's stale-socket probe, and it cleans up after a crashed daemon
// the same way: a port file that names a dead port, or a port now owned by
// some unrelated process, is removed and reported as not alive, so a crash
// never permanently blocks a restart.
//
// Answering the connection is not enough to count as alive. Ephemeral
// ports get recycled, so the process on the other end must also speak this
// protocol — hence the pingOp round trip.
func tcpDaemonAlive(addr Addr) bool {
	data, err := os.ReadFile(addr.portPath())
	if err != nil {
		return false // no port file at all: nothing to conflict with
	}

	discard := func() bool {
		os.Remove(addr.portPath())
		return false
	}

	port := strings.TrimSpace(string(data))
	if n, convErr := strconv.Atoi(port); convErr != nil || n <= 0 || n > 65535 {
		return discard()
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), dialProbeTimeout)
	if err != nil {
		return discard()
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(dialProbeTimeout)); err != nil {
		return discard()
	}
	if err := json.NewEncoder(conn).Encode(Request{Op: pingOp}); err != nil {
		return discard()
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return discard()
	}
	return true
}

// serveConn handles every Request on one connection until the client
// disconnects or sends something that fails to decode as JSON.
func serveConn(conn net.Conn, h Handler) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		if req.Op == pingOp {
			// Answered here so a liveness probe never reaches the
			// Handler, which would otherwise log it as an unknown op.
			if err := enc.Encode(Response{}); err != nil {
				return
			}
			continue
		}
		resp := safeHandle(h, req)
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

// safeHandle calls h and recovers a panic into an Err response, per Serve's
// reliability contract.
func safeHandle(h Handler, req Request) (resp Response) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ipc: handler panic on op %q: %v", req.Op, r)
			resp = Response{Err: fmt.Sprintf("internal error: %v", r)}
		}
	}()
	return h(req)
}

// Client is a connection to a running daemon, opened by Dial.
type Client struct {
	conn net.Conn
	dec  *json.Decoder
	enc  *json.Encoder
	mu   sync.Mutex
}

// Dial connects to the daemon at addr, trying the unix socket first and
// falling back to the TCP port recorded in addr.portPath() if that fails.
// It returns an error if neither transport has anything listening — the
// caller's correct response (see cmd/scry) is to fall back to the
// in-process query path, not to treat this as fatal.
func Dial(addr Addr) (*Client, error) {
	if c, err := net.DialTimeout("unix", addr.sockPath(), dialProbeTimeout); err == nil {
		return newClient(c), nil
	}

	data, err := os.ReadFile(addr.portPath())
	if err != nil {
		return nil, fmt.Errorf("ipc: no daemon listening at %s", addr.CacheDir)
	}
	port := strings.TrimSpace(string(data))
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, dialProbeTimeout)
	if err != nil {
		return nil, fmt.Errorf("ipc: no daemon listening at %s: %w", addr.CacheDir, err)
	}
	return newClient(c), nil
}

func newClient(conn net.Conn) *Client {
	return &Client{conn: conn, dec: json.NewDecoder(conn), enc: json.NewEncoder(conn)}
}

// Call sends req and waits for the matching Response. It is safe for
// concurrent use; calls are serialized over the one underlying connection.
func (c *Client) Call(req Request) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.enc.Encode(req); err != nil {
		return Response{}, fmt.Errorf("ipc: send: %w", err)
	}
	var resp Response
	if err := c.dec.Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("ipc: receive: %w", err)
	}
	return resp, nil
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
