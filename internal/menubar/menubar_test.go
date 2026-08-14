package menubar

import (
	"strings"
	"testing"

	"scry/internal/ipc"
)

func TestFormatCount(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0 files indexed"},
		{1, "1 file indexed"},
		{2, "2 files indexed"},
		{999, "999 files indexed"},
		{1000, "1,000 files indexed"},
		{247391, "247,391 files indexed"},
		{1234567, "1,234,567 files indexed"},
	}
	for _, c := range cases {
		if got := FormatCount(c.n); got != c.want {
			t.Errorf("FormatCount(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestGroupThousandsNegative(t *testing.T) {
	// Never a real input (counts aren't negative), but a formatting
	// helper should degrade gracefully rather than panic.
	if got := groupThousands(-1234); got != "-1,234" {
		t.Errorf("groupThousands(-1234) = %q, want %q", got, "-1,234")
	}
}

func TestTotalEntries(t *testing.T) {
	rows := []ipc.RootStatus{
		{Path: "/a", Entries: 100},
		{Path: "/b", Entries: 247291},
		{Path: "/c", Entries: 0},
	}
	if got := TotalEntries(rows); got != 247391 {
		t.Errorf("TotalEntries() = %d, want 247391", got)
	}
	if got := TotalEntries(nil); got != 0 {
		t.Errorf("TotalEntries(nil) = %d, want 0", got)
	}
}

func TestSearchURL(t *testing.T) {
	if got, want := SearchURL("127.0.0.1:8973"), "http://127.0.0.1:8973/"; got != want {
		t.Errorf("SearchURL() = %q, want %q", got, want)
	}
}

func TestOpenBrowserCmd(t *testing.T) {
	cmd := openBrowserCmd("http://127.0.0.1:8973/")
	if !strings.HasSuffix(cmd.Path, "open") && !strings.Contains(cmd.Path, "open") {
		t.Errorf("openBrowserCmd Path = %q, want it to invoke `open`", cmd.Path)
	}
	if len(cmd.Args) != 2 || cmd.Args[1] != "http://127.0.0.1:8973/" {
		t.Errorf("openBrowserCmd Args = %v, want [open http://127.0.0.1:8973/]", cmd.Args)
	}
}

func TestOpenPathCmd(t *testing.T) {
	cmd := openPathCmd("/Users/jake/.config/scry/config.toml")
	if len(cmd.Args) != 2 || cmd.Args[1] != "/Users/jake/.config/scry/config.toml" {
		t.Errorf("openPathCmd Args = %v", cmd.Args)
	}
}
