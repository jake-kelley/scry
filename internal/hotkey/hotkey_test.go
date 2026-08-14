package hotkey

import (
	"errors"
	"runtime"
	"testing"
)

func TestParseValid(t *testing.T) {
	cases := []struct {
		in       string
		wantMods []Mod
		wantKey  string
	}{
		{"alt+space", []Mod{ModAlt}, "space"},
		{"Alt+Space", []Mod{ModAlt}, "space"},
		{"option+space", []Mod{ModAlt}, "space"},
		{"opt+space", []Mod{ModAlt}, "space"},
		{"cmd+shift+k", []Mod{ModShift, ModCmd}, "k"},
		{"shift+cmd+k", []Mod{ModShift, ModCmd}, "k"}, // order in input doesn't matter
		{"command+k", []Mod{ModCmd}, "k"},
		{"control+alt+f5", []Mod{ModCtrl, ModAlt}, "f5"},
		{"space", nil, "space"},
		{"  alt + space  ", []Mod{ModAlt}, "space"},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) error = %v, want nil", c.in, err)
			continue
		}
		if got.Key != c.wantKey {
			t.Errorf("Parse(%q).Key = %q, want %q", c.in, got.Key, c.wantKey)
		}
		if !modsEqual(got.Mods, c.wantMods) {
			t.Errorf("Parse(%q).Mods = %v, want %v", c.in, got.Mods, c.wantMods)
		}
	}
}

func modsEqual(a, b []Mod) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseInvalid(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"alt+alt+space",
		"alt+shift", // modifiers only, no key
		"alt++space",
		"+space",
		"space+",
		"alt+space+k", // two keys
		"nope+space",  // unrecognised modifier token becomes the key, then "space" collides
	}
	for _, in := range cases {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) error = nil, want an error", in)
		}
	}
}

func TestParseOrderNormalized(t *testing.T) {
	a, err := Parse("cmd+alt+ctrl+shift+k")
	if err != nil {
		t.Fatal(err)
	}
	want := []Mod{ModCtrl, ModAlt, ModShift, ModCmd}
	if !modsEqual(a.Mods, want) {
		t.Errorf("Mods = %v, want %v (Control, Option, Shift, Command order)", a.Mods, want)
	}
}

func TestStringRoundTrip(t *testing.T) {
	c, err := Parse("shift+cmd+k")
	if err != nil {
		t.Fatal(err)
	}
	s := c.String()
	c2, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(String()) = %v, %v", c2, err)
	}
	if !modsEqual(c.Mods, c2.Mods) || c.Key != c2.Key {
		t.Errorf("round trip mismatch: %v != %v", c, c2)
	}
}

func TestLabel(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"alt+space", "⌥Space"},
		{"cmd+shift+k", "⇧⌘K"},
		{"ctrl+alt+f5", "⌃⌥F5"},
		{"cmd+return", "⌘↩"},
		{"space", "Space"},
	}
	for _, c := range cases {
		combo, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got := combo.Label(); got != c.want {
			t.Errorf("Parse(%q).Label() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDefaultComboParses(t *testing.T) {
	c, err := Parse(DefaultCombo)
	if err != nil {
		t.Fatalf("Parse(DefaultCombo) = %v", err)
	}
	if c.Key != "space" || !modsEqual(c.Mods, []Mod{ModAlt}) {
		t.Errorf("DefaultCombo parsed as %+v, want alt+space", c)
	}
}

func TestRegisterNotSupportedOffDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin has a real hotkey backend; nothing to assert here without a window server")
	}
	combo, err := Parse(DefaultCombo)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Register(combo, func() {})
	if h != nil {
		t.Errorf("Register() handle = %v, want nil", h)
	}
	if !errors.Is(err, ErrNotSupported) {
		t.Errorf("Register() error = %v, want ErrNotSupported", err)
	}
}
