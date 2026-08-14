package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	d := Default()
	if len(d.Roots) != 0 {
		t.Fatalf("Default() Roots = %v, want empty", d.Roots)
	}
	wantNames := []string{"node_modules", ".git", ".venv", "__pycache__", "build", "dist"}
	if !reflect.DeepEqual(d.Exclude.Names, wantNames) {
		t.Errorf("Default() Exclude.Names = %v, want %v", d.Exclude.Names, wantNames)
	}
	wantGlobs := []string{"*.tmp", "*.o", "*.pyc"}
	if !reflect.DeepEqual(d.Exclude.Globs, wantGlobs) {
		t.Errorf("Default() Exclude.Globs = %v, want %v", d.Exclude.Globs, wantGlobs)
	}
	if d.Index.FollowSymlinks {
		t.Error("Default() Index.FollowSymlinks = true, want false")
	}
	if d.Index.Hidden {
		t.Error("Default() Index.Hidden = true, want false")
	}
}

func TestLoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist", "config.toml")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for missing file", err)
	}
	if !reflect.DeepEqual(c, Default()) {
		t.Errorf("Load() = %+v, want Default() %+v", c, Default())
	}
}

func TestLoadMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("this is not [ valid toml"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want error for malformed TOML")
	}
}

func TestLoadPartialFillsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// Only roots specified; exclude/index sections absent entirely.
	content := `
[[root]]
path = "~/Documents"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(c.Roots) != 1 || c.Roots[0].Path != "~/Documents" {
		t.Errorf("Load() Roots = %v, want one root ~/Documents", c.Roots)
	}
	d := Default()
	if !reflect.DeepEqual(c.Exclude, d.Exclude) {
		t.Errorf("Load() Exclude = %+v, want default %+v", c.Exclude, d.Exclude)
	}
	if !reflect.DeepEqual(c.Index, d.Index) {
		t.Errorf("Load() Index = %+v, want default %+v", c.Index, d.Index)
	}
}

func TestLoadEmptyExcludeBlockDoesNotMeanExcludeNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// Explicit but empty exclude block: absent from the file entirely
	// (BurntSushi/toml has no way to distinguish "present but empty" from
	// "absent" for a table with no keys set), so this should still fill
	// from defaults.
	content := `
[index]
hidden = true
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(c.Exclude.Names) == 0 {
		t.Error("Load() Exclude.Names is empty, want defaults to be filled in")
	}
	if !c.Index.Hidden {
		t.Error("Load() Index.Hidden = false, want true (explicitly set)")
	}
}

func TestDefaultHotkeyCombo(t *testing.T) {
	if got := Default().Hotkey.Combo; got != "alt+space" {
		t.Errorf("Default().Hotkey.Combo = %q, want %q", got, "alt+space")
	}
}

func TestLoadHotkeySection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[hotkey]
combo = "cmd+shift+k"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.Hotkey.Combo != "cmd+shift+k" {
		t.Errorf("Load() Hotkey.Combo = %q, want %q", c.Hotkey.Combo, "cmd+shift+k")
	}
}

func TestLoadHotkeySectionEmptyComboFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// A [hotkey] block present but with no combo key set (BurntSushi/toml
	// can't distinguish this from combo = ""): should normalize to the
	// default, the same way an empty [exclude] block does, not leave the
	// hotkey unparseable.
	content := `
[hotkey]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.Hotkey.Combo != defaultHotkeyCombo {
		t.Errorf("Load() Hotkey.Combo = %q, want default %q", c.Hotkey.Combo, defaultHotkeyCombo)
	}
}

func TestSaveNeverWritesEmptyCombo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	c := Default()
	c.Hotkey.Combo = ""
	if err := Save(path, c); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Hotkey.Combo != defaultHotkeyCombo {
		t.Errorf("round-tripped Hotkey.Combo = %q, want default %q", got.Hotkey.Combo, defaultHotkeyCombo)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.toml")

	want := Default()
	want.Roots = []Root{
		// An empty OfflinePolicy is normalized to "keep" on Save (and
		// again on Load), so what round-trips is "keep", not "".
		{Path: "/home/user/Documents"},
		{Path: "/home/user/code", Exclude: []string{"target", "vendor"}, OfflinePolicy: "keep"},
	}

	if err := Save(path, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want.Roots[0].OfflinePolicy = "keep"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip mismatch:\ngot  = %+v\nwant = %+v", got, want)
	}
}

func TestSaveAtomicNoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	if err := Save(path, Default()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("dir contents = %v, want only [config.toml]", names)
	}
}

func TestSaveMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := Save(path, Default()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}
}

func TestSaveCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "config.toml")
	if err := Save(path, Default()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}
}

func TestAddRootThenSaveDefaultsOfflinePolicyToKeep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	var c Config
	if err := c.AddRoot("/tmp/a"); err != nil {
		t.Fatalf("AddRoot() error = %v", err)
	}
	if c.Roots[0].OfflinePolicy != "" {
		t.Fatalf("AddRoot() OfflinePolicy = %q, want empty before Save", c.Roots[0].OfflinePolicy)
	}

	if err := Save(path, c); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(raw), `offline_policy = "keep"`) {
		t.Fatalf("saved file does not contain offline_policy = \"keep\":\n%s", raw)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Roots[0].OfflinePolicy != "keep" {
		t.Errorf("Load() OfflinePolicy = %q, want %q", got.Roots[0].OfflinePolicy, "keep")
	}
}

func TestSaveExplicitDropOfflinePolicyRoundTripsUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	c := Default()
	c.Roots = []Root{{Path: "/tmp/a", OfflinePolicy: "drop"}}

	if err := Save(path, c); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(raw), `offline_policy = "drop"`) {
		t.Fatalf("saved file does not contain offline_policy = \"drop\":\n%s", raw)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Roots[0].OfflinePolicy != "drop" {
		t.Errorf("Load() OfflinePolicy = %q, want %q", got.Roots[0].OfflinePolicy, "drop")
	}
}

func TestAddRoot(t *testing.T) {
	var c Config
	if err := c.AddRoot("/tmp/a"); err != nil {
		t.Fatalf("AddRoot() error = %v", err)
	}
	if len(c.Roots) != 1 || c.Roots[0].Path != filepath.Clean("/tmp/a") {
		t.Errorf("Roots = %v", c.Roots)
	}

	if err := c.AddRoot("/tmp/a"); err == nil {
		t.Error("AddRoot() duplicate error = nil, want error")
	}
	if len(c.Roots) != 1 {
		t.Errorf("Roots = %v, want unchanged after duplicate add", c.Roots)
	}
}

func TestAddRootCaseInsensitive(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("case-insensitive matching only applies on windows/darwin")
	}
	var c Config
	if err := c.AddRoot("/tmp/A"); err != nil {
		t.Fatalf("AddRoot() error = %v", err)
	}
	if err := c.AddRoot("/tmp/a"); err == nil {
		t.Error("AddRoot() case-insensitive duplicate error = nil, want error")
	}
}

func TestRemoveRoot(t *testing.T) {
	var c Config
	if err := c.AddRoot("/tmp/a"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddRoot("/tmp/b"); err != nil {
		t.Fatal(err)
	}

	removed, err := c.RemoveRoot("/tmp/a")
	if err != nil {
		t.Fatalf("RemoveRoot() error = %v", err)
	}
	if !removed {
		t.Error("RemoveRoot() removed = false, want true")
	}
	if len(c.Roots) != 1 || c.Roots[0].Path != filepath.Clean("/tmp/b") {
		t.Errorf("Roots = %v, want only /tmp/b", c.Roots)
	}

	removed, err = c.RemoveRoot("/tmp/does-not-exist")
	if err != nil {
		t.Fatalf("RemoveRoot() error = %v", err)
	}
	if removed {
		t.Error("RemoveRoot() removed = true, want false for absent root")
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "bare tilde", in: "~", want: home},
		{name: "tilde slash", in: "~/Documents", want: filepath.Join(home, "Documents")},
		{name: "no tilde", in: "/etc/passwd", want: "/etc/passwd"},
		{name: "relative no tilde", in: "code", want: "code"},
		{name: "tilde user unsupported", in: "~jake/Documents", wantErr: true},
		{name: "tilde user bare unsupported", in: "~jake", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandTilde(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ExpandTilde(%q) error = nil, want error", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandTilde(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ExpandTilde(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidateEmptyRootPath(t *testing.T) {
	c := Config{Roots: []Root{{Path: ""}, {Path: "   "}}}
	errs := c.Validate()
	if len(errs) < 2 {
		t.Fatalf("Validate() = %v, want at least 2 errors for empty paths", errs)
	}
}

func TestValidateDuplicateRoots(t *testing.T) {
	c := Config{Roots: []Root{
		{Path: "/tmp/a"},
		{Path: "/tmp/a/"},
	}}
	errs := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "duplicate") {
			found = true
		}
	}
	if !found {
		t.Errorf("Validate() = %v, want a duplicate-root error", errs)
	}
}

func TestValidateOfflinePolicy(t *testing.T) {
	tests := []struct {
		policy  string
		wantErr bool
	}{
		{"keep", false},
		{"drop", false},
		{"", false},
		{"bogus", true},
	}
	for _, tt := range tests {
		c := Config{Roots: []Root{{Path: "/tmp/x", OfflinePolicy: tt.policy}}}
		errs := c.Validate()
		hasPolicyErr := false
		for _, e := range errs {
			if strings.Contains(e.Error(), "offline_policy") {
				hasPolicyErr = true
			}
		}
		if hasPolicyErr != tt.wantErr {
			t.Errorf("Validate() policy %q: got error=%v, want %v (errs=%v)", tt.policy, hasPolicyErr, tt.wantErr, errs)
		}
	}
}

func TestValidateUnparseableGlobs(t *testing.T) {
	c := Config{
		Roots: []Root{
			{Path: "/tmp/x", Exclude: []string{"[unclosed"}},
		},
		Exclude: Exclude{Globs: []string{"[also-unclosed"}},
	}
	errs := c.Validate()
	if len(errs) < 2 {
		t.Fatalf("Validate() = %v, want at least 2 errors for bad globs", errs)
	}
}

func TestValidateGoodConfigHasNoErrors(t *testing.T) {
	c := Default()
	c.Roots = []Root{
		{Path: "/tmp/a", OfflinePolicy: "keep"},
		{Path: "/tmp/b", OfflinePolicy: "drop", Exclude: []string{"*.log"}},
	}
	if errs := c.Validate(); len(errs) != 0 {
		t.Errorf("Validate() = %v, want no errors", errs)
	}
}

func TestExcludePathsParseAndValidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[[root]]
  path = "~/code"
  exclude_paths = ["~/code/vendor-dump"]

[exclude]
  names = ["node_modules"]
  paths = ["~/Library"]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Exclude.Paths; len(got) != 1 || got[0] != "~/Library" {
		t.Errorf("global exclude paths = %v, want [~/Library] verbatim (expansion happens at use)", got)
	}
	if got := cfg.Roots[0].ExcludePaths; len(got) != 1 || got[0] != "~/code/vendor-dump" {
		t.Errorf("per-root exclude paths = %v, want [~/code/vendor-dump]", got)
	}
	if errs := cfg.Validate(); len(errs) != 0 {
		t.Errorf("Validate on a good config returned %v", errs)
	}
}

// TestValidateRejectsRelativeExcludePath covers the trap the key invites:
// "Library" is a valid spelling in [exclude] names and matches nothing at
// all under [exclude] paths, so it has to be an error rather than a
// silently ineffective line.
func TestValidateRejectsRelativeExcludePath(t *testing.T) {
	cfg := Default()
	cfg.Roots = []Root{{Path: "/tmp/x"}}
	cfg.Exclude.Paths = []string{"Library"}

	errs := cfg.Validate()
	if len(errs) == 0 {
		t.Fatal("a bare name in [exclude] paths must be rejected, not silently ignored")
	}
	if !strings.Contains(errs[0].Error(), "absolute") {
		t.Errorf("error should explain the path must be absolute, got: %v", errs[0])
	}

	cfg.Exclude.Paths = []string{"   "}
	if errs := cfg.Validate(); len(errs) == 0 {
		t.Error("an empty exclude path must be rejected")
	}
}

func TestExpandExcludePaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}

	got := ExpandExcludePaths([]string{"~/Library", ""}, []string{filepath.Join(home, "code") + string(filepath.Separator)})
	want := []string{
		filepath.Join(home, "Library"),
		filepath.Join(home, "code"),
	}
	if len(got) != len(want) {
		t.Fatalf("ExpandExcludePaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ExpandExcludePaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRecrawlInterval(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{"unset means default", "", 0, false},
		{"five minutes", "5m", 5 * time.Minute, false},
		{"hours", "2h", 2 * time.Hour, false},
		{"whitespace is not a value", "   ", 0, false},
		{"unparseable", "5 minutes", 0, true},
		{"below the floor", "1s", 0, true},
		{"zero", "0s", 0, true},
		{"negative", "-5m", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Roots = []Root{{Path: "/tmp/x"}}
			cfg.Index.RecrawlInterval = tc.value

			if got := cfg.RecrawlInterval(); got != tc.want {
				t.Errorf("RecrawlInterval() = %v, want %v", got, tc.want)
			}
			errs := cfg.Validate()
			if tc.wantErr && len(errs) == 0 {
				t.Errorf("Validate() accepted %q", tc.value)
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Errorf("Validate() rejected %q: %v", tc.value, errs)
			}
		})
	}
}

// TestRecrawlIntervalRoundTrips guards the reason it is a string: it has
// to survive a Save/Load cycle as "5m", not as a nanosecond count.
func TestRecrawlIntervalRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.Roots = []Root{{Path: "/tmp/x"}}
	cfg.Index.RecrawlInterval = "5m"

	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), `"5m"`) {
		t.Errorf("saved config should contain the literal \"5m\", got:\n%s", raw)
	}

	back, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := back.RecrawlInterval(); got != 5*time.Minute {
		t.Errorf("after round trip RecrawlInterval() = %v, want 5m", got)
	}
}
