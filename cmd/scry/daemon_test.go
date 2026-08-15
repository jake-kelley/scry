package main

import (
	"testing"

	"scry/internal/index"
)

// shardAt builds a shard for root carrying an FSEvents position of eid. A
// zero eid is the "crawled but never watched" state these tests are about.
func shardAt(root string, eid uint64) *index.Shard {
	s := index.New(root)
	s.SetLastEID(eid)
	return s
}

func TestMinLastEID(t *testing.T) {
	cases := []struct {
		name   string
		shards []*index.Shard
		want   uint64
	}{
		{"no shards", nil, 0},
		{"single watched shard", []*index.Shard{shardAt("/a", 500)}, 500},
		{"oldest position wins", []*index.Shard{
			shardAt("/a", 900), shardAt("/b", 500), shardAt("/c", 700),
		}, 500},

		// The regression this function was changed for: a freshly added
		// root has no position, and must not drag the combined stream
		// forward to "now" and throw away /a's resume point.
		{"a fresh root does not pin the stream", []*index.Shard{
			shardAt("/a", 500), shardAt("/fresh", 0),
		}, 500},
		{"several fresh roots still do not pin it", []*index.Shard{
			shardAt("/fresh1", 0), shardAt("/a", 900), shardAt("/fresh2", 0), shardAt("/b", 500),
		}, 500},

		// Nothing has a position: starting from now is correct.
		{"all fresh", []*index.Shard{shardAt("/a", 0), shardAt("/b", 0)}, 0},

		{"nil shards are skipped", []*index.Shard{nil, shardAt("/a", 500), nil}, 500},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := minLastEID(tc.shards); got != tc.want {
				t.Errorf("minLastEID() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestUnwatchedRoots(t *testing.T) {
	shards := []*index.Shard{
		shardAt("/watched", 500),
		nil,
		shardAt("/fresh", 0),
		shardAt("/alsofresh", 0),
	}

	got := unwatchedRoots(shards)
	want := []string{"/fresh", "/alsofresh"}

	if len(got) != len(want) {
		t.Fatalf("unwatchedRoots() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unwatchedRoots()[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if r := unwatchedRoots([]*index.Shard{shardAt("/a", 1)}); len(r) != 0 {
		t.Errorf("unwatchedRoots() with nothing fresh = %v, want empty", r)
	}
}
