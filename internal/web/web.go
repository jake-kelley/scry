// Package web implements scry's `--serve` web UI, per
// "everything-macos-design.md" §10 build order step 6: one self-contained
// HTML page (no frameworks, no CDN, no build step, embedded with embed.FS)
// plus a JSON search endpoint, bound to 127.0.0.1 only.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"scry/internal/query"
)

//go:embed index.html
var assets embed.FS

// DefaultAddr is the fixed 127.0.0.1 address --serve binds to when the
// caller doesn't override the port.
const DefaultAddr = "127.0.0.1:8973"

// SearchFunc runs a query and returns ranked results. The daemon supplies
// this (backed by its in-memory shards); package web never touches an
// index.Shard, a qsyntax.Query, or a socket directly.
type SearchFunc func(q string, limit int) ([]query.Result, error)

// resultJSON is the JSON shape returned by /search.
type resultJSON struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix nanos
	IsDir bool   `json:"isDir"`
	Score int    `json:"score"`
}

// NewMux builds the handler: GET / serves the embedded page, GET /search
// runs a query and returns JSON results.
func NewMux(search SearchFunc) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := assets.ReadFile("index.html")
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		limit := 0
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil {
				limit = n
			}
		}

		results, err := search(q, limit)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		out := make([]resultJSON, len(results))
		for i, res := range results {
			out[i] = resultJSON{
				Name: res.Name, Path: res.Path, Size: res.Size,
				MTime: res.MTime, IsDir: res.IsDir, Score: res.Score,
			}
		}
		json.NewEncoder(w).Encode(out)
	})

	return mux
}

// ValidateLoopback reports an error if addr's host is anything other than
// 127.0.0.1 or localhost. Serve always calls this before binding — a
// caller-supplied --serve=host:port can change the port, never the host, so
// this file-search web UI can never end up reachable off the machine it
// runs on regardless of what gets passed on the command line.
func ValidateLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("web: invalid address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("web: refusing to bind non-loopback address %q (127.0.0.1 only)", addr)
	}
	return nil
}

// Serve binds addr (DefaultAddr if empty) and serves the UI until the
// listener errors or the process exits. Intended to run in its own
// goroutine from the daemon.
func Serve(addr string, search SearchFunc) error {
	if addr == "" {
		addr = DefaultAddr
	}
	if err := ValidateLoopback(addr); err != nil {
		return err
	}
	return http.ListenAndServe(addr, NewMux(search))
}
