package ipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"scry/internal/query"
)

// serveInBackground starts Serve in a goroutine and returns a cancel func
// that stops it and waits for it to actually return.
func serveInBackground(t *testing.T, addr Addr, h Handler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, addr, h) }()

	// Give Serve a moment to actually bind before the test tries to Dial —
	// stat-ing the socket/port path isn't enough proof by itself (a
	// pre-existing stale file at the path would satisfy it before the real
	// listener is up), so poll with a real connection attempt instead.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := Dial(addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve did not shut down after ctx cancel")
		}
	}
}

func TestRoundTrip(t *testing.T) {
	addr := Addr{CacheDir: t.TempDir()}
	stop := serveInBackground(t, addr, func(req Request) Response {
		if req.Op != "search" {
			t.Errorf("Op = %q, want %q", req.Op, "search")
		}
		return Response{Results: []query.Result{{Name: "hit.txt", Path: "/root/hit.txt", Score: 42}}}
	})
	defer stop()

	c, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	resp, err := c.Call(Request{Op: "search", Query: "hit", Limit: 10})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Err != "" {
		t.Fatalf("Response.Err = %q, want empty", resp.Err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Name != "hit.txt" {
		t.Fatalf("Results = %+v, want one hit.txt", resp.Results)
	}
}

func TestRoundTripMultipleCallsOneConnection(t *testing.T) {
	addr := Addr{CacheDir: t.TempDir()}
	calls := 0
	stop := serveInBackground(t, addr, func(req Request) Response {
		calls++
		return Response{Stats: Stats{Roots: []RootStatus{{Path: req.Query, Entries: calls}}}}
	})
	defer stop()

	c, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	for i := 1; i <= 3; i++ {
		resp, err := c.Call(Request{Op: "status", Query: "root"})
		if err != nil {
			t.Fatalf("Call #%d: %v", i, err)
		}
		if got := resp.Stats.Roots[0].Entries; got != i {
			t.Errorf("call #%d: Entries = %d, want %d", i, got, i)
		}
	}
}

func TestDialWithNoDaemonFails(t *testing.T) {
	addr := Addr{CacheDir: t.TempDir()}
	if _, err := Dial(addr); err == nil {
		t.Error("Dial succeeded against an address with no daemon listening")
	}
}

func TestHandlerPanicIsRecovered(t *testing.T) {
	addr := Addr{CacheDir: t.TempDir()}
	stop := serveInBackground(t, addr, func(req Request) Response {
		if req.Query == "boom" {
			panic("simulated handler panic")
		}
		return Response{Results: []query.Result{{Name: "ok"}}}
	})
	defer stop()

	c, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	resp, err := c.Call(Request{Op: "search", Query: "boom"})
	if err != nil {
		t.Fatalf("Call (panic case): %v", err)
	}
	if resp.Err == "" {
		t.Fatal("expected a non-empty Err after a handler panic, got success response")
	}

	// The daemon must keep serving after a handler panic: the same
	// connection should still work for the next request.
	resp, err = c.Call(Request{Op: "search", Query: "fine"})
	if err != nil {
		t.Fatalf("Call after panic: %v", err)
	}
	if resp.Err != "" {
		t.Fatalf("Response.Err = %q after recovery, want empty", resp.Err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Name != "ok" {
		t.Fatalf("Results after recovery = %+v", resp.Results)
	}

	// And a fresh connection should also still work — the panic must not
	// have taken down the listener either.
	c2, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial after panic: %v", err)
	}
	defer c2.Close()
	if resp, err := c2.Call(Request{Op: "search", Query: "fine"}); err != nil || resp.Err != "" {
		t.Fatalf("Call on new connection after panic: resp=%+v err=%v", resp, err)
	}
}

func TestStaleSocketIsRemovedAndReplaced(t *testing.T) {
	dir := t.TempDir()
	addr := Addr{CacheDir: dir}

	// Simulate what a crashed daemon leaves behind: a file at the socket
	// path with nothing listening on it. (A graceful net.Listen(...).Close()
	// on this platform removes the socket file itself, so a real orphaned
	// socket can only be produced by a hard crash — a plain leftover file
	// exercises the same "stat succeeds, dial fails" code path in bindUnix
	// without needing to simulate one.)
	if err := os.WriteFile(addr.sockPath(), nil, 0o600); err != nil {
		t.Fatalf("WriteFile (setup): %v", err)
	}

	stop := serveInBackground(t, addr, func(req Request) Response {
		return Response{Results: []query.Result{{Name: "alive"}}}
	})
	defer stop()

	c, err := Dial(addr)
	if err != nil {
		t.Fatalf("Dial after stale socket cleanup: %v", err)
	}
	defer c.Close()

	resp, err := c.Call(Request{Op: "search"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Name != "alive" {
		t.Fatalf("Results = %+v, want one 'alive'", resp.Results)
	}
}

func TestServeReturnsErrAlreadyRunningAgainstALiveDaemon(t *testing.T) {
	addr := Addr{CacheDir: t.TempDir()}
	stop := serveInBackground(t, addr, func(req Request) Response { return Response{} })
	defer stop()

	// A second Serve at the same address must not steal the socket out
	// from under the first: it should fail fast rather than falling back
	// to a second, competing TCP listener.
	err := Serve(context.Background(), addr, func(req Request) Response { return Response{} })
	if err == nil {
		t.Fatal("second Serve at the same address succeeded, want ErrAlreadyRunning")
	}
	if !isErrAlreadyRunning(err) {
		t.Errorf("Serve error = %v, want it to wrap ErrAlreadyRunning", err)
	}
}

func isErrAlreadyRunning(err error) bool {
	for err != nil {
		if err == ErrAlreadyRunning {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestTCPFallbackWhenUnixUnavailable(t *testing.T) {
	// bind() falls back to TCP whenever binding the unix socket fails for
	// a reason other than "already running" — exercised directly here
	// since forcing AF_UNIX itself to fail on this machine is not
	// portable. A *non-empty* directory occupying the socket path makes
	// net.Listen("unix", ...) fail, and also makes bindUnix's stale-socket
	// cleanup (os.Remove) fail too (a non-empty directory can't be
	// removed), so the error that reaches bind() is a plain bind failure,
	// not ErrAlreadyRunning — the same shape a genuinely AF_UNIX-incapable
	// platform would produce.
	dir := t.TempDir()
	addr := Addr{CacheDir: dir}
	if err := os.MkdirAll(addr.sockPath(), 0o700); err != nil {
		t.Fatalf("mkdir sock path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(addr.sockPath(), "occupied"), nil, 0o600); err != nil {
		t.Fatalf("write inside sock path: %v", err)
	}

	l, usingUnix, err := bind(addr)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer l.Close()
	if usingUnix {
		t.Fatal("bind reported usingUnix=true with a directory occupying the socket path")
	}

	portBytes, err := os.ReadFile(addr.portPath())
	if err != nil {
		t.Fatalf("expected a port file after TCP fallback: %v", err)
	}
	if len(portBytes) == 0 {
		t.Error("port file is empty")
	}

	tcpAddr := l.Addr().(*net.TCPAddr)
	if filepath.Base(addr.portPath()) != portFile {
		t.Fatalf("unexpected port file name %q", addr.portPath())
	}
	if tcpAddr.IP.String() != "127.0.0.1" && !tcpAddr.IP.IsLoopback() {
		t.Errorf("TCP fallback bound to non-loopback address %v", tcpAddr)
	}
}
