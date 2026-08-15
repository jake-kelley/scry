package main

import "testing"

// TestQueryFromArgs pins the contract that the shell's argv split does not
// change the query. This existed as a real bug: runSearch read args[0] and
// discarded the rest, so `scry ext:go path:cmd` searched `ext:go` only. A
// dropped filter widens the result set instead of erroring, so nothing about
// the output looked wrong.
func TestQueryFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"one bare term", []string{"rprt24"}, "rprt24"},
		{"two filters the shell split", []string{"ext:go", "path:cmd"}, "ext:go path:cmd"},
		{"a quoted phrase arrives as one arg, spaces intact", []string{`"foo bar"`}, `"foo bar"`},
		{"a quoted phrase alongside a filter", []string{`"foo bar"`, "ext:go"}, `"foo bar" ext:go`},
		{"negation survives", []string{"report", "!vendor"}, "report !vendor"},
		{"no args is the empty query, not a panic", nil, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := queryFromArgs(tc.args); got != tc.want {
				t.Errorf("queryFromArgs(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
